package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepSeekDefaultsAndStream(t *testing.T) {
	t.Setenv("LLM_API_KEY", "local-test-key")
	t.Setenv("LLM_MODEL", "")
	t.Setenv("LLM_PROVIDER", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer local-test-key" {
			t.Error("incorrect endpoint or authorization header")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		thinking, _ := payload["thinking"].(map[string]any)
		if payload["model"] != "deepseek-flash" || payload["stream"] != true || thinking["type"] != "disabled" {
			t.Error("wrong default model, streaming or thinking settings")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("LLM_BASE_URL", server.URL)
	var events []Event
	if err := streamCompletion([]map[string]any{{"role": "user", "content": "test"}}, nil, "", func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Delta != "OK" || events[1].Type != EvDone {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestProviderOverridesAndKeyRedaction(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "mock")
	t.Setenv("LLM_API_KEY", "")
	if err := streamCompletion(nil, nil, "", func(Event) {}); err != nil {
		t.Fatal("mock should not require a key")
	}
	t.Setenv("LLM_PROVIDER", "openai")
	if err := streamCompletion(nil, nil, "", func(Event) {}); err == nil || !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Fatal("missing key did not produce a clear error")
	}
	t.Setenv("LLM_API_KEY", "local-test-secret")
	t.Setenv("LLM_MODEL", "custom-model")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		if payload["model"] != "request-model" || payload["thinking"] != nil {
			t.Error("request model override or generic endpoint payload is incorrect")
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "invalid local-test-secret")
	}))
	defer server.Close()
	t.Setenv("LLM_BASE_URL", server.URL)
	err := streamCompletion(nil, nil, "request-model", func(Event) {})
	if err == nil || strings.Contains(err.Error(), "local-test-secret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatal("API failure was not redacted")
	}
}
