package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAuditUsesSharedSinkAndRejectsPayloads(t *testing.T) {
	s := sessionFixture(t)
	dir := t.TempDir()
	s.sink = newJSONLSink(dir, 1024)
	if _, err := s.meta.CreateSession(context.Background(), Session{ID: "s_audit"}); err != nil {
		t.Fatal(err)
	}
	valid := map[string]any{"ts": 1, "sessionId": "s_audit", "traceId": "s_audit-r1", "tool": "write_file", "argsDigest": "abcdef012345", "approved": false, "status": "denied", "ms": 2}
	send := func(body map[string]any) int {
		data, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		handleAudit(w, httptest.NewRequest("POST", "/audit", bytes.NewReader(data)))
		return w.Code
	}
	if status := send(valid); status != 202 {
		t.Fatal(status)
	}
	valid["arguments"] = "secret"
	if send(valid) != 400 {
		t.Fatal("accepted arguments")
	}
	delete(valid, "arguments")
	valid["sessionId"] = "../escape"
	if send(valid) != 400 {
		t.Fatal("accepted invalid session")
	}
	if err := s.sink.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record AuditRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Status != "denied" || record.Approved || bytes.Contains(data, []byte("secret")) {
		t.Fatal(string(data))
	}
}
