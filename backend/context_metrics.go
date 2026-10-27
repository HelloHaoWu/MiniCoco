package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Per-request only, created after the session lease is acquired. Never shared
// with speculative workers or retained across rounds. Writes invalidate it.
const maxReadSnapshotBytes = 16 << 20

type contextMetricsKey struct{}
type readSnapshot struct {
	value any
	size  int
}
type contextMetrics struct {
	mu        sync.Mutex
	cache     bool
	snapshots map[string]readSnapshot
	bytes     int
	reads     int64
	compact   time.Duration
	prefix    string
	previous  string
	stable    *bool
}

func withContextMetrics(ctx context.Context, cache bool) context.Context {
	if metricsFor(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, contextMetricsKey{}, &contextMetrics{cache: cache, snapshots: map[string]readSnapshot{}})
}
func metricsFor(ctx context.Context) *contextMetrics {
	m, _ := ctx.Value(contextMetricsKey{}).(*contextMetrics)
	return m
}
func cachedRead(ctx context.Context, key string) (any, bool) {
	m := metricsFor(ctx)
	if m == nil || !m.cache {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.snapshots[key]
	return v.value, ok
}
func saveRead(ctx context.Context, key string, value any, size int) {
	m := metricsFor(ctx)
	if m == nil || !m.cache {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.snapshots[key]; ok {
		m.bytes -= old.size
		delete(m.snapshots, key)
	}
	if size < 0 || size > maxReadSnapshotBytes-m.bytes {
		return
	}
	m.snapshots[key] = readSnapshot{value, size}
	m.bytes += size
}
func invalidateRead(ctx context.Context, key string) {
	m := metricsFor(ctx)
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.snapshots[key]; ok {
		m.bytes -= old.size
		delete(m.snapshots, key)
	}
}
func countObjectReads(ctx context.Context, n int) {
	if m := metricsFor(ctx); m != nil {
		m.mu.Lock()
		m.reads += int64(n)
		m.mu.Unlock()
	}
}
func compactTimer(ctx context.Context) func() {
	start := time.Now()
	return func() {
		if m := metricsFor(ctx); m != nil {
			m.mu.Lock()
			m.compact += time.Since(start)
			m.mu.Unlock()
		}
	}
}
func decorateContextMetrics(ctx context.Context, r *TraceRecord) {
	if m := metricsFor(ctx); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		ms, reads := float64(m.compact)/float64(time.Millisecond), m.reads
		r.CompactLatencyMs, r.ObjectReads, r.PromptCachePrefixStable = &ms, &reads, m.stable
	}
}
func promptPrefixFromContext(ctx context.Context) string {
	if m := metricsFor(ctx); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.prefix
	}
	return ""
}

func previousPromptPrefix(ctx context.Context) string {
	if m := metricsFor(ctx); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.previous
	}
	return ""
}

// Diagnostic hash of the actual fixed prefix through the summary anchor.
// Includes dynamic memory/environment/constraints that precede that anchor.
// It is not a provider cache-hit measurement. Adoption of a new summary changes it.
func recordPromptPrefix(ctx context.Context, messages []map[string]any, tools []json.RawMessage, current []Message, route ModelRoute, previous string) {
	m := metricsFor(ctx)
	if m == nil {
		return
	}
	n := len(messages) - len(current)
	if len(current) >= 2 && current[0]["role"] == "user" && current[0]["content"] == "Continue using the saved context." && current[1]["role"] == "assistant" {
		n += 2
	}
	effective, err := effectiveRoute(route)
	if err != nil {
		return
	}
	raw, _ := json.Marshal(struct {
		Messages                  []map[string]any
		Tools                     []json.RawMessage
		Model, Provider, Endpoint string
	}{messages[:n], tools, route.Name, effective.Provider, effective.BaseURL})
	hash := historyDigest([]Message{{"prefix": string(raw)}})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prefix = hash
	if previous != "" {
		stable := hash == previous
		m.stable = &stable
	}
}
