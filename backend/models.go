package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
)

//go:embed models.json
var embeddedModels []byte

// MaxTokens is a configured context-budget ceiling used by the v2 estimator,
// not the provider's output max_tokens parameter or an enforced token counter.
type ModelRoute struct {
	mcpTools       []MCPToolSpec
	agentType      string
	scene          string
	effective      bool
	Name           string  `json:"-"`
	Provider       string  `json:"provider"`
	BaseURL        string  `json:"baseURL"`
	Family         string  `json:"family"`
	MaxTokens      int     `json:"maxTokens"`
	IsFunctionCall bool    `json:"isFunctionCall"`
	Temperature    float64 `json:"temperature"`
	Thinking       string  `json:"thinking,omitempty"`
}

type modelRouteContextKey struct{}

func modelConfigError() error {
	return completionFailure(codeConfig, false, "invalid model routes: require provider openai|mock, HTTP(S) baseURL for openai, family openai|claude, positive maxTokens, isFunctionCall boolean, temperature 0..2 and optional thinking disabled; thinking enabled requires reasoning replay support", nil)
}

func validModelBase(base string) bool {
	endpoint, err := url.Parse(base)
	return err == nil && (endpoint.Scheme == "http" || endpoint.Scheme == "https") && endpoint.Host != "" && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == ""
}

func loadModelRoutes() (map[string]ModelRoute, error) {
	if os.Getenv("MINICOCO_MODEL_IS_FUNCTION_CALL") != "" {
		return nil, completionFailure(codeConfig, false, "MINICOCO_MODEL_IS_FUNCTION_CALL has been replaced by each model route's isFunctionCall; remove the old setting", nil)
	}
	data := embeddedModels
	if path := os.Getenv("MINICOCO_MODELS_FILE"); path != "" {
		file, err := os.Open(path)
		if err != nil {
			return nil, completionFailure(codeConfig, false, "cannot open MINICOCO_MODELS_FILE", nil)
		}
		data, err = io.ReadAll(io.LimitReader(file, 1024*1024+1))
		file.Close()
		if err != nil || len(data) > 1024*1024 {
			return nil, completionFailure(codeConfig, false, "cannot read MINICOCO_MODELS_FILE (limit 1 MiB)", nil)
		}
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) == 0 {
		return nil, modelConfigError()
	}
	routes := make(map[string]ModelRoute, len(entries))
	for name, raw := range entries {
		if name == "" || len(name) > 256 || strings.TrimSpace(name) != name {
			return nil, modelConfigError()
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil, modelConfigError()
		}
		for _, key := range []string{"provider", "baseURL", "family", "maxTokens", "isFunctionCall", "temperature"} {
			if len(fields[key]) == 0 || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
				return nil, modelConfigError()
			}
		}
		var route ModelRoute
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&route) != nil || (route.Provider != "openai" && route.Provider != "mock") || (route.Family != "openai" && route.Family != "claude") || route.MaxTokens <= 0 || route.Temperature < 0 || route.Temperature > 2 || (route.Thinking != "" && route.Thinking != "disabled") || (route.Provider == "openai" && !validModelBase(route.BaseURL)) {
			return nil, modelConfigError()
		}
		route.Name = name
		routes[name] = route
	}
	return routes, nil
}

func routeFor(model string) (ModelRoute, error) {
	routes, err := loadModelRoutes()
	if err != nil {
		return ModelRoute{}, err
	}
	if model == "" {
		model = getenv("LLM_MODEL", defaultModel)
	}
	route, exists := routes[model]
	if !exists {
		supported := make([]string, 0, len(routes))
		for name := range routes {
			supported = append(supported, name)
		}
		sort.Strings(supported)
		return ModelRoute{}, completionFailure(codeBadRequest, false, fmt.Sprintf("unsupported model; supported models: %s", strings.Join(supported, ", ")), nil)
	}
	return route, nil
}

func effectiveRoute(route ModelRoute) (ModelRoute, error) {
	if route.effective {
		return route, nil
	}
	// The offline override remains available for existing deployments. A route
	// whose provider is mock always stays local, including LLM_PROVIDER=openai.
	switch strings.ToLower(os.Getenv("LLM_PROVIDER")) {
	case "", "openai":
	case "mock":
		route.Provider = "mock"
	default:
		return ModelRoute{}, completionFailure(codeConfig, false, "unsupported LLM_PROVIDER; use openai or mock", nil)
	}
	if route.Provider == "openai" {
		if base := os.Getenv("LLM_BASE_URL"); base != "" {
			route.BaseURL = base
		}
		if !validModelBase(route.BaseURL) {
			return ModelRoute{}, completionFailure(codeConfig, false, "LLM_BASE_URL must be an HTTP(S) base URL without credentials, query or fragment", nil)
		}
	}
	route.effective = true
	return route, nil
}

// A round takes one immutable routing snapshot shared by prompt, formatter and
// transport; edits to an external table become visible on the next round.
func routeForContext(ctx context.Context, model string) (ModelRoute, error) {
	if route, ok := ctx.Value(modelRouteContextKey{}).(ModelRoute); ok && (model == "" || route.Name == model) {
		return route, nil
	}
	return routeFor(model)
}
