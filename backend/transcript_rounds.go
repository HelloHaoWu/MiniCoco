package main

import (
	"context"
	"encoding/json"
)

// Round input and its assistant reservation are committed in one file replace.
// Unfinished rounds remain on disk for retries, but are excluded from Latest.
func storedRounds(records []Message) ([]StoredRound, error) {
	order := []string{}
	rounds := map[string]StoredRound{}
	for _, record := range records {
		if record["request_hash"] == nil {
			continue
		}
		var wire struct {
			ID          string  `json:"round_id"`
			ReplyID     string  `json:"msg_id"`
			Fingerprint string  `json:"request_hash"`
			BaseCount   int     `json:"base_count"`
			InputCount  int     `json:"input_count"`
			Status      string  `json:"status"`
			Usage       Usage   `json:"usage"`
			Events      []Event `json:"events"`
		}
		data, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			return nil, err
		}
		if wire.ID == "" || len(wire.Fingerprint) != 64 || wire.BaseCount < 0 || wire.InputCount < 1 || !validUsage(wire.Usage) {
			return nil, ErrStoreInvalid
		}
		if _, exists := rounds[wire.ID]; !exists {
			order = append(order, wire.ID)
		}
		rounds[wire.ID] = StoredRound{ID: wire.ID, ReplyID: wire.ReplyID, Fingerprint: wire.Fingerprint, BaseCount: wire.BaseCount, InputCount: wire.InputCount, Final: wire.Status == "final", Usage: wire.Usage, Events: wire.Events}
	}
	result := make([]StoredRound, 0, len(order))
	for _, id := range order {
		result = append(result, rounds[id])
	}
	return result, nil
}

func findStoredRound(records []Message, roundID string) (StoredRound, error) {
	rounds, err := storedRounds(records)
	if err != nil {
		return StoredRound{}, err
	}
	for _, round := range rounds {
		if round.ID == roundID {
			return round, nil
		}
	}
	return StoredRound{}, ErrStoreNotFound
}

func (store *JSONLTranscriptStore) Round(ctx context.Context, sid, roundID string) (StoredRound, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return StoredRound{}, err
	}
	_, records, err := store.read(sid)
	if err != nil {
		return StoredRound{}, err
	}
	return findStoredRound(records, roundID)
}

func (store *JSONLTranscriptStore) ReserveRound(ctx context.Context, sid, roundID, fingerprint string, input []Message, baseCount int) (StoredRound, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return StoredRound{}, err
	}
	if roundID == "" || len(roundID) > 256 || len(fingerprint) != 64 || len(input) == 0 || baseCount < 0 {
		return StoredRound{}, ErrStoreInvalid
	}
	old, records, err := store.read(sid)
	if err != nil {
		return StoredRound{}, err
	}
	if previous, err := findStoredRound(records, roundID); err == nil {
		if previous.Fingerprint != fingerprint {
			return StoredRound{}, ErrStoreConflict
		}
		return previous, nil
	} else if err != ErrStoreNotFound {
		return StoredRound{}, err
	}
	batch := make([]Message, 0, len(input)+1)
	for _, message := range input {
		record, err := cloneStoreMessage(message)
		if err != nil {
			return StoredRound{}, err
		}
		id, err := storeID("m_")
		if err != nil {
			return StoredRound{}, err
		}
		record["msg_id"], record["round_id"], record["status"] = sid+"."+id, roundID, "final"
		batch = append(batch, record)
	}
	id, err := storeID("m_")
	if err != nil {
		return StoredRound{}, err
	}
	id = sid + "." + id
	batch = append(batch, Message{"msg_id": id, "round_id": roundID, "role": "assistant", "status": "pending", "request_hash": fingerprint, "base_count": baseCount, "input_count": len(input)})
	path, _ := store.path(sid)
	if err := appendStoreRecords(ctx, path, old, batch); err != nil {
		return StoredRound{}, err
	}
	return StoredRound{ID: roundID, ReplyID: id, Fingerprint: fingerprint, BaseCount: baseCount, InputCount: len(input)}, nil
}

func (store *JSONLTranscriptStore) CommitRound(ctx context.Context, sid, roundID string, reply Message, usage Usage, events []Event) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validUsage(usage) || len(events) == 0 || events[len(events)-1].Type != EvDone {
		return ErrStoreInvalid
	}
	record, err := cloneStoreMessage(reply)
	if err != nil {
		return err
	}
	if record["role"] != "assistant" {
		return ErrStoreInvalid
	}
	old, records, err := store.read(sid)
	if err != nil {
		return err
	}
	round, err := findStoredRound(records, roundID)
	if err != nil {
		return err
	}
	if round.Final {
		return ErrStoreConflict
	}
	record["msg_id"], record["round_id"], record["status"] = round.ReplyID, roundID, "final"
	record["request_hash"], record["base_count"], record["input_count"] = round.Fingerprint, round.BaseCount, round.InputCount
	record["usage"], record["events"] = usage, events
	path, _ := store.path(sid)
	return appendStoreRecords(ctx, path, old, []Message{record})
}

func (store *JSONLTranscriptStore) CommittedRounds(ctx context.Context, sid string) ([]StoredRound, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, records, err := store.read(sid)
	if err != nil {
		return nil, err
	}
	rounds, err := storedRounds(records)
	if err != nil {
		return nil, err
	}
	result := []StoredRound{}
	for _, round := range rounds {
		if round.Final {
			result = append(result, round)
		}
	}
	return result, nil
}
