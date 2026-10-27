package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func runReadSnapshotContract(t *testing.T, factory contractFactory) {
	stores := factory(t)()
	defer stores.close()
	ctx := withContextMetrics(context.Background(), true)
	const sid = "s_snapshot"
	raw := oldContext()
	if err := stores.transcript.Append(ctx, sid, "seed", raw); err != nil {
		t.Fatal(err)
	}
	first, err := stores.transcript.RawLatest(ctx, sid, allHistory)
	if err != nil {
		t.Fatal(err)
	}
	reads := metricsFor(ctx).reads
	first[0]["content"] = "caller mutation"
	again, err := stores.transcript.RawLatest(ctx, sid, allHistory)
	if err != nil || !reflect.DeepEqual(again, raw) || metricsFor(ctx).reads != reads {
		t.Fatal("snapshot changed or reread", err)
	}
	if _, err := stores.transcript.ReserveRound(ctx, sid, "r1", strings.Repeat("a", 64), []Message{{"role": "user", "content": "next"}}, len(raw)); err != nil {
		t.Fatal(err)
	}
	pending, err := stores.transcript.RawLatest(ctx, sid, allHistory)
	if err != nil || len(pending) != len(raw) {
		t.Fatal("pending leaked", err)
	}
	metricsFor(ctx).prefix = strings.Repeat("b", 64)
	if err := stores.transcript.CommitRound(ctx, sid, "r1", Message{"role": "assistant", "content": "done"}, Usage{}, []Event{{Type: EvDone, FinishReason: FinishStop}}); err != nil {
		t.Fatal(err)
	}
	after, err := stores.transcript.RawLatest(ctx, sid, allHistory)
	if err != nil || len(after) != len(raw)+2 {
		t.Fatal("write did not invalidate", err)
	}
	round, err := stores.transcript.Round(ctx, sid, "r1")
	if err != nil || round.PromptPrefixHash != strings.Repeat("b", 64) {
		t.Fatal("prefix not durable", err, round)
	}
	summary := Summary{OriginStartID: "message:1", OriginEndID: "message:4", PrefixCount: 4, PrefixHash: historyDigest(raw), Content: "saved"}
	summary.UsageID = "\x00summary:" + historyDigest([]Message{{"hash": summary.PrefixHash, "content": summary.Content}})
	if err := stores.transcript.SaveSummary(ctx, sid, summary); err != nil {
		t.Fatal(err)
	}
	view, err := stores.transcript.Latest(ctx, sid, allHistory)
	if err != nil || len(view) != 4 || view[1]["content"] != "[Context summary]\nsaved" {
		t.Fatal("summary write hidden by snapshot", err)
	}
	other := withContextMetrics(context.Background(), true)
	if _, err := stores.transcript.RawLatest(other, sid, allHistory); err != nil || metricsFor(other).reads == 0 {
		t.Fatal("cache leaked across requests", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := stores.transcript.RawLatest(cancelled, sid, allHistory); err == nil {
		t.Fatal("cached read ignored cancellation")
	}
}
func TestRequestReadSnapshot(t *testing.T) { runReadSnapshotContract(t, fileContractFactory) }

func TestReadSnapshotBoundAndRollback(t *testing.T) {
	ctx := withContextMetrics(context.Background(), true)
	saveRead(ctx, "large", "x", maxReadSnapshotBytes+1)
	if _, ok := cachedRead(ctx, "large"); ok {
		t.Fatal("unbounded cache")
	}
	saveRead(ctx, "a", "first", maxReadSnapshotBytes)
	saveRead(ctx, "b", "second", 1)
	if _, ok := cachedRead(ctx, "b"); ok {
		t.Fatal("aggregate cap ignored")
	}
	invalidateRead(ctx, "a")
	saveRead(ctx, "b", "second", 1)
	if _, ok := cachedRead(ctx, "b"); !ok {
		t.Fatal("capacity not released")
	}
	off := withContextMetrics(context.Background(), false)
	saveRead(off, "a", "x", 1)
	if _, ok := cachedRead(off, "a"); ok {
		t.Fatal("off switch cached")
	}
}

func TestProactivePinnedConstraints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "constraints.json")
	t.Setenv("MINICOCO_CONSTRAINTS_FILE", path)
	pin := "Do not modify deployment files; never expose API keys."
	data, _ := json.Marshal(map[string]string{"/tmp": pin})
	os.WriteFile(path, data, 0600)
	entries, err := loadWorkingConstraints()
	if err != nil || entries["/tmp"] != pin || entries["/elsewhere"] != "" {
		t.Fatal(entries, err)
	}
	route := ModelRoute{Name: "mock", Provider: "mock", Family: "openai", MaxTokens: 100000, workingConstraints: pin}
	permissions, _ := promptsFS.ReadFile("prompts/permissions.txt")
	current := append(oldContext(), Message{"role": "user", "content": "next"})
	end, err := chooseRange(current, len(current)-1, compactFull)
	if err != nil {
		t.Fatal(err)
	}
	compressed := append(summaryMessages("summary intentionally omitted every constraint"), current[end:]...)
	trimmed, err := trimToBudget(current, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range [][]Message{compressed, trimmed} {
		messages := []map[string]any{}
		for _, m := range view {
			messages = append(messages, m)
		}
		prompt, _, err := genPrompt(messages, "/tmp", ModeFull, route)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(prompt[0]["content"].(string), string(permissions)) || prompt[2]["role"] != "system" || !strings.Contains(prompt[2]["content"].(string), pin) {
			t.Fatal("constraint lost", prompt)
		}
	}
	pinnedBudget, _ := budgetFor(current, "/tmp", ModeFull, route)
	route.workingConstraints = ""
	plainBudget, _ := budgetFor(current, "/tmp", ModeFull, route)
	if pinnedBudget.Base-plainBudget.Base != textTokens(constraintPrompt(pin)) {
		t.Fatal("pin missing from budget")
	}
	route.workingConstraints = pin
	route.MaxTokens = 1
	if _, err := budgetFor(current, "/tmp", ModeFull, route); err == nil {
		t.Fatal("oversized mandatory prefix silently dropped")
	}
	for _, bad := range []string{`{}`, `{"relative":"pin"}`, `{"/tmp/../tmp":"pin"}`, `{"/tmp":""}`, strings.Repeat("x", 65537)} {
		os.WriteFile(path, []byte(bad), 0600)
		if _, err := loadWorkingConstraints(); err == nil {
			t.Fatal("bad config accepted")
		}
	}
}

func TestProactivePrefixInvalidation(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "mock")
	route := ModelRoute{Name: "mock", Provider: "mock", Family: "openai", workingConstraints: "no delete"}
	ctx := withContextMetrics(context.Background(), false)
	observe := func(view []Message, previous string) {
		messages := []map[string]any{}
		for _, m := range view {
			messages = append(messages, m)
		}
		prompt, tools, err := genPrompt(messages, "/tmp", ModeFull, route)
		if err != nil {
			t.Fatal(err)
		}
		recordPromptPrefix(ctx, prompt, tools, view, route, previous)
	}
	observe(oldContext(), "")
	first := metricsFor(ctx).prefix
	if metricsFor(ctx).stable != nil {
		t.Fatal("first observation must be unknown")
	}
	view := summaryMessages("first anchor")
	observe(view, first)
	if metricsFor(ctx).stable == nil || *metricsFor(ctx).stable {
		t.Fatal("adoption falsely stable")
	}
	previous := metricsFor(ctx).prefix
	view = append(view, Message{"role": "user", "content": "next"})
	observe(view, previous)
	if !*metricsFor(ctx).stable {
		t.Fatal("append invalidated anchor")
	}
	view = summaryMessages("new anchor")
	observe(view, previous)
	if *metricsFor(ctx).stable {
		t.Fatal("replacement falsely stable")
	}
	previous = metricsFor(ctx).prefix
	route.workingConstraints = "different pin"
	observe(view, previous)
	if *metricsFor(ctx).stable {
		t.Fatal("constraint change falsely stable")
	}
}
