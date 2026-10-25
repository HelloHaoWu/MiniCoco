package main

import (
	"embed"
	"encoding/json"
	"fmt"
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
	return messages, tools, nil
}
