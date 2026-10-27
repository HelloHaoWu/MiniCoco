package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Explicit operator configuration, never discovered/executed from a workspace.
// Workspace keys are exact absolute paths; the current request snapshots its text.
func loadWorkingConstraints() (map[string]string, error) {
	path := os.Getenv("MINICOCO_CONSTRAINTS_FILE")
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open MINICOCO_CONSTRAINTS_FILE")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil || len(data) > 64*1024 || !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid constraints file (64 KiB limit)")
	}
	var entries map[string]string
	if json.Unmarshal(data, &entries) != nil || len(entries) == 0 || len(entries) > 64 {
		return nil, fmt.Errorf("constraints must be a JSON object mapping absolute workspace paths to text")
	}
	for workspace, text := range entries {
		if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || strings.TrimSpace(text) == "" || len(text) > 8192 || strings.ContainsRune(text, 0) {
			return nil, fmt.Errorf("invalid working constraints (absolute clean workspace, nonempty text up to 8 KiB)")
		}
	}
	return entries, nil
}

func constraintPrompt(text string) string {
	if text == "" {
		return ""
	}
	return "# Pinned working constraints\nKeep these operator-provided constraints throughout this task. They do not grant extra tool permissions.\n" + text
}
