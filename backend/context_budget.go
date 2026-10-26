package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"unicode/utf8"
)

const charsPerToken = 2
const microRatio = 0.65
const fullRatio = 0.80

type compactMode string

const (
	compactNone  compactMode = ""
	compactMicro compactMode = "micro"
	compactFull  compactMode = "full"
)

type tokenBudget struct {
	Base, Available, Used int
	Ratio                 float64
	Mode                  compactMode
}

// Count UTF-16 code units to match the design's JavaScript len, rounding up.
// This is a heuristic, not a model tokenizer or supplier usage measurement.
func textTokens(text string) int {
	chars := 0
	for _, r := range text {
		chars += utf16Width(r)
	}
	return (chars + charsPerToken - 1) / charsPerToken
}
func contentTokens(value any) int {
	if text, ok := value.(string); ok {
		return textTokens(text)
	}
	if value == nil {
		return 0
	}
	data, _ := json.Marshal(value)
	var parts []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Image struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(data, &parts) != nil {
		return textTokens(string(data))
	}
	total := 0
	for _, part := range parts {
		if part.Type == "text" {
			total += textTokens(part.Text)
		} else if part.Type == "image_url" && strings.HasPrefix(part.Image.URL, "data:image/") && strings.Contains(part.Image.URL, ";base64,") {
			_, b64, _ := strings.Cut(part.Image.URL, ";base64,")
			total += (len(b64)*2 + 2999) / 3000
		} else {
			raw, _ := json.Marshal(part)
			total += textTokens(string(raw))
		}
	}
	return total
}
func estimateTokens(messages []Message) int {
	total := 0
	for _, message := range messages {
		total += contentTokens(message["content"])
		if calls := message["tool_calls"]; calls != nil {
			raw, _ := json.Marshal(calls)
			total += textTokens(string(raw))
		}
		if id, ok := message["tool_call_id"].(string); ok {
			total += textTokens(id)
		}
	}
	return total
}
func budgetFor(history []Message, workspace string, mode Mode, route ModelRoute) (tokenBudget, error) {
	system, tools, err := promptParts(mode, route)
	if err != nil {
		return tokenBudget{}, err
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return tokenBudget{}, err
	}
	base := textTokens(system) + textTokens(string(raw)) + textTokens(memoryReminder(route.memoryIndex))
	if workspace != "" {
		base += textTokens(environmentText(workspace))
	}
	budget := tokenBudget{Base: base, Available: route.MaxTokens - base, Used: estimateTokens(history)}
	if budget.Available <= 0 {
		return budget, cannotCompress("fixed prompt exceeds the configured model budget")
	}
	budget.Ratio = float64(budget.Used) / float64(budget.Available)
	if budget.Ratio > fullRatio {
		budget.Mode = compactFull
	} else if budget.Ratio > microRatio {
		budget.Mode = compactMicro
	}
	return budget, nil
}
func cannotCompress(message string) error {
	return completionFailure("cannot_compress", false, message, nil)
}

// Blocks keep an assistant call and its complete Native/XML result batch
// indivisible. A trailing pending call is allowed but never partially trimmed.
func historyBlocks(messages []Message) ([]int, error) {
	starts := []int{}
	for i := 0; i < len(messages); {
		starts = append(starts, i)
		message := messages[i]
		if message["role"] == "tool" {
			return nil, cannotCompress("orphan tool result in context")
		}
		if message["tool_calls"] == nil {
			i++
			continue
		}
		var calls []pendingToolCall
		raw, _ := json.Marshal(message["tool_calls"])
		if json.Unmarshal(raw, &calls) != nil || len(calls) == 0 {
			return nil, cannotCompress("invalid tool calls in context")
		}
		expected := map[string]bool{}
		for _, call := range calls {
			if call.ID == "" || expected[call.ID] {
				return nil, cannotCompress("invalid tool call identity")
			}
			expected[call.ID] = true
		}
		i++
		if i == len(messages) {
			continue
		}
		results := []string{}
		if messages[i]["role"] == "user" && strings.HasPrefix(fmt.Sprint(messages[i]["content"]), "<tool_result>") {
			var envelope struct {
				Rows []struct {
					ID string `xml:"id"`
				} `xml:"tool_result"`
			}
			if xml.Unmarshal([]byte("<results>"+fmt.Sprint(messages[i]["content"])+"</results>"), &envelope) != nil {
				return nil, cannotCompress("invalid XML tool result")
			}
			for _, row := range envelope.Rows {
				results = append(results, row.ID)
			}
			i++
		} else {
			for i < len(messages) && messages[i]["role"] == "tool" {
				id, _ := messages[i]["tool_call_id"].(string)
				results = append(results, id)
				i++
			}
		}
		if len(results) != len(expected) {
			return nil, cannotCompress("incomplete tool batch in context")
		}
		for _, id := range results {
			if !expected[id] {
				return nil, cannotCompress("mismatched tool result in context")
			}
			delete(expected, id)
		}
	}
	return starts, nil
}
func placeholderPrefix(messages []Message) []Message {
	if len(messages) > 0 && messages[0]["role"] == "assistant" {
		return append([]Message{{"role": "user", "content": "Continue."}}, messages...)
	}
	return messages
}
func protectedTail(messages []Message, starts []int) int {
	last := len(starts) - 1
	if last < 0 {
		return 0
	}
	if messages[starts[last]]["role"] == "user" {
		return starts[last]
	}
	for i := last; i >= 0; i-- {
		if messages[starts[i]]["tool_calls"] != nil {
			return starts[i]
		}
	}
	return starts[last]
}

// Trimming produces a prompt view, never edits the authoritative transcript.
// A protected tool block that cannot fit fails instead of breaking identities.
func trimToBudget(messages []Message, maxTokens int) ([]Message, error) {
	if len(messages) == 0 {
		return []Message{}, nil
	}
	if maxTokens < 1 {
		return nil, cannotCompress("no content budget remains")
	}
	starts, err := historyBlocks(messages)
	if err != nil {
		return nil, err
	}
	keep := len(messages)
	for i := len(starts) - 1; i >= 0; i-- {
		candidate := placeholderPrefix(messages[starts[i]:])
		if estimateTokens(candidate) > maxTokens {
			break
		}
		keep = starts[i]
	}
	tail := protectedTail(messages, starts)
	if keep > tail {
		if tail != len(messages)-1 || messages[tail]["tool_calls"] != nil {
			return nil, cannotCompress("protected tool batch does not fit the budget")
		}
		text, ok := messages[tail]["content"].(string)
		if !ok {
			return nil, cannotCompress("last content cannot be safely truncated")
		}
		copy := Message{}
		for key, value := range messages[tail] {
			copy[key] = value
		}
		overhead := estimateTokens(placeholderPrefix([]Message{copy})) - textTokens(text)
		room := maxTokens - overhead
		if room < 1 {
			return nil, cannotCompress("last message identity exceeds budget")
		}
		start := len(text)
		used := 0
		for start > 0 {
			r, size := utf8.DecodeLastRuneInString(text[:start])
			n := utf16Width(r)
			if used+n > room*charsPerToken {
				break
			}
			used += n
			start -= size
		}
		copy["content"] = text[start:]
		return placeholderPrefix([]Message{copy}), nil
	}
	return placeholderPrefix(append([]Message(nil), messages[keep:]...)), nil
}

func utf16Width(r rune) int {
	if r > 0xffff {
		return 2
	}
	return 1
}
