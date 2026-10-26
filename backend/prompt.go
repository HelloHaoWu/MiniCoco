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
//go:embed prompts/system_openai.txt prompts/system_claude.txt
var promptsFS embed.FS

//go:embed tools/*.json
var toolsFS embed.FS

// The backend owns the system prompt, tool policy and persisted conversation.
func genPrompt(history []map[string]any, workspace string, mode Mode, route ModelRoute) ([]map[string]any, []json.RawMessage, error) {
	tools, err := buildTools(mode)
	if err != nil {
		return nil, nil, completionFailure(codeConfig, false, "cannot build tool catalog: "+err.Error(), err)
	}
	prompt, err := promptsFS.ReadFile("prompts/system_" + route.Family + ".txt")
	if err != nil {
		return nil, nil, completionFailure(codeConfig, false, "unsupported prompt family", nil)
	}
	messages := make([]map[string]any, 0, len(history)+2)
	messages = append(messages, map[string]any{"role": "system", "content": string(prompt)})
	if workspace != "" {
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("# Environment\nThe workspace root is: %s\n", workspace)})
	}
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
