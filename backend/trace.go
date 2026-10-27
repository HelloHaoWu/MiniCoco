package main

import (
	"context"
	"strconv"
	"time"
)

// Payloads, prompts, arguments and model text deliberately never enter traces.
// RoundID disambiguates different failed requests at the same committed ordinal.
type TraceRecord struct {
	CompactLatencyMs        *float64 `json:"compact_latency_ms,omitempty"`
	ObjectReads             *int64   `json:"object_reads,omitempty"`
	PromptCachePrefixStable *bool    `json:"prompt_cache_prefix_stable,omitempty"`
	TraceID                 string   `json:"traceId"`
	SessionID               string   `json:"sessionId"`
	RoundID                 string   `json:"roundId"`
	Round                   int      `json:"round"`
	Stage                   string   `json:"stage"`
	Model                   string   `json:"model"`
	Provider                string   `json:"provider"`
	Family                  string   `json:"family"`
	Slash                   string   `json:"slash,omitempty"`
	Tool                    string   `json:"tool,omitempty"`
	Usage                   *Usage   `json:"usage,omitempty"`
	Code                    string   `json:"code,omitempty"`
	Retryable               *bool    `json:"retryable,omitempty"`
	Replay                  bool     `json:"replay,omitempty"`
	DurationMs              int64    `json:"durationMs"`
	Ts                      int64    `json:"ts"`
}

type roundTrace struct {
	base TraceRecord
	last time.Time
	emit func(TraceRecord)
}
type traceContextKey struct{}

func (s *sessionService) traceContext(ctx context.Context, req ChatRequest) context.Context {
	if s == nil {
		return ctx
	}
	if ctx.Value(traceContextKey{}) != nil {
		return ctx
	}
	n := 1
	if rounds, err := s.transcript.CommittedRounds(ctx, req.SessionID); err == nil {
		n = len(rounds) + 1
		for i, r := range rounds {
			if r.ID == req.RoundID {
				n = i + 1
				break
			}
		}
	}
	route, _ := routeFor(req.Model)
	if effective, err := effectiveRoute(route); err == nil {
		route = effective
	}
	t := &roundTrace{base: TraceRecord{TraceID: req.SessionID + "-r" + strconv.Itoa(n), SessionID: req.SessionID, RoundID: req.RoundID, Round: n, Model: req.Model, Provider: route.Provider, Family: route.Family, Slash: sceneFor(req.Slash).Name}, last: time.Now(), emit: s.trace}
	return context.WithValue(ctx, traceContextKey{}, t)
}

func traceStage(ctx context.Context, stage string, decorate func(*TraceRecord)) {
	t, _ := ctx.Value(traceContextKey{}).(*roundTrace)
	if t == nil {
		return
	}
	now := time.Now()
	r := t.base
	r.Stage = stage
	r.Ts = now.UnixMilli()
	r.DurationMs = now.Sub(t.last).Milliseconds()
	t.last = now
	if decorate != nil {
		decorate(&r)
	}
	if stage == "sse_done" || stage == "error" {
		decorateContextMetrics(ctx, &r)
	}
	if t.emit != nil {
		t.emit(r)
	}
}
