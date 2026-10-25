package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxHistoryBytes = 1024 * 1024
const maxHistoryMessages = 512
const maxToolOutputBytes = 64 * 1024

type sessionService struct {
	meta       MetaStore
	transcript TranscriptStore
	coord      Coordinator
}

func newSessionService(dir string) (*sessionService, error) {
	meta, err := NewJSONMetaStore(dir)
	if err != nil {
		return nil, err
	}
	transcript, err := NewJSONLTranscriptStore(dir)
	if err != nil {
		return nil, err
	}
	return &sessionService{meta: meta, transcript: transcript, coord: NewInprocCoordinator(nil)}, nil
}

func initialSessionID(roundID string) string {
	hash := sha256.Sum256([]byte("minicoco-session:" + roundID))
	return "s_" + hex.EncodeToString(hash[:16])
}

func validateDelta(req ChatRequest) error {
	if req.RoundID == "" || len(req.RoundID) > 256 || len(req.Model) > 256 || len(req.Workspace) > 4096 || (req.SessionID != "" && !validSessionID(req.SessionID)) {
		return ErrStoreInvalid
	}
	if (req.Query != nil) == (req.ToolUseResults != nil) {
		return fmt.Errorf("provide exactly one of query or toolUseResults")
	}
	if req.Query != nil {
		if strings.TrimSpace(*req.Query) == "" {
			return ErrStoreInvalid
		}
	} else {
		if req.SessionID == "" || len(req.ToolUseResults) < 1 || len(req.ToolUseResults) > 16 {
			return ErrStoreInvalid
		}
		seen := map[string]bool{}
		for _, result := range req.ToolUseResults {
			output := result.Output
			if result.IsError && !strings.HasPrefix(output, "ERROR:") {
				output = "ERROR: " + output
			}
			encoded, _ := json.Marshal(output)
			if result.ID == "" || len(result.ID) > 256 || result.Name == "" || len(result.Name) > 128 || seen[result.ID] || len(encoded) > maxToolOutputBytes {
				return ErrStoreInvalid
			}
			seen[result.ID] = true
		}
	}
	return nil
}

func (service *sessionService) resolve(ctx context.Context, req *ChatRequest) error {
	creating := req.SessionID == ""
	if creating {
		req.SessionID = initialSessionID(req.RoundID)
	}
	session, err := service.meta.GetSession(ctx, req.SessionID)
	if errors.Is(err, ErrStoreNotFound) && creating && req.Query != nil {
		model := req.Model
		if model == "" {
			model = getenv("LLM_MODEL", defaultModel)
		}
		_, err = service.meta.CreateSession(ctx, Session{ID: req.SessionID, Workspace: req.Workspace, Model: model})
		if err != nil && !errors.Is(err, ErrStoreConflict) {
			return err
		}
		session, err = service.meta.GetSession(ctx, req.SessionID)
	}
	if err != nil {
		return err
	}
	if session.Workspace != req.Workspace {
		return fmt.Errorf("%w: session workspace differs", ErrStoreConflict)
	}
	if req.Model == "" {
		req.Model = session.Model
	}
	return nil
}

func (service *sessionService) reconcile(ctx context.Context, sid string) error {
	rounds, err := service.transcript.CommittedRounds(ctx, sid)
	if err != nil {
		return err
	}
	for _, round := range rounds {
		if err := service.meta.CommitRoundUsage(ctx, sid, round.ID, round.Usage, round.BaseCount+round.InputCount+1); err != nil {
			return err
		}
	}
	return nil
}

func historyBudget(history []Message) error {
	data, err := json.Marshal(history)
	if err != nil {
		return err
	}
	if len(history) > maxHistoryMessages || len(data) > maxHistoryBytes {
		return fmt.Errorf("conversation budget reached (1 MiB / 512 messages); start a new session")
	}
	return nil
}

func incomingMessages(req ChatRequest, history []Message) ([]Message, error) {
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if len(history) > 0 {
		last := history[len(history)-1]
		if last["role"] == "assistant" && last["tool_calls"] != nil {
			data, _ := json.Marshal(last["tool_calls"])
			if err := json.Unmarshal(data, &calls); err != nil {
				return nil, err
			}
		}
	}
	if req.Query != nil {
		if len(calls) != 0 {
			return nil, fmt.Errorf("unfinished tool batch; automatic replay is unsafe")
		}
		return []Message{{"role": "user", "content": *req.Query}}, nil
	}
	if len(calls) != len(req.ToolUseResults) {
		return nil, fmt.Errorf("tool results must cover the complete pending batch")
	}
	byID := map[string]ToolUseResult{}
	for _, result := range req.ToolUseResults {
		byID[result.ID] = result
	}
	input := []Message{}
	for _, call := range calls {
		result, exists := byID[call.ID]
		if !exists || result.Name != call.Function.Name {
			return nil, fmt.Errorf("tool result identity does not match pending batch")
		}
		output := result.Output
		if result.IsError && !strings.HasPrefix(output, "ERROR:") {
			output = "ERROR: " + output
		}
		input = append(input, Message{"role": "tool", "tool_call_id": call.ID, "content": output})
	}
	return input, nil
}

// The Phase 5 seam is intentionally empty; it does not drop any history.
func compressSeam(ctx context.Context, session Session) error { return ctx.Err() }

func (service *sessionService) process(ctx context.Context, req ChatRequest, emit func(Event)) error {
	if err := service.reconcile(ctx, req.SessionID); err != nil {
		return storageFailure(err)
	}
	encoded, _ := json.Marshal(struct {
		Query            *string
		Results          []ToolUseResult
		Workspace, Model string
	}{req.Query, req.ToolUseResults, req.Workspace, req.Model})
	hash := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(hash[:])
	round, roundErr := service.transcript.Round(ctx, req.SessionID, req.RoundID)
	if roundErr != nil && !errors.Is(roundErr, ErrStoreNotFound) {
		return storageFailure(roundErr)
	}
	if roundErr == nil {
		if round.Fingerprint != fingerprint {
			return completionFailure(codeBadRequest, false, "roundId reused with different input", ErrStoreConflict)
		}
		if round.Final {
			for _, event := range round.Events {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				emit(event)
			}
			return nil
		}
	}
	history, err := service.transcript.Latest(ctx, req.SessionID, maxHistoryMessages+1)
	if err != nil {
		return storageFailure(err)
	}
	if roundErr == nil && round.BaseCount != len(history) {
		return completionFailure(codeBadRequest, false, "unfinished round is stale; use a new roundId", ErrStoreConflict)
	}
	input, err := incomingMessages(req, history)
	if err != nil {
		return completionFailure(codeBadRequest, false, err.Error(), err)
	}
	if flag, _ := service.coord.Flag(ctx, "flag:compress_enabled"); flag == "1" {
		session, err := service.meta.GetSession(ctx, req.SessionID)
		if err != nil {
			return storageFailure(err)
		}
		if session.MessageCount >= maxHistoryMessages {
			if err := compressSeam(ctx, session); err != nil {
				return err
			}
		}
	}
	current := append(append([]Message{}, history...), input...)
	if err := historyBudget(current); err != nil {
		return completionFailure(codeBadRequest, false, err.Error(), err)
	}
	if _, err := service.transcript.ReserveRound(ctx, req.SessionID, req.RoundID, fingerprint, input, len(history)); err != nil {
		return storageFailure(err)
	}
	messages := make([]map[string]any, len(current))
	for i, message := range current {
		messages[i] = message
	}
	messages, tools := genPrompt(messages, req.Workspace)
	buffer := eventBuffer{events: []Event{}}
	modelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	usage := Usage{}
	modelCtx = context.WithValue(modelCtx, usageContextKey{}, &usage)
	var eventError error
	err = streamCompletionContext(modelCtx, messages, tools, req.Model, func(event Event) {
		if eventError != nil {
			return
		}
		if eventError = buffer.append(event); eventError != nil {
			cancel()
			return
		}
		if event.Type == EvText || event.Type == EvReasoning {
			emit(event)
		}
	})
	if eventError != nil {
		return eventError
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reply, err := replyFromEvents(buffer.events)
	if err != nil {
		return err
	}
	next := append(current, reply)
	// Reserve the next full tool-result batch before exposing executable calls.
	reserved := append([]Message{}, next...)
	for _, event := range buffer.events {
		if event.Type == EvToolCall {
			reserved = append(reserved, Message{"role": "tool", "tool_call_id": event.ID, "content": strings.Repeat(" ", maxToolOutputBytes)})
		}
	}
	if err := historyBudget(reserved); err != nil {
		return completionFailure(codeBadRequest, false, err.Error(), err)
	}
	if err := service.transcript.CommitRound(ctx, req.SessionID, req.RoundID, reply, usage, buffer.events); err != nil {
		return storageFailure(err)
	}
	// Transcript commit is authoritative. Replay repairs interrupted metadata writes.
	if err := service.meta.CommitRoundUsage(context.WithoutCancel(ctx), req.SessionID, req.RoundID, usage, len(next)); err != nil {
		return storageFailure(err)
	}
	for _, event := range buffer.events {
		if event.Type != EvText && event.Type != EvReasoning {
			emit(event)
		}
	}
	return nil
}

func replyFromEvents(events []Event) (Message, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("model returned no events")
	}
	var text strings.Builder
	calls := []map[string]any{}
	for i, event := range events {
		switch event.Type {
		case EvText:
			text.WriteString(event.Delta)
		case EvReasoning:
		case EvToolCall:
			calls = append(calls, map[string]any{"id": event.ID, "type": "function", "function": map[string]any{"name": event.Name, "arguments": string(event.Arguments)}})
		case EvDone:
			if i != len(events)-1 || (event.FinishReason != FinishStop && event.FinishReason != FinishToolCalls) || (event.FinishReason == FinishToolCalls) != (len(calls) > 0) {
				return nil, fmt.Errorf("invalid terminal event")
			}
		default:
			return nil, fmt.Errorf("invalid completion event")
		}
	}
	if events[len(events)-1].Type != EvDone {
		return nil, fmt.Errorf("model did not finish")
	}
	reply := Message{"role": "assistant", "content": text.String()}
	if len(calls) > 0 {
		reply["tool_calls"] = calls
	}
	return reply, nil
}

func storageFailure(err error) error {
	return completionFailure("storage", true, "session storage: "+err.Error(), err)
}
