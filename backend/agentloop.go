package main

import (
	"context"
	"encoding/json"
)

// Per-turn orchestration.
//
// Mirrors Coco's AgentLoopV2ServiceImpl.processRequest: handle ONE round. It
// persists a round through sessionService, assembles a prompt and calls the
// model once. The client still drives the tool loop by sending incremental
// results; successful events/done are exposed only after durable commit.
func processModelContext(ctx context.Context, messages []map[string]any, workspace, model string, emit func(Event)) {
	route, err := routeFor(model)
	if err == nil {
		ctx = context.WithValue(ctx, modelRouteContextKey{}, route)
		var tools []json.RawMessage
		messages, tools, err = genPrompt(messages, workspace, ModeFull, route)
		if err == nil {
			err = streamCompletionContext(ctx, messages, tools, model, emit)
		}
	}
	if err != nil {
		// Surface upstream/LLM failures to the client, then still end the round.
		emit(failureEvent(err))
		emit(Event{Type: EvDone, FinishReason: "error"})
	}
}

func processRequestContext(ctx context.Context, req ChatRequest, emit func(Event)) {
	// Both HTTP transports enter here and select exactly one implementation.
	if err := pickLoop()(ctx, req, emit); err != nil {
		emit(failureEvent(err))
		emit(Event{Type: EvDone, FinishReason: "error"})
	}
}
