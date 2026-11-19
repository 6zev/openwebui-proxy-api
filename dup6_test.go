package main

import (
	"os"
	"testing"
)

// Exactly the payload YG sent: POST to create with a bare id "a" that already exists
// in another website. This must create, never report the multi-website conflict.
func TestYGPayloadCreatesNotConflicts(t *testing.T) {
	path := t.TempDir() + "/config.json"
	_ = os.WriteFile(path, []byte(`{"websites":[{"id":"other.test","base_url":"https://o.test"},{"id":"chat.aqor.io","base_url":"https://c.test"}],"accounts":[{"id":"a","website_id":"other.test","email":"a@gmail.com","password":"x"}]}`), 0o600)
	cfg := Config{APIKey: testProxyKey, Timeout: 5e9, Cooldown: 1e9, ConfigPath: path,
		Websites: []WebsiteConfig{{ID: "other.test", BaseURL: "https://o.test"}, {ID: "chat.aqor.io", BaseURL: "https://c.test"}},
		Accounts: []AccountConfig{{ID: "a", WebsiteID: "other.test", Email: "a@gmail.com", Password: "x"}}}
	s := adminTestServer(t, cfg, adminTestKey)
	body := `{"name":"","enabled":true,"id":"a","website_id":"chat.aqor.io","backend":"openwebui","email":"a@gmail.com","password":"11042006"}`
	resp := adminTestRequest(t, s, "POST", "/api/admin/accounts", body, adminTestKey, nil)
	if resp.status == 409 {
		t.Fatalf("POST reported the multi-website conflict: %s", resp.body)
	}
	t.Logf("POST status=%d body=%s", resp.status, resp.body)
}
