package main

import (
	"context"
	"errors"
	"time"
)

// The default file stores require a single writer instance per data directory.
type Session struct {
	ParentSessionID  *string `json:"parentSessionId"`
	AgentType        string  `json:"agentType,omitempty"`
	ForkedFrom       string  `json:"forkedFrom,omitempty"`
	ID               string  `json:"id"`
	User             string  `json:"user"`
	Model            string  `json:"model"`
	Workspace        string  `json:"workspace"`
	CreatedAt        int64   `json:"created_at"` // Unix milliseconds
	Ended            bool    `json:"ended"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	MessageCount     int     `json:"message_count"`
	LastMessageCount int     `json:"last_message_count"`
}

type Message map[string]any

type Usage struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
}

// PrefixCount addresses immutable committed-message ordinals, independent of
// the store-specific physical IDs. PrefixHash validates the exact source range.
// Old summaries without these fields remain saved-only records.
type Summary struct {
	DerivedFrom   string `json:"derived_from,omitempty"`
	Ready         bool   `json:"ready,omitempty"`
	SourceHash    string `json:"source_hash,omitempty"`
	ConfigHash    string `json:"config_hash,omitempty"`
	OriginStartID string `json:"origin_start_id"`
	OriginEndID   string `json:"origin_end_id"`
	Content       string `json:"content"`
	PrefixCount   int    `json:"prefix_count,omitempty"`
	PrefixHash    string `json:"prefix_hash,omitempty"`
	Usage         Usage  `json:"usage,omitempty"`
	UsageID       string `json:"usage_id,omitempty"`
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
	// Apply a committed transcript round once, including recovery after a crash
	// between transcript commit and metadata update.
	CommitRoundUsage(ctx context.Context, sid, roundID string, usage Usage, messageCount int) error
}

type TranscriptStore interface {
	Append(ctx context.Context, sid, roundID string, msgs []Message) error
	PreoccupyReply(ctx context.Context, sid string) (msgID string, err error)
	UpdateReply(ctx context.Context, msgID string, m Message, u Usage) error
	// Latest returns the prompt view (validated summary + remaining history).
	// RawLatest returns committed messages for identity, resume and ledger counts.
	// Both exclude pending/storage metadata; n == 0 is empty, n < 0 invalid.
	Latest(ctx context.Context, sid string, n int) ([]Message, error)
	RawLatest(ctx context.Context, sid string, n int) ([]Message, error)
	Summaries(ctx context.Context, sid string) ([]Summary, error)
	SaveSummary(ctx context.Context, sid string, s Summary) error
	Round(ctx context.Context, sid, roundID string) (StoredRound, error)
	ReserveRound(ctx context.Context, sid, roundID, fingerprint string, input []Message, baseCount int) (StoredRound, error)
	CommitRound(ctx context.Context, sid, roundID string, reply Message, usage Usage, events []Event) error
	CommittedRounds(ctx context.Context, sid string) ([]StoredRound, error)
}

type StoredRound struct {
	PromptPrefixHash string
	ID               string
	ReplyID          string
	Fingerprint      string
	BaseCount        int
	InputCount       int
	Final            bool
	Usage            Usage
	Events           []Event
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
