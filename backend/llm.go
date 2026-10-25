package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	return streamCompletionContext(context.Background(), messages, tools, model, emit)
}

func streamCompletionContext(ctx context.Context, messages []map[string]any, tools []json.RawMessage, model string, emit func(Event)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch strings.ToLower(getenv("LLM_PROVIDER", defaultProvider)) {
	case "openai":
		return openaiStreamContext(ctx, messages, tools, model, emit)
	case "mock":
		err := mockStream(messages, emit)
		if err == nil {
			recordUsage(ctx, Usage{1, 1, 2})
		} // Synthetic test usage only.
		return err
	default:
		return completionFailure(codeConfig, false, "unsupported LLM_PROVIDER; use openai or mock", nil)
	}
}

// --- real provider: OpenAI-compatible streaming -----------------------------

type oaChunk struct {
	Usage *struct {
		Prompt     int64 `json:"prompt_tokens"`
		Completion int64 `json:"completion_tokens"`
		Total      int64 `json:"total_tokens"`
	} `json:"usage"`
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
	return openaiStreamContext(context.Background(), messages, tools, model, emit)
}

func openaiStreamContext(ctx context.Context, messages []map[string]any, tools []json.RawMessage, model string, emit func(Event)) error {
	base := strings.TrimRight(getenv("LLM_BASE_URL", defaultBaseURL), "/")
	if model == "" {
		model = getenv("LLM_MODEL", defaultModel)
	}
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		return completionFailure(codeConfig, false, "LLM_API_KEY is missing; set it in the project .env or environment", nil)
	}
	endpoint, err := url.Parse(base)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return completionFailure(codeConfig, false, "LLM_BASE_URL must be an HTTP(S) base URL without credentials, query or fragment", nil)
	}
	payload := map[string]any{
		"model": model, "messages": messages, "tools": tools,
		"stream": true, "temperature": 0,
		"stream_options": map[string]bool{"include_usage": true},
	}
	// This small client does not replay reasoning_content between tool turns.
	// DeepSeek defaults to thinking mode; disable it for this text/tool harness.
	if strings.HasPrefix(model, "deepseek-") {
		payload["thinking"] = map[string]string{"type": "disabled"}
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return completionFailure(codeBadRequest, false, "encode LLM request: "+err.Error(), err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return completionFailure(codeConfig, false, "invalid LLM request configuration", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		code, retryable := classifyCompletionError(err)
		return completionFailure(code, retryable, "LLM request failed: "+strings.ReplaceAll(err.Error(), apiKey, "[redacted]"), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		buf.ReadFrom(io.LimitReader(resp.Body, 4096))
		detail := strings.ReplaceAll(strings.TrimSpace(buf.String()), apiKey, "[redacted]")
		code, retryable := classifyUpstream(resp.StatusCode, buf.String())
		return completionFailure(code, retryable, fmt.Sprintf("LLM HTTP %d: %s", resp.StatusCode, detail), nil)
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return fmt.Errorf("LLM did not return an SSE stream")
	}

	calls := map[int]*toolAcc{}
	finish := ""
	usage := Usage{}
	done := false
	limited := &io.LimitedReader{R: resp.Body, N: 4*1024*1024 + 1}
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataLines []string
	frameBytes := 0
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		frameBytes += len(line) + 1
		if frameBytes > 1024*1024 {
			return fmt.Errorf("LLM SSE frame exceeds 1 MiB")
		}
		if line != "" {
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
			continue
		}
		frameBytes = 0
		if len(dataLines) == 0 {
			continue
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if data == "[DONE]" {
			done = true
			break
		}
		var usageChunk oaChunk
		if json.Unmarshal([]byte(data), &usageChunk) != nil {
			return fmt.Errorf("invalid LLM SSE JSON")
		}
		if usageChunk.Usage != nil {
			usage = Usage{usageChunk.Usage.Prompt, usageChunk.Usage.Completion, usageChunk.Usage.Total}
			if !validUsage(usage) {
				return fmt.Errorf("invalid LLM usage")
			}
		}
		if finish != "" {
			// Usage-only chunks may follow the terminal choice.
			var usage struct {
				Choices []json.RawMessage `json:"choices"`
				Error   json.RawMessage   `json:"error"`
			}
			if json.Unmarshal([]byte(data), &usage) != nil || len(usage.Choices) != 0 || len(usage.Error) != 0 {
				return fmt.Errorf("unexpected data after LLM finish")
			}
			continue
		}
		var chunk oaChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return fmt.Errorf("invalid LLM SSE JSON")
		}
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(data), &envelope) != nil || len(envelope.Error) != 0 {
			return fmt.Errorf("LLM returned an error event")
		}
		if len(chunk.Choices) == 0 {
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
			if tc.Index < 0 || tc.Index >= 16 {
				return fmt.Errorf("invalid tool call index")
			}
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
			if len(acc.args) > 1024*1024 || len(acc.name) > 128 || len(acc.id) > 256 {
				return fmt.Errorf("tool call exceeds limits")
			}
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("LLM SSE frame exceeds 1 MiB")
		}
		code, retryable := classifyCompletionError(err)
		if code == codeInternal && !errors.Is(err, context.Canceled) {
			code, retryable = codeUpstream5xx, true
		}
		return completionFailure(code, retryable, "read LLM stream: "+strings.ReplaceAll(err.Error(), apiKey, "[redacted]"), err)
	}
	if limited.N <= 0 {
		return fmt.Errorf("LLM response exceeds 4 MiB")
	}
	if !done || finish == "" {
		return completionFailure(codeUpstream5xx, true, "incomplete LLM stream", nil)
	}
	if finish != FinishStop && finish != FinishToolCalls {
		return fmt.Errorf("LLM stopped unsuccessfully: %s", finish)
	}
	if (finish == FinishToolCalls) != (len(calls) > 0) {
		return fmt.Errorf("LLM finish reason does not match tool calls")
	}

	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	ids := map[string]bool{}
	// Validate the entire batch before emitting any executable tool calls.
	for _, i := range idxs {
		c := calls[i]
		args := strings.TrimSpace(c.args)
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(args), &object) != nil || object == nil {
			return fmt.Errorf("invalid JSON object arguments for tool %d", i)
		}
		if c.id == "" || c.name == "" || ids[c.id] {
			return fmt.Errorf("missing or duplicate tool identity")
		}
		ids[c.id] = true
	}
	for _, i := range idxs {
		c := calls[i]
		emit(Event{Type: EvToolCall, ID: c.id, Name: c.name, Arguments: json.RawMessage(c.args)})
	}
	recordUsage(ctx, usage)
	emit(Event{Type: EvDone, FinishReason: finish})
	return nil
}

type usageContextKey struct{}

func recordUsage(ctx context.Context, usage Usage) {
	if target, ok := ctx.Value(usageContextKey{}).(*Usage); ok {
		*target = usage
	}
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
	if consumeMockFailure() {
		return completionFailure(codeUpstream5xx, true, "mock transient failure (flaky)", nil)
	}
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
