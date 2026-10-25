package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

var activeSessions *sessionService

// HTTP + SSE entrypoint.
//
// Mirrors Coco's AutoworkChatController./chat/stream + CodeComposerJobExecutor:
// accept a streaming chat request, run one agent-loop round, and push each
// chunk back as Server-Sent Events.
//
// SECURITY: binds to 127.0.0.1 with NO authentication. Local developer harness
// only. The client it serves reads/writes files and runs shell commands on this
// machine. Do not expose this port to a network.

func main() {
	if err := loadLocalConfig(); err != nil {
		log.Fatal(err)
	}
	if err := configureMockFaults(); err != nil {
		log.Fatal(err)
	}
	if _, err := buildTools(ModeFull); err != nil {
		log.Fatal(err)
	}
	var err error
	var closeStores func()
	startup, stopStartup := context.WithTimeout(context.Background(), 15*time.Second)
	activeSessions, closeStores, err = wireStores(startup, getenv("MINICOCO_DATA_DIR", "data"))
	stopStartup()
	if err != nil {
		log.Fatal(err)
	}
	defer closeStores()
	host := getenv("MINICOCO_HOST", "127.0.0.1")
	port := getenv("MINICOCO_PORT", "8017")
	addr := host + ":" + port

	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/chat/stream", handleChatStream)
	http.HandleFunc("/sessions", handleSessions)
	http.HandleFunc("/sessions/", handleSessions)

	fmt.Printf("[MiniCoco backend] http://%s  (LLM_PROVIDER=%s, LLM_MODEL=%s)\n", addr, getenv("LLM_PROVIDER", defaultProvider), getenv("LLM_MODEL", defaultModel))
	fmt.Println("  POST /chat/stream   GET /health   GET /sessions   GET /sessions/<id>")
	server := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok", "provider": getenv("LLM_PROVIDER", defaultProvider),
	})
}

func handleChatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var req ChatRequest
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		var sizeError *http.MaxBytesError
		code := http.StatusBadRequest
		if errors.As(err, &sizeError) {
			code = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, code, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request must contain exactly one JSON object"})
		return
	}
	if err := validateDelta(req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if activeSessions == nil {
		writeJSON(w, 503, map[string]string{"error": "session service unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 125*time.Second)
	defer cancel()
	if err := activeSessions.resolve(ctx, &req); err != nil {
		writeSessionError(w, err)
		return
	}
	w.Header().Set("X-Session-Id", req.SessionID)
	unlock, ok, err := activeSessions.coord.Lock(ctx, "lock:session:"+req.SessionID, 250*time.Second)
	if err != nil {
		writeSessionError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusTooEarly, map[string]string{"error": "session round in progress"})
		return
	}
	defer unlock()
	if req.Stream != nil && !*req.Stream {
		writeBufferedCompletion(ctx, w, req)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	emitterID, err := storeID("e_")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "cannot allocate emitter"})
		return
	}
	h.Set("X-Emitter-Id", emitterID)
	w.WriteHeader(http.StatusOK)

	emit := func(ev Event) {
		data, err := json.Marshal(ev)
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			cancel()
			return
		}
		flusher.Flush()
	}
	unregister := activeEmitters.register(emitterID, emit)
	defer unregister()
	processRequestContext(ctx, req, func(event Event) { activeEmitters.emit(emitterID, event) })
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(payload)
}
