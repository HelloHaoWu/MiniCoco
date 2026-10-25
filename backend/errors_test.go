package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyUpstream(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body, code string
		retryable  bool
	}{
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded"}}`, codeRateLimited, true},
		{"renewable quota", 429, "request quota per minute exceeded; retry later", codeRateLimited, true},
		{"quota code", 429, `{"error":{"code":"insufficient_quota"}}`, codeQuotaExhausted, false},
		{"quota exhausted", 429, "QUOTA EXHAUSTED", codeQuotaExhausted, false},
		{"credits", 429, "Insufficient credits", codeQuotaExhausted, false},
		{"credit balance", 429, "Your credit balance is too low", codeQuotaExhausted, false},
		{"balance", 429, "账户余额不足", codeQuotaExhausted, false},
		{"billing", 429, "billing_hard_limit_reached", codeQuotaExhausted, false},
		{"server error", 500, "temporary", codeUpstream5xx, true},
		{"gateway", 502, "temporary", codeUpstream5xx, true},
		{"unavailable", 503, "temporary", codeUpstream5xx, true},
		{"gateway timeout", 504, "temporary", codeUpstream5xx, true},
		{"request timeout", 408, "timeout", codeUpstream5xx, true},
		{"too early", 425, "retry", codeUpstream5xx, true},
		{"context code", 400, `{"error":{"code":"context_length_exceeded"}}`, codeNeedCompression, true},
		{"maximum context", 400, "Maximum context exceeded", codeNeedCompression, true},
		{"token limit", 400, "too many tokens", codeNeedCompression, true},
		{"bad request", 400, "invalid messages", codeBadRequest, false},
		{"unauthorized", 401, "bad key", codeBadRequest, false},
		{"forbidden", 403, "forbidden", codeBadRequest, false},
		{"missing model", 404, "unknown model", codeBadRequest, false},
		{"too large", 413, "large", codeBadRequest, false},
		{"unprocessable", 422, "invalid", codeBadRequest, false},
		{"unknown status", 200, "", codeInternal, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, retryable := classifyUpstream(test.status, test.body)
			if code != test.code || retryable != test.retryable {
				t.Fatalf("got %s/%t, want %s/%t", code, retryable, test.code, test.retryable)
			}
		})
	}
}

func TestFailureEventWireCompatibility(t *testing.T) {
	for _, retryable := range []bool{false, true} {
		event := failureEvent(completionFailure(codeInternal, retryable, "test failure", nil))
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		value, present := wire["retryable"]
		if !present || value != retryable || wire["code"] != codeInternal || wire["type"] != EvError {
			t.Fatalf("missing or incorrect classification fields: %s", data)
		}
	}
	for _, event := range []Event{
		{Type: EvText, Delta: "text"}, {Type: EvReasoning, Delta: "thinking"},
		{Type: EvToolCall, ID: "id", Name: "list_dir", Arguments: json.RawMessage(`{}`)},
		{Type: EvDone, FinishReason: FinishStop},
	} {
		data, _ := json.Marshal(event)
		if strings.Contains(string(data), "retryable") || strings.Contains(string(data), `"code"`) {
			t.Fatalf("non-error event wire format changed: %s", data)
		}
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestCompletionFailurePaths(t *testing.T) {
	for _, test := range []struct {
		name                      string
		status                    int
		body, contentType         string
		transportError, readError error
		code                      string
		retryable                 bool
	}{
		{name: "HTTP rate limit", status: 429, body: "rate limit", code: codeRateLimited, retryable: true},
		{name: "HTTP quota", status: 429, body: "insufficient_quota fake-key", code: codeQuotaExhausted},
		{name: "HTTP context", status: 400, body: "maximum context", code: codeNeedCompression, retryable: true},
		{name: "HTTP auth", status: 401, body: "invalid fake-key", code: codeBadRequest},
		{name: "HTTP upstream", status: 503, code: codeUpstream5xx, retryable: true},
		{name: "network", transportError: &net.DNSError{Err: "temporary DNS failure", Name: "test.invalid", IsTemporary: true}, code: codeUpstream5xx, retryable: true},
		{name: "connect timeout", transportError: context.DeadlineExceeded, code: codeUpstreamTimeout, retryable: true},
		{name: "cancelled request", transportError: context.Canceled, code: codeInternal},
		{name: "transport redaction", transportError: errors.New("failed fake-key"), code: codeUpstream5xx, retryable: true},
		{name: "truncated clean EOF", status: 200, contentType: "text/event-stream", body: `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n", code: codeUpstream5xx, retryable: true},
		{name: "read reset", status: 200, contentType: "text/event-stream", readError: io.ErrUnexpectedEOF, code: codeUpstream5xx, retryable: true},
		{name: "read timeout", status: 200, contentType: "text/event-stream", readError: context.DeadlineExceeded, code: codeUpstreamTimeout, retryable: true},
		{name: "read cancelled", status: 200, contentType: "text/event-stream", readError: context.Canceled, code: codeInternal},
		{name: "malformed SSE", status: 200, contentType: "text/event-stream", body: "data: not-json\n\n", code: codeInternal},
		{name: "wrong content type", status: 200, contentType: "application/json", body: "{}", code: codeInternal},
		{name: "oversize SSE", status: 200, contentType: "text/event-stream", body: "data: " + strings.Repeat("x", 1024*1024+1), code: codeInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstreamFixture(t, "", "")
			http.DefaultTransport = regressionTransport(func(r *http.Request) (*http.Response, error) {
				if test.transportError != nil {
					return nil, test.transportError
				}
				var body io.Reader = strings.NewReader(test.body)
				if test.readError != nil {
					body = io.MultiReader(body, failingReader{test.readError})
				}
				return &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": {test.contentType}}, Body: io.NopCloser(body), Request: r}, nil
			})
			var events []Event
			processModelContext(context.Background(), nil, "", "", func(event Event) { events = append(events, event) })
			if len(events) < 2 {
				t.Fatalf("missing error/done pair: %+v", events)
			}
			failure := events[len(events)-2]
			if failure.Type != EvError || failure.Code != test.code || failure.Retryable == nil || *failure.Retryable != test.retryable || strings.Contains(failure.Message, "fake-key") {
				t.Fatalf("incorrect failure: %+v", failure)
			}
			if events[len(events)-1].FinishReason != "error" {
				t.Fatal("failure did not terminate as error")
			}
			for _, event := range events {
				if event.Type == EvToolCall {
					t.Fatal("failed completion emitted an executable tool")
				}
			}
		})
	}
}

func TestConfigurationAndCancellationClassification(t *testing.T) {
	for _, test := range []struct{ name, provider, key, base string }{
		{"provider", "unknown", "fake-key", "http://unused.invalid"},
		{"missing key", "openai", "", "http://unused.invalid"},
		{"invalid endpoint", "openai", "fake-key", ":bad-url"},
		{"unsupported endpoint", "openai", "fake-key", "ftp://unused.invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LLM_PROVIDER", test.provider)
			t.Setenv("LLM_API_KEY", test.key)
			t.Setenv("LLM_BASE_URL", test.base)
			err := streamCompletion(nil, nil, "model", func(Event) { t.Fatal("configuration failure emitted content") })
			if err == nil {
				t.Fatal("bad configuration accepted")
			}
			code, retryable := classifyCompletionError(err)
			if code != codeConfig || retryable {
				t.Fatalf("wrong config classification: %s/%t", code, retryable)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := streamCompletionContext(ctx, nil, nil, "model", func(Event) { t.Fatal("cancelled request emitted content") })
	code, retryable := classifyCompletionError(fmt.Errorf("wrapped: %w", err))
	if code != codeInternal || retryable {
		t.Fatal("user cancellation marked retryable")
	}
	code, retryable = classifyCompletionError(errors.New("unknown failure"))
	if code != codeInternal || retryable {
		t.Fatal("unknown error must be fatal")
	}
}
