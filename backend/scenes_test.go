package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestScenesRoutePromptAndToolsAcrossLoops(t *testing.T) {
	sessionFixture(t)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_API_KEY", "fake-scene-key")
	var last struct {
		Messages []map[string]any  `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&last); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "done"}, "finish_reason": "stop"}}})+"data: [DONE]\n\n")
	}))
	defer upstream.Close()
	t.Setenv("LLM_BASE_URL", upstream.URL)
	for _, loop := range []string{"v1", "v2"} {
		t.Setenv("LOOP_IMPL", loop)
		for _, slash := range []string{"", "composer", "unknown", "qa", "review"} {
			req := map[string]any{"roundId": "scene-" + loop + "-" + slash, "query": "inspect", "workspace": "/tmp", "model": "deepseek-flash", "slash": slash, "mode": "full", "stream": false}
			response, events := deltaResponse(t, req)
			requireRoundFinish(t, response, events, FinishStop)
			content := fmt.Sprint(last.Messages[0]["content"])
			if slash == "qa" || slash == "review" {
				if len(last.Tools) != 3 || !strings.Contains(content, "# Mode:") {
					t.Fatal(content, toolNames(t, last.Tools))
				}
			} else if len(last.Tools) != 7 || strings.Contains(content, "# Mode:") {
				t.Fatal(content, toolNames(t, last.Tools))
			}
			if slash == "qa" {
				req["slash"] = "review"
				_, events = deltaResponse(t, req)
				if events[0].Code != codeBadRequest {
					t.Fatal("same round changed scene", events)
				}
			}
		}
	}
	for _, family := range []string{"openai", "claude"} {
		for _, slash := range []string{"qa", "review"} {
			extra := appendScenePrompt([]byte("base"), promptsFS, family, slash)
			if !strings.Contains(string(extra), "# Mode:") {
				t.Fatal(string(extra))
			}
		}
	}
	if got := string(appendScenePrompt([]byte("base"), fstest.MapFS{}, "openai", "qa")); got != "base" {
		t.Fatal(got)
	}
	if sceneMode(ModeReadonly, "composer") != ModeReadonly || sceneMode(ModeSafe, "composer") != ModeSafe {
		t.Fatal("composer widened mode")
	}
}
