package main

import (
	"encoding/json"
	"sync"
)

// A live transport registry, never a transcript source or tool execution queue.
// Only trusted backend publishers may send already-authorized events to inbox.
type eventEmitter struct {
	mu     sync.Mutex
	closed bool
	send   func(Event)
}
type emitterHub struct {
	mu       sync.Mutex
	emitters map[string]*eventEmitter
}

var activeEmitters = newEmitterHub()

func newEmitterHub() *emitterHub { return &emitterHub{emitters: map[string]*eventEmitter{}} }
func (hub *emitterHub) register(id string, send func(Event)) func() {
	entry := &eventEmitter{send: send}
	hub.mu.Lock()
	hub.emitters[id] = entry
	hub.mu.Unlock()
	return func() {
		entry.mu.Lock()
		defer entry.mu.Unlock()
		entry.closed = true
		hub.mu.Lock()
		if hub.emitters[id] == entry {
			delete(hub.emitters, id)
		}
		hub.mu.Unlock()
	}
}
func (hub *emitterHub) emit(id string, event Event) bool {
	hub.mu.Lock()
	entry := hub.emitters[id]
	hub.mu.Unlock()
	if entry == nil {
		return false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed {
		return false
	}
	entry.send(event)
	return true
}
func validInboxChunk(chunk []byte) bool {
	if len(chunk) == 0 || len(chunk) > 1024*1024 {
		return false
	}
	var event Event
	if json.Unmarshal(chunk, &event) != nil {
		return false
	}
	switch event.Type {
	case EvText, EvReasoning, EvToolCall, EvDone, EvError:
		return true
	default:
		return false
	}
}
func (hub *emitterHub) dispatch(id string, chunk []byte) {
	if !validInboxChunk(chunk) {
		return
	}
	var event Event
	if json.Unmarshal(chunk, &event) == nil {
		hub.emit(id, event)
	}
}
