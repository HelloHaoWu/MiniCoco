package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const allHistory = int(^uint(0) >> 1)
const maxSummaryBytes = 8192

func historyDigest(messages []Message) string {
	raw, _ := json.Marshal(messages)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func validateSummary(summary Summary, raw []Message) error {
	if summary.Ready {
		if summary.PrefixCount == 0 || !validDigest(summary.SourceHash) || !validDigest(summary.ConfigHash) || !strings.HasPrefix(summary.UsageID, "\x00precompact:") || !validSessionID(strings.TrimPrefix(summary.UsageID, "\x00precompact:")) {
			return ErrStoreInvalid
		}
	}
	if summary.OriginStartID == "" || summary.OriginEndID == "" || strings.TrimSpace(summary.Content) == "" || !validUsage(summary.Usage) {
		return ErrStoreInvalid
	}
	if summary.PrefixCount == 0 {
		if summary.PrefixHash != "" || summary.UsageID != "" {
			return ErrStoreInvalid
		}
		return nil
	} // Legacy saved-only summaries.
	if summary.PrefixCount < 1 || summary.PrefixCount > len(raw) || len(summary.Content) > maxSummaryBytes || historyDigest(raw[:summary.PrefixCount]) != summary.PrefixHash {
		return ErrStoreConflict
	}
	starts, err := historyBlocks(raw)
	if err != nil {
		return ErrStoreInvalid
	}
	boundary := summary.PrefixCount == len(raw)
	for _, start := range starts {
		if start == summary.PrefixCount {
			boundary = true
		}
	}
	if !boundary || raw[summary.PrefixCount-1]["tool_calls"] != nil {
		return ErrStoreInvalid
	}
	if !summary.Ready && summary.UsageID != "\x00summary:"+historyDigest([]Message{{"hash": summary.PrefixHash, "content": summary.Content}}) {
		return ErrStoreInvalid
	}
	return nil
}
func activeSummary(summaries []Summary, raw []Message) (*Summary, error) {
	var active *Summary
	for i := range summaries {
		s := &summaries[i]
		// Speculative candidates must never change Latest, forks or sync compression.
		if s.Ready {
			continue
		}
		if err := validateSummary(*s, raw); err != nil {
			return nil, err
		}
		if s.PrefixCount != 0 {
			if active == nil || s.PrefixCount >= active.PrefixCount {
				active = s
			}
		}
	}
	return active, nil
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
func summaryMessages(content string) []Message {
	return []Message{{"role": "user", "content": "Continue using the saved context."}, {"role": "assistant", "content": "[Context summary]\n" + content}}
}
func mergedHistory(raw []Message, summaries []Summary, n int) ([]Message, error) {
	if n < 0 {
		return nil, ErrStoreInvalid
	}
	active, err := activeSummary(summaries, raw)
	if err != nil {
		return nil, err
	}
	result := raw
	if active != nil {
		result = append(summaryMessages(active.Content), raw[active.PrefixCount:]...)
	}
	if n < len(result) {
		result = result[len(result)-n:]
	}
	return result, nil
}
func (store *JSONLTranscriptStore) Summaries(ctx context.Context, sid string) ([]Summary, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := store.path(sid)
	if err != nil {
		return nil, err
	}
	path = strings.TrimSuffix(path, ".jsonl") + ".summary.jsonl"
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return nil, ErrStoreInvalid
	}
	result := []Summary{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var s Summary
		if json.Unmarshal(line, &s) != nil {
			return nil, fmt.Errorf("%w: invalid summary record", ErrStoreInvalid)
		}
		result = append(result, s)
	}
	return result, nil
}
func (store *JSONLTranscriptStore) Latest(ctx context.Context, sid string, n int) ([]Message, error) {
	raw, err := store.RawLatest(ctx, sid, allHistory)
	if err != nil {
		return nil, err
	}
	summaries, err := store.Summaries(ctx, sid)
	if err != nil {
		return nil, err
	}
	return mergedHistory(raw, summaries, n)
}
