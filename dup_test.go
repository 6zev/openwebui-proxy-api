package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Two websites may share an account ID. State must expose the composite key, a bare
// ID must be refused rather than silently hit one of them, and the composite key
// must address exactly one account.
func TestDuplicateAccountIDAcrossWebsites(t *testing.T) {
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte(`{"websites":[{"id":"site-a","base_url":"https://a.test"},{"id":"site-b","base_url":"https://b.test"}],"accounts":[{"id":"acc","website_id":"site-a","email":"a@x.test","password":"p"},{"id":"acc","website_id":"site-b","email":"b@x.test","password":"p"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second, ConfigPath: path,
		Websites: []WebsiteConfig{
			{ID: "site-a", BaseURL: "https://a.test"},
			{ID: "site-b", BaseURL: "https://b.test"},
		},
		Accounts: []AccountConfig{
			{ID: "acc", WebsiteID: "site-a", Email: "a@x.test", Password: "p"},
			{ID: "acc", WebsiteID: "site-b", Email: "b@x.test", Password: "p"},
		},
	}
	s := adminTestServer(t, cfg, adminTestKey)

	state := adminTestRequest(t, s, "GET", "/api/admin/state", "", adminTestKey, nil)
	if !strings.Contains(state.body, `"site-a/acc"`) || !strings.Contains(state.body, `"site-b/acc"`) {
		t.Fatalf("state must expose composite keys: %s", state.body)
	}

	// Deleting/editing by the bare id is ambiguous, so it must be refused.
	bare := adminTestRequest(t, s, "DELETE", "/api/admin/accounts/acc", "", adminTestKey, nil)
	if bare.status != http.StatusConflict {
		t.Errorf("DELETE bare id = %d, want 409: %s", bare.status, bare.body)
	}
	// The composite key addresses exactly one account.
	one := adminTestRequest(t, s, "DELETE", "/api/admin/accounts/site-a/acc", "", adminTestKey, nil)
	if one.status != http.StatusOK {
		t.Errorf("DELETE composite = %d: %s", one.status, one.body)
	}
	// The sibling account survives.
	after := adminTestRequest(t, s, "GET", "/api/admin/state", "", adminTestKey, nil)
	if strings.Contains(after.body, `"site-a/acc"`) || !strings.Contains(after.body, `"site-b/acc"`) {
		t.Errorf("wrong account removed: %s", after.body)
	}
}
