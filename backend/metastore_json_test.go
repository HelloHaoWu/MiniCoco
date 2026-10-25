package main

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestJSONMetaStorePersistsAndFilters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewJSONMetaStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Session{ID: "s_first", User: "alice", Model: "test-model", Workspace: "/tmp/work", CreatedAt: 10, MessageCount: 5}
	if id, err := store.CreateSession(ctx, want); err != nil || id != want.ID {
		t.Fatalf("create: %s, %v", id, err)
	}
	if got, err := store.GetSession(ctx, want.ID); err != nil || got != want {
		t.Fatalf("round trip: %#v, %v", got, err)
	}
	for _, usage := range []Usage{{2, 3, 5}, {7, 11, 18}} {
		if err := store.BumpUsage(ctx, want.ID, usage); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetLastMessageCount(ctx, want.ID, 4); err != nil {
		t.Fatal(err)
	}
	if err := store.SetLike(ctx, "s_first.m_reply", -1); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateSession(ctx, Session{User: "alice", CreatedAt: 20})
	if err != nil || !validSessionID(second) {
		t.Fatalf("generated ID: %q, %v", second, err)
	}
	if _, err := store.CreateSession(ctx, Session{User: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSession(ctx, Session{}); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewJSONMetaStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	want.PromptTokens, want.CompletionTokens, want.TotalTokens, want.LastMessageCount = 9, 14, 23, 4
	if got, err := reopened.GetSession(ctx, want.ID); err != nil || got != want {
		t.Fatalf("persisted: %#v, %v", got, err)
	}
	list, err := reopened.ListSessions(ctx, "alice")
	if err != nil || len(list) != 2 || list[0].ID != second || list[1] != want {
		t.Fatalf("user filter/order: %#v, %v", list, err)
	}
	anonymous, err := reopened.ListSessions(ctx, "")
	if err != nil || len(anonymous) != 1 || anonymous[0].User != "" {
		t.Fatalf("empty user must not list other users: %#v, %v", anonymous, err)
	}
	if reopened.state.Likes["s_first.m_reply"] != -1 {
		t.Fatal("like did not survive restart")
	}
	list[1].Model = "modified copy"
	if got, _ := reopened.GetSession(ctx, want.ID); got.Model != want.Model {
		t.Fatal("ListSessions leaked mutable state")
	}
}

func TestJSONMetaStoreConcurrentUsageAndRejectedWrites(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONMetaStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateSession(ctx, Session{})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 24; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := store.BumpUsage(ctx, id, Usage{1, 2, 3}); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	before, _ := store.GetSession(ctx, id)
	if before.PromptTokens != 24 || before.CompletionTokens != 48 || before.TotalTokens != 72 {
		t.Fatalf("lost usage update: %#v", before)
	}
	if _, err := store.CreateSession(ctx, before); !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("duplicate ID: %v", err)
	}
	if err := store.BumpUsage(ctx, id, Usage{Prompt: -1}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("negative usage: %v", err)
	}
	if err := store.BumpUsage(ctx, id, Usage{Prompt: math.MaxInt64}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("overflow: %v", err)
	}
	if err := store.SetLastMessageCount(ctx, id, -1); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("negative checkpoint: %v", err)
	}
	if err := store.SetLike(ctx, "message", 2); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("invalid like: %v", err)
	}
	for _, operation := range []func() error{
		func() error { _, err := store.GetSession(ctx, "missing"); return err },
		func() error { return store.BumpUsage(ctx, "missing", Usage{}) },
		func() error { return store.SetLastMessageCount(ctx, "missing", 0) },
	} {
		if err := operation(); !errors.Is(err, ErrStoreNotFound) {
			t.Fatalf("missing session: %v", err)
		}
	}
	// Force rename failure without relying on permissions (which root may bypass).
	backup := store.path + ".backup"
	if err := os.Rename(store.path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.BumpUsage(ctx, id, Usage{1, 1, 2}); err == nil {
		t.Fatal("write unexpectedly succeeded against a directory")
	}
	if got, _ := store.GetSession(ctx, id); got != before {
		t.Fatal("failed write changed in-memory metadata")
	}
	if err := os.Remove(store.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, store.path); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewJSONMetaStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.GetSession(ctx, id); got != before {
		t.Fatal("failed write changed persisted metadata")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(store.path), ".minicoco-store-*"))
	if len(files) != 0 {
		t.Fatalf("temporary file leaked: %v", files)
	}
}

func TestJSONMetaStoreCancellationAndCorruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	store, err := NewJSONMetaStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	operations := []func() error{
		func() error { _, err := store.CreateSession(ctx, Session{}); return err },
		func() error { _, err := store.GetSession(ctx, "s_test"); return err },
		func() error { return store.BumpUsage(ctx, "s_test", Usage{}) },
		func() error { return store.SetLastMessageCount(ctx, "s_test", 0) },
		func() error { return store.SetLike(ctx, "message", 1) },
		func() error { _, err := store.ListSessions(ctx, "user"); return err },
	}
	for _, operation := range operations {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	}
	if _, err := os.Stat(store.path); !os.IsNotExist(err) {
		t.Fatal("cancelled write created metadata")
	}
	for _, bad := range []string{`{`, `null`, `{}`, `{"sessions":{"s_x":{"id":"s_other"}},"likes":{}}`} {
		if err := os.WriteFile(store.path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewJSONMetaStore(dir); err == nil {
			t.Fatalf("accepted corrupt metadata: %s", bad)
		}
		got, _ := os.ReadFile(store.path)
		if string(got) != bad {
			t.Fatal("constructor overwrote corrupt data")
		}
	}
	if _, err := NewJSONMetaStore(""); !errors.Is(err, ErrStoreInvalid) {
		t.Fatal(err)
	}
}
