package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func writeTestRoutes(t *testing.T, routes map[string]ModelRoute) string {
	t.Helper()
	data, err := json.Marshal(routes)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINICOCO_MODELS_FILE", path)
	return path
}

func configureTestCapability(t *testing.T, native bool) {
	t.Helper()
	var routes map[string]ModelRoute
	if err := json.Unmarshal(embeddedModels, &routes); err != nil {
		t.Fatal(err)
	}
	route := routes[defaultModel]
	route.IsFunctionCall = native
	routes[defaultModel] = route
	writeTestRoutes(t, routes)
}

func TestModelRoutesAndOfflineOverride(t *testing.T) {
	t.Setenv("MINICOCO_MODELS_FILE", "")
	t.Setenv("LLM_PROVIDER", "")
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_MODEL", "")
	for _, name := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		route, err := routeFor(name)
		if err != nil || route.Name != name || route.Provider != "openai" || route.BaseURL != defaultBaseURL || route.MaxTokens != 131072 || !route.IsFunctionCall || route.Temperature != 0.7 || route.Thinking != "disabled" {
			t.Fatalf("bad route: %+v %v", route, err)
		}
	}
	t.Setenv("LLM_MODEL", "mock-claude")
	route, err := routeFor("")
	if err != nil || route.Family != "claude" || route.Provider != "mock" {
		t.Fatalf("family selection failed: %+v %v", route, err)
	}
	t.Setenv("LLM_PROVIDER", "openai")
	if err := streamCompletion(nil, nil, "mock", func(Event) {}); err != nil {
		t.Fatal("mock route must stay local", err)
	}
	t.Setenv("LLM_PROVIDER", "mock")
	t.Setenv("LLM_BASE_URL", "not-a-url")
	route, err = routeFor(defaultModel)
	if err == nil {
		route, err = effectiveRoute(route)
	}
	if err != nil || route.Provider != "mock" || route.Temperature != 0.7 {
		t.Fatal("offline override must retain model capabilities", err)
	}
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "")
	t.Setenv("LLM_MODEL", "mock")
	t.Setenv("LLM_MOCK_MODE", "flaky")
	t.Setenv("LLM_MOCK_FLAKY_FAILS", "1")
	if err := configureMockFaults(); err != nil || !consumeMockFailure() || consumeMockFailure() {
		t.Fatal("route-driven flaky mock failed", err)
	}
}

func TestUnknownModelRejectedBeforeSessionCreation(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	for _, stream := range []bool{true, false} {
		response, events := deltaResponse(t, map[string]any{"roundId": fmt.Sprint("unknown-", stream), "query": "hello", "model": "unknown", "stream": stream})
		var body struct {
			Code      string
			Retryable bool
			Error     string
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != 400 || len(events) != 0 || body.Code != codeBadRequest || body.Retryable || !strings.Contains(body.Error, "deepseek-flash, deepseek-v4-pro, mock, mock-claude, mock-xml") {
			t.Fatalf("unknown route not fatal: %d %s", response.Code, response.Body.String())
		}
	}
	sessions, err := service.meta.ListSessions(context.Background(), "")
	if err != nil || len(sessions) != 0 {
		t.Fatal("rejected model created a session", err)
	}
	t.Setenv("LLM_MODEL", "unknown")
	response, _ := deltaResponse(t, map[string]any{"roundId": "bad-default", "query": "hello"})
	if response.Code != 400 {
		t.Fatal("unknown configured default accepted")
	}
}

func TestModelRoutingValidation(t *testing.T) {
	t.Setenv("MINICOCO_MODEL_IS_FUNCTION_CALL", "")
	for _, raw := range []string{
		`null`, `{}`, `{"x":null}`, `{"x":{"provider":"mock"}}`,
		`{"x":{"provider":"mock","baseURL":"","family":"openai","maxTokens":1,"isFunctionCall":null,"temperature":0}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MINICOCO_MODELS_FILE", path)
			_, err := routeFor("x")
			code, retry := classifyCompletionError(err)
			if err == nil || code != codeConfig || retry {
				t.Fatal("invalid routes accepted", err)
			}
		})
	}
	good := ModelRoute{Provider: "openai", BaseURL: "http://127.0.0.1", Family: "openai", MaxTokens: 8192, IsFunctionCall: true, Temperature: 0}
	for _, field := range []string{"provider", "base", "family", "tokens", "temperature", "thinking"} {
		t.Run(field, func(t *testing.T) {
			bad := good
			switch field {
			case "provider":
				bad.Provider = "anthropic"
			case "base":
				bad.BaseURL = "https://fake-secret@host"
			case "family":
				bad.Family = "unknown"
			case "tokens":
				bad.MaxTokens = 0
			case "temperature":
				bad.Temperature = 3
			case "thinking":
				bad.Thinking = "enabled"
			}
			writeTestRoutes(t, map[string]ModelRoute{"custom": bad})
			_, err := routeFor("custom")
			if err == nil || strings.Contains(err.Error(), "fake-secret") {
				t.Fatal("invalid or unredacted route", err)
			}
		})
	}
	for _, kind := range []string{"missing", "oversize", "extra-field"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.json")
			switch kind {
			case "oversize":
				if err := os.WriteFile(path, bytes.Repeat([]byte(" "), 1024*1024+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "extra-field":
				if err := os.WriteFile(path, []byte(`{"x":{"provider":"mock","baseURL":"","family":"openai","maxTokens":1,"isFunctionCall":true,"temperature":0,"secret":"fake-secret"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("MINICOCO_MODELS_FILE", path)
			_, err := routeFor("x")
			if err == nil || strings.Contains(err.Error(), "fake-secret") {
				t.Fatal("bad file accepted or leaked", err)
			}
		})
	}
}

func TestFamilyPromptAndWorkspacePlacement(t *testing.T) {
	history := []map[string]any{{"role": "user", "content": "inspect"}}
	for _, family := range []string{"openai", "claude"} {
		a, atools, err := genPrompt(history, "/tmp/project-a", ModeReadonly, ModelRoute{Family: family})
		if err != nil {
			t.Fatal(err)
		}
		b, btools, err := genPrompt(history, "/tmp/project-b", ModeReadonly, ModelRoute{Family: family})
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != 3 || a[0]["role"] != "system" || a[1]["role"] != "user" || a[2]["content"] != "inspect" || !strings.Contains(a[1]["content"].(string), "/tmp/project-a") || strings.Contains(a[0]["content"].(string), "/tmp/project-a") || !reflect.DeepEqual(a[0], b[0]) || !reflect.DeepEqual(atools, btools) {
			t.Fatal("workspace contaminated static prompt/tools")
		}
	}
	a, _, _ := genPrompt(history, "", ModeFull, ModelRoute{Family: "openai"})
	b, _, _ := genPrompt(history, "", ModeFull, ModelRoute{Family: "claude"})
	if reflect.DeepEqual(a[0], b[0]) || len(a) != 2 {
		t.Fatal("family prompts identical or empty workspace injected")
	}
	if _, _, err := genPrompt(history, "", ModeFull, ModelRoute{Family: "unknown"}); err == nil {
		t.Fatal("unsupported prompt family accepted")
	}
}

func TestRoutingSnapshotControlsPromptFormatterAndTransport(t *testing.T) {
	service := sessionFixture(t)
	t.Setenv("LLM_PROVIDER", "")
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_API_KEY", "fake-route-key")
	preserveMockBudget(t)
	var calls atomic.Int32
	captured := make(chan []Message, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model       string
			Temperature float64
			Thinking    any
			MaxTokens   any `json:"max_tokens"`
			Messages    []Message
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		captured <- payload.Messages
		if payload.Model != "custom" || payload.Temperature != 0.25 || payload.Thinking != nil || payload.MaxTokens != nil {
			t.Errorf("incorrect routed payload: %+v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, toolChunk(0, "route-tool", "list_dir", `{}`, "tool_calls")+"data: [DONE]\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
		}
	}))
	defer server.Close()
	route := ModelRoute{Provider: "openai", BaseURL: server.URL, Family: "claude", MaxTokens: 8192, IsFunctionCall: false, Temperature: 0.25}
	path := writeTestRoutes(t, map[string]ModelRoute{"custom": route})
	response, events := deltaResponse(t, map[string]any{"roundId": "route-first", "query": "inspect", "model": "custom", "workspace": "/tmp/routed"})
	requireRoundFinish(t, response, events, FinishToolCalls)
	first := <-captured
	if !strings.Contains(first[0]["content"].(string), "# Working method") || first[1]["role"] != "user" {
		t.Fatal("routed family not applied")
	}
	sid := response.Header().Get("X-Session-Id")
	req := map[string]any{"sessionId": sid, "roundId": "route-result", "workspace": "/tmp/routed", "toolUseResults": []ToolUseResult{{ID: "route-tool", Name: "list_dir", Output: "found"}}}
	response, events = deltaResponse(t, req)
	requireRoundFinish(t, response, events, FinishStop)
	second := <-captured
	if last := second[len(second)-1]; last["role"] != "user" || !strings.HasPrefix(last["content"].(string), "<tool_result>") {
		t.Fatal("model capability not used for formatter")
	}
	history, err := service.transcript.Latest(context.Background(), sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range history {
		if text, ok := msg["content"].(string); ok && strings.Contains(text, "# Environment") {
			t.Fatal("environment persisted in transcript")
		}
	}
	_, replay := deltaResponse(t, req)
	if !reflect.DeepEqual(events, replay) || calls.Load() != 2 {
		t.Fatal("routing broke successful replay")
	}
	route.Name = "custom"
	ctx := context.WithValue(context.Background(), modelRouteContextKey{}, route)
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := streamCompletionContext(ctx, nil, nil, "custom", func(Event) {}); err != nil {
		t.Fatal("round routing snapshot reread configuration", err)
	}
	if _, err := routeFor("custom"); err == nil {
		t.Fatal("next round ignored edited configuration")
	}
}
