package main

import (
	"encoding/json"
	"regexp"
)

type MCPToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	ReadOnly    bool           `json:"readOnly"`
}

var mcpNamePattern = regexp.MustCompile(`^mcp__[A-Za-z0-9_-]{1,24}__[A-Za-z0-9_-]{1,24}$`)

func validMCPName(name string) bool { return len(name) <= 64 && mcpNamePattern.MatchString(name) }
func validateMCPTools(tools []MCPToolSpec) error {
	if len(tools) > 64 {
		return ErrStoreInvalid
	}
	data, err := json.Marshal(tools)
	if err != nil || len(data) > 65536 {
		return ErrStoreInvalid
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if !validMCPName(tool.Name) || seen[tool.Name] || tool.Parameters["type"] != "object" {
			return ErrStoreInvalid
		}
		seen[tool.Name] = true
	}
	return nil
}
func mcpAllowed(mode Mode, name string, tools []MCPToolSpec) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return mode == ModeFull || tool.ReadOnly
		}
	}
	return false
}
func appendMCPTools(out []json.RawMessage, mode Mode, tools []MCPToolSpec) ([]json.RawMessage, error) {
	if err := validateMCPTools(tools); err != nil {
		return nil, err
	}
	for _, tool := range tools {
		if mcpAllowed(mode, tool.Name, tools) {
			schema, err := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters}})
			if err != nil {
				return nil, err
			}
			out = append(out, schema)
		}
	}
	return out, nil
}
