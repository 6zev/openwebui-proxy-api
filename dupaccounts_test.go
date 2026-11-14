package main

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestAccountKeyHelpers(t *testing.T) {
	if got := accountKey("site", "acc"); got != "site/acc" {
		t.Errorf("accountKey = %q", got)
	}
	if got := accountKey("", "acc"); got != "acc" {
		t.Errorf("accountKey without website = %q", got)
	}
	website, id := splitAccountKey("site/acc")
	if website != "site" || id != "acc" {
		t.Errorf("splitAccountKey = (%q, %q)", website, id)
	}
	website, id = splitAccountKey("legacy")
	if website != "" || id != "legacy" {
		t.Errorf("splitAccountKey legacy = (%q, %q)", website, id)
	}
}

// Two websites may each own an account with the same ID; pinning must then use
// the website to disambiguate, and a bare ID must be rejected as ambiguous.
func TestDuplicateAccountIDsAcrossWebsites(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "")
	cfg.Websites = []WebsiteConfig{
		{ID: "w1", Name: "w1", BaseURL: u.server.URL},
		{ID: "w2", Name: "w2", BaseURL: u.server.URL},
	}
	cfg.Accounts = []AccountConfig{
		{ID: "dup", WebsiteID: "w1", Email: "a@example.test", Password: "pw1"},
		{ID: "dup", WebsiteID: "w2", Email: "b@example.test", Password: "pw2"},
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatalf("duplicate ids across websites rejected: %v", err)
	}
	picked, err := p.candidatesFor("dup", "w2")
	if err != nil {
		t.Fatalf("pinned lookup failed: %v", err)
	}
	if len(picked) != 1 || picked[0].config.WebsiteID != "w2" {
		t.Fatalf("picked %+v, want the w2 account", picked)
	}
	if _, err := p.candidatesFor("dup", ""); err == nil {
		t.Fatal("ambiguous account id accepted without a website")
	}
}

// The admin API must let a second website create an account with a taken ID and
// address both through the composite key.
func TestAdminDuplicateAccountIDDifferentWebsite(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "")
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	cfg.Websites = []WebsiteConfig{
		{ID: "w1", Name: "w1", BaseURL: u.server.URL},
		{ID: "w2", Name: "w2", BaseURL: u.server.URL},
	}
	cfg.Accounts = []AccountConfig{{ID: "dup", WebsiteID: "w1", Email: "a@example.test", Password: "pw1"}}
	s := adminTestServer(t, cfg, adminTestKey)

	adminTestMutation(t, s, "POST", "/api/admin/accounts", map[string]any{
		"id": "dup", "website_id": "w2", "email": "b@example.test", "password": "pw2",
	})

	state := adminTestReadState(t, s)
	if len(state.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2: %+v", len(state.Accounts), state.Accounts)
	}

	// Updating through the composite key must hit only the w2 account.
	adminTestMutation(t, s, "PUT", "/api/admin/accounts/w2/dup", map[string]any{
		"name": "second", "website_id": "w2", "enabled": true,
	})
	state = adminTestReadState(t, s)
	for _, account := range state.Accounts {
		if account.WebsiteID == "w2" && account.Name != "second" {
			t.Errorf("w2 account not updated: %+v", account)
		}
		if account.WebsiteID == "w1" && account.Name == "second" {
			t.Errorf("update leaked into w1 account: %+v", account)
		}
	}

	// A bare, ambiguous key must be refused rather than silently picking one.
	ambiguous := adminTestRequest(t, s, http.MethodDelete, "/api/admin/accounts/dup", "", adminTestKey, nil)
	if ambiguous.status != 409 {
		t.Errorf("ambiguous delete status = %d, want 409", ambiguous.status)
	}
}
