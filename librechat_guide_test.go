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
// Both credential shapes are valid, and carrying both at once is not: the shapes
// reach different LibreChat surfaces.
func TestLibreChatCredentialShapes(t *testing.T) {
	if err := validateBackendAccount(AccountConfig{ID: "lc", Backend: BackendLibreChat, APIKey: "agent-key-value"}); err != nil {
		t.Errorf("agent key rejected: %v", err)
	}
	if err := validateBackendAccount(AccountConfig{ID: "lc", Backend: BackendLibreChat, Email: "a@b.test", Password: "pw"}); err != nil {
		t.Errorf("email/password rejected: %v", err)
	}
	if err := validateBackendAccount(AccountConfig{ID: "lc", Backend: BackendLibreChat}); err == nil {
		t.Error("account with no credentials accepted")
	}
	err := validateBackendAccount(AccountConfig{ID: "lc", Backend: BackendLibreChat, APIKey: "agent-key-value", Email: "a@b.test", Password: "pw"})
	if err == nil {
		t.Fatal("both credential shapes accepted")
	}
	for _, want := range []string{"api_key", "email/password", "pick one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not explain the conflict (missing %q): %s", want, err)
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
