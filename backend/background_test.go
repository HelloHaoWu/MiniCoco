package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func runBackgroundPersistence(t *testing.T, factory contractFactory) {
	open := factory(t)
	stores := open()
	defer func() { stores.close() }()
	previous := activeSessions
	defer func() { activeSessions = previous }()
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	for _, model := range []string{"mock", "mock-xml"} {
		response, events := deltaResponse(t, map[string]any{"roundId": "bg-first-" + model, "query": "inspect", "workspace": "/tmp", "model": model, "stream": false})
		requireRoundFinish(t, response, events, FinishToolCalls)
		sid := response.Header().Get("X-Session-Id")
		results := []ToolUseResult{}
		for _, event := range events {
			if event.Type == EvToolCall {
				results = append(results, ToolUseResult{ID: event.ID, Name: event.Name, Output: "started task"})
			}
		}
		req := map[string]any{"sessionId": sid, "roundId": "bg-result-" + model, "toolUseResults": results, "backgroundResults": []BackgroundResult{{TaskID: "task-1", Content: "exit_code: 0\ndone"}}, "workspace": "/tmp", "model": model, "stream": false}
		response, events = deltaResponse(t, req)
		requireRoundFinish(t, response, events, FinishStop)
		raw, err := stores.transcript.RawLatest(context.Background(), sid, allHistory)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(raw)
		if len(raw) != 7 || !strings.Contains(string(data), "task_complete") || !strings.Contains(string(data), "[background task-1]") {
			t.Fatal(string(data))
		}
		if _, err := historyBlocks(raw); err != nil {
			t.Fatal(err)
		}
		stores.close()
		stores = open()
		activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
		response, events = deltaResponse(t, req)
		requireRoundFinish(t, response, events, FinishStop)
		session, err := stores.meta.GetSession(context.Background(), sid)
		if err != nil || session.TotalTokens != 4 || session.MessageCount != 7 {
			t.Fatal(session, err)
		}
		req["backgroundResults"] = []BackgroundResult{{TaskID: "task-1", Content: "changed"}}
		_, events = deltaResponse(t, req)
		if len(events) < 1 || events[0].Code != codeBadRequest {
			t.Fatal(events)
		}
	}
}
func TestFileBackgroundPersistence(t *testing.T) { runBackgroundPersistence(t, fileContractFactory) }
func TestBackgroundDeltaValidation(t *testing.T) {
	query := "q"
	req := ChatRequest{Query: &query, RoundID: "r", BackgroundResults: []BackgroundResult{{TaskID: "task", Content: "done"}}}
	if validateDelta(req) == nil {
		t.Fatal("new session accepted background")
	}
	req.SessionID = "s_test"
	if err := validateDelta(req); err != nil {
		t.Fatal(err)
	}
	req.BackgroundResults = append(req.BackgroundResults, req.BackgroundResults[0])
	if validateDelta(req) == nil {
		t.Fatal("duplicate task")
	}
}
