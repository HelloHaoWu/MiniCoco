package main

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

func writeSessionError(w http.ResponseWriter, err error) {
	var failure *completionError
	if errors.As(err, &failure) {
		status := http.StatusServiceUnavailable
		if failure.code == codeBadRequest {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{"error": failure.Error(), "code": failure.code, "retryable": failure.retryable})
		return
	}
	status := http.StatusServiceUnavailable
	if errors.Is(err, ErrStoreNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, ErrStoreConflict) {
		status = http.StatusConflict
	}
	if errors.Is(err, ErrStoreInvalid) {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "GET only"})
		return
	}
	if activeSessions == nil {
		writeJSON(w, 503, map[string]string{"error": "session service unavailable"})
		return
	}
	sid := strings.TrimPrefix(r.URL.Path, "/sessions")
	if sid == "" || sid == "/" {
		sessions, err := activeSessions.meta.ListSessions(r.Context(), "")
		if err != nil {
			writeSessionError(w, err)
			return
		}
		for i, session := range sessions {
			unlock, ok, err := activeSessions.coord.Lock(r.Context(), "lock:session:"+session.ID, 250*time.Second)
			if err != nil {
				writeSessionError(w, err)
				return
			}
			if ok {
				err = activeSessions.reconcile(r.Context(), session.ID)
				unlock()
				if err != nil {
					writeSessionError(w, err)
					return
				}
				updated, err := activeSessions.meta.GetSession(r.Context(), session.ID)
				if err != nil {
					writeSessionError(w, err)
					return
				}
				sessions[i] = updated
			}
		}
		writeJSON(w, 200, map[string]any{"sessions": sessions})
		return
	}
	sid = strings.TrimPrefix(sid, "/")
	if !validSessionID(sid) {
		writeJSON(w, 400, map[string]string{"error": "invalid session ID"})
		return
	}
	unlock, ok, err := activeSessions.coord.Lock(r.Context(), "lock:session:"+sid, 250*time.Second)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	if !ok {
		writeJSON(w, 425, map[string]string{"error": "session round in progress"})
		return
	}
	defer unlock()
	if err := activeSessions.reconcile(r.Context(), sid); err != nil {
		writeSessionError(w, err)
		return
	}
	session, err := activeSessions.meta.GetSession(r.Context(), sid)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	history, err := activeSessions.transcript.RawLatest(r.Context(), sid, 1)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	ready := len(history) == 0 || history[0]["tool_calls"] == nil
	writeJSON(w, 200, map[string]any{"session": session, "canResume": ready})
}
