package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func toolPair() []Message {
	return []Message{{"role": "assistant", "tool_calls": []any{map[string]any{"id": "a", "type": "function", "function": map[string]any{"name": "list_dir", "arguments": "{}"}}}}, {"role": "tool", "tool_call_id": "a", "content": "output"}}
}
func TestTokenEstimateAndThresholds(t *testing.T) {
	for _, test := range []struct {
		value any
		want  int
	}{{"abcd", 2}, {"中文", 1}, {"😀", 1}, {"abc", 2}, {nil, 0}, {[]any{map[string]any{"type": "text", "text": "hello"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("x", 3000)}}}, 5}} {
		if got := contentTokens(test.value); got != test.want {
			t.Fatalf("estimate=%d want %d", got, test.want)
		}
	}
	if estimateTokens(toolPair()) <= contentTokens("output") {
		t.Fatal("tool identity/arguments not counted")
	}
	route := ModelRoute{Family: "openai", MaxTokens: 100000}
	b, err := budgetFor(nil, "/tmp", ModeFull, route)
	if err != nil {
		t.Fatal(err)
	}
	route.MaxTokens = b.Base + 100
	for _, test := range []struct {
		n    int
		mode compactMode
	}{{65, compactNone}, {70, compactMicro}, {80, compactMicro}, {85, compactFull}} {
		b, err := budgetFor([]Message{{"role": "user", "content": strings.Repeat("x", test.n*2)}}, "/tmp", ModeFull, route)
		if err != nil || b.Mode != test.mode || b.Available != 100 {
			t.Fatalf("wrong threshold: %+v %v", b, err)
		}
	}
	route.MaxTokens = 1
	if _, err := budgetFor(nil, "", ModeFull, route); err == nil {
		t.Fatal("fixed prefix overflow accepted")
	}
}
func TestTrimBudgetPreservesTailAndToolBatches(t *testing.T) {
	for _, xml := range []bool{false, true} {
		pair := toolPair()
		if xml {
			result, _ := (XmlFormatter{}).Format([]ToolUseResult{{ID: "a", Name: "list_dir", Output: "output"}})
			pair = append(pair[:1], result...)
		}
		history := append([]Message{{"role": "user", "content": strings.Repeat("old", 1000)}}, pair...)
		original, _ := json.Marshal(history)
		need := estimateTokens(placeholderPrefix(pair))
		got, err := trimToBudget(history, need)
		if err != nil || len(got) != 3 || got[0]["role"] != "user" || !reflect.DeepEqual(got[1:], pair) || estimateTokens(got) > need {
			t.Fatalf("broken protected pair %+v %v", got, err)
		}
		if _, err := trimToBudget(history, 1); err == nil {
			t.Fatal("oversize protected pair silently broken")
		}
		after, _ := json.Marshal(history)
		if string(after) != string(original) {
			t.Fatal("trim mutated history")
		}
		history = append(history, Message{"role": "user", "content": "newest"})
		got, err = trimToBudget(history, 3)
		if err != nil || len(got) != 1 || got[0]["content"] != "newest" {
			t.Fatal("did not keep newest query", got, err)
		}
	}
	got, err := trimToBudget([]Message{{"role": "user", "content": "old😀中文tail"}}, 2)
	if err != nil || got[0]["content"] != "tail" {
		t.Fatal("last text was not safely truncated", got, err)
	}
	bad := toolPair()
	bad[1]["tool_call_id"] = "wrong"
	if _, err := trimToBudget(bad, 1000); err == nil {
		t.Fatal("mismatched batch accepted")
	}
}
func TestV2ProactiveBudgetStopsBeforeProvider(t *testing.T) {
	service := sessionFixture(t)
	_ = service
	t.Setenv("LOOP_IMPL", "v2")
	t.Setenv("LLM_PROVIDER", "unknown")
	route := ModelRoute{Provider: "mock", Family: "openai", MaxTokens: 100000, IsFunctionCall: true}
	b, _ := budgetFor(nil, "", ModeFull, route)
	route.MaxTokens = b.Base + 100
	writeTestRoutes(t, map[string]ModelRoute{"tiny": route})
	response, events := deltaResponse(t, map[string]any{"roundId": "proactive", "query": strings.Repeat("x", 170), "model": "tiny", "stream": false})
	requireRoundFinish(t, response, events, "error")
	if events[0].Code != "cannot_compress" {
		t.Fatalf("provider called before budget trigger: %+v", events)
	}
}
