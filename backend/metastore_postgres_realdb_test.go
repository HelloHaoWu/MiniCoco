//go:build realdb

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPostgresParallelStartup(t *testing.T) {
	uri := postgresTestURL(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s, err := NewPostgresMetaStore(ctx, uri)
			if err != nil {
				t.Error(err)
				return
			}
			s.Close()
		}()
	}
	wg.Wait()
}

func TestPostgresLedgerConcurrencyAndRollback(t *testing.T) {
	uri := postgresTestURL(t)
	ctx := context.Background()
	one, err := NewPostgresMetaStore(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := NewPostgresMetaStore(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	sid, err := one.CreateSession(ctx, Session{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := one
			if i%2 == 0 {
				store = two
			}
			for j := 0; j < 2; j++ {
				if err := store.CommitRoundUsage(ctx, sid, fmt.Sprintf("round-%d", i), Usage{1, 2, 3}, i+1); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	// Internal summary ids contain NUL; bytes must survive reopen and deduplication.
	internal := "\x00summary:fixture"
	if err := one.CommitRoundUsage(ctx, sid, internal, Usage{2, 3, 5}, 32); err != nil {
		t.Fatal(err)
	}
	if err := two.CommitRoundUsage(ctx, sid, internal, Usage{2, 3, 5}, 32); err != nil {
		t.Fatal(err)
	}
	got, err := two.GetSession(ctx, sid)
	if err != nil || got.TotalTokens != 101 || got.MessageCount != 32 {
		t.Fatalf("lost/duplicated increments: %+v %v", got, err)
	}
	if err := two.CommitRoundUsage(ctx, sid, internal, Usage{1, 1, 2}, 32); !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("conflict: %v", err)
	}
	// A failed counter update must roll back the new ledger key as well.
	if err := one.CommitRoundUsage(ctx, sid, "overflow", Usage{Prompt: math.MaxInt64}, 32); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("overflow: %v", err)
	}
	if err := two.CommitRoundUsage(ctx, sid, "overflow", Usage{1, 1, 2}, 32); err != nil {
		t.Fatalf("rolled-back ledger poisoned retry: %v", err)
	}
	// A busy session row must not serialize unrelated sessions across the database.
	other, err := one.CreateSession(ctx, Session{})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := one.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT id FROM session WHERE id=$1 FOR UPDATE", sid); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := two.BumpUsage(bounded, other, Usage{1, 1, 2}); err != nil {
		t.Fatalf("unrelated row blocked: %v", err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if err := two.CommitRoundUsage(cancelled, sid, "cancelled", Usage{}, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestPostgresProcessLedgerChild(t *testing.T) {
	if os.Getenv("MINICOCO_PG_LEDGER_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := NewPostgresMetaStore(ctx, os.Getenv("MINICOCO_PG_CHILD_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 10; i++ {
		if err := s.CommitRoundUsage(ctx, "s_process", fmt.Sprintf("r%d", i), Usage{1, 2, 3}, i+1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresCrossProcessLedger(t *testing.T) {
	uri := postgresTestURL(t)
	s, err := NewPostgresMetaStore(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(context.Background(), Session{ID: "s_process"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPostgresProcessLedgerChild$")
			cmd.Env = append(os.Environ(), "MINICOCO_PG_LEDGER_CHILD=1", "MINICOCO_PG_CHILD_URL="+uri)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child failed: %v %s", err, output)
			}
		}()
	}
	wg.Wait()
	s, err = NewPostgresMetaStore(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetSession(context.Background(), "s_process")
	if err != nil || got.TotalTokens != 30 || got.MessageCount != 10 {
		t.Fatalf("cross-process/reopen: %+v %v", got, err)
	}
}

func TestPostgresConfigErrorsRedactCredentials(t *testing.T) {
	for _, uri := range []string{"", "postgres://user:secret-password@%bad/minicoco"} {
		_, err := NewPostgresMetaStore(context.Background(), uri)
		if err == nil || strings.Contains(err.Error(), "secret-password") {
			t.Fatalf("unsafe configuration error: %v", err)
		}
	}
}
