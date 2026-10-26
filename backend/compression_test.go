package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func seedCompression(t *testing.T, service *sessionService, sid, model string, history []Message) {
	t.Helper()
	ctx := context.Background()
	if _, err := service.meta.CreateSession(ctx, Session{ID: sid, Model: model, Workspace: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if err := service.transcript.Append(ctx, sid, "seed", history); err != nil {
		t.Fatal(err)
	}
	if err := service.meta.CommitRoundUsage(ctx, sid, "seed", Usage{}, len(history)); err != nil {
		t.Fatal(err)
	}
}
func oldContext() []Message {
	return []Message{{"role": "user", "content": strings.Repeat("old-decision ", 70)}, {"role": "assistant", "content": strings.Repeat("file/path.go ", 70)}, {"role": "user", "content": strings.Repeat("unfinished task ", 60)}, {"role": "assistant", "content": "previous conclusion"}}
}
func TestCompressionRangesPreserveNativeAndXMLTail(t *testing.T) {
	for _, native := range []bool{true, false} {
		pair := toolPair()
		if !native {
			r, _ := (XmlFormatter{}).Format([]ToolUseResult{{ID: "a", Name: "list_dir", Output: "result"}})
			pair = append(pair[:1], r...)
		}
		current := append(oldContext(), pair...)
		for _, mode := range []compactMode{compactMicro, compactFull} {
			end, err := chooseRange(current, len(current)-1, mode)
			if err != nil || end < 3 || end > 4 {
				t.Fatal("tool pair split by range", end, err)
			}
		}
		current = append(current, Message{"role": "assistant", "content": "done"})
		end, err := chooseRange(current, len(current), compactFull)
		if err != nil || end != 4 {
			t.Fatal("last assistant lost previous complete tool pair", end, err)
		}
		current = append(current, Message{"role": "user", "content": "new query"})
		end, err = chooseRange(current, len(current)-1, compactFull)
		if err != nil || end != len(current)-1 {
			t.Fatal("new query not protected", end, err)
		}
	}
}
func runCompressionPersistence(t *testing.T, factory contractFactory) {
	open := factory(t)
	stores := open()
	defer func() { stores.close() }()
	previous := activeSessions
	defer func() { activeSessions = previous }()
	service := &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	activeSessions = service
	t.Setenv("LOOP_IMPL", "v2")
	t.Setenv("LLM_PROVIDER", "")
	preserveMockBudget(t)
	route := ModelRoute{Provider: "mock", Family: "openai", MaxTokens: 100000, IsFunctionCall: true}
	b, _ := budgetFor(nil, "/tmp", ModeFull, route)
	route.MaxTokens = b.Base + 1500
	writeTestRoutes(t, map[string]ModelRoute{"compress-mock": route})
	raw := oldContext()
	seedCompression(t, service, "s_compress", "compress-mock", raw)
	response, events := deltaResponse(t, map[string]any{"sessionId": "s_compress", "roundId": "compress-query", "query": "inspect", "workspace": "/tmp", "stream": false})
	requireRoundFinish(t, response, events, FinishToolCalls)
	records, err := service.transcript.Summaries(context.Background(), "s_compress")
	if err != nil || len(records) != 1 || records[0].PrefixCount != 4 {
		t.Fatal("summary checkpoint not saved", records, err)
	}
	got, err := service.transcript.Latest(context.Background(), "s_compress", allHistory)
	if err != nil || len(got) != 4 || !strings.HasPrefix(got[1]["content"].(string), "[Context summary]") {
		t.Fatal("summary not merged", got, err)
	}
	// Finish the tool batch, then reopen both stores and replay exactly the round.
	request := map[string]any{"sessionId": "s_compress", "roundId": "compress-result", "workspace": "/tmp", "stream": true, "toolUseResults": []ToolUseResult{{ID: "call_mock_1", Name: "list_dir", Output: "listing"}}}
	response, events = deltaResponse(t, request)
	requireRoundFinish(t, response, events, FinishStop)
	before, _ := service.meta.GetSession(context.Background(), "s_compress")
	stores.close()
	stores = open()
	service = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	activeSessions = service
	t.Setenv("LLM_PROVIDER", "unknown")
	response, replay := deltaResponse(t, request)
	requireRoundFinish(t, response, replay, FinishStop)
	if !reflect.DeepEqual(events, replay) {
		t.Fatal("restart changed events")
	}
	after, _ := service.meta.GetSession(context.Background(), "s_compress")
	if before != after || after.MessageCount != 8 || after.TotalTokens != 4 {
		t.Fatalf("summary/replay changed canonical counts: %+v %+v", before, after)
	}
	canonical, err := service.transcript.RawLatest(context.Background(), "s_compress", allHistory)
	if err != nil || len(canonical) != 8 || !reflect.DeepEqual(canonical[:4], raw) {
		t.Fatal("compression changed raw history", err)
	}
	merged, err := service.transcript.Latest(context.Background(), "s_compress", allHistory)
	if err != nil || len(merged) != 6 {
		t.Fatal("summary did not survive reopening", err)
	}
	// Simulate a crash after summary persistence but before its usage ledger write.
	recovered := records[0]
	recovered.Content = "Recovered checkpoint"
	recovered.Usage = Usage{7, 3, 10}
	recovered.UsageID = "\x00summary:" + historyDigest([]Message{{"hash": recovered.PrefixHash, "content": recovered.Content}})
	if err := service.transcript.SaveSummary(context.Background(), "s_compress", recovered); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := service.reconcile(context.Background(), "s_compress"); err != nil {
			t.Fatal(err)
		}
	}
	accounted, _ := service.meta.GetSession(context.Background(), "s_compress")
	if accounted.TotalTokens != 14 || accounted.MessageCount != 8 {
		t.Fatal("summary recovery double-counted or lost usage", accounted)
	}
	// Wrong source fingerprints and cuts through a pending tool call must fail.
	bad := records[0]
	bad.PrefixHash = strings.Repeat("f", 64)
	if service.transcript.SaveSummary(context.Background(), "s_compress", bad) == nil {
		t.Fatal("stale summary accepted")
	}
}
func TestFileCompressionPersistence(t *testing.T) { runCompressionPersistence(t, fileContractFactory) }
func TestReactiveCompressionRetriesAndAccountsSummary(t *testing.T) {
	for _, scenario := range []string{"recover", "exhaust", "summary-overflow"} {
		t.Run(scenario, func(t *testing.T) {
			alwaysFail := scenario == "exhaust"
			summaryOverflow := scenario == "summary-overflow"
			service := sessionFixture(t)
			t.Setenv("LOOP_IMPL", "v2")
			t.Setenv("LLM_PROVIDER", "")
			t.Setenv("LLM_BASE_URL", "")
			t.Setenv("LLM_API_KEY", "fake-compress-key")
			var mains, summaries atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []Message `json:"messages"`
					Tools    any       `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				summary := strings.HasPrefix(fmt.Sprint(body.Messages[0]["content"]), "Summarize")
				if summary {
					summaries.Add(1)
					if summaryOverflow {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"code":"context_length_exceeded"}}`)
						return
					}
					if body.Tools != nil {
						t.Error("summary request exposed tools")
					}
				} else if mains.Add(1) == 1 || alwaysFail {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"code":"context_length_exceeded","message":"context length exceeded"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				text := "OK"
				if summary {
					text = "Decisions and file/path.go retained; continue unfinished task."
				}
				fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": text}, "finish_reason": "stop"}}})+sse(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})+"data: [DONE]\n\n")
			}))
			defer server.Close()
			writeTestRoutes(t, map[string]ModelRoute{"reactive": {Provider: "openai", BaseURL: server.URL, Family: "openai", MaxTokens: 32768, IsFunctionCall: true}})
			seedCompression(t, service, "s_reactive", "reactive", oldContext())
			request := map[string]any{"sessionId": "s_reactive", "roundId": "reactive", "query": strings.Repeat("next", 20), "workspace": "/tmp", "stream": false}
			response, events := deltaResponse(t, request)
			if !alwaysFail {
				requireRoundFinish(t, response, events, FinishStop)
				if mains.Load() != 2 || summaries.Load() != 1 {
					t.Fatal("compression did not retry the model once")
				}
				meta, _ := service.meta.GetSession(context.Background(), "s_reactive")
				wantTokens := int64(10)
				if summaryOverflow {
					wantTokens = 5
				}
				if meta.TotalTokens != wantTokens || meta.MessageCount != 6 {
					t.Fatal("summary/main usage not accounted", meta)
				}
				_, replay := deltaResponse(t, request)
				if !reflect.DeepEqual(events, replay) || mains.Load() != 2 || summaries.Load() != 1 {
					t.Fatal("replay charged summary twice")
				}
			} else {
				requireRoundFinish(t, response, events, "error")
				if events[0].Code != "cannot_compress" || mains.Load() > maxCompressions+1 {
					t.Fatal("compression retry unbounded", events, mains.Load())
				}
				raw, _ := service.transcript.RawLatest(context.Background(), "s_reactive", allHistory)
				if len(raw) != 4 {
					t.Fatal("failed retry committed input")
				}
			}
		})
	}
}
func TestCompressionNoProgressCancellationAndFallback(t *testing.T) {
	service := sessionFixture(t)
	t.Setenv("LLM_PROVIDER", "mock")
	route := ModelRoute{Name: "mock", Provider: "mock", Family: "openai", MaxTokens: 32768, IsFunctionCall: true}
	raw := oldContext()
	seedCompression(t, service, "s_fallback", "mock", raw)
	input := []Message{{"role": "user", "content": "latest query"}}
	current := append(append([]Message(nil), raw...), input...)
	req := ChatRequest{SessionID: "s_fallback", Workspace: "/tmp"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	if _, err := service.compactContext(ctx, req, route, ModeFull, current, raw, input, compactFull, &attempts); err != context.Canceled {
		t.Fatal("cancellation lost", err)
	}
	service.coord.Allow(context.Background(), "rate:summary", 1, 10e9)
	next, err := service.compactContext(context.Background(), req, route, ModeFull, current, raw, input, compactFull, &attempts)
	if err != nil || estimateTokens(next) >= estimateTokens(current) || next[len(next)-1]["content"] != "latest query" {
		t.Fatal("rate fallback did not trim safely", err)
	}
	saved, _ := service.transcript.Summaries(context.Background(), "s_fallback")
	if len(saved) != 0 {
		t.Fatal("trim fallback persisted a fake summary")
	}
	attempts = maxCompressions
	if _, err := service.compactContext(context.Background(), req, route, ModeFull, current, raw, input, compactFull, &attempts); err == nil {
		t.Fatal("maximum compression attempts ignored")
	}
}

func TestCompressionMicroAndToolResultRounds(t *testing.T) {
	for _, kind := range []string{"micro", "native", "xml"} {
		t.Run(kind, func(t *testing.T) {
			service := sessionFixture(t)
			preserveMockBudget(t)
			t.Setenv("LOOP_IMPL", "v2")
			t.Setenv("LLM_PROVIDER", "")
			route := ModelRoute{Provider: "mock", Family: "openai", MaxTokens: 100000, IsFunctionCall: kind != "xml"}
			raw := oldContext()
			request := map[string]any{"sessionId": "s_batch", "roundId": "batch", "workspace": "/tmp", "stream": false}
			if kind == "micro" {
				request["query"] = "inspect"
			} else {
				raw = append(raw, toolPair()[0])
				request["toolUseResults"] = []ToolUseResult{{ID: "a", Name: "list_dir", Output: "listing"}}
			}
			b, _ := budgetFor(nil, "/tmp", ModeFull, route)
			available := 1500
			if kind == "micro" {
				available = 2000
			}
			route.MaxTokens = b.Base + available
			writeTestRoutes(t, map[string]ModelRoute{"batch": route})
			seedCompression(t, service, "s_batch", "batch", raw)
			response, events := deltaResponse(t, request)
			finish := FinishStop
			if kind == "micro" {
				finish = FinishToolCalls
			}
			requireRoundFinish(t, response, events, finish)
			summaries, _ := service.transcript.Summaries(context.Background(), "s_batch")
			want := 4
			if kind == "micro" {
				want = 3
			}
			if len(summaries) != 1 || summaries[0].PrefixCount != want {
				t.Fatal("wrong compact range", summaries)
			}
			view, err := service.transcript.Latest(context.Background(), "s_batch", allHistory)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := historyBlocks(view); err != nil {
				t.Fatal("compression split final tool batch", err)
			}
		})
	}
}

func TestSummaryNoProgressAndMalformedSource(t *testing.T) {
	service := sessionFixture(t)
	t.Setenv("LLM_PROVIDER", "mock")
	raw := []Message{{"role": "user", "content": "a"}, {"role": "assistant", "content": "b"}, {"role": "user", "content": "c"}, {"role": "assistant", "content": "d"}}
	seedCompression(t, service, "s_small", "mock", raw)
	input := []Message{{"role": "user", "content": "e"}}
	attempts := 0
	_, err := service.compactContext(context.Background(), ChatRequest{SessionID: "s_small"}, ModelRoute{Name: "mock", Provider: "mock", Family: "openai", MaxTokens: 32768}, ModeFull, append(raw, input...), raw, input, compactFull, &attempts)
	code, retry := classifyCompletionError(err)
	if code != "cannot_compress" || retry {
		t.Fatal("nonshrinking summary was accepted", err)
	}
	saved, _ := service.transcript.Summaries(context.Background(), "s_small")
	if len(saved) != 0 {
		t.Fatal("failed progress saved a summary")
	}
	// Invalid source boundaries cannot conceal a pending tool batch from resume.
	malformed := append(raw, toolPair()[0])
	seedCompression(t, service, "s_pending", "mock", malformed)
	s := Summary{OriginStartID: "message:1", OriginEndID: "message:5", Content: "short", PrefixCount: 5, PrefixHash: historyDigest(malformed)}
	s.UsageID = "\x00summary:" + historyDigest([]Message{{"hash": s.PrefixHash, "content": s.Content}})
	if err := service.transcript.SaveSummary(context.Background(), "s_pending", s); err == nil {
		t.Fatal("checkpoint hid pending tool batch")
	}
}
