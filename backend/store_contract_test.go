package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type contractStores struct {
	meta       MetaStore
	transcript TranscriptStore
	coord      Coordinator
	close      func()
}
type contractFactory func(*testing.T) func() contractStores

func fileContractFactory(t *testing.T) func() contractStores {
	t.Helper()
	dir := t.TempDir()
	return func() contractStores {
		service, err := newSessionService(dir)
		if err != nil {
			t.Fatal(err)
		}
		return contractStores{service.meta, service.transcript, service.coord, func() {}}
	}
}
func TestFileStoreContract(t *testing.T) { runStoreContract(t, fileContractFactory) }

// Exactly this suite runs against the default file/inproc and real SQL/Mongo/
// Redis implementations. Persistence assertions reopen the original resources.
func runStoreContract(t *testing.T, factory contractFactory) {
	t.Run("metadata", func(t *testing.T) {
		open := factory(t)
		stores := open()
		defer func() { stores.close() }()
		ctx := context.Background()
		id, err := stores.meta.CreateSession(ctx, Session{ID: "s_contract", User: "alice", Model: "model", Workspace: "/tmp", CreatedAt: 10})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stores.meta.CreateSession(ctx, Session{ID: id}); !errors.Is(err, ErrStoreConflict) {
			t.Fatalf("duplicate: %v", err)
		}
		if _, err := stores.meta.CreateSession(ctx, Session{User: "bob"}); err != nil {
			t.Fatal(err)
		}
		if _, err := stores.meta.CreateSession(ctx, Session{}); err != nil {
			t.Fatal(err)
		}
		var workers sync.WaitGroup
		for i := 0; i < 12; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				if err := stores.meta.BumpUsage(ctx, id, Usage{1, 2, 3}); err != nil {
					t.Error(err)
				}
			}()
		}
		workers.Wait()
		for i := 0; i < 3; i++ {
			if err := stores.meta.CommitRoundUsage(ctx, id, "round-usage", Usage{2, 3, 5}, 7); err != nil {
				t.Fatal(err)
			}
		}
		if err := stores.meta.CommitRoundUsage(ctx, id, "round-usage", Usage{1, 3, 4}, 7); !errors.Is(err, ErrStoreConflict) {
			t.Fatalf("usage conflict: %v", err)
		}
		if err := stores.meta.BumpUsage(ctx, id, Usage{Prompt: math.MaxInt64}); !errors.Is(err, ErrStoreInvalid) {
			t.Fatalf("overflow: %v", err)
		}
		if err := stores.meta.SetLastMessageCount(ctx, id, 4); err != nil {
			t.Fatal(err)
		}
		if err := stores.meta.SetLike(ctx, id+".m_reply", -1); err != nil {
			t.Fatal(err)
		}
		if err := stores.meta.SetLike(ctx, "bad", 2); !errors.Is(err, ErrStoreInvalid) {
			t.Fatal(err)
		}
		if err := stores.meta.CommitRoundUsage(ctx, "missing", "r", Usage{}, 0); !errors.Is(err, ErrStoreNotFound) {
			t.Fatalf("unknown usage session: %v", err)
		}
		stores.close()
		stores = open()
		got, err := stores.meta.GetSession(ctx, id)
		if err != nil || got.PromptTokens != 14 || got.CompletionTokens != 27 || got.TotalTokens != 41 || got.MessageCount != 7 || got.LastMessageCount != 4 {
			t.Fatalf("persistent metadata: %#v, %v", got, err)
		}
		if err := stores.meta.CommitRoundUsage(ctx, id, "round-usage", Usage{2, 3, 5}, 7); err != nil {
			t.Fatal(err)
		}
		after, _ := stores.meta.GetSession(ctx, id)
		if after.TotalTokens != 41 {
			t.Fatal("usage ledger did not survive reopen")
		}
		listed, err := stores.meta.ListSessions(ctx, "alice")
		if err != nil || len(listed) != 1 || listed[0].ID != id {
			t.Fatalf("user isolation: %#v, %v", listed, err)
		}
		anonymous, err := stores.meta.ListSessions(ctx, "")
		if err != nil || len(anonymous) != 1 || anonymous[0].User != "" {
			t.Fatal("empty user was treated as all users")
		}
	})
	t.Run("transcript", func(t *testing.T) {
		open := factory(t)
		stores := open()
		defer func() { stores.close() }()
		ctx := context.Background()
		input := []Message{{"role": "user", "content": "first\n中文", "extra": json.Number("9007199254740993")}, {"role": "assistant", "content": "second"}}
		if err := stores.transcript.Append(ctx, "s_transcript", "r1", input); err != nil {
			t.Fatal(err)
		}
		if err := stores.transcript.Append(ctx, "s_transcript", "r1", input); err != nil {
			t.Fatal(err)
		}
		id, err := stores.transcript.PreoccupyReply(ctx, "s_transcript")
		if err != nil {
			t.Fatal(err)
		}
		if got, err := stores.transcript.Latest(ctx, "s_transcript", 99); err != nil || !reflect.DeepEqual(got, input) {
			t.Fatalf("order/pending: %#v, %v", got, err)
		}
		stores.close()
		stores = open()
		final := Message{"role": "assistant", "content": "final"}
		usage := Usage{1, 1, 2}
		if err := stores.transcript.UpdateReply(ctx, id, final, usage); err != nil {
			t.Fatal(err)
		}
		if err := stores.transcript.UpdateReply(ctx, id, final, usage); err != nil {
			t.Fatal(err)
		}
		if err := stores.transcript.UpdateReply(ctx, id, Message{"role": "assistant", "content": "changed"}, usage); !errors.Is(err, ErrStoreConflict) {
			t.Fatalf("final conflict: %v", err)
		}
		if err := stores.transcript.Append(ctx, "s_transcript", "r1", input); err != nil {
			t.Fatal(err)
		}
		got, err := stores.transcript.Latest(ctx, "s_transcript", 99)
		if err != nil || len(got) != 3 || !reflect.DeepEqual(got[:2], input) || !reflect.DeepEqual(got[2], final) {
			t.Fatalf("reopen/idempotency/final: %#v, %v", got, err)
		}
		if got, err := stores.transcript.Latest(ctx, "s_transcript", 1); err != nil || !reflect.DeepEqual(got, []Message{final}) {
			t.Fatal("tail order changed")
		}
		if got, err := stores.transcript.Latest(ctx, "s_transcript", 0); err != nil || len(got) != 0 {
			t.Fatal("n=0 changed")
		}
		if _, err := stores.transcript.Latest(ctx, "s_transcript", -1); !errors.Is(err, ErrStoreInvalid) {
			t.Fatal(err)
		}
		if err := stores.transcript.Append(ctx, "../outside", "r", input); !errors.Is(err, ErrStoreInvalid) {
			t.Fatal("unsafe SID accepted")
		}
		if err := stores.transcript.SaveSummary(ctx, "s_transcript", Summary{OriginStartID: "first", OriginEndID: id, Content: "summary\n中文"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rounds", func(t *testing.T) {
		open := factory(t)
		stores := open()
		defer func() { stores.close() }()
		ctx := context.Background()
		input := []Message{{"role": "user", "content": "ask"}}
		fingerprint := strings.Repeat("a", 64)
		first, err := stores.transcript.ReserveRound(ctx, "s_round", "r1", fingerprint, input, 0)
		if err != nil {
			t.Fatal(err)
		}
		second, err := stores.transcript.ReserveRound(ctx, "s_round", "r1", fingerprint, input, 0)
		if err != nil || second.ReplyID != first.ReplyID {
			t.Fatal("pending was not reused")
		}
		if _, err := stores.transcript.ReserveRound(ctx, "s_round", "r1", strings.Repeat("b", 64), input, 0); !errors.Is(err, ErrStoreConflict) {
			t.Fatal("changed round input accepted")
		}
		if got, err := stores.transcript.Latest(ctx, "s_round", 10); err != nil || len(got) != 0 {
			t.Fatal("uncommitted input leaked into history")
		}
		stores.close()
		stores = open()
		pending, err := stores.transcript.Round(ctx, "s_round", "r1")
		if err != nil || pending.ReplyID != first.ReplyID || pending.Final {
			t.Fatal("pending state lost after reopen")
		}
		events := []Event{{Type: EvText, Delta: "answer"}, {Type: EvDone, FinishReason: FinishStop}}
		if err := stores.transcript.CommitRound(ctx, "s_round", "r1", Message{"role": "assistant", "content": "answer"}, Usage{2, 3, 5}, events); err != nil {
			t.Fatal(err)
		}
		if err := stores.transcript.CommitRound(ctx, "s_round", "r1", Message{"role": "assistant", "content": "answer"}, Usage{2, 3, 5}, events); !errors.Is(err, ErrStoreConflict) {
			t.Fatal("round commit overwrote a final")
		}
		rounds, err := stores.transcript.CommittedRounds(ctx, "s_round")
		if err != nil || len(rounds) != 1 || !rounds[0].Final || !reflect.DeepEqual(rounds[0].Events, events) || rounds[0].Usage.Total != 5 {
			t.Fatalf("committed replay: %#v %v", rounds, err)
		}
		got, err := stores.transcript.Latest(ctx, "s_round", 10)
		if err != nil || len(got) != 2 || got[0]["content"] != "ask" || got[1]["content"] != "answer" {
			t.Fatalf("atomic round history: %#v %v", got, err)
		}
	})
	t.Run("coordination", func(t *testing.T) {
		stores := factory(t)()
		defer stores.close()
		ctx := context.Background()
		old, ok, err := stores.coord.Lock(ctx, "lease", 40*time.Millisecond)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if _, ok, err := stores.coord.Lock(ctx, "lease", time.Second); err != nil || ok {
			t.Fatal("lock was not exclusive")
		}
		var next func()
		deadline := time.Now().Add(2 * time.Second)
		for {
			release, ok, err := stores.coord.Lock(ctx, "lease", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				next = release
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("TTL did not expire")
			}
			time.Sleep(5 * time.Millisecond)
		}
		old()
		old()
		if _, ok, err := stores.coord.Lock(ctx, "lease", time.Second); err != nil || ok {
			t.Fatal("old owner unlocked new lease")
		}
		next()
		if !stores.coord.Allow(ctx, "window", 2, 40*time.Millisecond) || !stores.coord.Allow(ctx, "window", 2, 40*time.Millisecond) || stores.coord.Allow(ctx, "window", 2, 40*time.Millisecond) {
			t.Fatal("rate limit diverged")
		}
		time.Sleep(60 * time.Millisecond)
		if !stores.coord.Allow(ctx, "window", 2, 40*time.Millisecond) {
			t.Fatal("rate window did not expire")
		}
		if stores.coord.Allow(ctx, "window", 0, time.Second) {
			t.Fatal("invalid rate allowed")
		}
		if _, ok := stores.coord.Flag(ctx, "missing-contract-flag"); ok {
			t.Fatal("missing flag returned present")
		}
		if err := stores.coord.Publish(ctx, "target", []byte(`{"type":"text","delta":"hello"}`)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		stores := factory(t)()
		defer stores.close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for _, operation := range []func() error{
			func() error { _, err := stores.meta.CreateSession(ctx, Session{}); return err },
			func() error {
				return stores.transcript.Append(ctx, "s_cancel", "r", []Message{{"role": "user", "content": "x"}})
			},
			func() error {
				_, err := stores.transcript.ReserveRound(ctx, "s_cancel", "r", strings.Repeat("a", 64), []Message{{"role": "user"}}, 0)
				return err
			},
			func() error { _, _, err := stores.coord.Lock(ctx, "cancel", time.Second); return err },
			func() error { return stores.coord.Publish(ctx, "target", []byte(`{"type":"text","delta":"hello"}`)) },
		} {
			if err := operation(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled operation: %v", err)
			}
		}
	})
}
