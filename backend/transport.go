package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const maxBufferedResponseBytes = 4 * 1024 * 1024

type eventBuffer struct {
	events []Event
	bytes  int
}

func (buffer *eventBuffer) append(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode buffered event: %w", err)
	}
	size := len(data)
	if len(buffer.events) == 0 {
		size += len(`{"events":[]}`) + 1
	} else {
		size++
	}
	if buffer.bytes+size > maxBufferedResponseBytes {
		return fmt.Errorf("buffered response exceeds 4 MiB")
	}
	buffer.bytes += size
	buffer.events = append(buffer.events, event)
	return nil
}

// Only the downstream transport changes. The provider/parser/orchestrator is
// shared with SSE, including all classification and tool-batch validation.
func writeBufferedCompletion(parent context.Context, w http.ResponseWriter, request ChatRequest) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	buffer := eventBuffer{events: []Event{}}
	var bufferingError error
	processRequestContext(ctx, request, func(event Event) {
		if bufferingError != nil {
			return
		}
		if bufferingError = buffer.append(event); bufferingError != nil {
			cancel()
		}
	})
	if parent.Err() != nil {
		return
	}
	if bufferingError != nil {
		buffer.events = []Event{failureEvent(bufferingError), {Type: EvDone, FinishReason: "error"}}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": buffer.events})
}
