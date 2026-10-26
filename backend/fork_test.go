package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForkBoundaryKeepsNativeAndXMLPairs(t *testing.T) {
	calls := Message{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "a", "type": "function", "function": map[string]any{"name": "list_dir", "arguments": "{}"}}, {"id": "b", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}}
	for _, formatter := range []Formatter{NativeFormatter{}, XmlFormatter{}} {
		raw := []Message{{"role": "user", "content": "first"}, {"role": "assistant", "content": "first answer"}, {"role": "user", "content": "inspect"}, calls}
		results, err := formatter.Format([]ToolUseResult{{ID: "a", Name: "list_dir", Output: "a"}, {ID: "b", Name: "read_file", Output: "b"}})
		if err != nil {
			t.Fatal(err)
		}
		completed := append(append([]Message{}, raw...), results...)
		index, end, err := resolveForkBoundary(completed, "assistant:2")
		if err != nil || index != 3 || end != len(completed) {
			t.Fatal(index, end, err)
		}
		index, end, err = resolveForkBoundary(raw, "")
		if err != nil || index != 1 || end != 2 {
			t.Fatal("pending call did not retreat", index, end, err)
		}
		if _, _, err := resolveForkBoundary(raw, "message:1"); err == nil {
			t.Fatal("user boundary allowed")
		}
		if _, _, err := resolveForkBoundary([]Message{{"role": "user", "content": "q"}, calls}, ""); err == nil {
			t.Fatal("unpaired only call allowed")
		}
	}
}

func runForkPersistence(t *testing.T, factory contractFactory) {
	ctx := context.Background()
	open := factory(t)
	stores := open()
	defer func() { stores.close() }()
	previous := activeSessions
	defer func() { activeSessions = previous }()
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	source := Session{ID: "s_fork_source", Model: "mock", Workspace: "/tmp", MessageCount: 6, PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	if _, err := stores.meta.CreateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	raw := []Message{}
	for i := 1; i <= 3; i++ {
		raw = append(raw, Message{"role": "user", "content": fmt.Sprint("q", i)}, Message{"role": "assistant", "content": fmt.Sprint("a", i)})
	}
	if err := stores.transcript.Append(ctx, source.ID, "source", raw); err != nil {
		t.Fatal(err)
	}
	summary := Summary{OriginStartID: "message:1", OriginEndID: "message:4", Content: "saved context", PrefixCount: 4, PrefixHash: historyDigest(raw[:4]), Usage: Usage{3, 2, 5}}
	summary.UsageID = "\x00summary:" + historyDigest([]Message{{"hash": summary.PrefixHash, "content": summary.Content}})
	if err := stores.transcript.SaveSummary(ctx, source.ID, summary); err != nil {
		t.Fatal(err)
	}
	before, _ := stores.meta.GetSession(ctx, source.ID)
	original, _ := stores.transcript.RawLatest(ctx, source.ID, allHistory)
	hash := historyDigest(original)
	forked, err := activeSessions.fork(ctx, ForkRequest{SessionID: source.ID, ForkMessageID: "assistant:2"})
	if err != nil {
		t.Fatal(err)
	}
	if forked.ResolvedForkMessageID != "message:4" || forked.CopiedThroughMessageID != "message:4" {
		t.Fatal(forked)
	}
	child, err := stores.meta.GetSession(ctx, forked.NewSessionID)
	if err != nil || child.ParentSessionID != nil || child.ForkedFrom != source.ID || child.MessageCount != 4 || child.TotalTokens != 0 {
		t.Fatal(child, err)
	}
	copied, _ := stores.transcript.RawLatest(ctx, child.ID, allHistory)
	if historyDigest(copied) != historyDigest(raw[:4]) {
		t.Fatal(copied)
	}
	view, err := stores.transcript.Latest(ctx, child.ID, allHistory)
	if err != nil || len(view) != 2 {
		t.Fatal(view, err)
	}
	if err := activeSessions.reconcile(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	child, _ = stores.meta.GetSession(ctx, child.ID)
	if child.TotalTokens != 0 {
		t.Fatal("inherited supplier usage", child)
	}
	after, _ := stores.meta.GetSession(ctx, source.ID)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("source metadata changed")
	}
	stores.close()
	stores = open()
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	child, err = stores.meta.GetSession(ctx, child.ID)
	if err != nil || child.ForkedFrom != source.ID || child.ParentSessionID != nil {
		t.Fatal(child, err)
	}
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	response, events := deltaResponse(t, map[string]any{"sessionId": child.ID, "roundId": "fork-next", "query": "continue", "workspace": "/tmp", "model": "mock", "stream": false})
	requireRoundFinish(t, response, events, FinishToolCalls)
	unchanged, _ := stores.transcript.RawLatest(ctx, source.ID, allHistory)
	if historyDigest(unchanged) != hash {
		t.Fatal("parent history changed")
	}
}
func TestFileForkPersistence(t *testing.T) { runForkPersistence(t, fileContractFactory) }

func TestForkHTTPBusyAndUnresolved(t *testing.T) {
	service := sessionFixture(t)
	sid := "s_busy"
	if _, err := service.meta.CreateSession(context.Background(), Session{ID: sid}); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := service.coord.Lock(context.Background(), "lock:session:"+sid, time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	send := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleFork(w, httptest.NewRequest("POST", "/fork", strings.NewReader(`{"sessionId":"s_busy"}`)))
		return w
	}
	response := send()
	if response.Code != 409 || !strings.Contains(response.Body.String(), "FORK_SESSION_BUSY") {
		t.Fatal(response.Code, response.Body.String())
	}
	unlock()
	response = send()
	if response.Code != 400 || !strings.Contains(response.Body.String(), "FORK_BOUNDARY_UNRESOLVABLE") {
		t.Fatal(response.Code, response.Body.String())
	}
}

type failForkCreate struct{ MetaStore }

func (store failForkCreate) CreateSession(ctx context.Context, session Session) (string, error) {
	if session.ForkedFrom != "" {
		return "", fmt.Errorf("injected metadata failure")
	}
	return store.MetaStore.CreateSession(ctx, session)
}
func TestFailedForkNeverPublishesPartialSession(t *testing.T) {
	service := sessionFixture(t)
	ctx := context.Background()
	sid := "s_fork_failure"
	if _, err := service.meta.CreateSession(ctx, Session{ID: sid, Model: "mock"}); err != nil {
		t.Fatal(err)
	}
	raw := []Message{{"role": "user", "content": "q"}, {"role": "assistant", "content": "a"}}
	if err := service.transcript.Append(ctx, sid, "r", raw); err != nil {
		t.Fatal(err)
	}
	service.meta = failForkCreate{service.meta}
	if _, err := service.fork(ctx, ForkRequest{SessionID: sid}); err == nil {
		t.Fatal("expected failure")
	}
	sessions, err := service.meta.ListSessions(ctx, "")
	if err != nil || len(sessions) != 1 || sessions[0].ID != sid {
		t.Fatal(sessions, err)
	}
	unchanged, _ := service.transcript.RawLatest(ctx, sid, allHistory)
	if historyDigest(unchanged) != historyDigest(raw) {
		t.Fatal("source altered")
	}
}
