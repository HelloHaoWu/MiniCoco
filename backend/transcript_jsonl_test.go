package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func transcriptLines(t *testing.T, store *JSONLTranscriptStore, sid string) []Message {
	t.Helper()
	_, records, err := store.read(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestJSONLTranscriptRoundIdempotencyAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewJSONLTranscriptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{
		{"role": "user", "content": "读取文件\n然后回答"},
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"a.txt"}`}}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "file content"},
	}
	if err := store.Append(ctx, "s_test", "round-1", want[:2]); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, "s_test", "round-2", want[2:]); err != nil {
		t.Fatal(err)
	}
	// Mutation of the caller's input must not change the persisted record.
	input := Message{"role": "user", "content": "another session"}
	if err := store.Append(ctx, "s_other", "round-1", []Message{input}); err != nil {
		t.Fatal(err)
	}
	input["content"] = "changed"
	store, err = NewJSONLTranscriptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := store.path("s_test")
	before, _ := os.ReadFile(path)
	if err := store.Append(ctx, "s_test", "round-1", []Message{{"role": "user", "content": "retry must be ignored"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || len(transcriptLines(t, store, "s_test")) != 3 {
		t.Fatal("retry appended duplicate round after reopening")
	}
	got, err := store.Latest(ctx, "s_test", 100)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("chronological message round trip: %#v, %v", got, err)
	}
	tail, err := store.Latest(ctx, "s_test", 1)
	if err != nil || !reflect.DeepEqual(tail, want[2:]) {
		t.Fatalf("tail: %#v, %v", tail, err)
	}
	for _, sid := range []string{"s_test", "s_missing"} {
		empty, err := store.Latest(ctx, sid, 0)
		if err != nil || len(empty) != 0 {
			t.Fatalf("empty latest: %#v, %v", empty, err)
		}
	}
	other, _ := store.Latest(ctx, "s_other", 10)
	if len(other) != 1 || other[0]["content"] != "another session" {
		t.Fatal("session isolation/input ownership failed")
	}
	got[0]["content"] = "changed output"
	untouched, _ := store.Latest(ctx, "s_test", 100)
	if !reflect.DeepEqual(untouched, want) {
		t.Fatal("returned messages leaked mutable store state")
	}
}

func TestJSONLTranscriptPendingFinalAndSummary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewJSONLTranscriptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, "s_reply", "r1", []Message{{"role": "user", "content": "question"}}); err != nil {
		t.Fatal(err)
	}
	id, err := store.PreoccupyReply(ctx, "s_reply")
	if err != nil {
		t.Fatal(err)
	}
	pending := transcriptLines(t, store, "s_reply")
	if len(pending) != 2 || pending[1]["status"] != "pending" || pending[1]["msg_id"] != id {
		t.Fatalf("missing pending physical record: %#v", pending)
	}
	if latest, err := store.Latest(ctx, "s_reply", 10); err != nil || len(latest) != 1 {
		t.Fatalf("pending leaked into model history: %#v, %v", latest, err)
	}
	// Route a reply by message ID alone, using a fresh store (no memory index).
	store, err = NewJSONLTranscriptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reply := Message{"role": "assistant", "content": "answer"}
	usage := Usage{Prompt: 3, Completion: 2, Total: 5}
	if err := store.UpdateReply(ctx, id, reply, usage); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateReply(ctx, id, reply, usage); err != nil {
		t.Fatalf("identical final commit must be idempotent: %v", err)
	}
	if err := store.UpdateReply(ctx, id, Message{"role": "assistant", "content": "different"}, usage); !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("conflicting final overwrite: %v", err)
	}
	physical := transcriptLines(t, store, "s_reply")
	if len(physical) != 3 || physical[2]["status"] != "final" || physical[2]["msg_id"] != id || physical[2]["usage"].(map[string]any)["total"] != json.Number("5") {
		t.Fatalf("expected user, pending, final with usage: %#v", physical)
	}
	latest, err := store.Latest(ctx, "s_reply", 10)
	if err != nil || len(latest) != 2 || !reflect.DeepEqual(latest[1], reply) {
		t.Fatalf("pending/final logical deduplication: %#v, %v", latest, err)
	}
	largeID, err := store.PreoccupyReply(ctx, "s_large")
	if err != nil {
		t.Fatal(err)
	}
	largeUsage := Usage{Prompt: 1<<53 + 1, Completion: 1, Total: 1<<53 + 2}
	if err := store.UpdateReply(ctx, largeID, reply, largeUsage); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateReply(ctx, largeID, reply, largeUsage); err != nil {
		t.Fatalf("large counters lost precision: %v", err)
	}
	largeUsage.Prompt++
	if err := store.UpdateReply(ctx, largeID, reply, largeUsage); !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("large-counter conflict was rounded away: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := store.SaveSummary(ctx, "s_reply", Summary{OriginStartID: "first", OriginEndID: id, Content: "摘要\n包含换行"}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "transcripts", "s_reply.summary.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("summary records: %s", data)
	}
	var summary Summary
	if err := json.Unmarshal(lines[0], &summary); err != nil || summary.Content != "摘要\n包含换行" || summary.OriginEndID != id {
		t.Fatalf("summary round trip: %#v, %v", summary, err)
	}
}

func TestJSONLTranscriptConcurrentRoundAppend(t *testing.T) {
	store, err := NewJSONLTranscriptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := store.Append(context.Background(), "s_concurrent", "same-round", []Message{{"role": "user", "content": "first"}, {"role": "assistant", "content": "second"}})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	if got := transcriptLines(t, store, "s_concurrent"); len(got) != 2 {
		t.Fatalf("concurrent retry wrote %d records", len(got))
	}
}

func TestJSONLTranscriptRejectsUnsafeAndIncompleteData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewJSONLTranscriptStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"", "../outside", "a/b", "a\\b", ".", "s.1"} {
		if err := store.Append(ctx, sid, "round", []Message{{"role": "user", "content": "x"}}); !errors.Is(err, ErrStoreInvalid) {
			t.Fatalf("unsafe session %q: %v", sid, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "outside.jsonl")); !os.IsNotExist(err) {
		t.Fatal("path traversal wrote outside transcript directory")
	}
	if err := store.Append(ctx, "s_bad", "round", []Message{{"role": "user", "content": "valid"}, {"role": "system", "content": "invalid"}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if got := transcriptLines(t, store, "s_bad"); len(got) != 0 {
		t.Fatal("failed batch committed its first message")
	}
	for _, message := range []Message{nil, {"role": "user", "status": "pending"}, {"role": "user", "content": make(chan int)}, {"role": "user", "extra": make(chan int)}} {
		if err := store.Append(ctx, "s_bad", "round", []Message{message}); err == nil {
			t.Fatalf("invalid message accepted: %#v", message)
		}
	}
	if err := store.Append(ctx, "s_bad", "", []Message{{"role": "user"}}); err == nil {
		t.Fatal("empty round accepted")
	}
	if err := store.Append(ctx, "s_bad", "round", nil); err == nil {
		t.Fatal("empty batch accepted")
	}
	if _, err := store.Latest(ctx, "s_bad", -1); !errors.Is(err, ErrStoreInvalid) {
		t.Fatal(err)
	}
	if err := store.UpdateReply(ctx, "s_bad.m_missing", Message{"role": "assistant"}, Usage{}); !errors.Is(err, ErrStoreNotFound) {
		t.Fatal(err)
	}
	if err := store.UpdateReply(ctx, "../outside", Message{"role": "assistant"}, Usage{}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatal(err)
	}
	if err := store.SaveSummary(ctx, "s_bad", Summary{}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatal(err)
	}
	path, _ := store.path("s_corrupt")
	for _, bad := range []string{"{\n", `{"role":"user"}` + "\n", `{"msg_id":"s_other.m_a","role":"user","status":"final"}` + "\n", `{"msg_id":"s_corrupt.m_a","role":"user","status":"final"}`, `{"msg_id":"s_corrupt.m_a","role":"user","status":"final"} {}` + "\n"} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := store.Append(ctx, "s_corrupt", "round", []Message{{"role": "user", "content": "x"}}); err == nil {
			t.Fatalf("corrupt log overwritten: %s", bad)
		}
		if data, _ := os.ReadFile(path); string(data) != bad {
			t.Fatal("corrupt transcript was changed")
		}
	}
	if _, err := NewJSONLTranscriptStore(""); !errors.Is(err, ErrStoreInvalid) {
		t.Fatal(err)
	}
}

func TestJSONLTranscriptCancellation(t *testing.T) {
	store, err := NewJSONLTranscriptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, operation := range []func() error{
		func() error { return store.Append(ctx, "s_cancel", "round", []Message{{"role": "user"}}) },
		func() error { _, err := store.PreoccupyReply(ctx, "s_cancel"); return err },
		func() error { return store.UpdateReply(ctx, "s_cancel.m_reply", Message{"role": "assistant"}, Usage{}) },
		func() error { _, err := store.Latest(ctx, "s_cancel", 10); return err },
		func() error {
			return store.SaveSummary(ctx, "s_cancel", Summary{OriginStartID: "a", OriginEndID: "b", Content: "text"})
		},
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled operation: %v", err)
		}
	}
	files, _ := os.ReadDir(store.dir)
	if len(files) != 0 {
		t.Fatal("cancelled transcript operations created files")
	}
}
