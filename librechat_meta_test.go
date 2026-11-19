package main

import (
	"encoding/json"
	"testing"
)

// The provider key is the endpoint name, so metadata lands on exactly the models
// that belong to that endpoint; anything unknown is left alone rather than guessed.
func TestApplyEndpointMetadata(t *testing.T) {
	models := []cachedModel{
		{ID: "deepseek-chat", OwnedBy: "Deepseek"},
		{ID: "claude-opus-4-7", OwnedBy: "anthropic"},
		{ID: "mystery", OwnedBy: "unknown-provider"},
	}
	info := map[string]libreChatEndpointInfo{
		"Deepseek":  {ModelDisplayLabel: "Deepseek", Type: "custom", Order: 3},
		"anthropic": {ModelDisplayLabel: "Anthropic", Capabilities: []string{"web_search"}},
	}
	applyEndpointMetadata(models, info)

	var label string
	if raw, ok := models[0].Extra["x_endpoint_label"]; !ok || json.Unmarshal(raw, &label) != nil || label != "Deepseek" {
		t.Errorf("deepseek label = %s", raw)
	}
	var caps []string
	if raw, ok := models[1].Extra["x_endpoint_capabilities"]; !ok || json.Unmarshal(raw, &caps) != nil || len(caps) != 1 {
		t.Errorf("anthropic capabilities = %s", raw)
	}
	if models[2].Extra != nil {
		t.Errorf("unknown provider must not be decorated: %v", models[2].Extra)
	}
	// An empty map is a no-op, not a wipe.
	untouched := []cachedModel{{ID: "x", OwnedBy: "Deepseek"}}
	applyEndpointMetadata(untouched, nil)
	if untouched[0].Extra != nil {
		t.Errorf("nil info modified the model: %v", untouched[0].Extra)
	}
}
