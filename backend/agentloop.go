package main

import "context"

// Per-turn orchestration.
//
// Mirrors Coco's AgentLoopV2ServiceImpl.processRequest: handle ONE round. It
// assembles the prompt, calls the model, and streams events out via emit. It
// does NOT loop over tool calls and keeps no state across requests — the client
// drives the multi-round loop by sending tool results back.
func processRequest(req ChatRequest, emit func(Event)) {
	processRequestContext(context.Background(), req, emit)
}

func processRequestContext(ctx context.Context, req ChatRequest, emit func(Event)) {
	messages, tools := genPrompt(req.Messages, req.Workspace)
	if err := streamCompletionContext(ctx, messages, tools, req.Model, emit); err != nil {
		// Surface upstream/LLM failures to the client, then still end the round.
		emit(Event{Type: EvError, Message: err.Error()})
		emit(Event{Type: EvDone, FinishReason: "error"})
	}
}
