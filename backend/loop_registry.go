package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// All implementations execute one durable round. They must preserve the same
// cancellation, idempotency and commit-before-tool-exposure contract as v1.
// Registration is a backend extension point, never a client-controlled field.
type LoopImpl func(context.Context, ChatRequest, func(Event)) error

var loopRegistryMu sync.RWMutex
var loopRegistry = map[string]LoopImpl{"v1": processRequestV1}

// Register before serving traffic. Locks also make lookup/registration safe
// when backend extensions are initialized concurrently. Existing keys are final.
func registerLoop(key string, impl LoopImpl) error {
	if key == "" || len(key) > 64 || strings.TrimSpace(key) != key || impl == nil {
		return fmt.Errorf("loop registration requires a nonempty key (up to 64 bytes) and implementation")
	}
	loopRegistryMu.Lock()
	defer loopRegistryMu.Unlock()
	if _, exists := loopRegistry[key]; exists {
		return fmt.Errorf("loop implementation already registered")
	}
	loopRegistry[key] = impl
	return nil
}

func selectedLoop() (string, LoopImpl) {
	key := getenv("LOOP_IMPL", "v1")
	loopRegistryMu.RLock()
	defer loopRegistryMu.RUnlock()
	if impl, ok := loopRegistry[key]; ok {
		return key, impl
	}
	// Unknown keys deliberately retain the baseline behavior, as in the design.
	return "v1", loopRegistry["v1"]
}

func pickLoop() LoopImpl {
	_, impl := selectedLoop()
	return impl
}

func processRequestV1(ctx context.Context, req ChatRequest, emit func(Event)) error {
	return activeSessions.process(ctx, req, emit)
}
