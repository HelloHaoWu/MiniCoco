package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errForkBoundary = errors.New("FORK_BOUNDARY_UNRESOLVABLE")

type ForkRequest struct {
	SessionID     string `json:"sessionId"`
	ForkMessageID string `json:"forkMessageId,omitempty"`
}
type ForkResult struct {
	NewSessionID           string `json:"newSessionId"`
	ResolvedForkMessageID  string `json:"resolvedForkMessageId"`
	CopiedThroughMessageID string `json:"copiedThroughMessageId"`
}

// Logical ordinals exclude storage reservations and are stable after compaction.
func resolveForkBoundary(messages []Message, target string) (int, int, error) {
	index := -1
	if target == "" {
		for i, m := range messages {
			if m["role"] == "assistant" {
				index = i
			}
		}
	} else {
		kind, value, ok := strings.Cut(target, ":")
		n, err := strconv.Atoi(value)
		if !ok || err != nil || n < 1 {
			return 0, 0, errForkBoundary
		}
		if kind == "message" {
			index = n - 1
		} else if kind == "assistant" {
			count := 0
			for i, m := range messages {
				if m["role"] == "assistant" {
					count++
					if count == n {
						index = i
						break
					}
				}
			}
		} else {
			return 0, 0, errForkBoundary
		}
	}
	if index < 0 || index >= len(messages) || messages[index]["role"] != "assistant" {
		return 0, 0, errForkBoundary
	}
	for attempts := 0; index >= 0 && attempts < 50; index-- {
		if messages[index]["role"] != "assistant" {
			continue
		}
		attempts++
		end := index + 1
		if messages[index]["tool_calls"] != nil {
			if end == len(messages) {
				continue
			}
			if messages[end]["role"] == "user" && strings.HasPrefix(fmt.Sprint(messages[end]["content"]), "<tool_result>") {
				end++
			} else {
				for end < len(messages) && messages[end]["role"] == "tool" {
					end++
				}
			}
			if end == index+1 {
				continue
			}
		}
		if _, err := historyBlocks(messages[:end]); err == nil {
			return index, end, nil
		}
	}
	return 0, 0, errForkBoundary
}

func (s *sessionService) fork(ctx context.Context, req ForkRequest) (ForkResult, error) {
	source, err := s.meta.GetSession(ctx, req.SessionID)
	if err != nil {
		return ForkResult{}, err
	}
	raw, err := s.transcript.RawLatest(ctx, req.SessionID, allHistory)
	if err != nil {
		return ForkResult{}, err
	}
	index, end, err := resolveForkBoundary(raw, req.ForkMessageID)
	if err != nil {
		return ForkResult{}, err
	}
	copied := raw[:end]
	data, err := json.Marshal(copied)
	if err != nil {
		return ForkResult{}, err
	}
	if len(data) > 16*1024*1024 || len(copied) > 10000 {
		return ForkResult{}, completionFailure(codeBadRequest, false, "fork exceeds 16 MiB / 10000 messages", nil)
	}
	summaries, err := s.transcript.Summaries(ctx, req.SessionID)
	if err != nil {
		return ForkResult{}, err
	}
	if _, err := activeSummary(summaries, raw); err != nil {
		return ForkResult{}, err
	}
	eligible := []Summary{}
	for _, summary := range summaries {
		if summary.PrefixCount > 0 && summary.PrefixCount <= end {
			eligible = append(eligible, summary)
		}
	}
	checkpoint, err := activeSummary(eligible, copied)
	if err != nil {
		return ForkResult{}, err
	}
	sid, err := storeID("s_")
	if err != nil {
		return ForkResult{}, err
	}
	// Publish metadata last: a failed copy can leave an unlisted orphan, never a
	// visible half-initialized fork. Source records and usage are never mutated.
	if err := s.transcript.Append(ctx, sid, "fork_"+sid, copied); err != nil {
		return ForkResult{}, err
	}
	if checkpoint != nil {
		summary := *checkpoint
		summary.Usage = Usage{}
		if err := s.transcript.SaveSummary(ctx, sid, summary); err != nil {
			return ForkResult{}, err
		}
	}
	if _, err := s.meta.CreateSession(ctx, Session{ID: sid, User: source.User, Model: source.Model, Workspace: source.Workspace, ForkedFrom: source.ID, MessageCount: end, LastMessageCount: end}); err != nil {
		return ForkResult{}, err
	}
	return ForkResult{NewSessionID: sid, ResolvedForkMessageID: fmt.Sprintf("message:%d", index+1), CopiedThroughMessageID: fmt.Sprintf("message:%d", end)}, nil
}

func handleFork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req ForkRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || !validSessionID(req.SessionID) {
		writeSessionError(w, ErrStoreInvalid)
		return
	}
	if activeSessions == nil {
		writeSessionError(w, ErrStoreNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	unlock, ok, err := activeSessions.coord.Lock(ctx, "lock:session:"+req.SessionID, 250*time.Second)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	if !ok {
		writeJSON(w, 409, map[string]string{"error": "session is busy", "code": "FORK_SESSION_BUSY"})
		return
	}
	defer unlock()
	result, err := activeSessions.fork(ctx, req)
	if errors.Is(err, errForkBoundary) {
		writeJSON(w, 400, map[string]string{"error": err.Error(), "code": "FORK_BOUNDARY_UNRESOLVABLE"})
		return
	}
	if err != nil {
		writeSessionError(w, err)
		return
	}
	writeJSON(w, 201, result)
}
