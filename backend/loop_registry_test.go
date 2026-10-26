package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func registerTestLoop(t *testing.T, key string, impl LoopImpl) {
	t.Helper()
	if err := registerLoop(key, impl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loopRegistryMu.Lock(); delete(loopRegistry, key); loopRegistryMu.Unlock() })
}

func TestLoopSelectionAndRegistration(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	for _, key := range []string{"", "v1", "not-registered"} {
		t.Setenv("LOOP_IMPL", key)
		selected, _ := selectedLoop()
		if selected != "v1" {
			t.Fatal("missing/unknown selection must fall back to v1")
		}
		response, events := deltaResponse(t, map[string]any{"roundId": "baseline", "query": "inspect", "stream": false})
		requireRoundFinish(t, response, events, FinishToolCalls)
	}
	meta, err := service.meta.GetSession(context.Background(), initialSessionID("baseline"))
	if err != nil || meta.MessageCount != 2 || meta.TotalTokens != 2 {
		t.Fatal("fallback changed v1 or replay accounting", err)
	}
	for _, invalid := range []struct {
		key  string
		impl LoopImpl
	}{
		{"", processRequestV1}, {" padded ", processRequestV1}, {"nil", nil}, {"v1", processRequestV1},
	} {
		if registerLoop(invalid.key, invalid.impl) == nil {
			t.Fatal("invalid/duplicate registration accepted")
		}
	}
	var called atomic.Int32
	registerTestLoop(t, "test-v2", func(ctx context.Context, req ChatRequest, emit func(Event)) error {
		called.Add(1)
		return processRequestV1(ctx, req, emit)
	})
	t.Setenv("LOOP_IMPL", "test-v2")
	// A registered implementation must not repeat an already committed round.
	response, events := deltaResponse(t, map[string]any{"roundId": "baseline", "query": "inspect", "stream": true})
	requireRoundFinish(t, response, events, FinishToolCalls)
	if called.Load() != 1 {
		t.Fatal("registered v2 not selected")
	}
	meta, _ = service.meta.GetSession(context.Background(), initialSessionID("baseline"))
	if meta.MessageCount != 2 || meta.TotalTokens != 2 {
		t.Fatal("switching implementations duplicated a committed round")
	}
}

func TestLoopDispatchBothTransportsAndFailure(t *testing.T) {
	sessionFixture(t)
	t.Setenv("LLM_PROVIDER", "mock")
	t.Setenv("LOOP_IMPL", "test-v2")
	var calls int
	var seen []ChatRequest
	registerTestLoop(t, "test-v2", func(ctx context.Context, req ChatRequest, emit func(Event)) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		calls++
		seen = append(seen, req)
		if calls > 2 {
			return completionFailure(codeBadRequest, false, "fake implementation failure", nil)
		}
		emit(Event{Type: EvText, Delta: "selected-v2"})
		emit(Event{Type: EvDone, FinishReason: FinishStop})
		return nil
	})
	var first []Event
	for i, stream := range []bool{true, false, true, false} {
		response, events := deltaResponse(t, map[string]any{"roundId": "selected", "query": "inspect", "workspace": "/tmp/selected", "mode": "readonly", "stream": stream})
		finish := FinishStop
		if i > 1 {
			finish = "error"
		}
		requireRoundFinish(t, response, events, finish)
		if i == 0 {
			first = events
		}
		if i == 1 && !reflect.DeepEqual(first, events) {
			t.Fatal("SSE/JSON dispatched different implementations")
		}
		if i > 1 && (len(events) != 2 || events[0].Code != codeBadRequest) {
			t.Fatal("implementation failure lost shared classification/done boundary")
		}
	}
	if calls != 4 {
		t.Fatal("implementation invoked more than once per HTTP round")
	}
	for _, req := range seen {
		if req.SessionID != initialSessionID("selected") || req.Model != defaultModel || req.Workspace != "/tmp/selected" || req.Mode != ModeReadonly {
			t.Fatal("dispatch lost resolved request fields")
		}
	}
}

func TestLoopCancellationAndConcurrentLookup(t *testing.T) {
	t.Setenv("LOOP_IMPL", "test-v2")
	key := struct{}{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key, "request-context"))
	cancel()
	registerTestLoop(t, "test-v2", func(got context.Context, _ ChatRequest, _ func(Event)) error {
		if got.Value(key) != "request-context" {
			t.Error("dispatch replaced request context")
		}
		return got.Err()
	})
	if err := pickLoop()(ctx, ChatRequest{}, func(Event) {}); !errors.Is(err, context.Canceled) {
		t.Fatal("loop cancellation not propagated", err)
	}
	var events []Event
	processRequestContext(ctx, ChatRequest{}, func(event Event) { events = append(events, event) })
	if len(events) != 2 || events[0].Type != EvError || events[0].Retryable == nil || *events[0].Retryable || events[1].FinishReason != "error" {
		t.Fatal("cancelled implementation returned a success")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if pickLoop() == nil {
					t.Error("concurrent lookup returned nil")
				}
			}
		}()
	}
	if err := registerLoop("other", processRequestV1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loopRegistryMu.Lock(); delete(loopRegistry, "other"); loopRegistryMu.Unlock() })
	wg.Wait()
}

func TestLoopLocalConfigAndHealth(t *testing.T) {
	t.Setenv("LOOP_IMPL", "")
	os.Unsetenv("LOOP_IMPL")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("LOOP_IMPL=v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOOP_IMPL") != "v2" {
		t.Fatal(".env loop selection not loaded")
	}
	t.Setenv("LOOP_IMPL", "v1")
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOOP_IMPL") != "v1" {
		t.Fatal(".env overrode exported selection")
	}
	t.Setenv("LLM_MODEL", "mock")
	t.Setenv("LOOP_IMPL", "test-v2") // Unregistered in production; health reports fallback.
	response := httptest.NewRecorder()
	handleHealth(response, httptest.NewRequest("GET", "/health", nil))
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || body["loop_impl"] != "v1" {
		t.Fatal("health did not report effective fallback implementation")
	}
}
