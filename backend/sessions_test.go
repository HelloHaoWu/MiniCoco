package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func sessionFixture(t *testing.T) *sessionService {
	t.Helper()
	service, err := newSessionService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previous := activeSessions
	activeSessions = service
	t.Cleanup(func() { activeSessions = previous })
	return service
}

func deltaResponse(t *testing.T, input map[string]any) (*httptest.ResponseRecorder, []Event) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handleChatStream(response, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(data)))
	if response.Code != 200 {
		return response, nil
	}
	if response.Header().Get("Content-Type") == "application/json" {
		var envelope struct {
			Events []Event `json:"events"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return response, envelope.Events
	}
	return response, decodeEvents(t, response.Body)
}

func requireRoundFinish(t *testing.T, response *httptest.ResponseRecorder, events []Event, finish string) {
	t.Helper()
	if response.Code != 200 || len(events) == 0 || events[len(events)-1].FinishReason != finish {
		t.Fatalf("unexpected round: HTTP %d, %+v, %s", response.Code, events, response.Body.String())
	}
}

func TestSessionIncrementalPersistenceAndSuccessfulReplay(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	first := map[string]any{"roundId": "first-stable", "query": "inspect workspace", "workspace": "/tmp", "stream": false}
	response, original := deltaResponse(t, first)
	requireRoundFinish(t, response, original, FinishToolCalls)
	sid := response.Header().Get("X-Session-Id")
	if sid != initialSessionID("first-stable") {
		t.Fatal("first round did not return stable session identity")
	}
	transcript := service.transcript.(*JSONLTranscriptStore)
	path, _ := transcript.path(sid)
	before, _ := os.ReadFile(path)
	if rows := transcriptLines(t, transcript, sid); len(rows) != 3 || rows[0]["role"] != "user" || rows[1]["status"] != "pending" || rows[2]["status"] != "final" {
		t.Fatalf("expected user/pending/final: %#v", rows)
	}
	for attempt := 0; attempt < 3; attempt++ {
		first["stream"] = attempt%2 == 0
		if attempt == 2 {
			first["sessionId"] = sid
		}
		response, events := deltaResponse(t, first)
		requireRoundFinish(t, response, events, FinishToolCalls)
		if response.Header().Get("X-Session-Id") != sid || !reflect.DeepEqual(events, original) {
			t.Fatal("replay changed identity or events")
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("success retry appended transcript records")
	}
	metadata, _ := service.meta.GetSession(context.Background(), sid)
	if metadata.TotalTokens != 2 || metadata.MessageCount != 2 {
		t.Fatalf("replay double-counted usage: %#v", metadata)
	}
	tools := map[string]any{"sessionId": sid, "roundId": "tool-stable", "toolUseResults": []ToolUseResult{{ID: "call_mock_1", Name: "list_dir", Output: "listing"}}, "workspace": "/tmp"}
	response, events := deltaResponse(t, tools)
	requireRoundFinish(t, response, events, FinishStop)
	history, err := service.transcript.Latest(context.Background(), sid, 10)
	if err != nil || len(history) != 4 || history[2]["role"] != "tool" || history[2]["tool_call_id"] != "call_mock_1" {
		t.Fatalf("incremental history: %#v, %v", history, err)
	}
	metadata, _ = service.meta.GetSession(context.Background(), sid)
	if metadata.TotalTokens != 4 || metadata.MessageCount != 4 {
		t.Fatalf("usage did not grow per round: %#v", metadata)
	}
	// Reopen both stores: no in-memory round cache may be needed for a replay.
	reopened, err := newSessionService(filepath.Dir(service.meta.(*JSONMetaStore).path))
	if err != nil {
		t.Fatal(err)
	}
	activeSessions = reopened
	t.Setenv("LLM_PROVIDER", "unknown") // A replay must not call a provider.
	response, events = deltaResponse(t, tools)
	requireRoundFinish(t, response, events, FinishStop)
	metadata, _ = reopened.meta.GetSession(context.Background(), sid)
	if metadata.TotalTokens != 4 {
		t.Fatal("restart replay double-counted usage")
	}
	list := httptest.NewRecorder()
	handleSessions(list, httptest.NewRequest("GET", "/sessions", nil))
	if list.Code != 200 || !strings.Contains(list.Body.String(), sid) {
		t.Fatalf("list failed: %s", list.Body.String())
	}
	info := httptest.NewRecorder()
	handleSessions(info, httptest.NewRequest("GET", "/sessions/"+sid, nil))
	if info.Code != 200 || !strings.Contains(info.Body.String(), `"canResume":true`) {
		t.Fatalf("resume metadata failed: %s", info.Body.String())
	}
}

func TestSessionTransientRetriesReuseOnePendingAndReportUpstreamUsage(t *testing.T) {
	upstreamFixture(t, "", "")
	service := sessionFixture(t)
	var calls atomic.Int32
	http.DefaultTransport = regressionTransport(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Messages) != 2 || payload.Messages[1]["role"] != "user" {
			t.Fatalf("retry duplicated input: %#v", payload.Messages)
		}
		status, body := 503, `{"error":"temporary"}`
		if calls.Add(1) >= 3 {
			status = 200
			body = sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "OK"}, "finish_reason": "stop"}}}) + sse(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}}) + "data: [DONE]\n\n"
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	input := map[string]any{"roundId": "retry-stable", "query": "ask"}
	for attempt := 0; attempt < 3; attempt++ {
		response, events := deltaResponse(t, input)
		if attempt < 2 {
			requireRoundFinish(t, response, events, "error")
			if events[0].Retryable == nil || !*events[0].Retryable {
				t.Fatal("transient failure not retryable")
			}
			rows := transcriptLines(t, service.transcript.(*JSONLTranscriptStore), initialSessionID("retry-stable"))
			if len(rows) != 2 || rows[1]["status"] != "pending" {
				t.Fatal("failed retry duplicated reservation")
			}
			if history, _ := service.transcript.Latest(context.Background(), initialSessionID("retry-stable"), 10); len(history) != 0 {
				t.Fatal("failed input became committed history")
			}
		} else {
			requireRoundFinish(t, response, events, FinishStop)
		}
	}
	response, events := deltaResponse(t, input)
	requireRoundFinish(t, response, events, FinishStop)
	if calls.Load() != 3 {
		t.Fatal("successful round called model again")
	}
	session, _ := service.meta.GetSession(context.Background(), initialSessionID("retry-stable"))
	if session.PromptTokens != 3 || session.CompletionTokens != 2 || session.TotalTokens != 5 {
		t.Fatalf("upstream usage not aggregated exactly once: %#v", session)
	}
}

type interruptedMeta struct {
	MetaStore
	fail bool
}

func (store *interruptedMeta) CommitRoundUsage(ctx context.Context, sid, roundID string, usage Usage, count int) error {
	if store.fail {
		return errors.New("injected metadata write failure")
	}
	return store.MetaStore.CommitRoundUsage(ctx, sid, roundID, usage, count)
}

func TestSessionRecoversTranscriptCommitBeforeUsageCommit(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	originalMeta := service.meta.(*JSONMetaStore)
	service.meta = &interruptedMeta{MetaStore: originalMeta, fail: true}
	input := map[string]any{"roundId": "commit-gap", "query": "ask"}
	response, events := deltaResponse(t, input)
	requireRoundFinish(t, response, events, "error")
	for _, event := range events {
		if event.Type == EvToolCall {
			t.Fatal("tool exposed before usage commit")
		}
	}
	round, err := service.transcript.Round(context.Background(), initialSessionID("commit-gap"), "commit-gap")
	if err != nil || !round.Final {
		t.Fatalf("transcript was not committed: %#v, %v", round, err)
	}
	reopened, err := newSessionService(filepath.Dir(originalMeta.path))
	if err != nil {
		t.Fatal(err)
	}
	activeSessions = reopened
	t.Setenv("LLM_PROVIDER", "unknown")
	for i := 0; i < 3; i++ {
		response, events = deltaResponse(t, input)
		requireRoundFinish(t, response, events, FinishToolCalls)
	}
	session, _ := reopened.meta.GetSession(context.Background(), initialSessionID("commit-gap"))
	if session.TotalTokens != 2 || session.MessageCount != 2 {
		t.Fatalf("commit recovery counted incorrectly: %#v", session)
	}
}

func TestSessionRejectsConflictsAndUnmatchedToolResults(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	response, events := deltaResponse(t, map[string]any{"roundId": "validation-first", "query": "ask", "workspace": "/tmp"})
	requireRoundFinish(t, response, events, FinishToolCalls)
	sid := response.Header().Get("X-Session-Id")
	transcript := service.transcript.(*JSONLTranscriptStore)
	path, _ := transcript.path(sid)
	before, _ := os.ReadFile(path)
	for _, input := range []map[string]any{
		{"sessionId": sid, "roundId": "validation-first", "query": "different", "workspace": "/tmp"},
		{"sessionId": sid, "roundId": "new-query", "query": "unsafe continuation", "workspace": "/tmp"},
		{"sessionId": sid, "roundId": "bad-tool", "toolUseResults": []ToolUseResult{{ID: "wrong", Name: "list_dir", Output: "x"}}, "workspace": "/tmp"},
		{"sessionId": sid, "roundId": "bad-name", "toolUseResults": []ToolUseResult{{ID: "call_mock_1", Name: "run_command", Output: "x"}}, "workspace": "/tmp"},
	} {
		response, events := deltaResponse(t, input)
		requireRoundFinish(t, response, events, "error")
		if events[len(events)-2].Code != codeBadRequest {
			t.Fatal("invalid input treated as retryable model error")
		}
	}
	response, _ = deltaResponse(t, map[string]any{"sessionId": sid, "roundId": "workspace-change", "query": "ask", "workspace": "/other"})
	if response.Code != 409 {
		t.Fatal("session workspace changed")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("invalid request modified transcript")
	}
	info := httptest.NewRecorder()
	handleSessions(info, httptest.NewRequest("GET", "/sessions/"+sid, nil))
	if !strings.Contains(info.Body.String(), `"canResume":false`) {
		t.Fatal("unfinished tool batch was offered for replay")
	}
	for _, input := range []map[string]any{
		{"query": "missing round"},
		{"roundId": "both", "query": "ask", "toolUseResults": []ToolUseResult{}},
		{"roundId": "legacy", "messages": []Message{{"role": "user", "content": "old protocol"}}},
		{"roundId": "empty-results", "sessionId": sid, "toolUseResults": []ToolUseResult{}},
		{"roundId": "large-tool", "sessionId": sid, "toolUseResults": []ToolUseResult{{ID: "x", Name: "list_dir", Output: strings.Repeat("\n", maxToolOutputBytes)}}},
	} {
		if response, _ := deltaResponse(t, input); response.Code != 400 {
			t.Fatalf("invalid protocol accepted: %s", response.Body.String())
		}
	}
	response, _ = deltaResponse(t, map[string]any{"sessionId": "s_missing", "roundId": "missing-session", "query": "ask"})
	if response.Code != 404 {
		t.Fatal("explicit unknown session was silently created")
	}
}

func TestSessionBudgetReservesToolResultsBeforeExposingCalls(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	response, events := deltaResponse(t, map[string]any{"roundId": "reserve-budget", "query": strings.Repeat("x", maxHistoryBytes-32*1024)})
	requireRoundFinish(t, response, events, "error")
	for _, event := range events {
		if event.Type == EvToolCall {
			t.Fatal("unbudgeted tool call exposed")
		}
	}
	history, err := service.transcript.Latest(context.Background(), initialSessionID("reserve-budget"), 10)
	if err != nil || len(history) != 0 {
		t.Fatal("failed budget round became committed history")
	}
}

func TestSessionConcurrentDuplicateReturnsBusyWithoutSecondModelCall(t *testing.T) {
	upstreamFixture(t, "", "")
	service := sessionFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	http.DefaultTransport = regressionTransport(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-release
		body := sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "ok"}, "finish_reason": "stop"}}}) + "data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	input := map[string]any{"roundId": "concurrent", "query": "ask"}
	data, _ := json.Marshal(input)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handleChatStream(response, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(data)))
		done <- response
	}()
	<-started
	response, _ := deltaResponse(t, input)
	if response.Code != 425 {
		close(release)
		<-done
		t.Fatalf("concurrent duplicate returned %d", response.Code)
	}
	close(release)
	if response := <-done; response.Code != 200 {
		t.Fatal(response.Code)
	}
	response, events := deltaResponse(t, input)
	requireRoundFinish(t, response, events, FinishStop)
	if calls.Load() != 1 {
		t.Fatal("duplicate ran a second model request")
	}
	if rows := transcriptLines(t, service.transcript.(*JSONLTranscriptStore), initialSessionID("concurrent")); len(rows) != 3 {
		t.Fatal("duplicate wrote extra records")
	}
}

func TestSessionFailedInputIsExcludedAndOldPendingCannotRunAfterNewCommit(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "unknown")
	service := sessionFixture(t)
	response, events := deltaResponse(t, map[string]any{"roundId": "failed-input", "query": "failed query"})
	requireRoundFinish(t, response, events, "error")
	sid := response.Header().Get("X-Session-Id")
	t.Setenv("LLM_PROVIDER", "mock")
	response, events = deltaResponse(t, map[string]any{"sessionId": sid, "roundId": "new-input", "query": "new query"})
	requireRoundFinish(t, response, events, FinishToolCalls)
	history, err := service.transcript.Latest(context.Background(), sid, 10)
	if err != nil || len(history) != 2 || history[0]["content"] != "new query" {
		t.Fatalf("failed query poisoned later history: %#v, %v", history, err)
	}
	response, events = deltaResponse(t, map[string]any{"sessionId": sid, "roundId": "failed-input", "query": "failed query"})
	requireRoundFinish(t, response, events, "error")
	if events[0].Code != codeBadRequest || !strings.Contains(events[0].Message, "stale") {
		t.Fatal("old pending round ran against a changed history")
	}
}
