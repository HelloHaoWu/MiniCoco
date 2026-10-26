package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func runSubagentPersistence(t *testing.T, factory contractFactory) {
	open := factory(t)
	stores := open()
	defer func() { stores.close() }()
	previous := activeSessions
	defer func() { activeSessions = previous }()
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	parent := Session{ID: "s_parent", Workspace: "/tmp", Model: "mock"}
	if _, err := stores.meta.CreateSession(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	req := map[string]any{"roundId": "sub-1", "query": "inspect", "workspace": "/tmp", "model": "mock", "agentType": "explore", "parentSessionId": parent.ID, "mode": "full", "stream": false}
	response, events := deltaResponse(t, req)
	requireRoundFinish(t, response, events, FinishToolCalls)
	sid := response.Header().Get("X-Session-Id")
	child, err := stores.meta.GetSession(context.Background(), sid)
	if err != nil || child.ParentSessionID == nil || *child.ParentSessionID != parent.ID || child.AgentType != "explore" {
		t.Fatal(child, err)
	}
	req["parentSessionId"], req["roundId"] = sid, "grandchild"
	response, _ = deltaResponse(t, req)
	if response.Code != 400 {
		t.Fatal(response.Code, response.Body.String())
	}
	stores.close()
	stores = open()
	reopened, err := stores.meta.GetSession(context.Background(), sid)
	if err != nil || reopened.ParentSessionID == nil || *reopened.ParentSessionID != parent.ID || reopened.AgentType != "explore" {
		t.Fatal(reopened, err)
	}
	history, err := stores.transcript.RawLatest(context.Background(), parent.ID, allHistory)
	if err != nil || len(history) != 0 {
		t.Fatal("child altered parent", history, err)
	}
}
func TestFileSubagentPersistence(t *testing.T) { runSubagentPersistence(t, fileContractFactory) }

func TestSubagentPromptAndCatalog(t *testing.T) {
	route, err := routeFor("mock")
	if err != nil {
		t.Fatal(err)
	}
	route.agentType = "explore"
	prompt, tools, err := genPrompt(nil, "/tmp", ModeReadonly, route)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prompt[0]["content"].(string), "You are a read-only exploration subagent.") || len(tools) != 3 {
		t.Fatal(prompt, tools)
	}
	full, err := buildTools(ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range full {
		var schema struct {
			Function struct {
				Name        string
				Description string
				Parameters  struct {
					Properties map[string]struct{ Enum []string }
				}
			}
		}
		if err := json.Unmarshal(tool, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Function.Name == "delegate_subagent" {
			if strings.Contains(schema.Function.Description, "${") || len(schema.Function.Parameters.Properties["agent_type"].Enum) != 1 {
				t.Fatal(string(tool))
			}
			return
		}
	}
	t.Fatal("missing delegate schema")
}

func TestSubagentSchemaRejectsMalformedResource(t *testing.T) {
	for _, schema := range []string{`{}`, `{"function":{}}`, `{"function":{"description":"x"}}`, `{"function":{"description":"x","parameters":{}}}`, `{"function":{"description":"x","parameters":{"properties":{}}}}`} {
		if _, err := injectSubagents(json.RawMessage(schema)); err == nil {
			t.Fatal(schema)
		}
	}
}
