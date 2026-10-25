package main

import "context"

// Per-turn orchestration.
//
// Mirrors Coco's AgentLoopV2ServiceImpl.processRequest: handle ONE round. It
// persists a round through sessionService, assembles a prompt and calls the
// model once. The client still drives the tool loop by sending incremental
// results; successful events/done are exposed only after durable commit.
func processModelContext(ctx context.Context, messages []map[string]any, workspace, model string, emit func(Event)) {
	messages, tools := genPrompt(messages, workspace)
	if err := streamCompletionContext(ctx, messages, tools, model, emit); err != nil {
		// Surface upstream/LLM failures to the client, then still end the round.
		emit(failureEvent(err))
		emit(Event{Type: EvDone, FinishReason: "error"})
	}
}

func processRequestContext(ctx context.Context, req ChatRequest, emit func(Event)) {
	if err := activeSessions.process(ctx, req, emit); err != nil {
		emit(failureEvent(err))
		emit(Event{Type: EvDone, FinishReason: "error"})
	}
}
