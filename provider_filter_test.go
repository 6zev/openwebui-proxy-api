package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A provider without a credential refuses every pick the same way. Those replies
// must be recognised so the model can be marked and hidden, and the caller must see
// a reason that names the real cause.
func TestProviderUnavailableMarkers(t *testing.T) {
	cases := map[string]bool{
		`{"type":"no_user_key"}`:                                       true,
		`{"error":"openAI API Key not provided."}`:                     true,
		`{ "type": "illegal_model_request", "info": "anthropic|x" }`:   true,
		`{ "type": "endpoint_models_not_loaded", "info": "Deepseek" }`: true,
		`{"detail":"You have no credits remaining"}`:                   false,
		`{"detail":"insufficient_quota"}`:                              false,
		`{"error":{"message":"bad request"}}`:                          false,
		`{"error":"API key not provided for this provider"}`:           true,
	}
	for body, want := range cases {
		if got := isProviderUnavailable([]byte(body)); got != want {
			t.Errorf("isProviderUnavailable(%s) = %v, want %v", body, got, want)
		}
	}
}

// After a provider answers "no credential", the model disappears from the listing
// (when hiding is on) and the chat error names the cause.
func TestUnavailableModelHiddenAndExplained(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		if strings.Contains(r.URL.Path, "/chat") {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `data: {"error":{"type":"no_user_key"}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"dead-model","owned_by":"p1"},{"id":"live-model","owned_by":"p1"}]}`)
	}))
	defer upstream.Close()

	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	cfg.HideUnavailableModels = true
	cfg.ProviderUnavailableCooldown = time.Minute
	cfg.ModelCacheTTL = time.Minute
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	prime := func(px *Proxy) {
		models, err := px.fetchModelList(context.Background(), px.accounts[0])
		if err != nil {
			t.Fatal(err)
		}
		px.modelCache.putCost(accountKey(px.accounts[0].config.WebsiteID, px.accounts[0].config.ID), models, time.Millisecond)
	}
	prime(p)
	// Take the model out the way a chat failure would.
	p.cooldownModel(p.accounts[0], "dead-model", time.Minute)

	// The filter runs at render time (a rejection happens after the list is cached),
	// so the check goes through visibleModels, which both write paths use.
	state := p.mergedModelList(p.accounts)
	ids := make([]string, 0, len(state.models))
	for _, model := range p.visibleModels(p.accounts, state.models) {
		ids = append(ids, model.ID)
	}
	if len(ids) != 1 || ids[0] != "live-model" {
		t.Errorf("listing = %v, want only live-model", ids)
	}

	// Hiding off still lists it: the operator keeps the full picture.
	cfg.HideUnavailableModels = false
	plain, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plainModels, err := plain.fetchModelList(context.Background(), plain.accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	plain.modelCache.putCost(accountKey(plain.accounts[0].config.WebsiteID, plain.accounts[0].config.ID), plainModels, time.Millisecond)
	plainState := plain.mergedModelList(plain.accounts)
	if got := len(plain.visibleModels(plain.accounts, plainState.models)); got != 2 {
		t.Errorf("listing without hiding = %d models, want 2", got)
	}
}
