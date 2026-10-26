package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTraceRoundStagesReplayAndFailure(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	records := []TraceRecord{}
	service.trace = func(r TraceRecord) { records = append(records, r) }
	req := map[string]any{"roundId": "trace-first", "query": "inspect", "workspace": "/tmp", "model": "mock", "stream": false}
	response, events := deltaResponse(t, req)
	requireRoundFinish(t, response, events, FinishToolCalls)
	sid := response.Header().Get("X-Session-Id")
	if response.Header().Get("X-Trace-Id") != sid+"-r1" {
		t.Fatal(response.Header())
	}
	stages := []string{}
	for _, r := range records {
		stages = append(stages, r.Stage)
		if r.TraceID != sid+"-r1" || r.RoundID != "trace-first" || r.Provider != "mock" || r.Family != "openai" {
			t.Fatalf("bad record %+v", r)
		}
	}
	want := []string{"request_received", "prompt_built", "llm_request", "llm_response"}
	for _, e := range events {
		if e.Type == EvToolCall {
			want = append(want, "tool_call")
		}
	}
	want = append(want, "sse_done")
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("%v != %v", stages, want)
	}
	records = nil
	response, _ = deltaResponse(t, req)
	if response.Header().Get("X-Trace-Id") != sid+"-r1" || len(records) != 3 || records[1].Stage != "replay" || !records[1].Replay {
		t.Fatal(records)
	}
	req["query"] = "changed"
	records = nil
	_, events = deltaResponse(t, req)
	last := records[len(records)-1]
	if last.Stage != "error" || last.Code != events[0].Code || last.Retryable == nil || *last.Retryable {
		t.Fatalf("%+v %+v", last, events)
	}
}

func TestTraceSinkThreeRoundsHealthAndRead(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	service.traceDir = t.TempDir()
	sink := newJSONLSink(service.traceDir, 1024)
	service.trace = func(r TraceRecord) { sink.emit("trace-"+r.SessionID+".jsonl", r) }
	first := map[string]any{"roundId": "trace-1", "query": "inspect", "workspace": "/tmp", "model": "mock", "stream": false}
	response, events := deltaResponse(t, first)
	requireRoundFinish(t, response, events, FinishToolCalls)
	sid := response.Header().Get("X-Session-Id")
	results := []ToolUseResult{}
	for _, event := range events {
		if event.Type == EvToolCall {
			results = append(results, ToolUseResult{ID: event.ID, Name: event.Name, Output: "ok"})
		}
	}
	response, events = deltaResponse(t, map[string]any{"sessionId": sid, "roundId": "trace-2", "toolUseResults": results, "workspace": "/tmp", "model": "mock"})
	requireRoundFinish(t, response, events, FinishStop)
	first["sessionId"], first["roundId"] = sid, "trace-3"
	response, events = deltaResponse(t, first)
	requireRoundFinish(t, response, events, FinishToolCalls)
	deltaResponse(t, first) // Replay must not produce another usage record.
	if err := sink.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := readTrace(service.traceDir, sid)
	if err != nil {
		t.Fatal(err)
	}
	responses := 0
	for _, row := range rows {
		if row.Stage == "llm_response" {
			responses++
			if row.Usage.Total != 2 {
				t.Fatal(row)
			}
		}
	}
	if responses != 3 {
		t.Fatal(rows)
	}
	health := httptest.NewRecorder()
	handleHealth(health, httptest.NewRequest("GET", "/health", nil))
	var body struct {
		Recent []struct {
			Rounds int `json:"rounds"`
			Total  int `json:"totalTokens"`
		} `json:"recentSessions"`
	}
	if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Recent) != 1 || body.Recent[0].Rounds != 3 || body.Recent[0].Total != 6 {
		t.Fatal(health.Body.String())
	}
	read := httptest.NewRecorder()
	handleTrace(read, httptest.NewRequest("GET", "/trace/"+sid+"?round=2", nil))
	var timeline struct {
		Records []TraceRecord `json:"records"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &timeline); err != nil {
		t.Fatal(err)
	}
	if len(timeline.Records) == 0 {
		t.Fatal(read.Body.String())
	}
	for _, row := range timeline.Records {
		if row.Round != 2 {
			t.Fatal(row)
		}
	}
}

func TestJSONLSinkDropsOldestAndIgnoresDiskFailure(t *testing.T) {
	sink := newJSONLSink(t.TempDir(), 2)
	entered, release := make(chan struct{}), make(chan struct{})
	var values []string
	sink.write = func(e sinkEntry) error {
		if len(values) == 0 {
			close(entered)
			<-release
		}
		values = append(values, string(e.data))
		return nil
	}
	sink.emit("audit.jsonl", 1)
	<-entered
	sink.emit("audit.jsonl", 2)
	sink.emit("audit.jsonl", 3)
	sink.emit("audit.jsonl", 4)
	close(release)
	if err := sink.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, []string{"1\n", "3\n", "4\n"}) || sink.dropped.Load() != 1 {
		t.Fatal(values, sink.dropped.Load())
	}
	// An invalid directory exercises actual OS errors, not a fake sink.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	broken := newJSONLSink(file, 1024)
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	service.trace = func(r TraceRecord) { broken.emit("trace-"+r.SessionID+".jsonl", r) }
	response, events := deltaResponse(t, map[string]any{"roundId": "disk-failure", "query": "inspect", "workspace": "/tmp"})
	requireRoundFinish(t, response, events, FinishToolCalls)
	if err := broken.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broken.failed.Load() == 0 {
		t.Fatal("expected disk failures")
	}
}
