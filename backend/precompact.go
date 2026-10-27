package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

const preCompactRatio = 0.60
const precompactTimeout = 60 * time.Second
const precompactLease = 2 * time.Minute
const precompactRateWindow = 30 * time.Second
const precompactConcurrency = 2

// Precompression and reuse stay opt-in until the Part 3 quality gate passes.
// 1 disables new scheduling/reuse without deleting existing checkpoints.
func precompactRatioFromEnv() (float64, error) {
	ratio, err := strconv.ParseFloat(getenv("MINICOCO_PRECOMPACT_RATIO", "1"), 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || (ratio != 1 && (ratio <= 0 || ratio >= fullRatio)) {
		return 0, fmt.Errorf("MINICOCO_PRECOMPACT_RATIO must be 1 (off) or greater than 0 and less than 0.8")
	}
	return ratio, nil
}

type precompactAfterKey struct{}
type precompactAfter struct{ job *precompactJob }

// The round hands ownership of its immutable committed snapshot to the worker.
// No request context, response writer, mutable trace or pending input is retained.
type precompactJob struct {
	sid, workspace string
	mode           Mode
	route          ModelRoute
	view, raw      []Message
}

type precompactWorker struct {
	service   *sessionService
	ctx       context.Context
	cancel    context.CancelFunc
	ratio     float64
	timeout   time.Duration
	summarize func(context.Context, []Message, ModelRoute) (string, Usage, error)
	mu        sync.Mutex
	closed    bool
	inflight  map[string]bool
	wg        sync.WaitGroup
}

func newPrecompactWorker(parent context.Context, service *sessionService, ratio float64) *precompactWorker {
	ctx, cancel := context.WithCancel(parent)
	return &precompactWorker{service: service, ctx: ctx, cancel: cancel, ratio: ratio,
		timeout: precompactTimeout, summarize: summarize, inflight: map[string]bool{}}
}

func (w *precompactWorker) close() {
	w.mu.Lock()
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	w.wg.Wait() // All production operations use the cancelled worker context.
}

// No storage, coordinator or provider IO on the response path. Excess work is
// dropped, never queued, so long transcripts cannot accumulate without bound.
func (w *precompactWorker) submit(job precompactJob) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.ctx.Err() != nil || w.ratio == 1 || w.inflight[job.sid] || len(w.inflight) >= precompactConcurrency {
		return false
	}
	w.inflight[job.sid] = true
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			_ = recover() // Speculation must never terminate the backend.
			w.mu.Lock()
			delete(w.inflight, job.sid)
			w.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(w.ctx, w.timeout)
		defer cancel()
		_ = w.run(ctx, job) // Failed speculation leaves the normal P5 path intact.
	}()
	return true
}

func summaryConfigHash(route ModelRoute) string {
	// Route JSON contains all exported model parameters but not its map key.
	data, _ := json.Marshal(route)
	return historyDigest([]Message{{"model": route.Name, "route": string(data), "prompt": summaryPrompt}})
}

func (w *precompactWorker) run(ctx context.Context, job precompactJob) error {
	budget, err := budgetFor(job.view, job.workspace, job.mode, job.route)
	if err != nil || budget.Ratio < w.ratio || budget.Ratio >= fullRatio {
		return err
	}
	end, err := chooseRange(job.view, len(job.view), compactFull)
	if err != nil || end == 0 {
		return err
	}
	// A view replaces a raw prefix with two summary messages. The untouched
	// suffix determines the corresponding raw ordinal even after repeated compaction.
	count := len(job.raw) - (len(job.view) - end)
	if count < 1 || count > len(job.raw) || historyDigest(job.raw[count:]) != historyDigest(job.view[end:]) {
		return ErrStoreConflict // Includes transient trim views; do not speculate.
	}
	route, err := effectiveRoute(job.route)
	if err != nil {
		return err
	}
	prefix, source, config := historyDigest(job.raw[:count]), historyDigest(job.view[:end]), summaryConfigHash(route)
	unlock, ok, err := w.service.coord.Lock(ctx, "precompact:"+job.sid, precompactLease)
	if err != nil || !ok {
		return err
	}
	defer unlock()
	records, err := w.service.transcript.Summaries(ctx, job.sid)
	if err != nil {
		return err
	}
	canonical, err := mergedHistory(job.raw, records, allHistory)
	if err != nil || historyDigest(canonical) != historyDigest(job.view) {
		return err // View changed or was trimmed transiently; skip this optimization.
	}
	for _, s := range records {
		if s.Ready && s.PrefixCount == count && s.PrefixHash == prefix && s.SourceHash == source && s.ConfigHash == config && validateSummary(s, job.raw) == nil {
			return nil
		}
	}
	if !w.service.coord.Allow(ctx, "rate:precompact:"+job.sid, 1, precompactRateWindow) {
		return nil
	}
	text, usage, err := w.summarize(ctx, job.view[:end], route)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next := append(summaryMessages(text), job.view[end:]...)
	if len(next) >= len(job.view) || estimateTokens(next) >= budget.Used {
		return cannotCompress("precomputed summary did not reduce context")
	}
	if err := historyBudget(next); err != nil {
		return err
	}
	id, err := storeID("p_")
	if err != nil {
		return err
	}
	checkpoint := Summary{Ready: true, SourceHash: source, ConfigHash: config,
		OriginStartID: "message:1", OriginEndID: fmt.Sprint("message:", count), PrefixCount: count,
		PrefixHash: prefix, Content: text, Usage: usage, UsageID: "\x00precompact:" + id}
	if err := w.service.transcript.SaveSummary(ctx, job.sid, checkpoint); err != nil {
		return err
	}
	// Save first; reconcile recovers a lost metadata write and accounts unused
	// candidates too. Unique computation IDs count distinct provider calls separately.
	return w.service.meta.CommitRoundUsage(ctx, job.sid, checkpoint.UsageID, usage, len(job.raw))
}
