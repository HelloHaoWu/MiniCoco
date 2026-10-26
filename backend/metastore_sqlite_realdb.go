//go:build realdb

package main

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

type SQLiteMetaStore struct{ db *sql.DB }

var _ MetaStore = (*SQLiteMetaStore)(nil)

const sessionColumns = `id,user,model,workspace,created_at,ended,prompt_tokens,completion_tokens,total_tokens,message_count,last_message_count`
const sessionLinkColumns = `,(SELECT parent_session_id FROM session_link WHERE session_id=session.id),COALESCE((SELECT agent_type FROM session_link WHERE session_id=session.id),'')`

func NewSQLiteMetaStore(ctx context.Context, path string) (*SQLiteMetaStore, error) {
	if path == "" {
		return nil, ErrStoreInvalid
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := storeDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	uri := &url.URL{Scheme: "file", Path: path}
	query := uri.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "foreign_keys(1)")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	schema := `CREATE TABLE IF NOT EXISTS session(
        id TEXT PRIMARY KEY, user TEXT NOT NULL, model TEXT NOT NULL, workspace TEXT NOT NULL,
        created_at INTEGER NOT NULL, ended INTEGER NOT NULL DEFAULT 0,
        prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
        total_tokens INTEGER NOT NULL DEFAULT 0, message_count INTEGER NOT NULL DEFAULT 0,
        last_message_count INTEGER NOT NULL DEFAULT 0);
    CREATE INDEX IF NOT EXISTS idx_session_user ON session(user,created_at);
    CREATE TABLE IF NOT EXISTS msg_index(msg_id TEXT PRIMARY KEY,session_id TEXT,role TEXT,ts INTEGER,like_status INTEGER NOT NULL DEFAULT 0,round_id TEXT);
    CREATE INDEX IF NOT EXISTS idx_msg_session ON msg_index(session_id,ts);
    CREATE TABLE IF NOT EXISTS usage_round(
        session_id TEXT NOT NULL REFERENCES session(id),round_id TEXT NOT NULL,
        prompt INTEGER NOT NULL,completion INTEGER NOT NULL,total INTEGER NOT NULL,
        PRIMARY KEY(session_id,round_id));
    CREATE TABLE IF NOT EXISTS session_link(session_id TEXT PRIMARY KEY REFERENCES session(id),parent_session_id TEXT,agent_type TEXT NOT NULL DEFAULT '');`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteMetaStore{db: db}, nil
}

func (store *SQLiteMetaStore) Close() error { return store.db.Close() }

func sqliteUnique(err error) bool {
	var failure *sqlite.Error
	return errors.As(err, &failure) && (failure.Code() == 1555 || failure.Code() == 2067)
}

func scanSQLiteSession(row interface{ Scan(...any) error }) (Session, error) {
	var session Session
	err := row.Scan(&session.ID, &session.User, &session.Model, &session.Workspace, &session.CreatedAt, &session.Ended,
		&session.PromptTokens, &session.CompletionTokens, &session.TotalTokens, &session.MessageCount, &session.LastMessageCount, &session.ParentSessionID, &session.AgentType)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrStoreNotFound
	}
	return session, err
}

func (store *SQLiteMetaStore) CreateSession(ctx context.Context, session Session) (string, error) {
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
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO session(`+sessionColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		session.ID, session.User, session.Model, session.Workspace, session.CreatedAt, session.Ended,
		session.PromptTokens, session.CompletionTokens, session.TotalTokens, session.MessageCount, session.LastMessageCount)
	if sqliteUnique(err) {
		return "", ErrStoreConflict
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_link(session_id,parent_session_id,agent_type) VALUES(?,?,?)`, session.ID, session.ParentSessionID, session.AgentType); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return session.ID, nil
}

func (store *SQLiteMetaStore) GetSession(ctx context.Context, id string) (Session, error) {
	return scanSQLiteSession(store.db.QueryRowContext(ctx, `SELECT `+sessionColumns+sessionLinkColumns+` FROM session WHERE id=?`, id))
}

func (store *SQLiteMetaStore) BumpUsage(ctx context.Context, id string, usage Usage) error {
	if !validUsage(usage) {
		return ErrStoreInvalid
	}
	result, err := store.db.ExecContext(ctx, `UPDATE session SET prompt_tokens=prompt_tokens+?,completion_tokens=completion_tokens+?,total_tokens=total_tokens+?
        WHERE id=? AND prompt_tokens<=? AND completion_tokens<=? AND total_tokens<=?`, usage.Prompt, usage.Completion, usage.Total, id,
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

func (store *SQLiteMetaStore) SetLastMessageCount(ctx context.Context, id string, n int) error {
	if n < 0 {
		return ErrStoreInvalid
	}
	result, err := store.db.ExecContext(ctx, `UPDATE session SET last_message_count=? WHERE id=?`, n, id)
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

func (store *SQLiteMetaStore) SetLike(ctx context.Context, msgID string, like int) error {
	if msgID == "" || len(msgID) > 256 || like < -1 || like > 1 {
		return ErrStoreInvalid
	}
	sid, _ := sessionFromMessageID(msgID)
	_, err := store.db.ExecContext(ctx, `INSERT INTO msg_index(msg_id,session_id,like_status) VALUES(?,?,?) ON CONFLICT(msg_id) DO UPDATE SET like_status=excluded.like_status`, msgID, sid, like)
	return err
}

func (store *SQLiteMetaStore) ListSessions(ctx context.Context, user string) ([]Session, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT `+sessionColumns+sessionLinkColumns+` FROM session WHERE user=? ORDER BY created_at DESC,id ASC`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Session{}
	for rows.Next() {
		session, err := scanSQLiteSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, rows.Err()
}

func (store *SQLiteMetaStore) CommitRoundUsage(ctx context.Context, sid, rid string, usage Usage, count int) error {
	if rid == "" || count < 0 || !validUsage(usage) {
		return ErrStoreInvalid
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// INSERT is the first statement, acquiring SQLite's writer lock before reads.
	// The ledger and all counters are committed by the same database transaction.
	result, err := tx.ExecContext(ctx, `INSERT INTO usage_round(session_id,round_id,prompt,completion,total) SELECT ?,?,?,?,? WHERE EXISTS(SELECT 1 FROM session WHERE id=?) ON CONFLICT(session_id,round_id) DO NOTHING`, sid, rid, usage.Prompt, usage.Completion, usage.Total, sid)
	if err != nil {
		return err
	}
	added, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if added == 0 {
		var previous Usage
		err := tx.QueryRowContext(ctx, `SELECT prompt,completion,total FROM usage_round WHERE session_id=? AND round_id=?`, sid, rid).Scan(&previous.Prompt, &previous.Completion, &previous.Total)
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
	result, err = tx.ExecContext(ctx, `UPDATE session SET prompt_tokens=prompt_tokens+?,completion_tokens=completion_tokens+?,total_tokens=total_tokens+?,message_count=MAX(message_count,?)
        WHERE id=? AND prompt_tokens<=? AND completion_tokens<=? AND total_tokens<=?`, usage.Prompt, usage.Completion, usage.Total, count, sid,
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
