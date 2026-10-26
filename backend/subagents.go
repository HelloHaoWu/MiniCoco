package main

import (
	"encoding/json"
	"strings"
)

var subagents = []string{"explore"}

func injectSubagents(schema json.RawMessage) (json.RawMessage, error) {
	var tool map[string]any
	if err := json.Unmarshal(schema, &tool); err != nil {
		return nil, err
	}
	function, ok := tool["function"].(map[string]any)
	if !ok {
		return nil, ErrStoreInvalid
	}
	description, ok := function["description"].(string)
	if !ok {
		return nil, ErrStoreInvalid
	}
	function["description"] = strings.ReplaceAll(description, "${available_sub_agents}", "- explore: read-only investigation; returns a findings report with file:line. Tools: read_file/list_dir/grep.")
	params, ok := function["parameters"].(map[string]any)
	if !ok {
		return nil, ErrStoreInvalid
	}
	properties, ok := params["properties"].(map[string]any)
	if !ok {
		return nil, ErrStoreInvalid
	}
	agent, ok := properties["agent_type"].(map[string]any)
	if !ok {
		return nil, ErrStoreInvalid
	}
	agent["enum"] = subagents
	return json.Marshal(tool)
}
