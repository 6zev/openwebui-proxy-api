package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An account without an agent key must say what to do, not just 502. A LibreChat
// session JWT cannot substitute: /api/agents/v1 answers invalid_api_key for it.
func TestLibreChatMissingKeyExplainsTheFix(t *testing.T) {
	cfg := Config{
		APIKey: testProxyKey, Timeout: 2 * time.Second,
		Websites: []WebsiteConfig{{ID: "site-lc", BaseURL: "http://127.0.0.1:1"}},
		Accounts: []AccountConfig{{ID: "lc", WebsiteID: "site-lc", Backend: BackendLibreChat, APIKey: "agent-key-value"}},
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Force the guard by clearing the key on the live account, as a stale config
	// would: validation runs at construction, this path must still explain itself.
	p.accounts[0].config.APIKey = ""
	_, err = backendAuthHeader(context.Background(), p, p.accounts[0])
	if err == nil {
		t.Fatal("expected an error without an agent key")
	}
	message := err.Error()
	for _, want := range []string{"Agent API key", "Settings", "Remote Agents"} {
		if !strings.Contains(message, want) {
			t.Errorf("message does not explain how to fix it (missing %q): %s", want, message)
		}
	}
}

// Email/password is refused with a reason that names the real obstacle.
func TestLibreChatSessionCredentialsRejectedWithReason(t *testing.T) {
	err := validateBackendAccount(AccountConfig{ID: "lc", Backend: BackendLibreChat, Email: "a@b.test", Password: "x"})
	if err == nil {
		t.Fatal("session credentials accepted for the agents backend")
	}
	for _, want := range []string{"invalid_api_key", "api_key only", "body schema"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not explain why (missing %q): %s", want, err)
		}
	}
}

// The LibreChat probe must hit the Agents API, because that is what an agent key
// authorises; probing /models would blame the key for an unrelated failure.
func TestLibreChatProbeUsesAgentsAPI(t *testing.T) {
	var probed atomic.Value
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record the first path only: later health-loop traffic must not mask it.
		mu.Lock()
		if probed.Load() == nil {
			probed.Store(r.URL.Path)
		}
		mu.Unlock()
		if r.URL.Path == "/api/agents/v1/agents" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid API key"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()

	cfg := Config{
		APIKey: testProxyKey, Timeout: 3 * time.Second,
		Websites: []WebsiteConfig{{ID: "site-lc", BaseURL: upstream.URL}},
		Accounts: []AccountConfig{{ID: "lc", WebsiteID: "site-lc", Backend: BackendLibreChat, APIKey: "agent-key-value"}},
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.runHealthProbes(context.Background(), true)
	if got, _ := probed.Load().(string); got != "/api/agents/v1/agents" {
		t.Errorf("probe hit %q, want the Agents API", got)
	}
	report := p.healthReport()
	if len(report) != 1 {
		t.Fatalf("report entries = %d", len(report))
	}
	reason, _ := report[0]["error"].(string)
	if !strings.Contains(reason, "Remote Agents") {
		t.Errorf("401 reason should point at key permissions: %q", reason)
	}
}
