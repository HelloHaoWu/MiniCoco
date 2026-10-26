package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The timeline is diagnostic and bounded independently of the transcript.
func readTrace(dir, sid string) ([]TraceRecord, error) {
	if !validSessionID(sid) {
		return nil, ErrStoreInvalid
	}
	f, err := os.Open(filepath.Join(dir, "trace-"+sid+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return []TraceRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 8*1024*1024 {
		return nil, ErrStoreInvalid
	}
	rows := []TraceRecord{}
	scan := bufio.NewScanner(io.LimitReader(f, 8*1024*1024))
	scan.Buffer(make([]byte, 4096), 16384)
	for scan.Scan() {
		var r TraceRecord
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Ts < rows[j].Ts })
	return rows, scan.Err()
}

func handleTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "GET only"})
		return
	}
	if activeSessions == nil {
		writeSessionError(w, ErrStoreNotFound)
		return
	}
	sid := strings.TrimPrefix(r.URL.Path, "/trace/")
	if !validSessionID(sid) {
		writeSessionError(w, ErrStoreInvalid)
		return
	}
	if _, err := activeSessions.meta.GetSession(r.Context(), sid); err != nil {
		writeSessionError(w, err)
		return
	}
	rows, err := readTrace(activeSessions.traceDir, sid)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	if value := r.URL.Query().Get("round"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			writeSessionError(w, ErrStoreInvalid)
			return
		}
		filtered := []TraceRecord{}
		for _, row := range rows {
			if row.Round == n {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	writeJSON(w, 200, map[string]any{"records": rows})
}

func (s *sessionService) recentStats(r *http.Request) []map[string]any {
	result := []map[string]any{}
	if s == nil {
		return result
	}
	sessions, err := s.meta.ListSessions(r.Context(), "")
	if err != nil {
		return result
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CreatedAt > sessions[j].CreatedAt })
	if len(sessions) > 10 {
		sessions = sessions[:10]
	}
	for _, session := range sessions {
		rounds, err := s.transcript.CommittedRounds(r.Context(), session.ID)
		if err != nil {
			continue
		}
		rows, _ := readTrace(s.traceDir, session.ID)
		failures := 0
		for _, row := range rows {
			if row.Stage == "error" {
				failures++
			}
		}
		result = append(result, map[string]any{"sessionId": session.ID, "rounds": len(rounds), "totalTokens": session.TotalTokens, "errors": failures})
	}
	return result
}
