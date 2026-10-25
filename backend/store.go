package main

import (
	"context"
	"errors"
	"time"
)

// Store implementations are independent of the HTTP/model loop until Phase 2
// Part 2. The default file stores require a single writer instance per data dir.
type Session struct {
	ID               string `json:"id"`
	User             string `json:"user"`
	Model            string `json:"model"`
	Workspace        string `json:"workspace"`
	CreatedAt        int64  `json:"created_at"` // Unix milliseconds
	Ended            bool   `json:"ended"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	MessageCount     int    `json:"message_count"`
	LastMessageCount int    `json:"last_message_count"`
}

type Message map[string]any

type Usage struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
}

type Summary struct {
	OriginStartID string `json:"origin_start_id"`
	OriginEndID   string `json:"origin_end_id"`
	Content       string `json:"content"`
}

var (
	ErrStoreNotFound = errors.New("store record not found")
	ErrStoreConflict = errors.New("store record already exists or is final")
	ErrStoreInvalid  = errors.New("invalid store input")
)

type MetaStore interface {
	CreateSession(context.Context, Session) (string, error)
	GetSession(context.Context, string) (Session, error)
	BumpUsage(context.Context, string, Usage) error
	SetLastMessageCount(context.Context, string, int) error
	SetLike(context.Context, string, int) error
	ListSessions(context.Context, string) ([]Session, error)
}

type TranscriptStore interface {
	Append(ctx context.Context, sid, roundID string, msgs []Message) error
	PreoccupyReply(ctx context.Context, sid string) (msgID string, err error)
	UpdateReply(ctx context.Context, msgID string, m Message, u Usage) error
	// Latest returns chronological final messages, excluding pending reservations
	// and storage metadata. n == 0 returns an empty slice; n < 0 is invalid.
	Latest(ctx context.Context, sid string, n int) ([]Message, error)
	SaveSummary(ctx context.Context, sid string, s Summary) error
}

type Coordinator interface {
	Lock(ctx context.Context, key string, ttl time.Duration) (unlock func(), ok bool, err error)
	Allow(ctx context.Context, key string, rate int, per time.Duration) bool
	Flag(ctx context.Context, key string) (val string, ok bool)
	Publish(ctx context.Context, emitterID string, chunk []byte) error
}

var (
	_ MetaStore       = (*JSONMetaStore)(nil)
	_ TranscriptStore = (*JSONLTranscriptStore)(nil)
	_ Coordinator     = (*InprocCoordinator)(nil)
)
