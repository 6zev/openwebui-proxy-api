package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Open WebUI fans out to several providers, so one account can be drained for
// OpenAI models while its Claude models still work. A quota rejection must
// therefore cool down only the rejected model.
func TestQuotaRejectionCoolsOnlyTheRejectedModel(t *testing.T) {
	var gptCalls, claudeCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "claude") {
			claudeCalls.Add(1)
			_, _ = io.WriteString(w, `{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			return
		}
		gptCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"You have no credits remaining."}`)
	}))
	defer upstream.Close()

	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	cfg.Cooldown = time.Minute
	s := newProxyTestServer(t, cfg)

	// First gpt call is rejected and records a per-model cooldown.
	rejected := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	requireProxyStatus(t, rejected, http.StatusBadRequest)
	if !strings.Contains(rejected.body, "no credits remaining") {
		t.Errorf("reason not forwarded: %s", rejected.body)
	}

	// Claude on the same account must still be served — the account is not down.
	claude := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	requireProxyStatus(t, claude, http.StatusOK)
	if claudeCalls.Load() != 1 {
		t.Errorf("claude calls = %d, want 1", claudeCalls.Load())
	}
}

// With several accounts, one drained for a model must not be picked first for
// that model, but must stay in rotation for other models.
func TestCandidatesOrderRejectedModelLast(t *testing.T) {
	cfg := Config{
		APIKey: testProxyKey, Timeout: time.Second, Cooldown: time.Minute,
		Websites: []WebsiteConfig{{ID: "site-a", BaseURL: "http://example.test"}},
		Accounts: []AccountConfig{
			{ID: "acc-1", WebsiteID: "site-a", Email: "1@example.test", Password: "p1"},
			{ID: "acc-2", WebsiteID: "site-a", Email: "2@example.test", Password: "p2"},
		},
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.cooldownModel(p.accounts[0], "gpt-5", time.Minute)

	ordered, err := p.candidatesForModel("", "site-a", "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 {
		t.Fatalf("candidates = %d, want 2 (rejected account must stay as last resort)", len(ordered))
	}
	if ordered[0].config.ID != "acc-2" {
		t.Errorf("first candidate = %s, want acc-2 (acc-1 rejected this model)", ordered[0].config.ID)
	}
	// A different model ignores the cooldown, so round-robin still includes both.
	other, err := p.candidatesForModel("", "site-a", "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 2 {
		t.Errorf("other model candidates = %d, want 2", len(other))
	}
	if cooled, _ := p.modelCooled(p.accounts[0], "claude-sonnet-5"); cooled {
		t.Error("cooldown leaked to another model")
	}
}
