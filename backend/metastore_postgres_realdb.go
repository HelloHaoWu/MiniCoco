//go:build realdb

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

type PostgresMetaStore struct{ db *sql.DB }

var _ MetaStore = (*PostgresMetaStore)(nil)

const sessionColumns = `id,"user",model,workspace,created_at,ended,prompt_tokens,completion_tokens,total_tokens,message_count,last_message_count`
const sessionLinkColumns = `,(SELECT parent_session_id FROM session_link WHERE session_id=session.id),COALESCE((SELECT agent_type FROM session_link WHERE session_id=session.id),''),COALESCE((SELECT source_id FROM session_fork WHERE session_id=session.id),'')`

// Each instance owns a bounded connection pool. Startup DDL is serialized inside
// the selected schema so simultaneous backends cannot race CREATE TABLE.
func NewPostgresMetaStore(ctx context.Context, uri string) (*PostgresMetaStore, error) {
	if strings.TrimSpace(uri) == "" {
		return nil, fmt.Errorf("POSTGRES_URL is required for real stores")
	}
	config, err := pgx.ParseConfig(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid POSTGRES_URL")
	}
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	var tx *sql.Tx
	fail := func() (*PostgresMetaStore, error) {
		if tx != nil {
			_ = tx.Rollback()
		}
		db.Close()
		return nil, fmt.Errorf("cannot initialize PostgreSQL; check POSTGRES_URL, schema permissions and service availability")
	}
	if err := db.PingContext(ctx); err != nil {
		return fail()
	}
	tx, err = db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fail()
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext(current_schema()), 170017)"); err != nil {
		return fail()
	}
	schema := `CREATE TABLE IF NOT EXISTS session(
        id TEXT PRIMARY KEY, "user" TEXT NOT NULL, model TEXT NOT NULL, workspace TEXT NOT NULL,
        created_at BIGINT NOT NULL, ended BOOLEAN NOT NULL DEFAULT FALSE,
        prompt_tokens BIGINT NOT NULL DEFAULT 0, completion_tokens BIGINT NOT NULL DEFAULT 0,
        total_tokens BIGINT NOT NULL DEFAULT 0, message_count BIGINT NOT NULL DEFAULT 0,
        last_message_count BIGINT NOT NULL DEFAULT 0);
    CREATE INDEX IF NOT EXISTS idx_session_user ON session("user",created_at);
    CREATE TABLE IF NOT EXISTS msg_index(msg_id TEXT PRIMARY KEY,session_id TEXT,role TEXT,ts BIGINT,like_status BIGINT NOT NULL DEFAULT 0,round_id TEXT);
    CREATE INDEX IF NOT EXISTS idx_msg_session ON msg_index(session_id,ts);
    CREATE TABLE IF NOT EXISTS usage_round(
        session_id TEXT NOT NULL REFERENCES session(id),round_id BYTEA NOT NULL,
        prompt BIGINT NOT NULL,completion BIGINT NOT NULL,total BIGINT NOT NULL,
        PRIMARY KEY(session_id,round_id));
    CREATE TABLE IF NOT EXISTS session_link(session_id TEXT PRIMARY KEY REFERENCES session(id),parent_session_id TEXT,agent_type TEXT NOT NULL DEFAULT '');
    CREATE TABLE IF NOT EXISTS session_fork(session_id TEXT PRIMARY KEY REFERENCES session(id),source_id TEXT NOT NULL);`
	for _, statement := range strings.Split(schema, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fail()
		}
	}
	if err := tx.Commit(); err != nil {
		return fail()
	}
	return &PostgresMetaStore{db: db}, nil
}

func (store *PostgresMetaStore) Close() error { return store.db.Close() }

func postgresUnique(err error) bool {
	var failure *pgconn.PgError
	return errors.As(err, &failure) && failure.Code == "23505"
}

func scanPostgresSession(row interface{ Scan(...any) error }) (Session, error) {
	var session Session
	err := row.Scan(&session.ID, &session.User, &session.Model, &session.Workspace, &session.CreatedAt, &session.Ended,
		&session.PromptTokens, &session.CompletionTokens, &session.TotalTokens, &session.MessageCount, &session.LastMessageCount, &session.ParentSessionID, &session.AgentType, &session.ForkedFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrStoreNotFound
	}
	return session, err
}

func (store *PostgresMetaStore) CreateSession(ctx context.Context, session Session) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
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
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO session(`+sessionColumns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		session.ID, session.User, session.Model, session.Workspace, session.CreatedAt, session.Ended,
		session.PromptTokens, session.CompletionTokens, session.TotalTokens, session.MessageCount, session.LastMessageCount)
	if postgresUnique(err) {
		return "", ErrStoreConflict
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_link(session_id,parent_session_id,agent_type) VALUES($1,$2,$3)`, session.ID, session.ParentSessionID, session.AgentType); err != nil {
		return "", err
	}
	if session.ForkedFrom != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_fork(session_id,source_id) VALUES($1,$2)`, session.ID, session.ForkedFrom); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return session.ID, nil
}

func (store *PostgresMetaStore) GetSession(ctx context.Context, id string) (Session, error) {
	return scanPostgresSession(store.db.QueryRowContext(ctx, `SELECT `+sessionColumns+sessionLinkColumns+` FROM session WHERE id=$1`, id))
}

func (store *PostgresMetaStore) BumpUsage(ctx context.Context, id string, usage Usage) error {
	if !validUsage(usage) {
		return ErrStoreInvalid
	}
	result, err := store.db.ExecContext(ctx, `UPDATE session SET prompt_tokens=prompt_tokens+$1,completion_tokens=completion_tokens+$2,total_tokens=total_tokens+$3
        WHERE id=$4 AND prompt_tokens<=$5 AND completion_tokens<=$6 AND total_tokens<=$7`, usage.Prompt, usage.Completion, usage.Total, id,
		math.MaxInt64-usage.Prompt, math.MaxInt64-usage.Completion, math.MaxInt64-usage.Total)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		if _, err := store.GetSession(ctx, id); err != nil {
			return err
		}
		return ErrStoreInvalid
	}
	return nil
}

func (store *PostgresMetaStore) SetLastMessageCount(ctx context.Context, id string, n int) error {
	if n < 0 {
		return ErrStoreInvalid
	}
	result, err := store.db.ExecContext(ctx, `UPDATE session SET last_message_count=$1 WHERE id=$2`, n, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrStoreNotFound
	}
	return nil
}

func (store *PostgresMetaStore) SetLike(ctx context.Context, msgID string, like int) error {
	if msgID == "" || len(msgID) > 256 || like < -1 || like > 1 {
		return ErrStoreInvalid
	}
	sid, _ := sessionFromMessageID(msgID)
	_, err := store.db.ExecContext(ctx, `INSERT INTO msg_index(msg_id,session_id,like_status) VALUES($1,$2,$3) ON CONFLICT(msg_id) DO UPDATE SET like_status=excluded.like_status`, msgID, sid, like)
	return err
}

func (store *PostgresMetaStore) ListSessions(ctx context.Context, user string) ([]Session, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT `+sessionColumns+sessionLinkColumns+` FROM session WHERE "user"=$1 ORDER BY created_at DESC,id ASC`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Session{}
	for rows.Next() {
		session, err := scanPostgresSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, rows.Err()
}

func (store *PostgresMetaStore) CommitRoundUsage(ctx context.Context, sid, rid string, usage Usage, count int) error {
	if rid == "" || count < 0 || !validUsage(usage) {
		return ErrStoreInvalid
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The unique ledger key serializes duplicate rounds. Read Committed lets
	// the following SELECT see a concurrent winner after ON CONFLICT waits.
	// The ledger and all counters are committed by the same database transaction.
	result, err := tx.ExecContext(ctx, `INSERT INTO usage_round(session_id,round_id,prompt,completion,total) SELECT $1,$2,$3,$4,$5 WHERE EXISTS(SELECT 1 FROM session WHERE id=$6) ON CONFLICT(session_id,round_id) DO NOTHING`, sid, []byte(rid), usage.Prompt, usage.Completion, usage.Total, sid)
	if err != nil {
		return err
	}
	added, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if added == 0 {
		var previous Usage
		err := tx.QueryRowContext(ctx, `SELECT prompt,completion,total FROM usage_round WHERE session_id=$1 AND round_id=$2`, sid, []byte(rid)).Scan(&previous.Prompt, &previous.Completion, &previous.Total)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrStoreNotFound
		}
		if err != nil {
			return err
		}
		if previous != usage {
			return ErrStoreConflict
		}
		return tx.Commit()
	}
	result, err = tx.ExecContext(ctx, `UPDATE session SET prompt_tokens=prompt_tokens+$1,completion_tokens=completion_tokens+$2,total_tokens=total_tokens+$3,message_count=GREATEST(message_count,$4)
        WHERE id=$5 AND prompt_tokens<=$6 AND completion_tokens<=$7 AND total_tokens<=$8`, usage.Prompt, usage.Completion, usage.Total, count, sid,
		math.MaxInt64-usage.Prompt, math.MaxInt64-usage.Completion, math.MaxInt64-usage.Total)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrStoreInvalid
	}
	return tx.Commit()
}
