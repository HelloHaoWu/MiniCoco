package main

import (
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
