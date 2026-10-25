package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type regressionTransport func(*http.Request) (*http.Response, error)

func (f regressionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func upstreamFixture(t *testing.T, body, contentType string) {
	t.Helper()
	t.Setenv("LLM_API_KEY", "fake-key")
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", "http://unused.invalid")
	previous := http.DefaultTransport
	http.DefaultTransport = regressionTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func sse(value any) string {
	data, _ := json.Marshal(value)
	return "data: " + string(data) + "\n\n"
}

func toolChunk(index int, id, name, arguments, finish string) string {
	return sse(map[string]any{"choices": []any{map[string]any{
		"delta":         map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": id, "function": map[string]string{"name": name, "arguments": arguments}}}},
		"finish_reason": finish,
	}}})
}

func TestFragmentedToolCallsAreOrderedAndPreserved(t *testing.T) {
	body := toolChunk(1, "second", "read_file", `{"target_file":`, "") +
		toolChunk(0, "first", "list_dir", `{}`, "") +
		toolChunk(1, "", "", `"a.txt"}`, "tool_calls") + "data: [DONE]\n\n"
	upstreamFixture(t, body, "text/event-stream")
	var events []Event
	if err := streamCompletion(nil, nil, "model", func(event Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].ID != "first" || events[1].ID != "second" || string(events[1].Arguments) != `{"target_file":"a.txt"}` || events[2].FinishReason != FinishToolCalls {
		t.Fatalf("incorrect tool assembly: %+v", events)
	}
}

func TestInvalidStreamsNeverEmitExecutableTools(t *testing.T) {
	for _, test := range []struct{ name, body, contentType string }{
		{"empty", "", "text/event-stream"},
		{"wrong content type", `{}`, "application/json"},
		{"missing terminal finish", sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "partial"}}}}) + "data: [DONE]\n\n", "text/event-stream"},
		{"missing DONE", toolChunk(0, "call", "list_dir", `{}`, "tool_calls"), "text/event-stream"},
		{"invalid JSON", "data: invalid\n\ndata: [DONE]\n\n", "text/event-stream"},
		{"upstream error", "data: {\"error\":{\"message\":\"failure\"}}\n\ndata: [DONE]\n\n", "text/event-stream"},
		{"bad arguments", toolChunk(0, "call", "list_dir", `{broken`, "tool_calls") + "data: [DONE]\n\n", "text/event-stream"},
		{"null arguments", toolChunk(0, "call", "list_dir", `null`, "tool_calls") + "data: [DONE]\n\n", "text/event-stream"},
		{"array arguments", toolChunk(0, "call", "list_dir", `[]`, "tool_calls") + "data: [DONE]\n\n", "text/event-stream"},
		{"missing id", toolChunk(0, "", "list_dir", `{}`, "tool_calls") + "data: [DONE]\n\n", "text/event-stream"},
		{"negative index", toolChunk(-1, "call", "list_dir", `{}`, "tool_calls") + "data: [DONE]\n\n", "text/event-stream"},
		{"truncated generation", toolChunk(0, "call", "list_dir", `{}`, "length") + "data: [DONE]\n\n", "text/event-stream"},
		{"bad second tool", toolChunk(0, "ok", "list_dir", `{}`, "") + toolChunk(1, "bad", "write_file", `{`, "tool_calls") + "data: [DONE]\n\n", "text/event-stream"},
		{"oversize frame", "data: " + strings.Repeat("x", 1024*1024+1) + "\n\n", "text/event-stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstreamFixture(t, test.body, test.contentType)
			var events []Event
			err := streamCompletion(nil, nil, "model", func(event Event) { events = append(events, event) })
			if err == nil {
				t.Fatal("invalid stream accepted")
			}
			wantRetryable := test.name == "empty" || test.name == "missing terminal finish" || test.name == "missing DONE"
			code, retryable := classifyCompletionError(err)
			wantCode := codeInternal
			if wantRetryable {
				wantCode = codeUpstream5xx
			}
			if retryable != wantRetryable || code != wantCode {
				t.Fatalf("incorrect stream classification: %s/%t", code, retryable)
			}
			for _, event := range events {
				if event.Type == EvToolCall || event.Type == EvDone {
					t.Fatalf("invalid stream emitted %+v", event)
				}
			}
		})
	}
}

func TestProcessFailureHasErrorFinish(t *testing.T) {
	upstreamFixture(t, "", "text/event-stream")
	var events []Event
	processRequest(ChatRequest{}, func(event Event) { events = append(events, event) })
	if len(events) != 2 || events[0].Type != EvError || events[1].FinishReason != "error" {
		t.Fatalf("failure looked successful: %+v", events)
	}
}

func TestHTTPValidation(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "mock")
	for _, test := range []struct {
		method, body string
		status       int
	}{
		{"GET", "", 405}, {"POST", "{", 400}, {"POST", `{}`, 400},
		{"POST", `{"messages":[{"role":"system","content":"x"}]}`, 400},
		{"POST", `{"messages":[{"role":"user","content":"x"}]} {}`, 400},
		{"POST", `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 2*1024*1024) + `"}]}`, 413},
		{"POST", `{"messages":[{"role":"user","content":"x"}]}`, 200},
	} {
		request := httptest.NewRequest(test.method, "/chat/stream", strings.NewReader(test.body))
		response := httptest.NewRecorder()
		handleChatStream(response, request)
		if response.Code != test.status {
			t.Errorf("%s request: expected %d, got %d", test.method, test.status, response.Code)
		}
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_API_KEY", "fake-key")
	started, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"started"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	t.Setenv("LLM_BASE_URL", upstream.URL)
	backend := httptest.NewServer(http.HandlerFunc(handleChatStream))
	defer backend.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", backend.URL, strings.NewReader(`{"messages":[{"role":"user","content":"test"}]}`))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive cancellation")
	}
}
