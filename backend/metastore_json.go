package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type metaSnapshot struct {
	Sessions   map[string]Session          `json:"sessions"`
	Likes      map[string]int              `json:"likes"`
	RoundUsage map[string]map[string]Usage `json:"round_usage,omitempty"`
}

type JSONMetaStore struct {
	mu    sync.Mutex
	path  string
	state metaSnapshot
}

func NewJSONMetaStore(dataDir string) (*JSONMetaStore, error) {
	dir, err := storeDirectory(dataDir)
	if err != nil {
		return nil, err
	}
	store := &JSONMetaStore{path: filepath.Join(dir, "meta.json"), state: metaSnapshot{Sessions: map[string]Session{}, Likes: map[string]int{}, RoundUsage: map[string]map[string]Usage{}}}
	data, err := os.ReadFile(store.path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var loaded metaSnapshot
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	store.state = loaded
	if store.state.RoundUsage == nil {
		store.state.RoundUsage = map[string]map[string]Usage{}
	}
	if store.state.Sessions == nil || store.state.Likes == nil {
		return nil, fmt.Errorf("%w: incomplete metadata", ErrStoreInvalid)
	}
	for id, session := range store.state.Sessions {
		if id != session.ID || !validSession(session) {
			return nil, fmt.Errorf("%w: corrupt session metadata", ErrStoreInvalid)
		}
	}
	for id, like := range store.state.Likes {
		if id == "" || like < -1 || like > 1 {
			return nil, fmt.Errorf("%w: corrupt like metadata", ErrStoreInvalid)
		}
	}
	for sid, rounds := range store.state.RoundUsage {
		if _, exists := store.state.Sessions[sid]; !exists {
			return nil, ErrStoreInvalid
		}
		for rid, usage := range rounds {
			if rid == "" || !validUsage(usage) {
				return nil, ErrStoreInvalid
			}
		}
	}
	return store, nil
}

func validSession(s Session) bool {
	if s.ForkedFrom != "" && !validSessionID(s.ForkedFrom) {
		return false
	}
	if (s.AgentType == "") != (s.ParentSessionID == nil) {
		return false
	}
	return (s.ParentSessionID == nil || validSessionID(*s.ParentSessionID)) && (s.AgentType == "" || s.AgentType == "explore") && validSessionID(s.ID) && s.CreatedAt >= 0 && s.MessageCount >= 0 && s.LastMessageCount >= 0 && validUsage(Usage{s.PromptTokens, s.CompletionTokens, s.TotalTokens})
}

// Copy before persisting: a failed write must not publish an in-memory update.
func (store *JSONMetaStore) mutate(ctx context.Context, change func(*metaSnapshot) error) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	next := metaSnapshot{Sessions: make(map[string]Session, len(store.state.Sessions)), Likes: make(map[string]int, len(store.state.Likes)), RoundUsage: map[string]map[string]Usage{}}
	for id, session := range store.state.Sessions {
		next.Sessions[id] = cloneSession(session)
	}
	for id, like := range store.state.Likes {
		next.Likes[id] = like
	}
	for sid, rounds := range store.state.RoundUsage {
		next.RoundUsage[sid] = map[string]Usage{}
		for rid, usage := range rounds {
			next.RoundUsage[sid][rid] = usage
		}
	}
	if err := change(&next); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := atomicStoreFile(ctx, store.path, append(data, '\n')); err != nil {
		return err
	}
	store.state = next
	return nil
}

func (store *JSONMetaStore) CommitRoundUsage(ctx context.Context, sid, roundID string, usage Usage, count int) error {
	if roundID == "" || count < 0 || !validUsage(usage) {
		return ErrStoreInvalid
	}
	return store.mutate(ctx, func(next *metaSnapshot) error {
		session, exists := next.Sessions[sid]
		if !exists {
			return ErrStoreNotFound
		}
		if next.RoundUsage[sid] == nil {
			next.RoundUsage[sid] = map[string]Usage{}
		}
		if previous, exists := next.RoundUsage[sid][roundID]; exists {
			if previous != usage {
				return ErrStoreConflict
			}
			return nil
		}
		if session.PromptTokens > math.MaxInt64-usage.Prompt || session.CompletionTokens > math.MaxInt64-usage.Completion || session.TotalTokens > math.MaxInt64-usage.Total {
			return ErrStoreInvalid
		}
		session.PromptTokens += usage.Prompt
		session.CompletionTokens += usage.Completion
		session.TotalTokens += usage.Total
		if count > session.MessageCount {
			session.MessageCount = count
		}
		next.Sessions[sid] = session
		next.RoundUsage[sid][roundID] = usage
		return nil
	})
}

func (store *JSONMetaStore) CreateSession(ctx context.Context, session Session) (string, error) {
	if session.ID == "" {
		id, err := storeID("s_")
		if err != nil {
			return "", err
		}
		session.ID = id
	}
	if session.CreatedAt == 0 {
		session.CreatedAt = time.Now().UnixMilli()
	}
	if !validSession(session) {
		return "", ErrStoreInvalid
	}
	err := store.mutate(ctx, func(next *metaSnapshot) error {
		if _, exists := next.Sessions[session.ID]; exists {
			return ErrStoreConflict
		}
		next.Sessions[session.ID] = cloneSession(session)
		return nil
	})
	if err != nil {
		return "", err
	}
	return session.ID, nil
}

func (store *JSONMetaStore) GetSession(ctx context.Context, id string) (Session, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	session, exists := store.state.Sessions[id]
	if !exists {
		return Session{}, ErrStoreNotFound
	}
	return cloneSession(session), nil
}

func (store *JSONMetaStore) BumpUsage(ctx context.Context, id string, usage Usage) error {
	if !validUsage(usage) {
		return ErrStoreInvalid
	}
	return store.mutate(ctx, func(next *metaSnapshot) error {
		session, exists := next.Sessions[id]
		if !exists {
			return ErrStoreNotFound
		}
		if session.PromptTokens > math.MaxInt64-usage.Prompt || session.CompletionTokens > math.MaxInt64-usage.Completion || session.TotalTokens > math.MaxInt64-usage.Total {
			return fmt.Errorf("%w: usage overflow", ErrStoreInvalid)
		}
		session.PromptTokens += usage.Prompt
		session.CompletionTokens += usage.Completion
		session.TotalTokens += usage.Total
		next.Sessions[id] = session
		return nil
	})
}

func (store *JSONMetaStore) SetLastMessageCount(ctx context.Context, id string, n int) error {
	if n < 0 {
		return ErrStoreInvalid
	}
	return store.mutate(ctx, func(next *metaSnapshot) error {
		session, exists := next.Sessions[id]
		if !exists {
			return ErrStoreNotFound
		}
		session.LastMessageCount = n
		next.Sessions[id] = session
		return nil
	})
}

func (store *JSONMetaStore) SetLike(ctx context.Context, msgID string, like int) error {
	if msgID == "" || len(msgID) > 256 || like < -1 || like > 1 {
		return ErrStoreInvalid
	}
	return store.mutate(ctx, func(next *metaSnapshot) error { next.Likes[msgID] = like; return nil })
}

func (store *JSONMetaStore) ListSessions(ctx context.Context, user string) ([]Session, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := []Session{}
	for _, session := range store.state.Sessions {
		if session.User == user {
			result = append(result, cloneSession(session))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt == result[j].CreatedAt {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt > result[j].CreatedAt
	})
	return result, nil
}

func cloneSession(s Session) Session {
	if s.ParentSessionID != nil {
		parent := *s.ParentSessionID
		s.ParentSessionID = &parent
	}
	return s
}
