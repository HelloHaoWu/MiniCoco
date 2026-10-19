package main

import (
    "embed"
    "encoding/json"
    "fmt"
)

// Prompt assembly: system prompt + tool schemas.
//
// Mirrors Coco's AgentLoopV2PromptGenerator.genPrompt / genFunctionCallToolsJson:
// the backend is the only place that knows the system prompt and tool list; the
// client never sees them. Resources are embedded into the binary so the server
// is self-contained.

//go:embed prompts/system.txt
var systemPrompt string

//go:embed tools/*.json
var toolsFS embed.FS

// Fixed order keeps the prompt (and any prompt cache) stable.
var toolFiles = []string{
    "read_file.json",
    "list_dir.json",
    "grep.json",
    "write_file.json",
    "edit_file.json",
    "run_command.json",
}

func loadTools() []json.RawMessage {
    tools := make([]json.RawMessage, 0, len(toolFiles))
    for _, name := range toolFiles {
        data, err := toolsFS.ReadFile("tools/" + name)
        if err != nil {
            panic(fmt.Sprintf("embedded tool missing: %s: %v", name, err))
        }
        tools = append(tools, json.RawMessage(data))
    }
    return tools
}

// genPrompt builds the full message list sent to the model: a system message
// (with the workspace injected) followed by the client's running conversation.
func genPrompt(clientMessages []map[string]any, workspace string) ([]map[string]any, []json.RawMessage) {
    sys := systemPrompt
    if workspace != "" {
        sys += fmt.Sprintf("\n\n# Environment\nThe workspace root is: %s\n", workspace)
    }
    messages := make([]map[string]any, 0, len(clientMessages)+1)
    messages = append(messages, map[string]any{"role": "system", "content": sys})
    messages = append(messages, clientMessages...)
    return messages, loadTools()
}

