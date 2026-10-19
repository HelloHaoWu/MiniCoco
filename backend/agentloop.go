package main

// Per-turn orchestration.
//
// Mirrors Coco's AgentLoopV2ServiceImpl.processRequest: handle ONE round. It
// assembles the prompt, calls the model, and streams events out via emit. It
// does NOT loop over tool calls and keeps no state across requests — the client
// drives the multi-round loop by sending tool results back.
func processRequest(req ChatRequest, emit func(Event)) {
	messages, tools := genPrompt(req.Messages, req.Workspace)
	if err := streamCompletion(messages, tools, req.Model, emit); err != nil {
		// Surface upstream/LLM failures to the client, then still end the round.
		emit(Event{Type: EvError, Message: err.Error()})
		emit(Event{Type: EvDone, FinishReason: FinishStop})
	}
}
