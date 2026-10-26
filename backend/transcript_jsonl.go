package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type JSONLTranscriptStore struct {
	mu  sync.Mutex
	dir string
}

func NewJSONLTranscriptStore(dataDir string) (*JSONLTranscriptStore, error) {
	if dataDir == "" {
		return nil, ErrStoreInvalid
	}
	dir, err := storeDirectory(filepath.Join(dataDir, "transcripts"))
	if err != nil {
		return nil, err
	}
	return &JSONLTranscriptStore{dir: dir}, nil
}

func (store *JSONLTranscriptStore) path(sid string) (string, error) {
	if !validSessionID(sid) {
		return "", fmt.Errorf("%w: unsafe session ID", ErrStoreInvalid)
	}
	return filepath.Join(store.dir, sid+".jsonl"), nil
}

func sessionFromMessageID(id string) (string, error) {
	sid, suffix, found := strings.Cut(id, ".")
	if !found || !validSessionID(sid) || !validSessionID(suffix) {
		return "", fmt.Errorf("%w: invalid message ID", ErrStoreInvalid)
	}
	return sid, nil
}

func cloneStoreMessage(message Message) (Message, error) {
	if message == nil {
		return nil, ErrStoreInvalid
	}
	role, _ := message["role"].(string)
	if role != "user" && role != "assistant" && role != "tool" {
		return nil, fmt.Errorf("%w: invalid message role", ErrStoreInvalid)
	}
	if content, exists := message["content"]; exists && content != nil {
		if _, ok := content.(string); !ok {
			return nil, fmt.Errorf("%w: content must be a string or null", ErrStoreInvalid)
		}
	}
	for _, key := range []string{"msg_id", "round_id", "status", "usage", "events", "request_hash", "base_count", "input_count"} {
		if _, exists := message[key]; exists {
			return nil, fmt.Errorf("%w: reserved message field %s", ErrStoreInvalid, key)
		}
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("%w: message is not JSON: %v", ErrStoreInvalid, err)
	}
	var copy Message
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&copy); err != nil {
		return nil, err
	}
	return copy, nil
}

// The append-only logical record sequence is replaced atomically as a file.
// This avoids partial multi-message rounds being mistaken for committed rounds
// after a crash. This small single-process implementation rewrites the full log.
func appendStoreRecords(ctx context.Context, path string, old []byte, records []Message) error {
	var buffer bytes.Buffer
	buffer.Write(old)
	encoder := json.NewEncoder(&buffer)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return atomicStoreFile(ctx, path, buffer.Bytes())
}

func (store *JSONLTranscriptStore) read(sid string) ([]byte, []Message, error) {
	path, err := store.path(sid)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, []Message{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return nil, nil, fmt.Errorf("%w: incomplete transcript line", ErrStoreInvalid)
	}
	records := []Message{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil, nil, fmt.Errorf("%w: invalid transcript JSON line", ErrStoreInvalid)
		}
		var record Message
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&record); err != nil {
			return nil, nil, fmt.Errorf("read transcript: %w", err)
		}
		id, _ := record["msg_id"].(string)
		owner, err := sessionFromMessageID(id)
		status, _ := record["status"].(string)
		role, _ := record["role"].(string)
		if err != nil || owner != sid || (status != "pending" && status != "final") || (role != "user" && role != "assistant" && role != "tool") || (status == "pending" && role != "assistant") {
			return nil, nil, fmt.Errorf("%w: corrupt transcript record", ErrStoreInvalid)
		}
		records = append(records, record)
	}
	return data, records, nil
}

func (store *JSONLTranscriptStore) Append(ctx context.Context, sid, roundID string, messages []Message) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if roundID == "" || len(roundID) > 256 || len(messages) == 0 {
		return ErrStoreInvalid
	}
	old, records, err := store.read(sid)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record["round_id"] == roundID {
			return nil // Same round is already fully committed, including after reopen.
		}
	}
	batch := make([]Message, 0, len(messages))
	for _, message := range messages {
		record, err := cloneStoreMessage(message)
		if err != nil {
			return err
		}
		id, err := storeID("m_")
		if err != nil {
			return err
		}
		record["msg_id"], record["round_id"], record["status"] = sid+"."+id, roundID, "final"
		batch = append(batch, record)
	}
	path, _ := store.path(sid)
	return appendStoreRecords(ctx, path, old, batch)
}

func (store *JSONLTranscriptStore) PreoccupyReply(ctx context.Context, sid string) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	old, _, err := store.read(sid)
	if err != nil {
		return "", err
	}
	id, err := storeID("m_")
	if err != nil {
		return "", err
	}
	id = sid + "." + id
	path, _ := store.path(sid)
	if err := appendStoreRecords(ctx, path, old, []Message{{"msg_id": id, "role": "assistant", "status": "pending"}}); err != nil {
		return "", err
	}
	return id, nil
}

func (store *JSONLTranscriptStore) UpdateReply(ctx context.Context, msgID string, message Message, usage Usage) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	sid, err := sessionFromMessageID(msgID)
	if err != nil {
		return err
	}
	record, err := cloneStoreMessage(message)
	if err != nil {
		return err
	}
	if record["role"] != "assistant" || !validUsage(usage) {
		return ErrStoreInvalid
	}
	old, records, err := store.read(sid)
	if err != nil {
		return err
	}
	var previous Message
	for _, candidate := range records {
		if candidate["msg_id"] == msgID {
			previous = candidate
		}
	}
	if previous == nil || previous["role"] != "assistant" {
		return ErrStoreNotFound
	}
	if previous["request_hash"] != nil {
		return ErrStoreInvalid
	} // Use CommitRound for protocol rounds.
	record["msg_id"], record["status"], record["usage"] = msgID, "final", map[string]int64{"prompt": usage.Prompt, "completion": usage.Completion, "total": usage.Total}
	if previous["status"] == "final" {
		before, _ := json.Marshal(previous)
		after, _ := json.Marshal(record)
		if bytes.Equal(before, after) {
			return nil // Repeating an identical commit adds no extra final record.
		}
		return ErrStoreConflict
	}
	path, _ := store.path(sid)
	return appendStoreRecords(ctx, path, old, []Message{record})
}

func (store *JSONLTranscriptStore) RawLatest(ctx context.Context, sid string, n int) ([]Message, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, ErrStoreInvalid
	}
	_, records, err := store.read(sid)
	if err != nil {
		return nil, err
	}
	order := []string{}
	latest := map[string]Message{}
	boundRounds := map[string]bool{}
	for _, record := range records {
		if _, ok := record["request_hash"].(string); ok {
			rid, _ := record["round_id"].(string)
			boundRounds[rid] = record["status"] == "final"
		}
	}
	for _, record := range records {
		id := record["msg_id"].(string)
		if _, exists := latest[id]; !exists {
			order = append(order, id)
		}
		latest[id] = record
	}
	messages := []Message{}
	for _, id := range order {
		record := latest[id]
		if rid, ok := record["round_id"].(string); ok {
			if committed, bound := boundRounds[rid]; bound && !committed {
				continue
			}
		}
		if record["status"] == "pending" {
			continue
		}
		for _, key := range []string{"msg_id", "round_id", "status", "usage", "events", "request_hash", "base_count", "input_count"} {
			delete(record, key)
		}
		messages = append(messages, record)
	}
	if n < len(messages) {
		messages = messages[len(messages)-n:]
	}
	return messages, nil
}

func (store *JSONLTranscriptStore) SaveSummary(ctx context.Context, sid string, summary Summary) error {
	raw, err := store.RawLatest(ctx, sid, allHistory)
	if err != nil {
		return err
	}
	if err := validateSummary(summary, raw); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := store.path(sid)
	if err != nil {
		return err
	}
	if summary.OriginStartID == "" || summary.OriginEndID == "" || strings.TrimSpace(summary.Content) == "" {
		return ErrStoreInvalid
	}
	path = strings.TrimSuffix(path, ".jsonl") + ".summary.jsonl"
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range bytes.Split(old, []byte{'\n'}) {
		if len(line) > 0 && !json.Valid(line) {
			return fmt.Errorf("%w: corrupt summary log", ErrStoreInvalid)
		}
	}
	if len(old) > 0 && old[len(old)-1] != '\n' {
		return fmt.Errorf("%w: incomplete summary line", ErrStoreInvalid)
	}
	data, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	var record Message
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	return appendStoreRecords(ctx, path, old, []Message{record})
}
