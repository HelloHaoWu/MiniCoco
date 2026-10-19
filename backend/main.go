package main

import (
    "encoding/json"
    "fmt"
    "log"
    "net/http"
)

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
    host := getenv("MINICOCO_HOST", "127.0.0.1")
    port := getenv("MINICOCO_PORT", "8017")
    addr := host + ":" + port

    http.HandleFunc("/health", handleHealth)
    http.HandleFunc("/chat/stream", handleChatStream)

    fmt.Printf("[MiniCoco backend] http://%s  (LLM_PROVIDER=%s, LLM_MODEL=%s)\n", addr, getenv("LLM_PROVIDER", defaultProvider), getenv("LLM_MODEL", defaultModel))
    fmt.Println("  POST /chat/stream   GET /health")
    if err := http.ListenAndServe(addr, nil); err != nil {
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
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
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
    w.WriteHeader(http.StatusOK)

    emit := func(ev Event) {
        data, err := json.Marshal(ev)
        if err != nil {
            return
        }
        fmt.Fprintf(w, "data: %s\n\n", data)
        flusher.Flush()
    }
    processRequest(req, emit)
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(code)
    json.NewEncoder(w).Encode(payload)
}

