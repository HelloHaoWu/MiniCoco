package main

import "encoding/json"

// Wire protocol shared with the TypeScript client (see client/src/protocol.ts).
// One Event per SSE frame, or the same sequence inside a JSON events array.
const (
	EvText      = "text"      // assistant text delta        {type, delta}
	EvReasoning = "reasoning" // thinking delta              {type, delta}
	EvToolCall  = "tool_call" // a fully-assembled tool call {type, id, name, arguments}
	EvDone      = "done"      // end of this round           {type, finish_reason}
	EvError     = "error"     // backend/LLM failure         {type, message, code, retryable}

	FinishToolCalls = "tool_calls" // model wants tools run -> client loops
	FinishStop      = "stop"       // model produced a final answer -> loop ends
)

// Event is one SSE chunk. Empty fields are omitted from the JSON.
type Event struct {
	Type         string          `json:"type"`
	Delta        string          `json:"delta,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Message      string          `json:"message,omitempty"`
	Code         string          `json:"code,omitempty"`
	// A pointer emits false explicitly on error events, but omits the field on
	// all other events, preserving their existing wire representation.
	Retryable *bool `json:"retryable,omitempty"`
}

// ChatRequest carries only the input for a single durable round. A round ID is
// stable across retries; a fresh client must use fresh IDs even when resuming.
type ChatRequest struct {
	MemoryIndex       string             `json:"memoryIndex,omitempty"`
	MCPTools          []MCPToolSpec      `json:"mcpTools,omitempty"`
	Slash             string             `json:"slash,omitempty"`
	BackgroundResults []BackgroundResult `json:"backgroundResults,omitempty"`
	ParentSessionID   string             `json:"parentSessionId,omitempty"`
	AgentType         string             `json:"agentType,omitempty"`
	SessionID         string             `json:"sessionId"`
	RoundID           string             `json:"roundId"`
	Query             *string            `json:"query,omitempty"`
	ToolUseResults    []ToolUseResult    `json:"toolUseResults,omitempty"`
	Workspace         string             `json:"workspace"`
	Model             string             `json:"model"`
	Mode              Mode               `json:"mode,omitempty"` // Absent/empty means full.
	// Absent/null retains the original streaming transport; false buffers events.
	Stream *bool `json:"stream,omitempty"`
}

type BackgroundResult struct {
	TaskID  string `json:"taskId"`
	Content string `json:"content"`
	IsError bool   `json:"isError"`
}

type ToolUseResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Output  string `json:"output"`
	IsError bool   `json:"isError"`
}
