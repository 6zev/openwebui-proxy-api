package main

import (
	"encoding/json"
	"testing"
	"time"
)

// filterTestConfig needs no upstream: the filter reads a map the test hands it.
func filterTestConfig() Config {
	return Config{
		BaseURL: "http://librechat.invalid", APIKey: testProxyKey, Timeout: time.Second, Cooldown: time.Second,
		FilterUnconfiguredProviders: true,
		Accounts: []AccountConfig{{
			ID: "lc", Backend: BackendLibreChat,
			Email: "a@b.test", Password: "pw",
		}},
	}
}

// The provider key is the endpoint name, so metadata lands on exactly the models
// that belong to that endpoint; anything unknown is left alone rather than guessed.
func TestApplyEndpointMetadata(t *testing.T) {
	models := []cachedModel{
		{ID: "deepseek-chat", OwnedBy: "Deepseek"},
		{ID: "claude-opus-4-7", OwnedBy: "anthropic"},
		{ID: "mystery", OwnedBy: "unknown-provider"},
	}
	info := map[string]libreChatEndpointInfo{
		"Deepseek":  {ModelDisplayLabel: "Deepseek", Type: "custom", Order: 3, APIKey: "sk-real-deepseek-key"},
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
	// The credential fields drive the filter; they must never reach the listing.
	for _, model := range models {
		for _, key := range []string{"apiKey", "api_key", "x_api_key"} {
			if raw, ok := model.Extra[key]; ok {
				t.Errorf("%s leaked into extras: %s", key, raw)
			}
		}
	}
	// An empty map is a no-op, not a wipe.
	untouched := []cachedModel{{ID: "x", OwnedBy: "Deepseek"}}
	applyEndpointMetadata(untouched, nil)
	if untouched[0].Extra != nil {
		t.Errorf("nil info modified the model: %v", untouched[0].Extra)
	}
}

// A provider with no key can never answer, so its models are dropped; the providers
// that do have a key are kept, and an unknown provider (a built-in endpoint the map
// does not describe) is never dropped on a guess.
func TestFilterProviderModels(t *testing.T) {
	models := []cachedModel{
		{ID: "deepseek-chat", OwnedBy: "deepseek"},
		{ID: "grok-4", OwnedBy: "xai"},
		{ID: "claude-opus-4-7", OwnedBy: "Anthropic"},
		{ID: "qwen3-235b", OwnedBy: "openRouter"},
		{ID: "llama3", OwnedBy: "Ollama"},
		{ID: "gpt-5.4", OwnedBy: "openAI"},
	}
	endpoints := map[string]libreChatEndpointInfo{
		"deepseek":   {APIKey: "sk-0123456789abcdef"},
		"xai":        {APIKeyRef: "${XAI_API_KEY}"},
		"Anthropic":  {},
		"openRouter": {},
	}
	p, err := NewProxy(filterTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	kept := p.filterProviderModels(p.accounts[0], endpoints, models)
	ids := make([]string, 0, len(kept))
	for _, model := range kept {
		ids = append(ids, model.ID)
	}
	want := []string{"deepseek-chat", "grok-4", "llama3", "gpt-5.4"}
	if len(ids) != len(want) {
		t.Fatalf("kept = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("kept = %v, want %v", ids, want)
		}
	}
}

// A shape this build does not understand must not empty the picker: with nothing
// configured, or with no endpoint map at all, the list is returned untouched.
func TestFilterProviderModelsKeepsListWhenInconclusive(t *testing.T) {
	models := []cachedModel{
		{ID: "a", OwnedBy: "deepseek"},
		{ID: "b", OwnedBy: "xai"},
	}
	p, err := NewProxy(filterTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	keyless := map[string]libreChatEndpointInfo{
		"deepseek": {},
		"xai":      {},
	}
	if got := p.filterProviderModels(p.accounts[0], keyless, models); len(got) != 2 {
		t.Errorf("all-keyless listing = %d models, want 2", len(got))
	}
	if got := p.filterProviderModels(p.accounts[0], nil, models); len(got) != 2 {
		t.Errorf("no endpoint map listing = %d models, want 2", len(got))
	}
	// Filtering everything away is refused too: keep the list rather than blank it.
	onlyDead := []cachedModel{{ID: "dead", OwnedBy: "Anthropic"}}
	endpoints := map[string]libreChatEndpointInfo{
		"deepseek":  {APIKey: "sk-0123456789abcdef"},
		"Anthropic": {},
	}
	if got := p.filterProviderModels(p.accounts[0], endpoints, onlyDead); len(got) != 1 {
		t.Errorf("emptying filter = %d models, want the original list", len(got))
	}
}

func TestIsPlaceholderCredential(t *testing.T) {
	cases := map[string]bool{
		"":                           true,
		"   ":                        true,
		"${XAI_API_KEY}":             true,
		"{{OPENAI_API_KEY}}":         true,
		"user_provided":              true,
		"USER_PROVIDED":              true,
		"your-api-key":               true,
		"YOUR_OPENAI_KEY":            true,
		"<your key here>":            true,
		"sk-xxx":                     true,
		"sk-proj-0123456789abcdef":   false,
		"xai-9f8e7d6c5b4a3210":       false,
		"0123456789abcdef0123456789": false,
	}
	for key, want := range cases {
		if got := isPlaceholderCredential(key); got != want {
			t.Errorf("isPlaceholderCredential(%q) = %v, want %v", key, got, want)
		}
	}
}
