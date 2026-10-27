package main

import (
	"bytes"
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

func TestPrecompactHTTPTransportsAndReplay(t *testing.T) {
	for _, loop := range []string{"v1", "v2"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", loop, stream), func(t *testing.T) {
				service := sessionFixture(t)
				t.Setenv("LOOP_IMPL", loop)
				t.Setenv("LLM_PROVIDER", "")
				preserveMockBudget(t)
				job := precompactFixture(t, service, "s_http_prewarm")
				writeTestRoutes(t, map[string]ModelRoute{"mock": job.route})
				service.precompact = newPrecompactWorker(context.Background(), service, preCompactRatio)
				defer service.precompact.close()
				started := make(chan struct{}, 1)
				release := make(chan struct{})
				defer close(release)
				var calls atomic.Int32
				service.precompact.summarize = func(ctx context.Context, _ []Message, _ ModelRoute) (string, Usage, error) {
					calls.Add(1)
					started <- struct{}{}
					select {
					case <-release:
						return "summary", Usage{}, nil
					case <-ctx.Done():
						return "", Usage{}, ctx.Err()
					}
				}
				request := map[string]any{"sessionId": job.sid, "roundId": "warm-round", "query": "inspect", "workspace": "/tmp", "stream": stream}
				data, _ := json.Marshal(request)
				w := &precompactResponseGate{ResponseRecorder: httptest.NewRecorder(), written: make(chan struct{}), release: make(chan struct{})}
				done := make(chan struct{})
				go func() {
					handleChatStream(w, httptest.NewRequest("POST", "/chat/stream", bytes.NewReader(data)))
					close(done)
				}()
				<-w.written
				if calls.Load() != 0 {
					t.Error("worker ran before response write")
				}
				close(w.release)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("response blocked on summary")
				}
				if w.Code != 200 || !strings.Contains(w.Body.String(), "tool_calls") {
					t.Fatal(w.Body.String())
				}
				if loop == "v2" {
					select {
					case <-started:
					case <-time.After(3 * time.Second):
						t.Fatal("v2 did not schedule")
					}
				}
				if loop == "v1" && calls.Load() != 0 {
					t.Fatal("v1 scheduled speculation")
				}
				// Successful replay must never create a second job, even while worker runs.
				response, events := deltaResponse(t, request)
				requireRoundFinish(t, response, events, FinishToolCalls)
				service.precompact.close()
				want := int32(0)
				if loop == "v2" {
					want = 1
				}
				if calls.Load() != want {
					t.Fatal("replay triggered provider work", calls.Load())
				}
			})
		}
	}
}

type precompactResponseGate struct {
	*httptest.ResponseRecorder
	written, release chan struct{}
	first            bool
}

func (w *precompactResponseGate) Write(data []byte) (int, error) {
	if !w.first {
		w.first = true
		close(w.written)
		<-w.release
	}
	return w.ResponseRecorder.Write(data)
}

func precompactFixture(t *testing.T, service *sessionService, sid string) precompactJob {
	t.Helper()
	raw := append(oldContext(), Message{"role": "user", "content": "latest task"}, Message{"role": "assistant", "content": "latest answer"})
	route := ModelRoute{Name: "mock", Provider: "mock", Family: "openai", MaxTokens: 100000, IsFunctionCall: true}
	b, err := budgetFor(raw, "/tmp", ModeFull, route)
	if err != nil {
		t.Fatal(err)
	}
	route.MaxTokens = b.Base + int(float64(b.Used)/0.62)
	seedCompression(t, service, sid, "mock", raw)
	return precompactJob{sid: sid, workspace: "/tmp", mode: ModeFull, route: route, raw: raw, view: raw}
}

func runPrecompactPersistence(t *testing.T, factory contractFactory) {
	open := factory(t)
	stores := open()
	defer func() { stores.close() }()
	service := &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	t.Setenv("LLM_PROVIDER", "")
	job := precompactFixture(t, service, "s_prewarm")
	w := newPrecompactWorker(context.Background(), service, preCompactRatio)
	defer w.close()
	calls := 0
	w.summarize = func(ctx context.Context, messages []Message, route ModelRoute) (string, Usage, error) {
		calls++
		if !reflect.DeepEqual(messages, job.raw[:5]) {
			t.Error("wrong immutable source")
		}
		return "decisions, paths, constraints", Usage{3, 2, 5}, nil
	}
	ctx := context.Background()
	if err := w.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := w.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	summaries, err := service.transcript.Summaries(ctx, job.sid)
	if err != nil || calls != 1 || len(summaries) != 1 {
		t.Fatal(calls, summaries, err)
	}
	s := summaries[0]
	if !s.Ready || s.PrefixCount != 5 || s.PrefixHash != historyDigest(job.raw[:5]) || s.SourceHash != historyDigest(job.view[:5]) || !validDigest(s.ConfigHash) {
		t.Fatal("invalid candidate", s)
	}
	// Re-saving one computation is idempotent, conflicting mutation is rejected.
	if err := service.transcript.SaveSummary(ctx, job.sid, s); err != nil {
		t.Fatal(err)
	}
	bad := s
	bad.Content = "conflicting content"
	if !errors.Is(service.transcript.SaveSummary(ctx, job.sid, bad), ErrStoreConflict) {
		t.Fatal("candidate overwrite accepted")
	}
	bad = s
	bad.PrefixCount = 0
	if service.transcript.SaveSummary(ctx, job.sid, bad) == nil {
		t.Fatal("legacy candidate accepted")
	}
	view, err := service.transcript.Latest(ctx, job.sid, allHistory)
	if err != nil || !reflect.DeepEqual(view, job.raw) {
		t.Fatal("candidate changed live history", err)
	}
	active, err := activeSummary([]Summary{s, {Ready: true}}, job.raw)
	if err != nil || active != nil {
		t.Fatal("candidate poisoned active selection", err)
	}
	// A different source within 30s cannot issue another provider request.
	job.raw = append(job.raw, Message{"role": "user", "content": "next"}, Message{"role": "assistant", "content": "done"})
	job.view = job.raw
	if err := w.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("per-session rate limit did not coalesce")
	}
	// Simulate persistence followed by failed metadata accounting, then restart.
	s.UsageID = "\x00precompact:p_crash_recovery"
	if err := service.transcript.SaveSummary(ctx, job.sid, s); err != nil {
		t.Fatal(err)
	}
	stores.close()
	stores = open()
	service = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	for i := 0; i < 2; i++ {
		if err := service.reconcile(ctx, job.sid); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := service.meta.GetSession(ctx, job.sid)
	if err != nil || meta.TotalTokens != 10 || meta.MessageCount != 6 {
		t.Fatal("unused candidates not accounted exactly once", meta, err)
	}
	view, err = service.transcript.Latest(ctx, job.sid, allHistory)
	if err != nil || len(view) != 6 || view[0]["content"] != job.raw[0]["content"] {
		t.Fatal("restart promoted candidate", err)
	}
	fork, err := service.fork(ctx, ForkRequest{SessionID: job.sid})
	if err != nil {
		t.Fatal(err)
	}
	forkSummaries, err := service.transcript.Summaries(ctx, fork.NewSessionID)
	if err != nil || len(forkSummaries) != 0 {
		t.Fatal("fork copied speculative candidates", forkSummaries, err)
	}
}

func TestFilePrecompactPersistence(t *testing.T) { runPrecompactPersistence(t, fileContractFactory) }

func TestPrecompactLifecycle(t *testing.T) {
	for _, scenario := range []string{"error", "panic", "timeout", "shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			service, err := newSessionService(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			job := precompactFixture(t, service, "s_lifecycle")
			w := newPrecompactWorker(context.Background(), service, preCompactRatio)
			defer w.close()
			w.timeout = 50 * time.Millisecond
			started := make(chan struct{})
			w.summarize = func(ctx context.Context, _ []Message, _ ModelRoute) (string, Usage, error) {
				close(started)
				switch scenario {
				case "error":
					return "", Usage{}, errors.New("injected provider failure")
				case "panic":
					panic("injected panic")
				default:
					<-ctx.Done()
					return "late answer", Usage{1, 1, 2}, nil
				}
			}
			if !w.submit(job) {
				t.Fatal("job not scheduled")
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not start")
			}
			if scenario == "shutdown" {
				w.close()
			} else {
				w.wg.Wait()
			}
			records, err := service.transcript.Summaries(context.Background(), job.sid)
			if err != nil || len(records) != 0 {
				t.Fatal("failed/late worker persisted candidate", records, err)
			}
			if scenario == "shutdown" && w.submit(job) {
				t.Fatal("scheduled after close")
			}
			// A speculative failure cannot alter the main P5 state.
			view, err := service.transcript.Latest(context.Background(), job.sid, allHistory)
			if err != nil || !reflect.DeepEqual(view, job.raw) {
				t.Fatal("failure changed normal context", err)
			}
		})
	}
}

func TestPrecompactAdmissionAndBounds(t *testing.T) {
	service, err := newSessionService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := newPrecompactWorker(context.Background(), service, preCompactRatio)
	defer w.close()
	started := make(chan struct{}, precompactConcurrency)
	release := make(chan struct{})
	defer close(release)
	w.summarize = func(ctx context.Context, _ []Message, _ ModelRoute) (string, Usage, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		case <-release:
			return "summary", Usage{}, nil
		}
	}
	first := precompactFixture(t, service, "s_limit0")
	if !w.submit(first) {
		t.Fatal("first job rejected")
	}
	<-started
	if w.submit(first) {
		t.Fatal("same-session parallel work")
	}
	// A second manager has no shared local admission map; the coordinator lease
	// must still prevent duplicate provider calls without waiting for the first.
	peer := newPrecompactWorker(context.Background(), service, preCompactRatio)
	defer peer.close()
	peer.summarize = func(context.Context, []Message, ModelRoute) (string, Usage, error) {
		t.Error("shared lease bypassed")
		return "", Usage{}, nil
	}
	if err := peer.run(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if !w.submit(precompactFixture(t, service, "s_limit1")) {
		t.Fatal("second job rejected")
	}
	<-started
	if w.submit(precompactFixture(t, service, "s_limit2")) {
		t.Fatal("unbounded concurrency")
	}
	w.close()
}

func TestPrecompactExistingSummaryAndToolBoundaries(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			service, _ := newSessionService(t.TempDir())
			pair := toolPair()
			if !native {
				results, _ := (XmlFormatter{}).Format([]ToolUseResult{{ID: "a", Name: "list_dir", Output: "result"}})
				pair = append(pair[:1], results...)
			}
			raw := append(append(oldContext(), oldContext()...), pair...)
			raw = append(raw, Message{"role": "assistant", "content": "done"})
			seedCompression(t, service, "s_mapped", "mock", raw)
			formal := Summary{OriginStartID: "message:1", OriginEndID: "message:4", PrefixCount: 4, PrefixHash: historyDigest(raw[:4]), Content: "existing formal summary"}
			formal.UsageID = "\x00summary:" + historyDigest([]Message{{"hash": formal.PrefixHash, "content": formal.Content}})
			if err := service.transcript.SaveSummary(context.Background(), "s_mapped", formal); err != nil {
				t.Fatal(err)
			}
			view, _ := service.transcript.Latest(context.Background(), "s_mapped", allHistory)
			route := ModelRoute{Name: "mock", Provider: "mock", Family: "openai", MaxTokens: 100000, IsFunctionCall: native}
			b, _ := budgetFor(view, "/tmp", ModeFull, route)
			route.MaxTokens = b.Base + int(float64(b.Used)/0.62)
			w := newPrecompactWorker(context.Background(), service, preCompactRatio)
			defer w.close()
			job := precompactJob{sid: "s_mapped", workspace: "/tmp", mode: ModeFull, route: route, raw: raw, view: view}
			if err := w.run(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			records, _ := service.transcript.Summaries(context.Background(), job.sid)
			if len(records) != 2 || !records[1].Ready || records[1].PrefixCount != 8 || records[1].PrefixHash != historyDigest(raw[:8]) {
				t.Fatal("view/raw mapping or protected tool block lost", records)
			}
			after, _ := service.transcript.Latest(context.Background(), job.sid, allHistory)
			if !reflect.DeepEqual(view, after) {
				t.Fatal("candidate replaced formal summary")
			}
		})
	}
}

func TestPrecompactWindowAndSourceSafety(t *testing.T) {
	for _, ratio := range []float64{0.59, 0.60, 0.79, 0.80, 0.90} {
		t.Run(fmt.Sprint(ratio), func(t *testing.T) {
			service, _ := newSessionService(t.TempDir())
			job := precompactFixture(t, service, "s_window")
			// Fix Used at 1000 for exact threshold tests.
			job.raw[0]["content"] = strings.Repeat("x", 2000)
			for i := 1; i < len(job.raw); i++ {
				job.raw[i]["content"] = ""
			}
			b, _ := budgetFor(job.view, job.workspace, job.mode, job.route)
			job.route.MaxTokens = b.Base + int(1000/ratio)
			w := newPrecompactWorker(context.Background(), service, preCompactRatio)
			defer w.close()
			calls := 0
			w.summarize = func(context.Context, []Message, ModelRoute) (string, Usage, error) {
				calls++
				return "", Usage{}, errors.New("stop before save")
			}
			_ = w.run(context.Background(), job)
			want := 0
			if ratio >= 0.60 && ratio < 0.80 {
				want = 1
			}
			if calls != want {
				t.Fatal("wrong prewarm window", calls, want)
			}
		})
	}
	service, _ := newSessionService(t.TempDir())
	job := precompactFixture(t, service, "s_source")
	w := newPrecompactWorker(context.Background(), service, preCompactRatio)
	defer w.close()
	var calls atomic.Int32
	w.summarize = func(context.Context, []Message, ModelRoute) (string, Usage, error) {
		calls.Add(1)
		return "summary", Usage{}, nil
	}
	// A transient trim is not a summary of the complete raw prefix.
	job.view = job.view[1:]
	b, _ := budgetFor(job.view, job.workspace, job.mode, job.route)
	job.route.MaxTokens = b.Base + int(float64(b.Used)/0.62)
	_ = w.run(context.Background(), job)
	if calls.Load() != 0 {
		t.Fatal("transient trim accepted as full prefix")
	}
}

func TestPrecompactConfig(t *testing.T) {
	for _, value := range []string{"", "1", "0.6", "0", "0.8", "NaN", "Inf", "nope"} {
		t.Setenv("MINICOCO_PRECOMPACT_RATIO", value)
		ratio, err := precompactRatioFromEnv()
		valid := value == "" || value == "1" || value == "0.6"
		if (err == nil) != valid {
			t.Fatal(value, ratio, err)
		}
		if value == "" && ratio != 1 {
			t.Fatal("default enabled before quality gate")
		}
	}
	route := ModelRoute{Name: "one", Provider: "mock", Temperature: 0.7}
	hash := summaryConfigHash(route)
	route.Temperature = 0
	if hash == summaryConfigHash(route) {
		t.Fatal("temperature absent from cache identity")
	}
	service, _ := newSessionService(t.TempDir())
	w := newPrecompactWorker(context.Background(), service, 1)
	defer w.close()
	if w.submit(precompactJob{}) {
		t.Fatal("off switch scheduled work")
	}
}

func TestPrecompactUsesProductionSummarizer(t *testing.T) {
	service, _ := newSessionService(t.TempDir())
	job := precompactFixture(t, service, "s_provider_snapshot")
	t.Setenv("LLM_API_KEY", "fake-precompact-key")
	t.Setenv("LLM_PROVIDER", "")
	t.Setenv("LLM_BASE_URL", "")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages    []Message `json:"messages"`
			Temperature float64   `json:"temperature"`
			Model       string    `json:"model"`
			Tools       any       `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Messages) != 2 || body.Messages[0]["content"] != summaryPrompt || body.Temperature != 0.7 || body.Model != "snapshot-model" || body.Tools != nil {
			t.Errorf("unexpected summary request: %+v", body)
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "Preserved decisions and paths"}, "finish_reason": "stop"}}})+sse(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})+"data: [DONE]\n\n")
	}))
	defer upstream.Close()
	job.route.Name = "snapshot-model"
	job.route.Provider = "openai"
	job.route.BaseURL = upstream.URL
	job.route.Temperature = 0.7
	// Freeze the effective endpoint before the worker, as the HTTP handoff does.
	job.route, _ = effectiveRoute(job.route)
	w := newPrecompactWorker(context.Background(), service, preCompactRatio)
	defer w.close()
	if err := w.run(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("production summarizer not called")
	}
	meta, _ := service.meta.GetSession(context.Background(), job.sid)
	if meta.TotalTokens != 5 {
		t.Fatal("provider usage lost", meta)
	}
}

func TestPrecompactFailedRoundDoesNotSchedule(t *testing.T) {
	service := sessionFixture(t)
	t.Setenv("LOOP_IMPL", "v2")
	t.Setenv("LLM_PROVIDER", "")
	preserveMockBudget(t)
	job := precompactFixture(t, service, "s_failed_prewarm")
	writeTestRoutes(t, map[string]ModelRoute{"mock": job.route})
	service.precompact = newPrecompactWorker(context.Background(), service, preCompactRatio)
	defer service.precompact.close()
	service.precompact.summarize = func(context.Context, []Message, ModelRoute) (string, Usage, error) {
		t.Error("failed round scheduled precompression")
		return "", Usage{}, nil
	}
	flakyRemaining.Store(1)
	_, events := deltaResponse(t, map[string]any{"sessionId": job.sid, "roundId": "failed-warm", "query": "inspect", "workspace": "/tmp", "stream": false})
	service.precompact.close()
	if len(events) == 0 || events[len(events)-1].FinishReason != "error" {
		t.Fatal(events)
	}
	records, _ := service.transcript.Summaries(context.Background(), job.sid)
	if len(records) != 0 {
		t.Fatal("failed request persisted candidate")
	}
}
