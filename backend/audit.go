package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
)

type AuditRecord struct {
	Ts         int64  `json:"ts"`
	SessionID  string `json:"sessionId"`
	TraceID    string `json:"traceId"`
	Tool       string `json:"tool"`
	ArgsDigest string `json:"argsDigest"`
	Approved   bool   `json:"approved"`
	Status     string `json:"status"`
	Ms         int64  `json:"ms"`
}

var auditDigest = regexp.MustCompile(`^[a-f0-9]{12}$`)
var auditName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var auditTrace = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

// Local client reports are diagnostic, not attestations or authorization.
func handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	var record AuditRecord
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || !validSessionID(record.SessionID) || !auditDigest.MatchString(record.ArgsDigest) || !auditName.MatchString(record.Tool) || record.Ts < 0 || record.Ms < 0 || (record.TraceID != "" && !auditTrace.MatchString(record.TraceID)) || (record.Status != "ok" && record.Status != "error" && record.Status != "denied") {
		writeSessionError(w, ErrStoreInvalid)
		return
	}
	if activeSessions == nil {
		writeSessionError(w, ErrStoreNotFound)
		return
	}
	if _, err := activeSessions.meta.GetSession(r.Context(), record.SessionID); err != nil {
		writeSessionError(w, err)
		return
	}
	activeSessions.sink.emit("audit.jsonl", record)
	writeJSON(w, 202, map[string]bool{"accepted": true})
}
