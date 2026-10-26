package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func validateBackgroundResults(req ChatRequest) error {
	if len(req.BackgroundResults) > 4 {
		return ErrStoreInvalid
	}
	if len(req.BackgroundResults) > 0 && (req.SessionID == "" || req.AgentType != "") {
		return ErrStoreInvalid
	}
	seen := map[string]bool{}
	for _, result := range req.BackgroundResults {
		encoded, err := json.Marshal(result.Content)
		if err != nil || len(encoded) > maxToolOutputBytes || !validSessionID(result.TaskID) || seen[result.TaskID] {
			return ErrStoreInvalid
		}
		seen[result.TaskID] = true
	}
	return nil
}

// Canonical complete tool pair, built server-side from incremental task results.
// This acknowledgement is internal; it is never an executable model tool.
func backgroundMessages(req ChatRequest, formatter Formatter) ([]Message, error) {
	if len(req.BackgroundResults) == 0 {
		return nil, nil
	}
	messages := []Message{}
	ids := []string{}
	for _, result := range req.BackgroundResults {
		status := "ok"
		if result.IsError {
			status = "error"
		}
		messages = append(messages, Message{"role": "user", "content": fmt.Sprintf("[background %s] (%s)\n%s", result.TaskID, status, result.Content)})
		ids = append(ids, result.TaskID)
	}
	digest := sha256.Sum256([]byte(req.RoundID))
	id := "tc_" + hex.EncodeToString(digest[:12])
	args, _ := json.Marshal(map[string]any{"tasks": ids})
	messages = append(messages, Message{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "task_complete", "arguments": string(args)}}}})
	ack, err := formatter.Format([]ToolUseResult{{ID: id, Name: "task_complete", Output: "acknowledged"}})
	if err != nil {
		return nil, err
	}
	return append(messages, ack...), nil
}
