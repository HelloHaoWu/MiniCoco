package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

func toolNames(t *testing.T, tools []json.RawMessage) []string {
	t.Helper()
	names := []string{}
	for _, raw := range tools {
		var tool struct{ Function struct{ Name string } }
		if err := json.Unmarshal(raw, &tool); err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Function.Name)
	}
	return names
}

func TestBuildToolsModesAndStableOrder(t *testing.T) {
	for _, test := range []struct {
		mode Mode
		want []string
	}{
		{"", []string{"read_file", "list_dir", "grep", "write_file", "edit_file", "run_command", "delegate_subagent"}},
		{ModeFull, []string{"read_file", "list_dir", "grep", "write_file", "edit_file", "run_command", "delegate_subagent"}},
		{ModeReadonly, []string{"read_file", "list_dir", "grep"}},
		{ModeSafe, []string{"read_file", "list_dir", "grep", "write_file", "edit_file", "delegate_subagent"}},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			first, err := buildTools(test.mode)
			if err != nil || !reflect.DeepEqual(toolNames(t, first), test.want) {
				t.Fatalf("mode %s: %s %v", test.mode, first, err)
			}
			for i := 0; i < 10; i++ {
				second, err := buildTools(test.mode)
				a, _ := json.Marshal(first)
				b, _ := json.Marshal(second)
				if err != nil || !bytes.Equal(a, b) {
					t.Fatal("tool JSON order/bytes changed")
				}
			}
			for _, raw := range first {
				var fields map[string]any
				if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 || fields["type"] != "function" || fields["availableVersion"] != nil {
					t.Fatal("harness metadata leaked into provider schema")
				}
			}
		})
	}
	if _, err := buildTools("read-only"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if toolAllowed(ModeFull, "unknown") || toolAllowed("invalid", ReadFile) {
		t.Fatal("unknown policy or tool accepted")
	}
}

func schemaFixture(version int, description string) []byte {
	return []byte(fmt.Sprintf(`{"type":"function","availableVersion":%d,"function":{"name":"read_file","description":%q,"parameters":{"type":"object"}}}`, version, description))
}

func TestToolCatalogVersionSelectionAndValidation(t *testing.T) {
	resources := fstest.MapFS{
		"tools/read_file__v1.json":  {Data: schemaFixture(1, "one")},
		"tools/read_file__v2.json":  {Data: schemaFixture(1, "two")},
		"tools/read_file__v10.json": {Data: schemaFixture(1, "ten")},
		"tools/read_file__v20.json": {Data: schemaFixture(2, "twenty")},
	}
	for _, test := range []struct {
		agent int
		want  string
	}{{1, "ten"}, {2, "twenty"}} {
		catalog, err := loadToolsByName(resources, test.agent)
		if err != nil || len(catalog) != 1 || !strings.Contains(string(catalog[ReadFile]), `"description":"`+test.want+`"`) {
			t.Fatalf("highest compatible numeric version: %s %v", catalog[ReadFile], err)
		}
	}
	delete(resources, "tools/read_file__v10.json")
	catalog, err := loadToolsByName(resources, 1)
	if err != nil || !strings.Contains(string(catalog[ReadFile]), `"description":"two"`) {
		t.Fatal("v1 + v2 must select v2")
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"read_file.json", schemaFixture(1, "bad")},
		{"read_file__v0.json", schemaFixture(1, "bad")},
		{"read_file__v01.json", schemaFixture(1, "bad")},
		{"unknown__v1.json", schemaFixture(1, "bad")},
		{"read_file__v1.json", []byte(`{}`)},
		{"read_file__v1.json", schemaFixture(0, "bad")},
		{"read_file__v1.json", []byte(`{"type":"function","availableVersion":1,"function":{"name":"other","parameters":{"type":"object"}}}`)},
	} {
		if _, err := loadToolsByName(fstest.MapFS{"tools/" + test.name: {Data: test.data}}, 1); err == nil {
			t.Fatalf("bad schema accepted: %s %s", test.name, test.data)
		}
	}
	if _, err := loadToolsByName(resources, 0); err == nil {
		t.Fatal("invalid harness version accepted")
	}
}

func TestHTTPModeFiltersProviderTools(t *testing.T) {
	service := sessionFixture(t)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_API_KEY", "fake-mode-key")
	received := make(chan []json.RawMessage, 9)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		calls.Add(1)
		received <- input.Tools
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": "done"}, "finish_reason": "stop"}}})+"data: [DONE]\n\n")
	}))
	defer upstream.Close()
	t.Setenv("LLM_BASE_URL", upstream.URL)
	for index, mode := range []Mode{ModeFull, ModeReadonly, ModeSafe} {
		for _, stream := range []bool{true, false} {
			response, events := deltaResponse(t, map[string]any{"roundId": fmt.Sprintf("mode-%d-%t", index, stream), "query": "inspect", "mode": mode, "stream": stream})
			requireRoundFinish(t, response, events, FinishStop)
			want, _ := buildTools(mode)
			a, _ := json.Marshal(want)
			b, _ := json.Marshal(<-received)
			if !bytes.Equal(a, b) {
				t.Fatalf("filtered provider payload differs: %s", b)
			}
		}
	}
	before, _ := service.meta.ListSessions(context.Background(), "")
	for _, mode := range []any{"unknown", 2, []string{"full"}} {
		response, _ := deltaResponse(t, map[string]any{"roundId": "invalid-mode", "query": "inspect", "mode": mode})
		if response.Code != 400 {
			t.Fatalf("invalid mode: HTTP %d", response.Code)
		}
	}
	after, _ := service.meta.ListSessions(context.Background(), "")
	if len(before) != len(after) || calls.Load() != 6 {
		t.Fatal("invalid mode created sessions or called a model")
	}
}

func TestSessionModeRejectsForbiddenBatchBeforeCommit(t *testing.T) {
	for _, test := range []struct {
		mode Mode
		tool string
	}{
		{ModeReadonly, "write_file"}, {ModeReadonly, "edit_file"}, {ModeReadonly, "run_command"}, {ModeSafe, "run_command"}, {ModeFull, "unknown"},
	} {
		t.Run(string(test.mode)+"/"+test.tool, func(t *testing.T) {
			service := sessionFixture(t)
			body := toolChunk(0, "allowed", "list_dir", `{}`, "") + toolChunk(1, "denied", test.tool, `{}`, "tool_calls") + "data: [DONE]\n\n"
			upstreamFixture(t, body, "text/event-stream")
			for _, stream := range []bool{true, false} {
				response, events := deltaResponse(t, map[string]any{"roundId": fmt.Sprintf("forbidden-%t", stream), "query": "inspect", "mode": test.mode, "stream": stream})
				requireRoundFinish(t, response, events, "error")
				for _, event := range events {
					if event.Type == EvToolCall || (event.Type == EvError && (event.Code != codeBadRequest || event.Retryable == nil || *event.Retryable)) {
						t.Fatalf("forbidden batch exposed or retryable: %+v", events)
					}
				}
				sid := response.Header().Get("X-Session-Id")
				history, err := service.transcript.Latest(context.Background(), sid, 20)
				session, _ := service.meta.GetSession(context.Background(), sid)
				if err != nil || len(history) != 0 || session.MessageCount != 0 || session.TotalTokens != 0 {
					t.Fatal("forbidden response committed history or usage")
				}
			}
		})
	}
}

func TestModeRoundIdentityAndLegacyReplay(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	service := sessionFixture(t)
	input := map[string]any{"roundId": "legacy-mode", "query": "inspect", "workspace": "/tmp", "stream": false}
	response, original := deltaResponse(t, input)
	requireRoundFinish(t, response, original, FinishToolCalls)
	sid := response.Header().Get("X-Session-Id")
	round, err := service.transcript.Round(context.Background(), sid, "legacy-mode")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := service.meta.GetSession(context.Background(), sid)
	query := "inspect"
	legacy, _ := json.Marshal(struct {
		Query            *string
		Results          []ToolUseResult
		Workspace, Model string
	}{&query, nil, "/tmp", session.Model})
	hash := sha256.Sum256(legacy)
	if round.Fingerprint != hex.EncodeToString(hash[:]) {
		t.Fatal("full changed legacy round fingerprint")
	}
	input["mode"] = "full"
	_, replay := deltaResponse(t, input)
	if !reflect.DeepEqual(original, replay) {
		t.Fatal("explicit full did not replay omitted mode")
	}
	input["mode"] = "readonly"
	_, rejected := deltaResponse(t, input)
	if len(rejected) != 2 || rejected[0].Code != codeBadRequest {
		t.Fatal("same round changed mode and replayed old tools")
	}
	input["roundId"] = "readonly-mode"
	readonlyResponse, readonly := deltaResponse(t, input)
	requireRoundFinish(t, readonlyResponse, readonly, FinishToolCalls)
	input["mode"] = "safe"
	_, rejected = deltaResponse(t, input)
	if len(rejected) != 2 || rejected[0].Code != codeBadRequest {
		t.Fatal("restricted mode was omitted from round fingerprint")
	}
	// A mock tool batch leaves this session unfinished; fresh rounds above use
	// their own deterministic session IDs, rather than replaying local tools.
}
