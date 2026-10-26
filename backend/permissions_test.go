package main

import (
	"strings"
	"testing"
)

func TestPermissionsAreStaticAndIncludedInBudget(t *testing.T) {
	for _, model := range []string{"mock", "mock-claude"} {
		route, err := routeFor(model)
		if err != nil {
			t.Fatal(err)
		}
		system, _, err := promptParts(ModeFull, route)
		if err != nil {
			t.Fatal(err)
		}
		permission, _ := promptsFS.ReadFile("prompts/permissions.txt")
		if !strings.HasSuffix(system, string(permission)) || !strings.Contains(system, "# Permissions & safety") {
			t.Fatal(system)
		}
		messages, _, err := genPrompt(nil, "/tmp", ModeFull, route)
		if err != nil {
			t.Fatal(err)
		}
		if messages[0]["content"] != system {
			t.Fatal("permissions missing from prompt")
		}
		budget, err := budgetFor(nil, "/tmp", ModeFull, route)
		if err != nil {
			t.Fatal(err)
		}
		if budget.Available >= route.MaxTokens-textTokens(system) {
			t.Fatal("fixed prompt not included in budget")
		}
	}
}
