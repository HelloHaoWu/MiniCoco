package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPCatalogPolicyReplayAndToolResults(t *testing.T) {
	sessionFixture(t)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_API_KEY", "fake-mcp-key")
	calls := 0
	catalog := []MCPToolSpec{{Name: "mcp__echo__ping", Description: "echo", Parameters: map[string]any{"type": "object"}, ReadOnly: true}, {Name: "mcp__writer__ping", Parameters: map[string]any{"type": "object"}}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Tools    []json.RawMessage `json:"tools"`
			Messages []Message         `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("decode")
		}
		names := toolNames(t, body.Tools)
		if len(names) != 4 || names[3] != "mcp__echo__ping" {
			t.Error(names)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "mcp-call", "type": "function", "function": map[string]any{"name": "mcp__echo__ping", "arguments": "{\"msg\":\"hello\"}"}}}}, "finish_reason": "tool_calls"}}})+"data: [DONE]\n\n")
		} else {
			if fmt.Sprint(body.Messages[len(body.Messages)-1]["content"]) != "hello" {
				t.Error(body.Messages)
			}
			fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "done"}, "finish_reason": "stop"}}})+"data: [DONE]\n\n")
		}
	}))
	defer upstream.Close()
	t.Setenv("LLM_BASE_URL", upstream.URL)
	req := map[string]any{"roundId": "mcp-first", "query": "echo", "workspace": "/tmp", "model": "deepseek-flash", "mode": "readonly", "mcpTools": catalog, "stream": false}
	response, events := deltaResponse(t, req)
	requireRoundFinish(t, response, events, FinishToolCalls)
	req["sessionId"] = response.Header().Get("X-Session-Id")
	req["roundId"] = "mcp-result"
	delete(req, "query")
	req["toolUseResults"] = []ToolUseResult{{ID: "mcp-call", Name: "mcp__echo__ping", Output: "hello"}}
	response, events = deltaResponse(t, req)
	requireRoundFinish(t, response, events, FinishStop)
	response, events = deltaResponse(t, req)
	requireRoundFinish(t, response, events, FinishStop)
	if calls != 2 {
		t.Fatal(calls)
	}
	catalog[0].Description = "changed"
	_, events = deltaResponse(t, req)
	if events[0].Code != codeBadRequest {
		t.Fatal(events)
	}
	if validateToolMode(ModeReadonly, []Event{{Type: EvToolCall, Name: "mcp__writer__ping"}}, catalog) == nil {
		t.Fatal("write allowed")
	}
	if validateToolMode(ModeFull, []Event{{Type: EvToolCall, Name: "mcp__unknown__ping"}}, catalog) == nil {
		t.Fatal("unknown allowed")
	}
	if _, err := (XmlFormatter{}).Format([]ToolUseResult{{ID: "x", Name: "mcp__echo__ping", Output: "<&"}}); err != nil {
		t.Fatal(err)
	}
	catalog[0].Name = "mcp__bad<tag"
	if validateMCPTools(catalog) == nil {
		t.Fatal("invalid name allowed")
	}
}
