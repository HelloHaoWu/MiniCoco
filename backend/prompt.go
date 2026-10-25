package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Resources are embedded so the backend remains a self-contained binary.
//
//go:embed prompts/system.txt
var systemPrompt string

//go:embed tools/*.json
var toolsFS embed.FS

// The backend owns the system prompt, tool policy and persisted conversation.
func genPrompt(history []map[string]any, workspace string, mode Mode) ([]map[string]any, []json.RawMessage, error) {
	tools, err := buildTools(mode)
	if err != nil {
		return nil, nil, completionFailure(codeConfig, false, "cannot build tool catalog: "+err.Error(), err)
	}
	sys := systemPrompt
	if workspace != "" {
		sys += fmt.Sprintf("\n\n# Environment\nThe workspace root is: %s\n", workspace)
	}
	messages := make([]map[string]any, 0, len(history)+1)
	messages = append(messages, map[string]any{"role": "system", "content": sys})
	messages = append(messages, history...)
	if err := debugPrompt(messages); err != nil {
		return nil, nil, err
	}
	return messages, tools, nil
}

var promptDebugMu sync.Mutex

// Full conversation content is logged only with explicit local opt-in. The
// model API key and HTTP headers are not part of these assembled messages.
func debugPrompt(messages []map[string]any) error {
	if os.Getenv("MINICOCO_DEBUG_PROMPT") != "1" {
		return nil
	}
	var output bytes.Buffer
	output.WriteString("[MiniCoco prompt] ")
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(messages); err != nil {
		return fmt.Errorf("cannot encode debug prompt: %w", err)
	}
	promptDebugMu.Lock()
	defer promptDebugMu.Unlock()
	_, err := os.Stderr.Write(output.Bytes())
	return err
}
