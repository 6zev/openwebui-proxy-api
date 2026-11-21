package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// providerFilterFixture stands in for a LibreChat session deployment whose config
// mentions more providers than have a key: the model list names every one of them,
// /api/endpoints describes them with and without credentials.
type providerFilterFixture struct {
	server    *httptest.Server
	endpoints atomic.Int32
	// endpointsBody overrides the /api/endpoints document (nil = default).
	endpointsBody *string
}

const providerFilterModels = `{
	"deepseek": ["deepseek-chat", "deepseek-reasoner"],
	"xai": ["grok-4", "grok-4-mini"],
	"Anthropic": ["claude-opus-4-7"],
	"openRouter": ["qwen3-235b"],
	"Ollama": ["llama3"],
	"openAI": ["gpt-5.4"]
}`

const providerFilterEndpoints = `{
	"deepseek": {"modelDisplayLabel": "DeepSeek", "apiKey": "sk-0123456789abcdef", "models": ["deepseek-chat"]},
	"xai": {"modelDisplayLabel": "xAI", "apiKeyRef": "${XAI_API_KEY}", "models": ["grok-4"]},
	"Anthropic": {"modelDisplayLabel": "Claude", "apiKey": "", "apiKeyRef": ""},
	"openRouter": {"modelDisplayLabel": "OpenRouter"},
	"Ollama": {"modelDisplayLabel": "Local"}
}`

func newProviderFilterFixture(t *testing.T) *providerFilterFixture {
	t.Helper()
	f := &providerFilterFixture{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "refreshToken", Value: "r1", Path: "/"})
			_, _ = io.WriteString(w, `{"token":"jwt-1","user":{"id":"u1"}}`)
		case "/api/models":
			_, _ = io.WriteString(w, providerFilterModels)
		case "/api/endpoints":
			f.endpoints.Add(1)
			if f.endpointsBody != nil {
				_, _ = io.WriteString(w, *f.endpointsBody)
				return
			}
			_, _ = io.WriteString(w, providerFilterEndpoints)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func providerFilterProxy(t *testing.T, f *providerFilterFixture) *Proxy {
	t.Helper()
	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second,
		FilterUnconfiguredProviders: true,
		ModelCacheTTL:               0,
		Websites:                    []WebsiteConfig{{ID: "site-lc", BaseURL: f.server.URL}},
		Accounts:                    []AccountConfig{{ID: "lc", WebsiteID: "site-lc", Backend: BackendLibreChat, Email: "a@b.test", Password: "pw"}},
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func modelIDs(models []cachedModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	sort.Strings(ids)
	return ids
}

// Only the providers with a key survive the listing: LibreChat answers with every
// provider its config mentions, and picking a key-less one can never work.
func TestProviderFilterDropsKeylessProviders(t *testing.T) {
	f := newProviderFilterFixture(t)
	p := providerFilterProxy(t, f)

	models, err := p.fetchModelList(context.Background(), p.accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"deepseek-chat", "deepseek-reasoner", "gpt-5.4", "grok-4", "grok-4-mini"}
	ids := modelIDs(models)
	if len(ids) != len(want) {
		t.Fatalf("listing = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("listing = %v, want %v", ids, want)
		}
	}
	// The metadata decoration still runs on what survived.
	var label string
	for _, model := range models {
		if model.ID != "deepseek-chat" {
			continue
		}
		raw, ok := model.Extra["x_endpoint_label"]
		if !ok || json.Unmarshal(raw, &label) != nil || label != "DeepSeek" {
			t.Errorf("label = %s", raw)
		}
	}
	// One listing costs one /api/endpoints call: the filter reads the map already
	// cached for decoration instead of asking again.
	if got := f.endpoints.Load(); got != 1 {
		t.Errorf("endpoints calls = %d, want 1", got)
	}
}

// The switch turns the filter off: the operator asked for the raw list.
func TestProviderFilterRespectsToggle(t *testing.T) {
	f := newProviderFilterFixture(t)
	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second,
		FilterUnconfiguredProviders: false,
		ModelCacheTTL:               0,
		Websites:                    []WebsiteConfig{{ID: "site-lc", BaseURL: f.server.URL}},
		Accounts:                    []AccountConfig{{ID: "lc", WebsiteID: "site-lc", Backend: BackendLibreChat, Email: "a@b.test", Password: "pw"}},
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.fetchModelList(context.Background(), p.accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := len(models); got != 8 {
		t.Errorf("listing without the filter = %d models, want 8: %v", got, modelIDs(models))
	}
}

// An endpoint map this build does not understand (every entry key-less) must not
// empty the picker: the filter gives up and the listing stays as the upstream sent it.
func TestProviderFilterKeepsListingWhenShapeChanged(t *testing.T) {
	f := newProviderFilterFixture(t)
	keyless := `{"deepseek": {"modelDisplayLabel": "DeepSeek"}, "xai": {"modelDisplayLabel": "xAI"}}`
	f.endpointsBody = &keyless
	p := providerFilterProxy(t, f)

	models, err := p.fetchModelList(context.Background(), p.accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := len(models); got != 8 {
		t.Errorf("listing = %d models, want the untouched 8: %v", got, modelIDs(models))
	}
}

// The filtered list must survive the cache: a second call serves the same ids.
func TestProviderFilterSurvivesCache(t *testing.T) {
	f := newProviderFilterFixture(t)
	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second,
		FilterUnconfiguredProviders: true,
		ModelCacheTTL:               time.Minute,
		Websites:                    []WebsiteConfig{{ID: "site-lc", BaseURL: f.server.URL}},
		Accounts:                    []AccountConfig{{ID: "lc", WebsiteID: "site-lc", Backend: BackendLibreChat, Email: "a@b.test", Password: "pw"}},
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := p.fetchModelList(ctx, p.accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	key := accountKey(p.accounts[0].config.WebsiteID, p.accounts[0].config.ID)
	p.modelCache.putCost(key, first, time.Millisecond)
	cached, _, ok := p.modelCache.peek(key)
	if !ok {
		t.Fatal("model list was not cached")
	}
	if ids := modelIDs(cached); len(ids) != 5 || strings.Contains(strings.Join(ids, ","), "claude-opus-4-7") {
		t.Errorf("cached listing = %v, want the filtered five", ids)
	}
	if got := f.endpoints.Load(); got != 1 {
		t.Errorf("endpoints calls = %d, want 1 (the cache must serve the second call)", got)
	}
}
