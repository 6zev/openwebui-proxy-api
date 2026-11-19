package main

import (
	"os"
	"testing"
)

// The exact live payload. Two accounts already share id "a" in other websites, and
// creating a third in a new website must succeed — the report YG saw was a 409.
func TestCreateWithIDExistingInTwoOtherWebsites(t *testing.T) {
	path := t.TempDir() + "/config.json"
	_ = os.WriteFile(path, []byte(`{"websites":[{"id":"w1","base_url":"https://a.test"},{"id":"w2","base_url":"https://b.test"},{"id":"chat.aqor.io","base_url":"https://c.test"}],"accounts":[{"id":"a","website_id":"w1","email":"e@x.test","password":"x"},{"id":"a","website_id":"w2","email":"e@x.test","password":"x"}]}`), 0o600)
	cfg := Config{APIKey: testProxyKey, Timeout: 5e9, Cooldown: 1e9, ConfigPath: path,
		Websites: []WebsiteConfig{{ID: "w1", BaseURL: "https://a.test"}, {ID: "w2", BaseURL: "https://b.test"}, {ID: "chat.aqor.io", BaseURL: "https://c.test"}},
		Accounts: []AccountConfig{
			{ID: "a", WebsiteID: "w1", Email: "e@x.test", Password: "x"},
			{ID: "a", WebsiteID: "w2", Email: "e@x.test", Password: "x"},
		}}
	s := adminTestServer(t, cfg, adminTestKey)
	body := `{"name":"","enabled":true,"id":"a","website_id":"chat.aqor.io","backend":"openwebui","email":"a@gmail.com","password":"11042006"}`
	resp := adminTestRequest(t, s, "POST", "/api/admin/accounts", body, adminTestKey, nil)
	if resp.status != 201 {
		t.Fatalf("create = %d: %s", resp.status, resp.body)
	}
}
