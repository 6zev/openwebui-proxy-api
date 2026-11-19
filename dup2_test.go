package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Moving an existing account to a website that already has the same ID must be
// refused with a clear reason, not accepted and later collide.
func TestMoveAccountIntoWebsiteWithSameID(t *testing.T) {
	path := t.TempDir() + "/config.json"
	_ = os.WriteFile(path, []byte(`{"websites":[{"id":"site-a","base_url":"https://a.test"},{"id":"site-b","base_url":"https://b.test"}],"accounts":[{"id":"acc","website_id":"site-a","email":"a@x.test","password":"p"},{"id":"acc","website_id":"site-b","email":"b@x.test","password":"p"}]}`), 0o600)
	cfg := Config{APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second, ConfigPath: path,
		Websites: []WebsiteConfig{{ID: "site-a", BaseURL: "https://a.test"}, {ID: "site-b", BaseURL: "https://b.test"}},
		Accounts: []AccountConfig{
			{ID: "acc", WebsiteID: "site-a", Email: "a@x.test", Password: "p"},
			{ID: "acc", WebsiteID: "site-b", Email: "b@x.test", Password: "p"},
		}}
	s := adminTestServer(t, cfg, adminTestKey)
	body := `{"website_id":"site-b","email":"a@x.test","password":"p"}`
	resp := adminTestRequest(t, s, "PUT", "/api/admin/accounts/site-a/acc", body, adminTestKey, nil)
	if resp.status != http.StatusConflict {
		t.Fatalf("move = %d, want 409: %s", resp.status, resp.body)
	}
	// The reason must name the clash and say what to do about it.
	for _, want := range []string{"already exists", "site-b", "pick a different ID"} {
		if !strings.Contains(resp.body, want) {
			t.Errorf("reason missing %q: %s", want, resp.body)
		}
	}
}
