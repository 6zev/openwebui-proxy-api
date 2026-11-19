package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The exact message YG saw came from a PUT/POST that resolved to the wrong account.
// Creating acc in site-b while site-a already has it must not report a clash with
// site-a.
func TestCreateDuplicateAcrossWebsitesDoesNotClash(t *testing.T) {
	path := t.TempDir() + "/config.json"
	_ = os.WriteFile(path, []byte(`{"websites":[{"id":"site-a","base_url":"https://a.test"},{"id":"site-b","base_url":"https://b.test"}],"accounts":[{"id":"acc","website_id":"site-a","email":"a@x.test","password":"p"},{"id":"other","website_id":"site-b","email":"o@x.test","password":"p"}]}`), 0o600)
	cfg := Config{APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second, ConfigPath: path,
		Websites: []WebsiteConfig{{ID: "site-a", BaseURL: "https://a.test"}, {ID: "site-b", BaseURL: "https://b.test"}},
		Accounts: []AccountConfig{
			{ID: "acc", WebsiteID: "site-a", Email: "a@x.test", Password: "p"},
			{ID: "other", WebsiteID: "site-b", Email: "o@x.test", Password: "p"},
		}}
	s := adminTestServer(t, cfg, adminTestKey)
	body := `{"id":"acc","website_id":"site-b","email":"b@x.test","password":"p"}`
	resp := adminTestRequest(t, s, "POST", "/api/admin/accounts", body, adminTestKey, nil)
	if resp.status >= 400 {
		t.Fatalf("create = %d: %s", resp.status, resp.body)
	}
	state := adminTestRequest(t, s, "GET", "/api/admin/state", "", adminTestKey, nil)
	if !strings.Contains(state.body, `"site-b/acc"`) {
		t.Errorf("new account missing: %s", state.body)
	}
}
