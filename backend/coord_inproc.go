package main

import (
	"context"
	"os"
	"sync"
	"time"
)

type localLease struct {
	expires time.Time
	timer   *time.Timer
}

type rateWindow struct {
	until time.Time
	count int
	rate  int
	per   time.Duration
}

type InprocCoordinator struct {
	mu      sync.Mutex
	locks   map[string]*localLease
	windows map[string]rateWindow
	flags   map[string]string
	now     func() time.Time
}

func NewInprocCoordinator(flags map[string]string) *InprocCoordinator {
	copy := make(map[string]string, len(flags))
	for key, value := range flags {
		copy[key] = value
	}
	return &InprocCoordinator{locks: map[string]*localLease{}, windows: map[string]rateWindow{}, flags: copy, now: time.Now}
}

// Lock is a non-blocking lease: a holder may outlive its TTL. The caller must
// finish work within TTL; expiry does not cancel work or provide fencing.
func (coord *InprocCoordinator) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if key == "" || ttl <= 0 {
		return nil, false, ErrStoreInvalid
	}
	now := coord.now()
	if old := coord.locks[key]; old != nil {
		if now.Before(old.expires) {
			return nil, false, nil
		}
		old.timer.Stop()
	}
	lease := &localLease{expires: now.Add(ttl)}
	coord.locks[key] = lease
	release := func() {
		coord.mu.Lock()
		defer coord.mu.Unlock()
		if coord.locks[key] == lease {
			delete(coord.locks, key)
			lease.timer.Stop()
		}
	}
	lease.timer = time.AfterFunc(ttl, release)
	return release, true, nil
}

// Allow uses a fixed window, allowing at most rate calls per key per window.
// This deliberately has fixed-window boundary behavior, not token-bucket bursts.
func (coord *InprocCoordinator) Allow(ctx context.Context, key string, rate int, per time.Duration) bool {
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if ctx.Err() != nil || key == "" || rate <= 0 || per <= 0 {
		return false
	}
	now := coord.now()
	for key, window := range coord.windows {
		if !now.Before(window.until) {
			delete(coord.windows, key)
		}
	}
	window, exists := coord.windows[key]
	if !exists || window.rate != rate || window.per != per {
		window = rateWindow{until: now.Add(per), rate: rate, per: per}
	}
	if window.count >= rate {
		return false
	}
	window.count++
	coord.windows[key] = window
	return true
}

// Explicit flags take precedence over environment variables with the exact key.
func (coord *InprocCoordinator) Flag(ctx context.Context, key string) (string, bool) {
	if ctx.Err() != nil || key == "" {
		return "", false
	}
	if value, exists := coord.flags[key]; exists {
		return value, true
	}
	return os.LookupEnv(key)
}

func (coord *InprocCoordinator) Publish(ctx context.Context, emitterID string, chunk []byte) error {
	return ctx.Err() // Local callers emit directly; no cross-instance broadcast.
}
