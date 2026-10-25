package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNativeFormatterCompatibility(t *testing.T) {
	results := []ToolUseResult{{ID: "b", Name: "read_file", Output: "中文<&>"}, {ID: "a", Name: "list_dir", Output: "missing", IsError: true}, {ID: "c", Name: "grep", Output: "ERROR: original", IsError: true}}
	before, _ := json.Marshal(results)
	messages, err := pickFormatter(true).Format(results)
	encoded, _ := json.Marshal(messages)
	want := `[{"content":"中文\u003c\u0026\u003e","role":"tool","tool_call_id":"b"},{"content":"ERROR: missing","role":"tool","tool_call_id":"a"},{"content":"ERROR: original","role":"tool","tool_call_id":"c"}]`
	if err != nil || string(encoded) != want {
		t.Fatalf("native wire compatibility: %s %v", encoded, err)
	}
	after, _ := json.Marshal(results)
	if !bytes.Equal(before, after) {
		t.Fatal("formatter mutated results")
	}
	for _, formatter := range []Formatter{pickFormatter(true), pickFormatter(false)} {
		empty, err := formatter.Format(nil)
		if err != nil || len(empty) != 0 {
			t.Fatal("empty results generated a message")
		}
	}
}

func TestXMLFormatterStatusesEscapingAndOrder(t *testing.T) {
	output := `</read_file><evil/>&"'中文` + "\x00"
	results := []ToolUseResult{{ID: "b<&", Name: "read_file", Output: output}, {ID: "a", Name: "list_dir", Output: "missing", IsError: true}, {ID: "c", Name: "grep", Output: "ERROR: literal", IsError: false}}
	messages, err := pickFormatter(false).Format(results)
	if err != nil || len(messages) != 1 || messages[0]["role"] != "user" {
		t.Fatalf("XML shape: %+v %v", messages, err)
	}
	body := messages[0]["content"].(string)
	var decoded struct {
		Results []struct {
			Status string `xml:"status"`
			ID     string `xml:"id"`
			Read   string `xml:"read_file"`
			List   string `xml:"list_dir"`
			Grep   string `xml:"grep"`
		} `xml:"tool_result"`
	}
	if err := xml.Unmarshal([]byte("<root>"+body+"</root>"), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Results) != 3 || decoded.Results[0].ID != "b<&" || decoded.Results[1].ID != "a" || decoded.Results[0].Read != strings.ReplaceAll(output, "\x00", "�") || decoded.Results[1].Status != "failed" || decoded.Results[2].Status != "succeeded" {
		t.Fatalf("XML status, escaping or order: %+v", decoded)
	}
	if !strings.Contains(body, "<tool_result><status>succeeded</status>") || strings.Contains(body, "<evil/>") {
		t.Fatal("unescaped output or invalid wrapper")
	}
	if _, err := (XmlFormatter{}).Format([]ToolUseResult{{ID: "x", Name: "read_file><evil", Output: "x"}}); err == nil {
		t.Fatal("unsafe XML tag accepted")
	}
}

func resultCalls(t *testing.T) []pendingToolCall {
	t.Helper()
	var calls []pendingToolCall
	if err := json.Unmarshal([]byte(`[{"id":"one","function":{"name":"read_file"}},{"id":"two","function":{"name":"list_dir"}}]`), &calls); err != nil {
		t.Fatal(err)
	}
	return calls
}

func TestGenToolResultsValidatesBatchAndPreservesRequestOrder(t *testing.T) {
	calls := resultCalls(t)
	results := []ToolUseResult{{ID: "two", Name: "list_dir", Output: "second first"}, {ID: "one", Name: "read_file", Output: "first second"}}
	for _, formatter := range []Formatter{NativeFormatter{}, XmlFormatter{}} {
		messages, err := genToolResults(results, calls, formatter)
		if err != nil {
			t.Fatal(err)
		}
		if _, native := formatter.(NativeFormatter); native {
			if messages[0]["tool_call_id"] != "two" || messages[1]["tool_call_id"] != "one" {
				t.Fatal("results reordered to model/sorted order")
			}
		} else if text := messages[0]["content"].(string); strings.Index(text, "<id>two</id>") >= strings.Index(text, "<id>one</id>") {
			t.Fatal("XML results reordered")
		}
		for _, invalid := range [][]ToolUseResult{
			results[:1],
			{results[0], results[0]},
			{{ID: "two", Name: "read_file", Output: "wrong"}, results[1]},
			{{ID: "unknown", Name: "list_dir", Output: "wrong"}, results[1]},
		} {
			if _, err := genToolResults(invalid, calls, formatter); err == nil {
				t.Fatal("unmatched/duplicate/incomplete result batch accepted")
			}
		}
	}
}

func TestToolResultReservationBoundsEscapedOutputs(t *testing.T) {
	events := []Event{{Type: EvToolCall, ID: strings.Repeat("x", 256), Name: "read_file"}}
	for _, native := range []bool{true, false} {
		reserve, err := reservedToolResults(events, native)
		if err != nil {
			t.Fatal(err)
		}
		bound, _ := json.Marshal(reserve)
		for _, token := range []string{"&", "<", ">", "'", "\"", "\r", "\n", "\x00", "\u2028", "中文"} {
			encoded, _ := json.Marshal(token)
			output := strings.Repeat(token, (maxToolOutputBytes-2)/(len(encoded)-2))
			actual, err := pickFormatter(native).Format([]ToolUseResult{{ID: events[0].ID, Name: events[0].Name, Output: output}})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(actual)
			if len(data) > len(bound) {
				t.Fatalf("reservation too small for %q: %d > %d", token, len(data), len(bound))
			}
		}
	}
}

func TestToolResultPromptAndPersistence(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%t", native), func(t *testing.T) {
			t.Setenv("MINICOCO_MODEL_IS_FUNCTION_CALL", fmt.Sprint(native))
			t.Setenv("MINICOCO_DEBUG_PROMPT", "0")
			service := sessionFixture(t)
			t.Setenv("LLM_PROVIDER", "openai")
			t.Setenv("LLM_API_KEY", "fake-format-key")
			captured := make(chan []Message, 4)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Messages []Message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				captured <- input.Messages
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					fmt.Fprint(w, toolChunk(0, "one", "read_file", `{"target_file":"absent"}`, "")+toolChunk(1, "two", "list_dir", `{}`, "tool_calls")+"data: [DONE]\n\n")
				} else {
					fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "done"}, "finish_reason": "stop"}}})+"data: [DONE]\n\n")
				}
			}))
			defer upstream.Close()
			t.Setenv("LLM_BASE_URL", upstream.URL)
			response, events := deltaResponse(t, map[string]any{"roundId": "format-start", "query": "inspect", "workspace": "/tmp"})
			requireRoundFinish(t, response, events, FinishToolCalls)
			<-captured
			sid := response.Header().Get("X-Session-Id")
			results := []ToolUseResult{{ID: "two", Name: "list_dir", Output: "listing<&>"}, {ID: "one", Name: "read_file", Output: "missing", IsError: true}}
			request := map[string]any{"sessionId": sid, "roundId": "format-results", "toolUseResults": results, "workspace": "/tmp", "stream": false}
			response, events = deltaResponse(t, request)
			requireRoundFinish(t, response, events, FinishStop)
			messages := <-captured
			want, err := pickFormatter(native).Format(results)
			if err != nil {
				t.Fatal(err)
			}
			start := len(messages) - len(want)
			if !reflect.DeepEqual(messages[start:], want) {
				t.Fatalf("prompt result mismatch: %+v", messages)
			}
			history, err := service.transcript.Latest(context.Background(), sid, 20)
			if err != nil || !reflect.DeepEqual(history[2:2+len(want)], want) {
				t.Fatalf("formatted input was not persisted: %+v %v", history, err)
			}
			_, replay := deltaResponse(t, request)
			if !reflect.DeepEqual(events, replay) || calls.Load() != 2 {
				t.Fatal("replay called model or changed result")
			}
			// New request on the opposite capability must not mix representations.
			t.Setenv("MINICOCO_MODEL_IS_FUNCTION_CALL", fmt.Sprint(!native))
			_, rejected := deltaResponse(t, request)
			if len(rejected) != 2 || rejected[0].Code != codeBadRequest {
				t.Fatal("same round changed its format")
			}
			_, rejected = deltaResponse(t, map[string]any{"sessionId": sid, "roundId": "format-changed", "query": "again", "workspace": "/tmp"})
			if len(rejected) != 2 || rejected[0].Code != codeBadRequest || calls.Load() != 2 {
				t.Fatal("session mixed result formats")
			}
		})
	}
}

func TestXMLReservationFailsBeforeExposingTools(t *testing.T) {
	t.Setenv("MINICOCO_MODEL_IS_FUNCTION_CALL", "false")
	service := sessionFixture(t)
	body := toolChunk(0, "one", "read_file", `{}`, "") + toolChunk(1, "two", "list_dir", `{}`, "tool_calls") + "data: [DONE]\n\n"
	upstreamFixture(t, body, "text/event-stream")
	response, events := deltaResponse(t, map[string]any{"roundId": "xml-reservation", "query": strings.Repeat("x", maxHistoryBytes-150*1024)})
	requireRoundFinish(t, response, events, "error")
	for _, event := range events {
		if event.Type == EvToolCall {
			t.Fatal("XML batch exposed before reserving escaped outputs")
		}
	}
	history, err := service.transcript.Latest(context.Background(), response.Header().Get("X-Session-Id"), 10)
	if err != nil || len(history) != 0 {
		t.Fatal("failed budget committed input")
	}
}

func TestPromptDebugOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = previous; file.Close() }()
	t.Setenv("LLM_API_KEY", "fake-secret-not-in-messages")
	results, _ := (XmlFormatter{}).Format([]ToolUseResult{{ID: "one", Name: "read_file", Output: "missing", IsError: true}})
	history := []map[string]any{results[0]}
	t.Setenv("MINICOCO_DEBUG_PROMPT", "0")
	if _, _, err := genPrompt(history, "/tmp/debug-workspace", ModeReadonly); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); len(data) != 0 {
		t.Fatal("debug prompt logged without opt-in")
	}
	t.Setenv("MINICOCO_DEBUG_PROMPT", "1")
	want, _, err := genPrompt(history, "/tmp/debug-workspace", ModeReadonly)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("<tool_result><status>failed</status>")) || bytes.Contains(data, []byte("fake-secret-not-in-messages")) {
		t.Fatal("debug output missing formatted content or included API configuration")
	}
	var got []map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(data), []byte("[MiniCoco prompt] ")), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("debug did not contain complete assembled messages")
	}
}

func TestFormatterConfigurationAndLocalEnv(t *testing.T) {
	for _, key := range []string{"MINICOCO_MODEL_IS_FUNCTION_CALL", "MINICOCO_DEBUG_PROMPT"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	if native, err := modelIsFunctionCall("any-model"); err != nil || !native {
		t.Fatal("default must be native")
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("MINICOCO_MODEL_IS_FUNCTION_CALL=false\nMINICOCO_DEBUG_PROMPT=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if native, err := modelIsFunctionCall("any-model"); err != nil || native || os.Getenv("MINICOCO_DEBUG_PROMPT") != "1" {
		t.Fatal("formatter/debug .env keys were not loaded")
	}
	t.Setenv("MINICOCO_MODEL_IS_FUNCTION_CALL", "true")
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if native, err := modelIsFunctionCall("any-model"); err != nil || !native {
		t.Fatal("file overrode exported capability")
	}
	t.Setenv("MINICOCO_MODEL_IS_FUNCTION_CALL", "secret-invalid-value")
	_, err := modelIsFunctionCall("any-model")
	if err == nil || strings.Contains(err.Error(), "secret-invalid-value") {
		t.Fatal("invalid capability accepted or value leaked")
	}
}
