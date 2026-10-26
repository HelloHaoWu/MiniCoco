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
//go:embed prompts/*.txt
var promptsFS embed.FS

//go:embed tools/*.json
var toolsFS embed.FS

// The backend owns the system prompt, tool policy and persisted conversation.
func genPrompt(history []map[string]any, workspace string, mode Mode, route ModelRoute) ([]map[string]any, []json.RawMessage, error) {
	system, tools, err := promptParts(mode, route)
	if err != nil {
		return nil, nil, err
	}
	// Keep this system/tools pair independent of per-request data. Append the
	// environment and chronological history without rewriting their prefix.
	messages := make([]map[string]any, 0, len(history)+2)
	messages = append(messages, map[string]any{"role": "system", "content": system})
	if workspace != "" {
		messages = append(messages, map[string]any{"role": "user", "content": environmentText(workspace)})
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

func environmentText(workspace string) string {
	return fmt.Sprintf("# Environment\nThe workspace root is: %s\n", workspace)
}

func promptParts(mode Mode, route ModelRoute) (string, []json.RawMessage, error) {
	tools, err := buildTools(mode)
	if err != nil {
		return "", nil, completionFailure(codeConfig, false, "cannot build tool catalog: "+err.Error(), err)
	}
	prompt, err := promptsFS.ReadFile("prompts/system_" + route.Family + ".txt")
	if route.agentType != "" {
		prompt, err = promptsFS.ReadFile("prompts/subagent_" + route.agentType + ".txt")
	}
	if err != nil {
		return "", nil, completionFailure(codeConfig, false, "unsupported prompt family", nil)
	}
	prompt = appendScenePrompt(prompt, promptsFS, route.Family, route.scene)
	permissions, err := promptsFS.ReadFile("prompts/permissions.txt")
	if err != nil {
		return "", nil, err
	}
	return string(prompt) + "\n" + string(permissions), tools, nil
}
