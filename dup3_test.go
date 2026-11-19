package main

import (
	"net/http"
	"os"
	"testing"
	"time"
)

// Creating an account with an ID that already exists in ANOTHER website must be
// allowed: IDs only need to be unique inside one website.
func TestCreateSameIDInDifferentWebsite(t *testing.T) {
	path := t.TempDir() + "/config.json"
	_ = os.WriteFile(path, []byte(`{"websites":[{"id":"site-a","base_url":"https://a.test"},{"id":"site-b","base_url":"https://b.test"}],"accounts":[{"id":"acc","website_id":"site-a","email":"a@x.test","password":"p"}]}`), 0o600)
	cfg := Config{APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second, ConfigPath: path,
		Websites: []WebsiteConfig{{ID: "site-a", BaseURL: "https://a.test"}, {ID: "site-b", BaseURL: "https://b.test"}},
		Accounts: []AccountConfig{{ID: "acc", WebsiteID: "site-a", Email: "a@x.test", Password: "p"}}}
	s := adminTestServer(t, cfg, adminTestKey)

	body := `{"id":"acc","website_id":"site-b","email":"b@x.test","password":"p"}`
	resp := adminTestRequest(t, s, "POST", "/api/admin/accounts", body, adminTestKey, nil)
	if resp.status != http.StatusOK && resp.status != http.StatusCreated {
		t.Fatalf("create in another website = %d: %s", resp.status, resp.body)
	}
}
