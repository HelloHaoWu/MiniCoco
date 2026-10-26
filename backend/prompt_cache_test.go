package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Diagnostic identity from the design: system text followed by the exact tools
// JSON. This is not a provider cache key or a hash of the entire HTTP payload.
func cachePrefixHash(t *testing.T, messages []map[string]any, tools []byte) [32]byte {
	t.Helper()
	if len(messages) == 0 || messages[0]["role"] != "system" {
		t.Fatal("missing leading system message")
	}
	system, ok := messages[0]["content"].(string)
	if !ok {
		t.Fatal("system is not text")
	}
	return sha256.Sum256(append([]byte(system), tools...))
}

func TestPromptCachePrefixStableAcrossWorkspacesAndRounds(t *testing.T) {
	t.Setenv("MINICOCO_DEBUG_PROMPT", "0")
	identities := map[[32]byte]bool{}
	for _, family := range []string{"openai", "claude"} {
		for _, mode := range []Mode{ModeFull, ModeReadonly, ModeSafe} {
			t.Run(family+"/"+string(mode), func(t *testing.T) {
				route := ModelRoute{Family: family}
				query := map[string]any{"role": "user", "content": "inspect project"}
				history := []map[string]any{query}
				messages, tools, err := genPrompt(history, "/tmp/prefix-a", mode, route)
				if err != nil {
					t.Fatal(err)
				}
				toolsJSON, err := json.Marshal(tools)
				if err != nil {
					t.Fatal(err)
				}
				first := append([]map[string]any(nil), messages...)
				baseline := cachePrefixHash(t, first, toolsJSON)
				if identities[baseline] {
					t.Fatal("family or mode change did not invalidate prefix")
				}
				identities[baseline] = true
				// Growing history and different paths/queries must not alter the static part.
				for _, workspace := range []string{"/tmp/prefix-a", "/tmp/prefix-b-中文<&>"} {
					for round := 0; round < 3; round++ {
						growing := append([]map[string]any(nil), history...)
						for i := 0; i < round; i++ {
							growing = append(growing, map[string]any{"role": "assistant", "content": "previous answer"}, map[string]any{"role": "user", "content": fmt.Sprint("next ", i)})
						}
						actual, actualTools, err := genPrompt(growing, workspace, mode, route)
						if err != nil {
							t.Fatal(err)
						}
						raw, err := json.Marshal(actualTools)
						if err != nil {
							t.Fatal(err)
						}
						if baseline != cachePrefixHash(t, actual, raw) {
							t.Fatal("static cache prefix changed with workspace/round")
						}
						if len(actual) != len(growing)+3 || actual[1]["role"] != "system" || actual[2]["role"] != "user" || !strings.Contains(actual[2]["content"].(string), workspace) || strings.Contains(actual[0]["content"].(string), workspace) {
							t.Fatal("environment leaked into static prefix")
						}
						if !reflect.DeepEqual(actual[3:], growing) {
							t.Fatal("prompt construction mutated history")
						}
						if workspace == "/tmp/prefix-a" && !reflect.DeepEqual(actual[:len(first)], first) {
							t.Fatal("existing message prefix changed instead of appending")
						}
					}
				}
				// Changing the tool schema deliberately invalidates the diagnostic identity.
				changed := append([]json.RawMessage(nil), tools...)
				changed[0] = json.RawMessage(`{"type":"function","function":{"name":"read_file","description":"new schema"}}`)
				raw, _ := json.Marshal(changed)
				if baseline == cachePrefixHash(t, first, raw) {
					t.Fatal("tool schema change failed to invalidate identity")
				}
			})
		}
	}
}

type cacheWireRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Tools    json.RawMessage  `json:"tools"`
}

func TestPromptCacheWirePrefixAppendAndReplay(t *testing.T) {
	for _, family := range []string{"openai", "claude"} {
		for _, native := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/native=%t", family, native), func(t *testing.T) {
				service := sessionFixture(t)
				t.Setenv("LOOP_IMPL", "v1")
				t.Setenv("LLM_PROVIDER", "")
				t.Setenv("LLM_BASE_URL", "")
				t.Setenv("LLM_API_KEY", "fake-cache-key")
				captured := make(chan cacheWireRequest, 8)
				// Synthetic cache bookkeeping checks behavior for hit and miss usage. It
				// cannot establish any real provider cache hit or discount.
				var cacheMu sync.Mutex
				var previous [32]byte
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req cacheWireRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					captured <- req
					prefix := cachePrefixHash(t, req.Messages, req.Tools)
					cacheMu.Lock()
					hit := 0
					if prefix == previous {
						hit = 16
					}
					previous = prefix
					cacheMu.Unlock()
					w.Header().Set("Content-Type", "text/event-stream")
					last := req.Messages[len(req.Messages)-1]
					if last["content"] == "inspect" {
						fmt.Fprint(w, toolChunk(0, "prefix-tool", "list_dir", `{}`, FinishToolCalls))
					} else {
						fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "OK"}, "finish_reason": FinishStop}}}))
					}
					fmt.Fprint(w, sse(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 2, "total_tokens": 22, "prompt_cache_hit_tokens": hit, "prompt_cache_miss_tokens": 20 - hit}})+"data: [DONE]\n\n")
				}))
				defer upstream.Close()
				writeTestRoutes(t, map[string]ModelRoute{"cache-test": {Provider: "openai", BaseURL: upstream.URL, Family: family, MaxTokens: 32768, IsFunctionCall: native, Temperature: 0}})
				firstReq := map[string]any{"roundId": "cache-first", "query": "inspect", "workspace": "/tmp/cache-a", "model": "cache-test", "mode": "readonly", "stream": true}
				response, events := deltaResponse(t, firstReq)
				requireRoundFinish(t, response, events, FinishToolCalls)
				sid := response.Header().Get("X-Session-Id")
				first := <-captured
				resultReq := map[string]any{"sessionId": sid, "roundId": "cache-result", "workspace": "/tmp/cache-a", "mode": "readonly", "stream": false, "toolUseResults": []ToolUseResult{{ID: "prefix-tool", Name: "list_dir", Output: "listing <safe>"}}}
				response, events = deltaResponse(t, resultReq)
				requireRoundFinish(t, response, events, FinishStop)
				second := <-captured
				response, events = deltaResponse(t, map[string]any{"sessionId": sid, "roundId": "cache-next", "query": "again", "workspace": "/tmp/cache-a", "mode": "readonly", "stream": true})
				requireRoundFinish(t, response, events, FinishStop)
				third := <-captured
				other := map[string]any{"roundId": "cache-other", "query": "inspect", "workspace": "/tmp/cache-b", "model": "cache-test", "mode": "readonly", "stream": false}
				response, otherEvents := deltaResponse(t, other)
				requireRoundFinish(t, response, otherEvents, FinishToolCalls)
				fourth := <-captured
				hash := cachePrefixHash(t, first.Messages, first.Tools)
				for _, req := range []cacheWireRequest{second, third, fourth} {
					if !bytes.Equal(first.Tools, req.Tools) || hash != cachePrefixHash(t, req.Messages, req.Tools) {
						t.Fatal("serialized upstream prefix changed across round/transport/workspace")
					}
				}
				if !reflect.DeepEqual(second.Messages[:len(first.Messages)], first.Messages) || !reflect.DeepEqual(third.Messages[:len(second.Messages)], second.Messages) {
					t.Fatal("upstream message prefix was rewritten")
				}
				if !reflect.DeepEqual(first.Messages[:2], fourth.Messages[:2]) || reflect.DeepEqual(first.Messages[2], fourth.Messages[2]) || !reflect.DeepEqual(first.Messages[3:], fourth.Messages[3:]) {
					t.Fatal("workspace difference escaped the environment message")
				}
				meta, err := service.meta.GetSession(context.Background(), sid)
				if err != nil || meta.PromptTokens != 60 || meta.CompletionTokens != 6 || meta.TotalTokens != 66 {
					t.Fatalf("synthetic hit/miss metadata changed total usage: %+v %v", meta, err)
				}
				before := len(captured)
				response, replay := deltaResponse(t, resultReq)
				requireRoundFinish(t, response, replay, FinishStop)
				if len(captured) != before {
					t.Fatal("durable round replay was sent to simulated cache/provider")
				}
				after, err := service.meta.GetSession(context.Background(), sid)
				if err != nil || !reflect.DeepEqual(meta, after) {
					t.Fatal("replay counted usage again")
				}
			})
		}
	}
}
