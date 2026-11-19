package main

import (
	"os"
	"testing"
)

func osWriteFile(p, c string) error { return os.WriteFile(p, []byte(c), 0o600) }

// The API must never be asked to edit by a bare ID when that ID lives in two
// websites — that is the message YG saw. Verify a PUT on the bare id is refused and
// the composite works, so the UI has a working path.
func TestBareIDAmbiguityRefusedCompositeWorks(t *testing.T) {
	cfg, path := dupConfig(t)
	_ = path
	s := adminTestServer(t, cfg, adminTestKey)
	bare := adminTestRequest(t, s, "PUT", "/api/admin/accounts/acc", `{"website_id":"site-a","email":"z@x.test","password":"p"}`, adminTestKey, nil)
	if bare.status != 409 {
		t.Errorf("PUT bare = %d, want 409: %s", bare.status, bare.body)
	}
	ok := adminTestRequest(t, s, "PUT", "/api/admin/accounts/site-b/acc", `{"website_id":"site-b","email":"b2@x.test","password":"p"}`, adminTestKey, nil)
	if ok.status != 200 {
		t.Errorf("PUT composite = %d: %s", ok.status, ok.body)
	}
}

func dupConfig(t *testing.T) (Config, string) {
	t.Helper()
	path := t.TempDir() + "/config.json"
	_ = osWriteFile(path, `{"websites":[{"id":"site-a","base_url":"https://a.test"},{"id":"site-b","base_url":"https://b.test"}],"accounts":[{"id":"acc","website_id":"site-a","email":"a@x.test","password":"p"},{"id":"acc","website_id":"site-b","email":"b@x.test","password":"p"}]}`)
	return Config{APIKey: testProxyKey, Timeout: 5e9, Cooldown: 1e9, ConfigPath: path,
		Websites: []WebsiteConfig{{ID: "site-a", BaseURL: "https://a.test"}, {ID: "site-b", BaseURL: "https://b.test"}},
		Accounts: []AccountConfig{
			{ID: "acc", WebsiteID: "site-a", Email: "a@x.test", Password: "p"},
			{ID: "acc", WebsiteID: "site-b", Email: "b@x.test", Password: "p"},
		}}, path
}
