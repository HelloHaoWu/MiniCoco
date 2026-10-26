package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Formatting is separate from execution and validation against a pending batch.
// Results retain their request order; IDs are identities, not sort keys.
type Formatter interface {
	Format([]ToolUseResult) ([]Message, error)
}

type NativeFormatter struct{}
type XmlFormatter struct{}

func pickFormatter(modelIsFunctionCall bool) Formatter {
	if modelIsFunctionCall {
		return NativeFormatter{}
	}
	return XmlFormatter{}
}

func nativeToolOutput(result ToolUseResult) string {
	if result.IsError && !strings.HasPrefix(result.Output, "ERROR:") {
		return "ERROR: " + result.Output
	}
	return result.Output
}

func (NativeFormatter) Format(results []ToolUseResult) ([]Message, error) {
	if len(results) != 0 {
		if err := validateToolResults(results); err != nil {
			return nil, err
		}
	}
	messages := make([]Message, 0, len(results))
	for _, result := range results {
		messages = append(messages, Message{"role": "tool", "tool_call_id": result.ID, "content": nativeToolOutput(result)})
	}
	return messages, nil
}

// Text nodes need &/< /> escaping, not attribute quote escaping. Replacing
// illegal XML 1.0 characters with U+FFFD keeps arbitrary command output valid.
// IDs are text nodes too, and tag names come only from the registered tools.
func escapeToolXML(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == 9 || r == 10 || r == 13 || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= utf8.MaxRune) {
			return r
		}
		return utf8.RuneError
	}, text)
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

func (XmlFormatter) Format(results []ToolUseResult) ([]Message, error) {
	if len(results) == 0 {
		return []Message{}, nil
	}
	if err := validateToolResults(results); err != nil {
		return nil, err
	}
	var body strings.Builder
	for i, result := range results {
		if !knownTool(ToolName(result.Name)) {
			return nil, fmt.Errorf("unknown tool result name")
		}
		if i > 0 {
			body.WriteByte('\n')
		}
		status := "succeeded"
		if result.IsError {
			status = "failed"
		}
		fmt.Fprintf(&body, "<tool_result><status>%s</status><id>%s</id><%s>%s</%s></tool_result>", status, escapeToolXML(result.ID), result.Name, escapeToolXML(result.Output), result.Name)
	}
	return []Message{{"role": "user", "content": body.String()}}, nil
}

func modelIsFunctionCall(model string) (bool, error) {
	route, err := routeFor(model)
	return route.IsFunctionCall, err
}

type pendingToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

func genToolResults(results []ToolUseResult, calls []pendingToolCall, formatter Formatter) ([]Message, error) {
	if err := validateToolResults(results); err != nil {
		return nil, err
	}
	if len(calls) != len(results) {
		return nil, fmt.Errorf("tool results must cover the complete pending batch")
	}
	expected := map[string]string{}
	for _, call := range calls {
		if call.ID == "" || expected[call.ID] != "" {
			return nil, fmt.Errorf("invalid pending tool batch")
		}
		expected[call.ID] = call.Function.Name
	}
	for _, result := range results {
		if name, ok := expected[result.ID]; !ok || result.Name != name {
			return nil, fmt.Errorf("tool result identity does not match pending batch")
		}
	}
	return formatter.Format(results)
}

// Detect stored representation only at actual tool-result positions, never by
// interpreting an ordinary user query that happens to contain XML text.
func historyToolFormat(history []Message) string {
	for i, message := range history {
		if message["role"] == "tool" {
			return "native"
		}
		if i > 0 && message["role"] == "user" && history[i-1]["role"] == "assistant" && history[i-1]["tool_calls"] != nil {
			if text, ok := message["content"].(string); ok && strings.HasPrefix(text, "<tool_result>") {
				return "xml"
			}
		}
	}
	return ""
}

// Reserve the actual representation before exposing any tool. Under Go JSON,
// '&' costs six bytes and XML '&amp;' costs ten, the maximum growth ratio for
// text-node escaping. Fill the remainder with ASCII to bound every legal output.
func reservedToolResults(events []Event, isFunctionCall bool) ([]Message, error) {
	output := strings.Repeat(" ", maxToolOutputBytes-2)
	if !isFunctionCall {
		output = strings.Repeat("&", (maxToolOutputBytes-2)/6) + strings.Repeat("a", (maxToolOutputBytes-2)%6)
	}
	results := []ToolUseResult{}
	for _, event := range events {
		if event.Type == EvToolCall {
			results = append(results, ToolUseResult{ID: event.ID, Name: event.Name, Output: output})
		}
	}
	return pickFormatter(isFunctionCall).Format(results)
}
