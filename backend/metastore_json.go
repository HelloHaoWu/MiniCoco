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
	Sessions map[string]Session `json:"sessions"`
	Likes    map[string]int     `json:"likes"`
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
	store := &JSONMetaStore{path: filepath.Join(dir, "meta.json"), state: metaSnapshot{Sessions: map[string]Session{}, Likes: map[string]int{}}}
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
	return store, nil
}

func validSession(s Session) bool {
	return validSessionID(s.ID) && s.CreatedAt >= 0 && s.MessageCount >= 0 && s.LastMessageCount >= 0 && validUsage(Usage{s.PromptTokens, s.CompletionTokens, s.TotalTokens})
}

// Copy before persisting: a failed write must not publish an in-memory update.
func (store *JSONMetaStore) mutate(ctx context.Context, change func(*metaSnapshot) error) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	next := metaSnapshot{Sessions: make(map[string]Session, len(store.state.Sessions)), Likes: make(map[string]int, len(store.state.Likes))}
	for id, session := range store.state.Sessions {
		next.Sessions[id] = session
	}
	for id, like := range store.state.Likes {
		next.Likes[id] = like
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
		next.Sessions[session.ID] = session
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
	return session, nil
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
			result = append(result, session)
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
