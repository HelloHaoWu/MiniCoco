package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMemoryPromptLiteralIsolationBudgetAndReplay(t *testing.T) {
	sessionFixture(t)
	index := "- [Indent](pref.md) — 4 spaces\n"
	route := ModelRoute{Family: "openai", MaxTokens: 32768, memoryIndex: index}
	history := []Message{{"role": "user", "content": "inspect"}}
	messages, tools, err := genPrompt([]map[string]any{map[string]any(history[0])}, "/tmp/work", ModeFull, route)
	if err != nil {
		t.Fatal(err)
	}
	literal, _ := promptsFS.ReadFile("prompts/memory.txt")
	if !strings.Contains(fmt.Sprint(messages[0]["content"]), string(literal)) || len(messages) != 4 || messages[1]["role"] != "system" || messages[1]["content"] != memoryReminder(index) || messages[2]["role"] != "user" || messages[3]["content"] != "inspect" {
		t.Fatal(messages)
	}
	baseline, _ := budgetFor(history, "/tmp/work", ModeFull, route)
	route.memoryIndex = strings.Repeat("new memory\n", 100)
	changed, changedTools, _ := genPrompt([]map[string]any{map[string]any(history[0])}, "/tmp/work", ModeFull, route)
	next, _ := budgetFor(history, "/tmp/work", ModeFull, route)
	if !reflect.DeepEqual(messages[0], changed[0]) || !reflect.DeepEqual(tools, changedTools) || next.Base <= baseline.Base {
		t.Fatal("memory not independent or unbudgeted")
	}
	req := map[string]any{"roundId": "memory-one", "query": "inspect", "model": "mock", "memoryIndex": index, "stream": false}
	response, events := deltaResponse(t, req)
	requireRoundFinish(t, response, events, FinishToolCalls)
	response, events = deltaResponse(t, req)
	requireRoundFinish(t, response, events, FinishToolCalls)
	req["memoryIndex"] = "changed"
	_, events = deltaResponse(t, req)
	if events[0].Code != codeBadRequest {
		t.Fatal(events)
	}
	if validateMemoryIndex(strings.Repeat("x", 151)) == nil || validateMemoryIndex(strings.Repeat("x\n", 9000)) == nil {
		t.Fatal("unbounded index")
	}
	if !strings.Contains(memoryReminder(""), "(empty)") {
		t.Fatal("empty reminder")
	}
}
func TestMemoryAuditWithoutModelSession(t *testing.T) {
	service := sessionFixture(t)
	dir := t.TempDir()
	service.sink = newJSONLSink(dir, 16)
	record := AuditRecord{Ts: 1, WorkspaceDigest: "abcdef012345", Tool: "memory_write", ArgsDigest: "123456abcdef", Approved: true, Status: "ok"}
	raw, _ := json.Marshal(record)
	w := httptest.NewRecorder()
	handleAudit(w, httptest.NewRequest("POST", "/audit", bytes.NewReader(raw)))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	record.WorkspaceDigest = ""
	raw, _ = json.Marshal(record)
	w = httptest.NewRecorder()
	handleAudit(w, httptest.NewRequest("POST", "/audit", bytes.NewReader(raw)))
	if w.Code != 400 {
		t.Fatal("missing identity accepted")
	}
	if err := service.sink.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil || !bytes.Contains(saved, []byte("workspaceDigest")) || bytes.Contains(saved, []byte("body")) {
		t.Fatal(string(saved), err)
	}
}
