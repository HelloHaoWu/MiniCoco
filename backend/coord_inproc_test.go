package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInprocCoordinatorLockOwnershipAndExpiry(t *testing.T) {
	ctx := context.Background()
	coord := NewInprocCoordinator(nil)
	now := time.Now()
	coord.now = func() time.Time { return now }
	unlock, ok, err := coord.Lock(ctx, "summary:s1", time.Hour)
	if err != nil || !ok {
		t.Fatalf("first lock: %v, %v", ok, err)
	}
	if _, ok, err := coord.Lock(ctx, "summary:s1", time.Hour); err != nil || ok {
		t.Fatalf("second lock: %v, %v", ok, err)
	}
	other, ok, err := coord.Lock(ctx, "summary:s2", time.Hour)
	if err != nil || !ok {
		t.Fatal("independent key was locked out")
	}
	other()
	now = now.Add(time.Hour)
	releaseNew, ok, err := coord.Lock(ctx, "summary:s1", time.Hour)
	if err != nil || !ok {
		t.Fatalf("expired lease not available: %v, %v", ok, err)
	}
	unlock()
	unlock() // An expired/double unlock cannot release someone else's new lease.
	if _, ok, err := coord.Lock(ctx, "summary:s1", time.Hour); err != nil || ok {
		t.Fatal("stale unlock released the new owner")
	}
	releaseNew()
	release, ok, err := coord.Lock(ctx, "summary:s1", time.Hour)
	if err != nil || !ok {
		t.Fatal("explicit unlock did not release lease")
	}
	release()
}

func TestInprocCoordinatorLockTTLRemovesIdleKey(t *testing.T) {
	coord := NewInprocCoordinator(nil)
	_, ok, err := coord.Lock(context.Background(), "idle", 5*time.Millisecond)
	if err != nil || !ok {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		coord.mu.Lock()
		remaining := len(coord.locks)
		coord.mu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL did not clean idle lock entry")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInprocCoordinatorConcurrentLockAndRateLimit(t *testing.T) {
	ctx := context.Background()
	coord := NewInprocCoordinator(nil)
	var workers sync.WaitGroup
	var locks, allowed atomic.Int32
	releases := make(chan func(), 32)
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			unlock, ok, err := coord.Lock(ctx, "one", time.Hour)
			if err != nil {
				t.Error(err)
			}
			if ok {
				locks.Add(1)
				releases <- unlock
			}
			if coord.Allow(ctx, "limited", 7, time.Hour) {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	workers.Wait()
	close(releases)
	for release := range releases {
		release()
	}
	if locks.Load() != 1 || allowed.Load() != 7 {
		t.Fatalf("concurrent lock/rate counts: %d, %d", locks.Load(), allowed.Load())
	}
}

func TestInprocCoordinatorRateFlagsAndCancellation(t *testing.T) {
	ctx := context.Background()
	flags := map[string]string{"feature": "explicit"}
	coord := NewInprocCoordinator(flags)
	flags["feature"] = "changed outside"
	t.Setenv("feature", "environment")
	t.Setenv("flag:compress_enabled", "1")
	if value, ok := coord.Flag(ctx, "feature"); !ok || value != "explicit" {
		t.Fatalf("flag ownership/precedence: %q, %v", value, ok)
	}
	if value, ok := coord.Flag(ctx, "flag:compress_enabled"); !ok || value != "1" {
		t.Fatalf("env flag: %q, %v", value, ok)
	}
	if _, ok := coord.Flag(ctx, "MINICOCO_UNDEFINED_TEST_FLAG"); ok {
		t.Fatal("missing flag reported present")
	}
	now := time.Now()
	coord.now = func() time.Time { return now }
	if !coord.Allow(ctx, "rate", 2, time.Minute) || !coord.Allow(ctx, "rate", 2, time.Minute) || coord.Allow(ctx, "rate", 2, time.Minute) {
		t.Fatal("fixed-window rate bound failed")
	}
	if !coord.Allow(ctx, "independent", 1, time.Minute) {
		t.Fatal("rate keys not independent")
	}
	now = now.Add(time.Minute)
	if !coord.Allow(ctx, "rate", 2, time.Minute) || len(coord.windows) != 1 {
		t.Fatal("expired window did not reset/clean entries")
	}
	if coord.Allow(ctx, "rate", 0, time.Minute) || coord.Allow(ctx, "rate", 1, 0) || coord.Allow(ctx, "", 1, time.Minute) {
		t.Fatal("invalid rate policy accepted")
	}
	if err := coord.Publish(ctx, "local", []byte("chunk")); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok, err := coord.Lock(cancelled, "cancel", time.Hour); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lock: %v, %v", ok, err)
	}
	if coord.Allow(cancelled, "rate", 100, time.Minute) {
		t.Fatal("cancelled rate request allowed")
	}
	if _, ok := coord.Flag(cancelled, "feature"); ok {
		t.Fatal("cancelled flag returned a value")
	}
	if err := coord.Publish(cancelled, "local", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, key := range []string{"", "bad-ttl"} {
		if _, ok, err := coord.Lock(ctx, key, 0); ok || !errors.Is(err, ErrStoreInvalid) {
			t.Fatalf("invalid lock: %v, %v", ok, err)
		}
	}
}
