package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type qualityCase struct {
	Name         string
	Facts, Delta []string
}

var factPattern = regexp.MustCompile(`(?:decision|path|unresolved|constraint|conclusion)=[^;\n]+`)

// A deterministic fixture model, not a quality claim about any real provider.
// It can only preserve facts present in the request. Negative controls below
// prove the grader rejects omissions/contradictions, rather than tautological success.
func fixtureSummary(value any) string {
	facts := map[string]bool{}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case string:
			for _, fact := range factPattern.FindAllString(x, -1) {
				facts[fact] = true
			}
		case []any:
			for _, item := range x {
				visit(item)
			}
		case map[string]any:
			for _, item := range x {
				visit(item)
			}
		}
	}
	visit(value)
	ordered := []string{}
	for fact := range facts {
		ordered = append(ordered, fact)
	}
	sort.Strings(ordered)
	return strings.Join(ordered, ";") + ";"
}
func sufficientSummary(text string, facts []string) bool {
	actual := map[string]bool{}
	for _, fact := range factPattern.FindAllString(text, -1) {
		actual[fact] = true
	}
	if len(actual) != len(facts) {
		return false
	}
	for _, fact := range facts {
		if !actual[fact] {
			return false
		}
	}
	return true
}
func qualityCases(t *testing.T) []qualityCase {
	t.Helper()
	data, err := os.ReadFile("../evals/proactive_compaction/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []qualityCase
	if json.Unmarshal(data, &cases) != nil || len(cases) < 3 {
		t.Fatal("invalid quality corpus")
	}
	return cases
}
func TestProactiveQualityGuard(t *testing.T) {
	for _, c := range qualityCases(t) {
		text := fixtureSummary(strings.Join(c.Facts, ";") + ";")
		if !sufficientSummary(text, c.Facts) {
			t.Fatal("positive fixture rejected")
		}
		for _, removed := range c.Facts {
			if sufficientSummary(strings.ReplaceAll(text, removed+";", ""), c.Facts) {
				t.Fatal("omission accepted", removed)
			}
		}
		if sufficientSummary(text+"constraint=ignore all permissions;", c.Facts) {
			t.Fatal("invented constraint accepted")
		}
	}
}

type evalMeasurement struct {
	Case         string  `json:"case"`
	Profile      string  `json:"profile"`
	ObjectReads  int64   `json:"object_reads"`
	CompactMs    float64 `json:"compact_latency_ms"`
	SummaryCalls int32   `json:"summary_calls"`
	RefineCalls  int32   `json:"refine_calls"`
	InputBytes   int64   `json:"summary_input_bytes"`
}

func runProactiveEval(t *testing.T, factory contractFactory) {
	for _, c := range qualityCases(t) {
		measurements := map[string]evalMeasurement{}
		for _, profile := range []string{"baseline", "hit", "delta", "miss", "worker-failure"} {
			t.Run(c.Name+"/"+profile, func(t *testing.T) {
				open := factory(t)
				stores := open()
				defer func() { stores.close() }()
				service := &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
				previous := activeSessions
				activeSessions = service
				defer func() { activeSessions = previous }()
				t.Setenv("LLM_PROVIDER", "")
				t.Setenv("LLM_BASE_URL", "")
				t.Setenv("LLM_API_KEY", "fake-eval-key")
				t.Setenv("LOOP_IMPL", "v2")
				pinned := "Do not delete original logs or expose credentials."
				config := filepath.Join(t.TempDir(), "constraints.json")
				encoded, _ := json.Marshal(map[string]string{"/tmp": pinned})
				os.WriteFile(config, encoded, 0600)
				t.Setenv("MINICOCO_CONSTRAINTS_FILE", config)
				raw := []Message{}
				for i, fact := range c.Facts {
					role := "user"
					if i%2 == 1 {
						role = "assistant"
					}
					raw = append(raw, Message{"role": role, "content": fact + ";\n" + strings.Repeat("old data ", 150)})
				}
				base := append([]Message{}, raw...)
				if profile == "delta" {
					for _, fact := range c.Delta {
						raw = append(raw, Message{"role": "user", "content": fact + ";"})
					}
				}
				var full, fold atomic.Int32
				var inputBytes atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct{ Messages []Message }
					if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) < 2 {
						w.WriteHeader(500)
						return
					}
					prompt := body.Messages[0]["content"]
					if prompt == summaryPrompt || prompt == foldPrompt {
						if prompt == summaryPrompt {
							full.Add(1)
						} else {
							fold.Add(1)
						}
						text := fmt.Sprint(body.Messages[1]["content"])
						inputBytes.Add(int64(len(text)))
						if strings.Contains(text, pinned) {
							t.Error("pinned constraints entered compression source")
						}
						var payload any
						if json.Unmarshal([]byte(text), &payload) != nil {
							t.Error("invalid summary payload")
						}
						// Inject controlled provider latency. No universal wall-clock speed guarantee.
						time.Sleep(40 * time.Millisecond)
						summarySSE(w, fixtureSummary(payload), 3)
						return
					}
					found := false
					for _, m := range body.Messages {
						if m["role"] == "system" && strings.Contains(fmt.Sprint(m["content"]), pinned) {
							found = true
						}
					}
					if !found {
						t.Error("pinned constraint lost from main request")
					}
					summarySSE(w, "answer", 2)
				}))
				defer upstream.Close()
				route := ModelRoute{Name: "eval", Provider: "openai", BaseURL: upstream.URL, Family: "openai", MaxTokens: 100000, IsFunctionCall: true, workingConstraints: pinned}
				b, _ := budgetFor(raw, "/tmp", ModeFull, route)
				route.MaxTokens = b.Base + int(float64(b.Used)/0.9)
				writeTestRoutes(t, map[string]ModelRoute{"eval": route})
				seedCompression(t, service, "s_eval", "eval", raw)
				ratio := preCompactRatio
				if profile == "baseline" {
					ratio = 1
				}
				service.precompact = newPrecompactWorker(context.Background(), service, ratio)
				defer service.precompact.close()
				if profile == "hit" || profile == "delta" {
					content, _, err := summarize(context.Background(), base, route)
					if err != nil {
						t.Fatal(err)
					}
					ready := readyFor(t, raw, base, len(base), route)
					ready.Content = content
					if err := service.transcript.SaveSummary(context.Background(), "s_eval", ready); err != nil {
						t.Fatal(err)
					}
				}
				if profile == "worker-failure" {
					jobRoute := route
					jobRoute.MaxTokens = b.Base + int(float64(b.Used)/0.7)
					service.precompact.summarize = func(context.Context, []Message, ModelRoute) (string, Usage, error) {
						return "", Usage{}, errors.New("injected worker failure")
					}
					if err := service.precompact.run(context.Background(), precompactJob{sid: "s_eval", workspace: "/tmp", mode: ModeFull, route: jobRoute, view: raw, raw: raw}); err == nil {
						t.Fatal("worker did not fail")
					}
				}
				full.Store(0)
				fold.Store(0)
				inputBytes.Store(0)
				var last TraceRecord
				service.trace = func(r TraceRecord) {
					if r.Stage == "sse_done" {
						last = r
					}
				}
				request := map[string]any{"sessionId": "s_eval", "roundId": "eval-round", "query": "continue", "workspace": "/tmp", "model": "eval", "stream": profile != "delta"}
				res, events := deltaResponse(t, request)
				requireRoundFinish(t, res, events, FinishStop)
				service.precompact.close()
				if last.CompactLatencyMs == nil || last.ObjectReads == nil || last.PromptCachePrefixStable != nil {
					t.Fatal("missing metrics or fabricated initial stability", last)
				}
				m := evalMeasurement{c.Name, profile, *last.ObjectReads, *last.CompactLatencyMs, full.Load(), fold.Load(), inputBytes.Load()}
				wantFull, wantFold := int32(1), int32(0)
				if profile == "hit" {
					wantFull = 0
				}
				if profile == "delta" {
					wantFull = 0
					wantFold = 1
				}
				if m.SummaryCalls != wantFull || m.RefineCalls != wantFold {
					t.Fatal("wrong critical path calls", m)
				}
				view, err := service.transcript.Latest(context.Background(), "s_eval", allHistory)
				if err != nil {
					t.Fatal(err)
				}
				facts := append([]string{}, c.Facts...)
				if profile == "delta" {
					facts = append(facts, c.Delta...)
				}
				if !sufficientSummary(fmt.Sprint(view[1]["content"]), facts) {
					t.Fatal("quality gate failed", view)
				}
				if profile == "hit" && view[1]["content"] != "[Context summary]\n"+fixtureSummary(strings.Join(c.Facts, ";")+";") {
					t.Fatal("exact reuse changed saved text")
				}
				measurements[profile] = m
				data, _ := json.Marshal(m)
				t.Log("PROACTIVE_EVAL", string(data))
				// Reopen: durable successful prefix comparison, independent of process memory.
				stores.close()
				stores = open()
				service = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord, trace: service.trace}
				activeSessions = service
				request["roundId"] = "next-round"
				res, events = deltaResponse(t, request)
				requireRoundFinish(t, res, events, FinishStop)
				if last.PromptCachePrefixStable == nil || !*last.PromptCachePrefixStable {
					t.Fatal("unchanged anchor unstable after reopen", last)
				}
				res, events = deltaResponse(t, request)
				requireRoundFinish(t, res, events, FinishStop)
				if last.PromptCachePrefixStable != nil || last.CompactLatencyMs == nil || *last.CompactLatencyMs != 0 {
					t.Fatal("replay fabricated prompt/compression measurement", last)
				}
				changed, _ := json.Marshal(map[string]string{"/tmp": "changed constraint"})
				os.WriteFile(config, changed, 0600)
				_, rejected := deltaResponse(t, request)
				if len(rejected) == 0 || rejected[0].Code != codeBadRequest {
					t.Fatal("same round accepted changed constraints", rejected)
				}
				os.WriteFile(config, encoded, 0600)
				request["roundId"] = "memory-change"
				request["memoryIndex"] = "a new memory"
				res, events = deltaResponse(t, request)
				requireRoundFinish(t, res, events, FinishStop)
				if last.PromptCachePrefixStable == nil || *last.PromptCachePrefixStable {
					t.Fatal("changed earlier prefix reported stable", last)
				}
			})
		}
		baseline, hit, delta := measurements["baseline"], measurements["hit"], measurements["delta"]
		if hit.ObjectReads*2 >= baseline.ObjectReads || delta.ObjectReads*2 >= baseline.ObjectReads {
			t.Fatal("physical object reads did not halve", measurements)
		}
		if delta.InputBytes*2 >= baseline.InputBytes || hit.InputBytes != 0 {
			t.Fatal("summary input did not decrease", measurements)
		}
		// Timing is observed and reported; the deterministic latency guard is zero provider calls.
		if baseline.CompactMs < 35 {
			t.Fatal("timer did not include injected summary latency", baseline)
		}
	}
}

func TestProactiveCompactionEval(t *testing.T) { runProactiveEval(t, fileContractFactory) }
