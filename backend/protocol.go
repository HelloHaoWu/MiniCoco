package main

import "encoding/json"

// Wire protocol shared with the TypeScript client (see client/src/protocol.ts).
// Each SSE `data:` line the backend emits is one Event. This mirrors Coco's
// CompletionStreamResult chunks.
const (
    EvText      = "text"      // assistant text delta        {type, delta}
    EvReasoning = "reasoning" // thinking delta              {type, delta}
    EvToolCall  = "tool_call" // a fully-assembled tool call {type, id, name, arguments}
    EvDone      = "done"      // end of this round           {type, finish_reason}
    EvError     = "error"     // backend/LLM failure         {type, message}

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
}

// ChatRequest is the body the client POSTs to /chat/stream. `Messages` is the
// running OpenAI-shaped conversation the client maintains and replays each
// round (so the backend stays stateless).
type ChatRequest struct {
    Messages  []map[string]any `json:"messages"`
    Workspace string           `json:"workspace"`
    Model     string           `json:"model"`
}

