package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

type ToolName string

const (
	ReadFile   ToolName = "read_file"
	ListDir    ToolName = "list_dir"
	Grep       ToolName = "grep"
	WriteFile  ToolName = "write_file"
	EditFile   ToolName = "edit_file"
	RunCommand ToolName = "run_command"
)

// Explicit declaration order, independent of directory or map iteration order.
var toolOrder = [...]ToolName{ReadFile, ListDir, Grep, WriteFile, EditFile, RunCommand}
var writesOrExecutes = map[ToolName]bool{WriteFile: true, EditFile: true, RunCommand: true}

// availableVersion is the minimum harness version, not the schema file version.
// The current harness advertises v1; client-controlled version selection is reserved.
const toolAgentVersion = 1

type Mode string

const (
	ModeFull     Mode = "full"
	ModeReadonly Mode = "readonly"
	ModeSafe     Mode = "safe"
)

func normalizeMode(mode Mode) (Mode, error) {
	switch mode {
	case "", ModeFull:
		return ModeFull, nil
	case ModeReadonly, ModeSafe:
		return mode, nil
	default:
		return "", fmt.Errorf("mode must be full, readonly or safe")
	}
}

func knownTool(name ToolName) bool {
	for _, candidate := range toolOrder {
		if name == candidate {
			return true
		}
	}
	return false
}

func toolAllowed(mode Mode, name ToolName) bool {
	mode, err := normalizeMode(mode)
	return err == nil && knownTool(name) && !(mode == ModeReadonly && writesOrExecutes[name]) && !(mode == ModeSafe && name == RunCommand)
}

type toolSchema struct {
	Type             string          `json:"type"`
	AvailableVersion int             `json:"availableVersion"`
	Function         json.RawMessage `json:"function"`
}

// Metadata stays in embedded assets. Only provider-compatible type/function
// fields are sent upstream; availableVersion is not part of the OpenAI schema.
func loadToolsByName(resources fs.FS, agentVersion int) (map[ToolName]json.RawMessage, error) {
	if agentVersion < 1 {
		return nil, fmt.Errorf("agent version must be positive")
	}
	entries, err := fs.ReadDir(resources, "tools")
	if err != nil {
		return nil, err
	}
	versions := map[ToolName]int{}
	selected := map[ToolName]json.RawMessage{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name, suffix, found := strings.Cut(strings.TrimSuffix(entry.Name(), ".json"), "__v")
		version, err := strconv.Atoi(suffix)
		if !found || err != nil || version < 1 || strconv.Itoa(version) != suffix || !knownTool(ToolName(name)) {
			return nil, fmt.Errorf("invalid versioned tool filename: %s", entry.Name())
		}
		data, err := fs.ReadFile(resources, "tools/"+entry.Name())
		if err != nil {
			return nil, err
		}
		var schema toolSchema
		var function struct {
			Name       string `json:"name"`
			Parameters struct {
				Type string `json:"type"`
			} `json:"parameters"`
		}
		if json.Unmarshal(data, &schema) != nil || schema.Type != "function" || schema.AvailableVersion < 1 ||
			json.Unmarshal(schema.Function, &function) != nil || function.Name != name || function.Parameters.Type != "object" {
			return nil, fmt.Errorf("invalid tool schema: %s", entry.Name())
		}
		key := ToolName(name)
		if schema.AvailableVersion > agentVersion || version <= versions[key] {
			continue
		}
		provider, err := json.Marshal(struct {
			Type     string          `json:"type"`
			Function json.RawMessage `json:"function"`
		}{schema.Type, schema.Function})
		if err != nil {
			return nil, err
		}
		versions[key], selected[key] = version, provider
	}
	return selected, nil
}

func buildTools(mode Mode) ([]json.RawMessage, error) {
	mode, err := normalizeMode(mode)
	if err != nil {
		return nil, err
	}
	catalog, err := loadToolsByName(toolsFS, toolAgentVersion)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(toolOrder))
	for _, name := range toolOrder {
		schema, ok := catalog[name]
		if !ok {
			return nil, fmt.Errorf("no compatible schema for tool %s", name)
		}
		if toolAllowed(mode, name) {
			out = append(out, schema)
		}
	}
	return out, nil
}

func validateToolMode(mode Mode, events []Event) error {
	for _, event := range events {
		if event.Type == EvToolCall && !toolAllowed(mode, ToolName(event.Name)) {
			return completionFailure(codeBadRequest, false, fmt.Sprintf("tool %s is not allowed in mode %s", event.Name, mode), nil)
		}
	}
	return nil
}
