package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const reuseFacts = "decisions: Go; paths: app.go; unresolved: tests; constraints: readonly"
const refinedFacts = reuseFacts + "; new decision: cache"

func readyFor(t *testing.T, raw, source []Message, count int, route ModelRoute) Summary {
	t.Helper()
	effective, err := effectiveRoute(route)
	if err != nil {
		t.Fatal(err)
	}
	return Summary{Ready: true, SourceHash: historyDigest(source), ConfigHash: summaryConfigHash(effective),
		OriginStartID: "message:1", OriginEndID: fmt.Sprint("message:", count), PrefixCount: count,
		PrefixHash: historyDigest(raw[:count]), Content: reuseFacts, Usage: Usage{3, 2, 5}, UsageID: "\x00precompact:p_reuse_test"}
}

func summarySSE(w http.ResponseWriter, text string, total int) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": text}, "finish_reason": "stop"}}})+sse(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": total - 1, "completion_tokens": 1, "total_tokens": total}})+"data: [DONE]\n\n")
}

func runReusePersistence(t *testing.T, factory contractFactory) {
	for _, delta := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("delta=%v/stream=%v", delta, stream), func(t *testing.T) {
				open := factory(t)
				stores := open()
				defer func() { stores.close() }()
				service := &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
				previous := activeSessions
				activeSessions = service
				defer func() { activeSessions = previous }()
				t.Setenv("LOOP_IMPL", "v2")
				t.Setenv("LLM_PROVIDER", "")
				t.Setenv("LLM_BASE_URL", "")
				t.Setenv("LLM_API_KEY", "fake-reuse-key")
				raw := oldContext()
				if delta {
					raw = append(raw, Message{"role": "user", "content": "new decision: cache"}, Message{"role": "assistant", "content": "keep readonly"})
				}
				var fullCalls, foldCalls, mainCalls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Messages []Message `json:"messages"`
						Tools    any       `json:"tools"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if len(body.Messages) == 0 {
						t.Error("empty prompt")
						w.WriteHeader(500)
						return
					}
					switch body.Messages[0]["content"] {
					case foldPrompt:
						foldCalls.Add(1)
						var payload struct {
							Previous string    `json:"previousSummary"`
							Delta    []Message `json:"delta"`
						}
						encoded := fmt.Sprint(body.Messages[1]["content"])
						if json.Unmarshal([]byte(encoded), &payload) != nil || payload.Previous != reuseFacts || !reflect.DeepEqual(payload.Delta, raw[4:]) || body.Tools != nil {
							t.Error("refine did not send exactly previous summary + raw delta")
						}
						full, _ := json.Marshal(raw)
						if len(encoded)*2 >= len(full) {
							t.Error("refine input not smaller than full history")
						}
						summarySSE(w, refinedFacts, 3)
					case summaryPrompt:
						fullCalls.Add(1)
						w.WriteHeader(500)
					default:
						mainCalls.Add(1)
						summarySSE(w, "answer", 2)
					}
				}))
				defer upstream.Close()
				route := ModelRoute{Name: "reuse", Provider: "openai", BaseURL: upstream.URL, Family: "openai", MaxTokens: 100000, IsFunctionCall: true}
				b, _ := budgetFor(raw, "/tmp", ModeFull, route)
				route.MaxTokens = b.Base + int(float64(b.Used)/0.9)
				writeTestRoutes(t, map[string]ModelRoute{"reuse": route})
				seedCompression(t, service, "s_reuse", "reuse", raw)
				ready := readyFor(t, raw, raw[:4], 4, route)
				if err := service.transcript.SaveSummary(context.Background(), "s_reuse", ready); err != nil {
					t.Fatal(err)
				}
				service.precompact = newPrecompactWorker(context.Background(), service, preCompactRatio)
				defer service.precompact.close()
				// Busy speculative lease must not block reuse. Exact hits also bypass
				// the provider rate gate, since they make no summary model call.
				unlock, ok, err := service.coord.Lock(context.Background(), "precompact:s_reuse", time.Minute)
				if err != nil || !ok {
					t.Fatal(err)
				}
				defer unlock()
				if !delta {
					service.coord.Allow(context.Background(), "rate:summary", 1, 10*time.Second)
				}
				request := map[string]any{"sessionId": "s_reuse", "roundId": "reuse-round", "query": "continue", "workspace": "/tmp", "stream": stream}
				response, events := deltaResponse(t, request)
				requireRoundFinish(t, response, events, FinishStop)
				service.precompact.close()
				wantFold := int32(0)
				wantTokens := int64(7)
				wantText := reuseFacts
				if delta {
					wantFold = 1
					wantTokens = 10
					wantText = refinedFacts
				}
				if fullCalls.Load() != 0 || foldCalls.Load() != wantFold || mainCalls.Load() != 1 {
					t.Fatal("unexpected model calls", fullCalls.Load(), foldCalls.Load(), mainCalls.Load())
				}
				records, _ := service.transcript.Summaries(context.Background(), "s_reuse")
				formal, err := activeSummary(records, append(raw, Message{"role": "user", "content": "continue"}, Message{"role": "assistant", "content": "answer"}))
				if err != nil || formal == nil || formal.Ready || formal.Content != wantText || formal.DerivedFrom != ready.UsageID || formal.PrefixCount != len(raw) {
					t.Fatal("formal checkpoint invalid", formal, err)
				}
				for _, fact := range []string{"Go", "app.go", "tests", "readonly"} {
					if !strings.Contains(formal.Content, fact) {
						t.Fatal("fixture fact lost", fact)
					}
				}
				if err := service.transcript.SaveSummary(context.Background(), "s_reuse", *formal); err != nil {
					t.Fatal(err)
				}
				stores.close()
				stores = open()
				service = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
				activeSessions = service
				t.Setenv("LLM_PROVIDER", "invalid-for-replay")
				response, replay := deltaResponse(t, request)
				requireRoundFinish(t, response, replay, FinishStop)
				if !reflect.DeepEqual(events, replay) {
					t.Fatal("restart changed successful replay")
				}
				meta, _ := service.meta.GetSession(context.Background(), "s_reuse")
				if meta.TotalTokens != wantTokens || meta.MessageCount != len(raw)+2 {
					t.Fatal("reused usage double counted or raw count changed", meta)
				}
				view, _ := service.transcript.Latest(context.Background(), "s_reuse", allHistory)
				if len(view) != 4 || !strings.Contains(fmt.Sprint(view[1]["content"]), wantText) {
					t.Fatal("promoted/refined view lost on reopen", view)
				}
				canonical, _ := service.transcript.RawLatest(context.Background(), "s_reuse", allHistory)
				if !reflect.DeepEqual(canonical[:len(raw)], raw) {
					t.Fatal("raw history changed")
				}
				fork, err := service.fork(context.Background(), ForkRequest{SessionID: "s_reuse"})
				if err != nil {
					t.Fatal(err)
				}
				if err := service.reconcile(context.Background(), fork.NewSessionID); err != nil {
					t.Fatal(err)
				}
				forkMeta, _ := service.meta.GetSession(context.Background(), fork.NewSessionID)
				if forkMeta.TotalTokens != 0 {
					t.Fatal("fork inherited paid summary usage")
				}
			})
		}
	}
}

func TestFileReusePersistence(t *testing.T) { runReusePersistence(t, fileContractFactory) }

type failDerivedUsage struct {
	MetaStore
	id string
}

func (s failDerivedUsage) CommitRoundUsage(ctx context.Context, sid, id string, u Usage, n int) error {
	if id == s.id {
		return errors.New("injected metadata failure")
	}
	return s.MetaStore.CommitRoundUsage(ctx, sid, id, u, n)
}

func runReuseRecovery(t *testing.T, factory contractFactory) {
	open := factory(t)
	stores := open()
	defer func() { stores.close() }()
	service := &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	ctx := context.Background()
	raw := oldContext()
	t.Setenv("LLM_PROVIDER", "")
	route := ModelRoute{Name: "mock", Provider: "mock", Family: "openai", MaxTokens: 100000, IsFunctionCall: true}
	seedCompression(t, service, "s_recovery", "mock", raw)
	ready := readyFor(t, raw, raw, 4, route)
	if err := service.transcript.SaveSummary(ctx, "s_recovery", ready); err != nil {
		t.Fatal(err)
	}
	formal := ready
	formal.Ready = false
	formal.DerivedFrom = ready.UsageID
	formal.Usage = Usage{}
	formal.UsageID = promotionID(formal)
	for _, delta := range []bool{false, true} {
		if delta {
			formal.Content = refinedFacts
			formal.Usage = Usage{4, 3, 7}
			formal.UsageID = "\x00refine:r_recovery"
		}
		service.meta = failDerivedUsage{stores.meta, formal.UsageID}
		if service.commitReusedSummary(ctx, "s_recovery", ready, formal, len(raw)) == nil {
			t.Fatal("metadata failure hidden")
		}
		// The formal checkpoint is already authoritative after a failed ledger write.
		view, err := service.transcript.Latest(ctx, "s_recovery", allHistory)
		if err != nil || len(view) != 2 || !strings.Contains(fmt.Sprint(view[1]["content"]), formal.Content) {
			t.Fatal("checkpoint was not durable", err, view)
		}
		stores.close()
		stores = open()
		service = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
		for i := 0; i < 2; i++ {
			if err := service.reconcile(ctx, "s_recovery"); err != nil {
				t.Fatal(err)
			}
		}
		meta, _ := service.meta.GetSession(ctx, "s_recovery")
		want := int64(5)
		if delta {
			want = 12
		}
		if meta.TotalTokens != want || meta.MessageCount != len(raw) {
			t.Fatal("recovery double counted usage", meta)
		}
		if err := service.transcript.SaveSummary(ctx, "s_recovery", formal); err != nil {
			t.Fatal(err)
		}
		conflict := formal
		conflict.Content = "changed"
		if !errors.Is(service.transcript.SaveSummary(ctx, "s_recovery", conflict), ErrStoreConflict) && delta {
			t.Fatal("same computation overwritten")
		}
		records, _ := service.transcript.Summaries(ctx, "s_recovery")
		if candidate := findReadySummary(records, raw, raw, len(raw), len(raw), ready.ConfigHash); candidate != nil {
			t.Fatal("consumed candidate remained eligible")
		}
	}
}

func TestFileReuseRecovery(t *testing.T) { runReuseRecovery(t, fileContractFactory) }

func TestReuseViewMappingAndProtectedTools(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			service, _ := newSessionService(t.TempDir())
			ctx := context.Background()
			raw := append(oldContext(), oldContext()...)
			pair := toolPair()
			raw = append(raw, pair[0])
			input := pair[1:]
			if !native {
				input, _ = (XmlFormatter{}).Format([]ToolUseResult{{ID: "a", Name: "list_dir", Output: "tool result"}})
			}
			seedCompression(t, service, "s_mapping", "mock", raw)
			previous := Summary{OriginStartID: "message:1", OriginEndID: "message:4", PrefixCount: 4, PrefixHash: historyDigest(raw[:4]), Content: "previous formal"}
			previous.UsageID = "\x00summary:" + historyDigest([]Message{{"hash": previous.PrefixHash, "content": previous.Content}})
			if err := service.transcript.SaveSummary(ctx, "s_mapping", previous); err != nil {
				t.Fatal(err)
			}
			view, _ := service.transcript.Latest(ctx, "s_mapping", allHistory)
			route := ModelRoute{Name: "mock", Provider: "mock", Family: "openai", MaxTokens: 100000, IsFunctionCall: native}
			ready := readyFor(t, raw, view[:6], 8, route)
			if err := service.transcript.SaveSummary(ctx, "s_mapping", ready); err != nil {
				t.Fatal(err)
			}
			service.precompact = newPrecompactWorker(ctx, service, preCompactRatio)
			defer service.precompact.close()
			current := append(append([]Message{}, view...), input...)
			attempts := 0
			next, err := service.compactContext(ctx, ChatRequest{SessionID: "s_mapping", Workspace: "/tmp"}, route, ModeFull, current, raw, input, compactFull, &attempts)
			if err != nil || !strings.Contains(fmt.Sprint(next[1]["content"]), reuseFacts) || !reflect.DeepEqual(next[2:], append([]Message{pair[0]}, input...)) {
				t.Fatal("view mapping split protected batch", err, next)
			}
			// Same raw history, different active-summary text: source hash must miss.
			changed := append([]Message{}, current...)
			changed[1] = Message{"role": "assistant", "content": "different prior summary"}
			if findReadySummary([]Summary{ready}, raw, changed, 6, 8, ready.ConfigHash) != nil {
				t.Fatal("stale summary view reused")
			}
			// A newer/larger candidate cannot consume the protected pending call.
			bad := ready
			bad.PrefixCount = len(raw)
			bad.PrefixHash = historyDigest(raw)
			if findReadySummary([]Summary{bad}, raw, current, 6, 8, ready.ConfigHash) != nil {
				t.Fatal("pending tool boundary reused")
			}
		})
	}
}

// Optional corrupt candidates must act as misses, without poisoning Latest.
type candidateOverlay struct {
	TranscriptStore
	change func([]Summary) []Summary
}

func (s candidateOverlay) Summaries(ctx context.Context, sid string) ([]Summary, error) {
	records, err := s.TranscriptStore.Summaries(ctx, sid)
	return s.change(records), err
}

func TestReuseFallbackAndCancellation(t *testing.T) {
	for _, scenario := range []string{"missing", "hash", "source", "config", "oversized-range", "off", "delta-failure", "delta-overflow", "delta-no-progress", "cancel", "sync-failure", "busy-worker"} {
		t.Run(scenario, func(t *testing.T) {
			service, _ := newSessionService(t.TempDir())
			t.Setenv("LLM_PROVIDER", "")
			t.Setenv("LLM_BASE_URL", "")
			t.Setenv("LLM_API_KEY", "fake-fallback-key")
			raw := append(oldContext(), Message{"role": "user", "content": "new decision: cache"}, Message{"role": "assistant", "content": "ok"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var full, fold atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []Message `json:"messages"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.Messages[0]["content"] == foldPrompt {
					fold.Add(1)
					switch scenario {
					case "cancel":
						cancel()
						<-r.Context().Done()
					case "delta-overflow":
						summarySSE(w, strings.Repeat("x", maxSummaryBytes+1), 3)
					case "delta-no-progress":
						summarySSE(w, strings.Repeat("x", 7000), 3)
					default:
						w.WriteHeader(503)
					}
					return
				}
				full.Add(1)
				if scenario == "sync-failure" {
					w.WriteHeader(503)
					return
				}
				summarySSE(w, "synchronous fallback summary", 3)
			}))
			defer upstream.Close()
			route := ModelRoute{Name: "fallback", Provider: "openai", BaseURL: upstream.URL, Family: "openai", MaxTokens: 100000, IsFunctionCall: true}
			seedCompression(t, service, "s_fallback", "fallback", raw)
			ready := readyFor(t, raw, raw[:4], 4, route)
			if scenario != "missing" && scenario != "busy-worker" {
				if err := service.transcript.SaveSummary(ctx, "s_fallback", ready); err != nil {
					t.Fatal(err)
				}
			}
			service.transcript = candidateOverlay{service.transcript, func(records []Summary) []Summary {
				for i := range records {
					switch scenario {
					case "hash":
						records[i].PrefixHash = strings.Repeat("0", 64)
					case "source":
						records[i].SourceHash = strings.Repeat("0", 64)
					case "config":
						records[i].ConfigHash = strings.Repeat("0", 64)
					case "oversized-range":
						records[i].PrefixCount = len(raw) + 1
					}
				}
				return records
			}}
			ratio := preCompactRatio
			if scenario == "off" {
				ratio = 1
			}
			service.precompact = newPrecompactWorker(context.Background(), service, ratio)
			defer service.precompact.close()
			unlock, _, _ := service.coord.Lock(ctx, "precompact:s_fallback", time.Minute)
			defer unlock()
			input := []Message{{"role": "user", "content": "next"}}
			current := append(append([]Message{}, raw...), input...)
			attempts := 0
			next, err := service.compactContext(ctx, ChatRequest{SessionID: "s_fallback", Workspace: "/tmp"}, route, ModeFull, current, raw, input, compactFull, &attempts)
			if scenario == "cancel" {
				if !errors.Is(err, context.Canceled) || full.Load() != 0 {
					t.Fatal("cancellation fell back", err, full.Load())
				}
				return
			}
			if scenario == "sync-failure" {
				if err == nil || full.Load() != 1 {
					t.Fatal("sync failure hidden", err)
				}
				return
			}
			if err != nil || full.Load() != 1 || len(next) != 3 || next[2]["content"] != "next" {
				t.Fatal("did not fall back safely", err, full.Load(), next)
			}
			want := int32(0)
			if strings.HasPrefix(scenario, "delta-") {
				want = 1
			}
			if fold.Load() != want || attempts != 1 {
				t.Fatal("unexpected fallback calls/attempts", fold.Load(), attempts)
			}
		})
	}
}
