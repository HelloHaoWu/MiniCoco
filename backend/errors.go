package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
)

const (
	codeUpstream5xx     = "upstream_5xx"
	codeRateLimited     = "rate_limited"
	codeQuotaExhausted  = "quota_exhausted"
	codeNeedCompression = "need_compression"
	codeBadRequest      = "bad_request"
	codeUpstreamTimeout = "upstream_timeout"
	codeConfig          = "config"
	codeInternal        = "internal"
)

// Classification travels with the failure rather than being guessed from its
// display message. The cause remains available for cancellation/timeout checks.
type completionError struct {
	code      string
	retryable bool
	message   string
	cause     error
}

func (e *completionError) Error() string { return e.message }
func (e *completionError) Unwrap() error { return e.cause }

func completionFailure(code string, retryable bool, message string, cause error) error {
	return &completionError{code: code, retryable: retryable, message: message, cause: cause}
}

func classifyUpstream(status int, body string) (code string, retryable bool) {
	switch {
	case status == 429 && hasQuotaExhaustedSignal(body):
		return codeQuotaExhausted, false
	case status == 429:
		return codeRateLimited, true
	case status == 400 && hasContextLengthSignal(body):
		// This requires compression before retrying, not a blind retransmission.
		return codeNeedCompression, true
	case status >= 500 && status <= 599, status == 408, status == 425:
		return codeUpstream5xx, true
	case status >= 400 && status <= 499:
		return codeBadRequest, false
	default:
		return codeInternal, false
	}
}

func hasQuotaExhaustedSignal(body string) bool {
	text := strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(body))
	// A bare "quota" can refer to a renewable request/token rate limit.
	for _, signal := range []string{
		"insufficient quota", "insufficient credit", "insufficient balance",
		"quota exhausted", "quota exceeded", "exhausted quota", "credit balance",
		"credits exhausted", "credits depleted", "out of credits", "credit exhausted",
		"billing hard limit", "balance not enough", "余额不足", "额度耗尽",
	} {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func hasContextLengthSignal(body string) bool {
	text := strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(body))
	for _, signal := range []string{"context length", "maximum context", "too many tokens", "context window exceeded"} {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func classifyCompletionError(err error) (code string, retryable bool) {
	if errors.Is(err, context.Canceled) {
		return codeInternal, false
	}
	var classified *completionError
	if errors.As(err, &classified) {
		return classified.code, classified.retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return codeUpstreamTimeout, true
	}
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return codeUpstreamTimeout, true
		}
		return codeUpstream5xx, true
	}
	var request *url.Error
	if errors.As(err, &request) {
		return codeUpstream5xx, true
	}
	// Protocol/limit failures and unknown internal errors are fatal by default.
	return codeInternal, false
}

func failureEvent(err error) Event {
	code, retryable := classifyCompletionError(err)
	return Event{Type: EvError, Message: err.Error(), Code: code, Retryable: &retryable}
}
