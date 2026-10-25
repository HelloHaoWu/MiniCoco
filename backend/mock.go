package main

import (
	"strconv"
	"strings"
	"sync/atomic"
)

// Process-wide fault budget. Only mock completion attempts consume it; health
// probes and real providers never do. Initialized once before serving HTTP.
var flakyRemaining atomic.Int64

func configureMockFaults() error {
	flakyRemaining.Store(0)
	if strings.ToLower(getenv("LLM_PROVIDER", defaultProvider)) != "mock" {
		return nil
	}
	switch getenv("LLM_MOCK_MODE", "normal") {
	case "normal":
		return nil
	case "flaky":
		count, err := strconv.ParseInt(getenv("LLM_MOCK_FLAKY_FAILS", "2"), 10, 64)
		if err != nil || count < 0 {
			return completionFailure(codeConfig, false, "LLM_MOCK_FLAKY_FAILS must be a non-negative integer", nil)
		}
		flakyRemaining.Store(count)
		return nil
	default:
		return completionFailure(codeConfig, false, "unsupported LLM_MOCK_MODE; use normal or flaky", nil)
	}
}

func consumeMockFailure() bool {
	for {
		remaining := flakyRemaining.Load()
		if remaining <= 0 {
			return false
		}
		if flakyRemaining.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
}
