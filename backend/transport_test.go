package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func requestEvents(t *testing.T, body string, buffered bool) []Event {
	t.Helper()
	response := httptest.NewRecorder()
	handleChatStream(response, httptest.NewRequest("POST", "/chat/stream", strings.NewReader(body)))
	if response.Code != 200 {
		t.Fatalf("unexpected status: %d", response.Code)
	}
	if !buffered {
		if response.Header().Get("Content-Type") != "text/event-stream" {
			t.Fatal("missing SSE content type")
		}
		return decodeEvents(t, response.Body)
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatal("missing JSON content type")
	}
	var envelope struct {
		Events []Event `json:"events"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Events
}

func TestBufferedAndStreamingEventsAreEquivalent(t *testing.T) {
	preserveMockBudget(t)
	for _, test := range []struct{ name, provider, role string }{
		{"tool request", "mock", "user"}, {"final answer", "mock", "tool"}, {"classified failure", "unknown", "user"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LLM_PROVIDER", test.provider)
			base := `{"messages":[{"role":"` + test.role + `","content":"test"}]`
			missing := requestEvents(t, base+`}`, false)
			explicit := requestEvents(t, base+`,"stream":true}`, false)
			buffered := requestEvents(t, base+`,"stream":false}`, true)
			if !reflect.DeepEqual(missing, explicit) || !reflect.DeepEqual(explicit, buffered) {
				t.Fatal("transport changed event semantics")
			}
		})
	}
	response := httptest.NewRecorder()
	handleChatStream(response, httptest.NewRequest("POST", "/chat/stream", strings.NewReader(`{"messages":[{"role":"user","content":"test"}],"stream":"false"}`)))
	if response.Code != 400 {
		t.Fatal("non-boolean stream flag accepted")
	}
}

func TestBufferedFlakyUsesSameProcessBudget(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	t.Setenv("LLM_MOCK_MODE", "flaky")
	t.Setenv("LLM_MOCK_FLAKY_FAILS", "2")
	if err := configureMockFaults(); err != nil {
		t.Fatal(err)
	}
	for attempt, buffered := range []bool{false, true, true, false} {
		role := "user"
		if attempt == 3 {
			role = "tool"
		}
		body, _ := json.Marshal(map[string]any{"messages": []any{map[string]string{"role": role, "content": "test"}}, "stream": !buffered})
		assertMockRound(t, requestEvents(t, string(body), buffered), attempt)
	}
}

func TestEventBufferRejectsOversizeWithoutGrowing(t *testing.T) {
	buffer := eventBuffer{}
	if err := buffer.append(Event{Type: EvText, Delta: "small"}); err != nil {
		t.Fatal(err)
	}
	size := buffer.bytes
	if err := buffer.append(Event{Type: EvText, Delta: strings.Repeat("x", maxBufferedResponseBytes)}); err == nil {
		t.Fatal("oversize event accepted")
	}
	if len(buffer.events) != 1 || buffer.bytes != size {
		t.Fatal("oversize event changed buffer")
	}
}

func TestBufferedResponseReusesStreamingProvider(t *testing.T) {
	upstreamFixture(t, "", "")
	called := false
	http.DefaultTransport = regressionTransport(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		called = true
		if !payload.Stream {
			t.Fatal("downstream JSON unexpectedly changed upstream protocol")
		}
		body := sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "OK"}, "finish_reason": "stop"}}}) + "data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	events := requestEvents(t, `{"messages":[{"role":"user","content":"test"}],"stream":false}`, true)
	if !called || len(events) != 2 || events[0].Delta != "OK" || events[1].FinishReason != FinishStop {
		t.Fatal("buffered model response failed")
	}
}

func TestBufferedClientCancellationReachesUpstream(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_API_KEY", "fake-key")
	started := make(chan struct{})
	cancelled := make(chan bool, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			cancelled <- true
		case <-time.After(2 * time.Second):
			cancelled <- false
		}
	}))
	defer upstream.Close()
	t.Setenv("LLM_BASE_URL", upstream.URL)
	backend := httptest.NewServer(http.HandlerFunc(handleChatStream))
	defer backend.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", backend.URL, bytes.NewBufferString(`{"messages":[{"role":"user","content":"test"}],"stream":false}`))
	completed := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		completed <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("model request did not start")
	}
	cancel()
	select {
	case ok := <-cancelled:
		if !ok {
			t.Fatal("JSON transport lost upstream cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation timed out")
	}
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("cancelled client succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client request remained blocked")
	}
}
