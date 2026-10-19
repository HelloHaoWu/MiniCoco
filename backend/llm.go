package main

import (
    "bufio"
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "sort"
    "strings"
    "time"
)

// LLM access layer + streaming tool-call parsing.
//
// Mirrors Coco's ModelFactory -> LLMBase -> FunctionCallStreamProcessor: pick
// a provider, stream the completion, and turn raw SSE deltas into the small set
// of Events in protocol.go. Two providers, chosen by env LLM_PROVIDER:
//   - "openai": any OpenAI-compatible /chat/completions endpoint (DeepSeek, ...)
//   - "mock":   deterministic, offline; runs the whole harness with no API key.

func streamCompletion(messages []map[string]any, tools []json.RawMessage, model string, emit func(Event)) error {
    switch strings.ToLower(getenv("LLM_PROVIDER", defaultProvider)) {
    case "openai":
        return openaiStream(messages, tools, model, emit)
    case "mock":
        return mockStream(messages, emit)
    default:
        return fmt.Errorf("unsupported LLM_PROVIDER; use openai or mock")
    }
}

// --- real provider: OpenAI-compatible streaming -----------------------------

type oaChunk struct {
    Choices []struct {
        Delta struct {
            Content          string `json:"content"`
            ReasoningContent string `json:"reasoning_content"`
            ToolCalls        []struct {
                Index    int    `json:"index"`
                ID       string `json:"id"`
                Function struct {
                    Name      string `json:"name"`
                    Arguments string `json:"arguments"`
                } `json:"function"`
            } `json:"tool_calls"`
        } `json:"delta"`
        FinishReason string `json:"finish_reason"`
    } `json:"choices"`
}

type toolAcc struct {
    id, name, args string
}

func openaiStream(messages []map[string]any, tools []json.RawMessage, model string, emit func(Event)) error {
    base := strings.TrimRight(getenv("LLM_BASE_URL", defaultBaseURL), "/")
    if model == "" {
        model = getenv("LLM_MODEL", defaultModel)
    }
    apiKey := os.Getenv("LLM_API_KEY")
    if apiKey == "" {
        return fmt.Errorf("LLM_API_KEY is missing; set it in the project .env or environment")
    }
    payload := map[string]any{
        "model": model, "messages": messages, "tools": tools,
        "stream": true, "temperature": 0,
    }
    // This small client does not replay reasoning_content between tool turns.
    // DeepSeek defaults to thinking mode; disable it for this text/tool harness.
    if strings.HasPrefix(model, "deepseek-") {
        payload["thinking"] = map[string]string{"type": "disabled"}
    }
    reqBody, err := json.Marshal(payload)
    if err != nil {
        return fmt.Errorf("encode LLM request: %w", err)
    }
    req, err := http.NewRequest("POST", base+"/chat/completions", bytes.NewReader(reqBody))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer "+apiKey)

    client := &http.Client{Timeout: 120 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 {
        buf := new(bytes.Buffer)
        buf.ReadFrom(io.LimitReader(resp.Body, 4096))
        detail := strings.ReplaceAll(strings.TrimSpace(buf.String()), apiKey, "[redacted]")
        return fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, detail)
    }

    calls := map[int]*toolAcc{}
    finish := FinishStop
    sc := bufio.NewScanner(resp.Body)
    sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
    for sc.Scan() {
        line := strings.TrimSpace(sc.Text())
        if !strings.HasPrefix(line, "data:") {
            continue
        }
        data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
        if data == "[DONE]" {
            break
        }
        var chunk oaChunk
        if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
            continue
        }
        ch := chunk.Choices[0]
        if ch.Delta.Content != "" {
            emit(Event{Type: EvText, Delta: ch.Delta.Content})
        }
        if ch.Delta.ReasoningContent != "" {
            emit(Event{Type: EvReasoning, Delta: ch.Delta.ReasoningContent})
        }
        for _, tc := range ch.Delta.ToolCalls {
            acc := calls[tc.Index]
            if acc == nil {
                acc = &toolAcc{}
                calls[tc.Index] = acc
            }
            if tc.ID != "" {
                acc.id = tc.ID
            }
            acc.name += tc.Function.Name
            acc.args += tc.Function.Arguments
        }
        if ch.FinishReason != "" {
            finish = ch.FinishReason
        }
    }
    if err := sc.Err(); err != nil {
        return err
    }

    idxs := make([]int, 0, len(calls))
    for i := range calls {
        idxs = append(idxs, i)
    }
    sort.Ints(idxs)
    for _, i := range idxs {
        c := calls[i]
        args := strings.TrimSpace(c.args)
        if args == "" || !json.Valid([]byte(args)) {
            args = "{}"
        }
        id := c.id
        if id == "" {
            id = fmt.Sprintf("call_%d", i)
        }
        emit(Event{Type: EvToolCall, ID: id, Name: c.name, Arguments: json.RawMessage(args)})
    }
    if len(calls) > 0 {
        finish = FinishToolCalls
    }
    emit(Event{Type: EvDone, FinishReason: finish})
    return nil
}

func getenv(k, def string) string {
    if v := os.Getenv(k); v != "" {
        return v
    }
    return def
}

// --- mock provider: deterministic, offline ---------------------------------

// mockStream scripts a tiny "model" so the loop runs with no API key. Turn 1
// (last message is the user query): narrate, then call list_dir on the
// workspace root. Turn 2 (last message is a tool result): give a final answer.
func mockStream(messages []map[string]any, emit func(Event)) error {
    last := map[string]any{}
    if len(messages) > 0 {
        last = messages[len(messages)-1]
    }
    if last["role"] == "tool" {
        for _, ch := range "Done. I inspected the workspace and listed its contents above." {
            emit(Event{Type: EvText, Delta: string(ch)})
        }
        emit(Event{Type: EvDone, FinishReason: FinishStop})
        return nil
    }
    for _, ch := range "I'll list the workspace to see what's here.\n" {
        emit(Event{Type: EvText, Delta: string(ch)})
    }
    emit(Event{Type: EvToolCall, ID: "call_mock_1", Name: "list_dir",
        Arguments: json.RawMessage(`{"target_directory":"."}`)})
    emit(Event{Type: EvDone, FinishReason: FinishToolCalls})
    return nil
}

