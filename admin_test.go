package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const adminTestKey = "admin-only-secret-at-least-24-characters"

type adminTestState struct {
	Version     string          `json:"version"`
	Revision    json.RawMessage `json:"revision"`
	Persistent  bool            `json:"persistent"`
	APIBasePath string          `json:"api_base_path"`
	Websites    []struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		BaseURL         string `json:"base_url"`
		Enabled         bool   `json:"enabled"`
		ProxyURL        string `json:"proxy_url"`
		ProxyConfigured bool   `json:"proxy_configured"`
	} `json:"websites"`
	Accounts []struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		WebsiteID       string `json:"website_id"`
		Email           string `json:"email"`
		Enabled         bool   `json:"enabled"`
		HasPassword     bool   `json:"has_password"`
		ProxyURL        string `json:"proxy_url"`
		ProxyConfigured bool   `json:"proxy_configured"`
		Available       bool   `json:"available"`
		CooldownSeconds int    `json:"cooldown_seconds"`
		Backend         string `json:"backend"`
		HasAPIKey       bool   `json:"has_api_key"`
		Health          string `json:"health"`
	} `json:"accounts"`
}

func adminTestServer(t *testing.T, cfg Config, key string) *httptest.Server {
	t.Helper()
	app, err := NewApplication(cfg, key)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	s := httptest.NewServer(app)
	t.Cleanup(s.Close)
	return s
}

func adminTestRequest(t *testing.T, s *httptest.Server, method, path, body, key string, headers map[string]string) proxyTestResponse {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return proxyTestResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: string(raw)}
}

func adminTestReadState(t *testing.T, s *httptest.Server) adminTestState {
	t.Helper()
	resp := adminTestRequest(t, s, "GET", "/api/admin/state", "", adminTestKey, nil)
	requireProxyStatus(t, resp, 200)
	var state adminTestState
	if err := json.Unmarshal([]byte(resp.body), &state); err != nil {
		t.Fatalf("state JSON: %v", err)
	}
	if state.Version == "" || len(state.Revision) == 0 || state.APIBasePath != "/v1" {
		t.Errorf("state metadata missing: %s", resp.body)
	}
	return state
}

func adminTestMutation(t *testing.T, s *httptest.Server, method, path string, body any) proxyTestResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := adminTestRequest(t, s, method, path, string(raw), adminTestKey, nil)
	if resp.status != 200 && resp.status != 201 {
		t.Fatalf("%s %s status=%d body=%s", method, path, resp.status, resp.body)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(resp.body), &result); err != nil || !result.OK {
		t.Fatalf("mutation missing ok:true: %s", resp.body)
	}
	return resp
}

func adminTestAddWebsite(t *testing.T, s *httptest.Server, id, url string) {
	t.Helper()
	adminTestMutation(t, s, "POST", "/api/admin/websites", map[string]any{"id": id, "name": "Website " + id, "base_url": url})
}
func adminTestAddAccount(t *testing.T, s *httptest.Server, id, site string) {
	t.Helper()
	adminTestMutation(t, s, "POST", "/api/admin/accounts", map[string]any{"id": id, "website_id": site, "name": "Account " + id, "email": id + "@example.test", "password": "password-" + id})
}

func TestAdminBootstrapEmptyAndPublicAssets(t *testing.T) {
	cfg := Config{APIKey: testProxyKey, ConfigPath: filepath.Join(t.TempDir(), "newfolder", "config.json")}
	s := adminTestServer(t, cfg, adminTestKey)
	state := adminTestReadState(t, s)
	if !state.Persistent || state.Websites == nil || state.Accounts == nil || len(state.Websites) != 0 || len(state.Accounts) != 0 {
		t.Fatalf("setup state should contain empty arrays: %+v", state)
	}
	requireProxyStatus(t, adminTestRequest(t, s, "GET", "/v1/models", "", testProxyKey, nil), 503)
	for _, tc := range []struct{ path, content string }{{"/", "text/html"}, {"/assets/app.js", "javascript"}, {"/assets/style.css", "text/css"}} {
		resp := adminTestRequest(t, s, "GET", tc.path, "", "", nil)
		requireProxyStatus(t, resp, 200)
		if resp.body == "" || !strings.Contains(resp.header.Get("Content-Type"), tc.content) {
			t.Errorf("asset %s content-type=%q body bytes=%d", tc.path, resp.header.Get("Content-Type"), len(resp.body))
		}
		if strings.Contains(resp.body, adminTestKey) || strings.Contains(resp.body, testProxyKey) {
			t.Errorf("asset %s leaked configured key", tc.path)
		}
	}
	if _, err := NewProxy(cfg); err == nil {
		t.Fatal("NewProxy accepted zero accounts")
	}
}

func TestAdminAuthenticationFallbackAndOrigin(t *testing.T) {
	cfg := Config{APIKey: testProxyKey}
	if _, err := NewApplication(cfg, "abcde"); err != nil {
		t.Errorf("5-character admin key rejected: %v", err)
	}
	if _, err := NewApplication(cfg, "abcd"); err == nil {
		t.Error("admin key under the 5-character minimum accepted")
	}
	s := adminTestServer(t, cfg, adminTestKey)
	for _, key := range []string{"", "wrong-key", testProxyKey} {
		for _, route := range []struct{ method, path string }{
			{"GET", "/api/admin/state"}, {"GET", "/api/admin/models"},
			{"POST", "/api/admin/chat/completions"}, {"POST", "/api/admin/websites"},
			{"PUT", "/api/admin/websites/east"}, {"DELETE", "/api/admin/websites/east"},
			{"POST", "/api/admin/accounts"}, {"PUT", "/api/admin/accounts/alpha"},
			{"DELETE", "/api/admin/accounts/alpha"}, {"POST", "/api/admin/accounts/alpha/test"},
		} {
			requireProxyStatus(t, adminTestRequest(t, s, route.method, route.path, "{}", key, nil), 401)
		}
	}
	requireProxyStatus(t, adminTestRequest(t, s, "GET", "/api/admin/state", "", adminTestKey, nil), 200)
	requireProxyStatus(t, adminTestRequest(t, s, "GET", "/api/admin/state", "", "", map[string]string{"Cookie": "Authorization=Bearer " + adminTestKey + "; admin_key=" + adminTestKey}), 401)
	for _, origin := range []string{"https://unrelated.example", "null"} {
		resp := adminTestRequest(t, s, "GET", "/api/admin/state", "", adminTestKey, map[string]string{"Origin": origin})
		requireProxyStatus(t, resp, 403)
		if resp.header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("foreign Origin received CORS grant: %v", resp.header)
		}
	}
	resp := adminTestRequest(t, s, "GET", "/api/admin/state", "", adminTestKey, map[string]string{"Origin": s.URL})
	requireProxyStatus(t, resp, 200)
	if resp.header.Get("Access-Control-Allow-Origin") == "*" {
		t.Error("permissive CORS header")
	}
	fallback := adminTestServer(t, cfg, "")
	requireProxyStatus(t, adminTestRequest(t, fallback, "GET", "/api/admin/state", "", testProxyKey, nil), 200)
	requireProxyStatus(t, adminTestRequest(t, fallback, "GET", "/api/admin/state", "", adminTestKey, nil), 401)
}

func TestAdminCRUDPersistencePasswordPreservationAndLiveRoutes(t *testing.T) {
	east, west := newProxyTestUpstream(t), newProxyTestUpstream(t)
	cfg := Config{APIKey: testProxyKey, ConfigPath: filepath.Join(t.TempDir(), "nested", "config.json"), Timeout: time.Second}
	s := adminTestServer(t, cfg, adminTestKey)
	adminTestAddWebsite(t, s, "east", east.server.URL)
	adminTestAddWebsite(t, s, "west", west.server.URL)
	adminTestAddAccount(t, s, "alpha", "east")
	adminTestAddAccount(t, s, "gamma", "west")
	// Empty and absent password fields both preserve the original secret.
	for _, password := range []bool{true, false} {
		body := map[string]any{"id": "alpha", "website_id": "east", "name": "Renamed alpha", "email": "alpha@example.test"}
		if password {
			body["password"] = ""
		}
		adminTestMutation(t, s, "PUT", "/api/admin/accounts/alpha", body)
	}
	adminTestMutation(t, s, "PUT", "/api/admin/websites/east", map[string]any{"id": "east", "name": "Renamed East", "base_url": east.server.URL})
	state := adminTestReadState(t, s)
	if len(state.Websites) != 2 || len(state.Accounts) != 2 {
		t.Fatalf("CRUD counts: %+v", state)
	}
	for _, a := range state.Accounts {
		if !a.Enabled || !a.HasPassword || !a.Available || a.CooldownSeconds != 0 {
			t.Errorf("unexpected active account state: %+v", a)
		}
	}
	for _, tc := range []struct{ id, site string }{{"alpha", "east"}, {"gamma", "west"}} {
		headers := map[string]string{"X-Account-ID": tc.id, "X-Website-ID": tc.site}
		resp := adminTestRequest(t, s, "GET", "/api/admin/models", "", adminTestKey, headers)
		requireProxyStatus(t, resp, 200)
		if !strings.Contains(resp.body, "test-model") || resp.header.Get("X-Website-ID") != tc.site {
			t.Errorf("admin models routing: %s %v", resp.body, resp.header)
		}
		resp = adminTestRequest(t, s, "POST", "/api/admin/chat/completions", testChatBody, adminTestKey, headers)
		requireProxyStatus(t, resp, 200)
		if !strings.Contains(resp.body, "chat.completion") || resp.header.Get("X-Account-ID") != tc.id {
			t.Errorf("admin chat routing: %s %v", resp.body, resp.header)
		}
		resp = adminTestRequest(t, s, "POST", "/api/admin/accounts/"+tc.id+"/test", "", adminTestKey, nil)
		requireProxyStatus(t, resp, 200)
		var result struct {
			OK        bool              `json:"ok"`
			AccountID string            `json:"account_id"`
			WebsiteID string            `json:"website_id"`
			Models    []json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal([]byte(resp.body), &result); err != nil || !result.OK || result.AccountID != tc.id || result.WebsiteID != tc.site || len(result.Models) == 0 {
			t.Fatalf("account test result: %s", resp.body)
		}
	}
	// Admin credentials do not double as a public proxy credential.
	requireProxyStatus(t, adminTestRequest(t, s, "GET", "/v1/models", "", adminTestKey, nil), 401)
	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("stored file mode=%o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.ConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(cfg.ConfigPath) {
		t.Errorf("atomic storage left temporary files: %v", entries)
	}
	for _, secret := range []string{adminTestKey, testProxyKey, "alpha-token-", "gamma-token-"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("stored config leaked runtime credential %q", secret)
		}
	}
	var stored struct {
		Websites []WebsiteConfig `json:"websites"`
		Accounts []AccountConfig `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil || len(stored.Websites) != 2 || len(stored.Accounts) != 2 {
		t.Fatalf("stored object invalid: %v %s", err, raw)
	}
	for _, a := range stored.Accounts {
		if a.Password != "password-"+a.ID || a.WebsiteID == "" {
			t.Errorf("password/site was not preserved: account=%s", a.ID)
		}
	}
	// Explicitly reload persisted public config without relying on env-parser internals.
	cfg.Websites, cfg.Accounts = stored.Websites, stored.Accounts
	reloaded := adminTestServer(t, cfg, adminTestKey)
	if got := adminTestReadState(t, reloaded); len(got.Accounts) != 2 || len(got.Websites) != 2 {
		t.Fatalf("reload counts: %+v", got)
	}
	requireProxyStatus(t, multisiteTestRequest(t, reloaded, "GET", "/v1/models", "", "east", "alpha"), 200)
	requireProxyStatus(t, adminTestRequest(t, s, "DELETE", "/api/admin/websites/east", "", adminTestKey, nil), 409)
	adminTestMutation(t, s, "DELETE", "/api/admin/accounts/alpha", nil)
	adminTestMutation(t, s, "DELETE", "/api/admin/websites/east", nil)
	state = adminTestReadState(t, s)
	if len(state.Accounts) != 1 || len(state.Websites) != 1 || state.Accounts[0].ID != "gamma" || state.Websites[0].ID != "west" {
		t.Fatalf("delete failed: %+v", state)
	}
	for _, tc := range []struct {
		u  *proxyTestUpstream
		id string
	}{{east, "alpha"}, {west, "gamma"}} {
		_, records := tc.u.snapshot()
		for _, r := range records {
			if r.ID != tc.id {
				t.Errorf("cross-site admin credentials: %+v", r)
			}
		}
	}
}

func TestAdminStateRedactsSecretsAndProxyEdits(t *testing.T) {
	const proxySecret = "http://proxy-user:proxy-password@example.test:8080"
	u := newProxyTestUpstream(t)
	cfg := Config{APIKey: testProxyKey, ConfigPath: filepath.Join(t.TempDir(), "config.json"), Websites: []WebsiteConfig{{ID: "east", Name: "East", BaseURL: u.server.URL, ProxyURL: proxySecret}}, Accounts: []AccountConfig{{ID: "alpha", Name: "Alpha", WebsiteID: "east", Email: "alpha@example.test", Password: "password-alpha", ProxyURL: proxySecret}}}
	s := adminTestServer(t, cfg, adminTestKey)
	resp := adminTestRequest(t, s, "GET", "/api/admin/state", "", adminTestKey, nil)
	requireProxyStatus(t, resp, 200)
	for _, secret := range []string{"password-alpha", "proxy-user", "proxy-password", testProxyKey, adminTestKey, `"password":`, `"token":`} {
		if strings.Contains(resp.body, secret) {
			t.Errorf("state leaked %q: %s", secret, resp.body)
		}
	}
	state := adminTestReadState(t, s)
	if !state.Websites[0].ProxyConfigured || !state.Accounts[0].ProxyConfigured || !state.Accounts[0].HasPassword {
		t.Fatalf("redacted configuration flags missing: %+v", state)
	}
	for _, kind := range []string{"websites", "accounts"} {
		id := "east"
		body := map[string]any{"id": "east", "name": "Updated site", "base_url": u.server.URL}
		if kind == "accounts" {
			id = "alpha"
			body = map[string]any{"id": "alpha", "website_id": "east", "email": "alpha@example.test", "name": "Updated account"}
		}
		adminTestMutation(t, s, "PUT", "/api/admin/"+kind+"/"+id, body)
		raw, err := os.ReadFile(cfg.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		var stored struct {
			Websites []WebsiteConfig `json:"websites"`
			Accounts []AccountConfig `json:"accounts"`
		}
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		got := stored.Websites[0].ProxyURL
		if kind == "accounts" {
			got = stored.Accounts[0].ProxyURL
		}
		if got != proxySecret {
			t.Errorf("omitted %s proxy changed: %q", kind, got)
		}
		body["proxy_url"] = ""
		adminTestMutation(t, s, "PUT", "/api/admin/"+kind+"/"+id, body)
		raw, err = os.ReadFile(cfg.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		stored.Websites, stored.Accounts = nil, nil
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		got = stored.Websites[0].ProxyURL
		if kind == "accounts" {
			got = stored.Accounts[0].ProxyURL
		}
		if got != "" {
			t.Errorf("explicit empty %s proxy did not clear", kind)
		}
	}
}

func TestAdminValidationStrictJSONAndNoPublication(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := Config{APIKey: testProxyKey, ConfigPath: filepath.Join(t.TempDir(), "config.json")}
	s := adminTestServer(t, cfg, adminTestKey)
	adminTestAddWebsite(t, s, "east", u.server.URL)
	adminTestAddAccount(t, s, "alpha", "east")
	before := adminTestReadState(t, s)
	rawBefore, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/admin/websites", `{"id":"east","name":"Duplicate","base_url":"https://example.test"}`, 409},
		{"POST", "/api/admin/websites", `{"id":"bad site","base_url":"https://example.test"}`, 400},
		{"POST", "/api/admin/websites", `{"id":"bad-url","base_url":"file:///tmp/private"}`, 400},
		{"POST", "/api/admin/accounts", `{"id":"alpha","website_id":"east","email":"x","password":"do-not-leak"}`, 409},
		{"POST", "/api/admin/accounts", `{"id":"new","website_id":"missing","email":"x","password":"do-not-leak"}`, 400},
		{"POST", "/api/admin/accounts", `{"id":"new","website_id":"east","email":"x","password":""}`, 400},
		{"PUT", "/api/admin/accounts/alpha", `{"id":"renamed","website_id":"east","email":"alpha@example.test"}`, 400},
		{"PUT", "/api/admin/websites/east", `{"id":"renamed","base_url":"https://example.test"}`, 400},
		{"POST", "/api/admin/websites", `{"id":"new","base_url":"https://example.test","surprise":"do-not-leak"}`, 400},
		{"PUT", "/api/admin/accounts/alpha", `{"website_id":"east","email":"alpha@example.test","surprise":"do-not-leak"}`, 400},
		{"POST", "/api/admin/websites", `{"id":"new","base_url":"https://example.test"} {}`, 400},
		{"POST", "/api/admin/accounts", `{`, 400},
		{"POST", "/api/admin/websites", `{"id":"new","name":"` + strings.Repeat("x", 1<<20) + `","base_url":"https://example.test"}`, 413},
		{"PUT", "/api/admin/accounts/alpha", `{"name":"` + strings.Repeat("x", 1<<20) + `"}`, 413},
	}
	for _, tc := range cases {
		resp := adminTestRequest(t, s, tc.method, tc.path, tc.body, adminTestKey, nil)
		requireProxyStatus(t, resp, tc.want)
		if strings.Contains(resp.body, "do-not-leak") || strings.Contains(resp.body, "password-alpha") {
			t.Errorf("validation response leaked secret: %s", resp.body)
		}
		raw, err := os.ReadFile(cfg.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(rawBefore) {
			t.Fatalf("invalid mutation modified storage: %s %s", tc.method, tc.path)
		}
	}
	if after := adminTestReadState(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid requests published state: before=%+v after=%+v", before, after)
	}
}

func TestAdminReadOnlyAndFailedWriteDoNotSwap(t *testing.T) {
	u := newProxyTestUpstream(t)
	ro := adminTestServer(t, Config{APIKey: testProxyKey}, adminTestKey)
	if adminTestReadState(t, ro).Persistent {
		t.Error("read-only state claims persistence")
	}
	requireProxyStatus(t, adminTestRequest(t, ro, "POST", "/api/admin/websites", fmt.Sprintf(`{"id":"east","base_url":%q}`, u.server.URL), adminTestKey, nil), 503)
	parent := filepath.Join(t.TempDir(), "storage")
	cfg := Config{APIKey: testProxyKey, ConfigPath: filepath.Join(parent, "config.json")}
	s := adminTestServer(t, cfg, adminTestKey)
	adminTestAddWebsite(t, s, "east", u.server.URL)
	adminTestAddAccount(t, s, "alpha", "east")
	before := adminTestReadState(t, s)
	rawBefore, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	// A regular file as the parent deterministically fails even when tests run as root.
	backup := parent + "-backup"
	if err := os.Rename(parent, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	resp := adminTestRequest(t, s, "PUT", "/api/admin/accounts/alpha", `{"id":"alpha","website_id":"east","email":"alpha@example.test","name":"Unpublished","password":"new-private-password"}`, adminTestKey, nil)
	requireProxyStatus(t, resp, 500)
	if strings.Contains(resp.body, "new-private-password") {
		t.Error("write error leaked password")
	}
	after := adminTestReadState(t, s)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("failed write published state: before=%+v after=%+v", before, after)
	}
	raw, err := os.ReadFile(filepath.Join(backup, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(rawBefore) {
		t.Error("failed write changed old config")
	}
	requireProxyStatus(t, multisiteTestRequest(t, s, "GET", "/v1/models", "", "east", "alpha"), 200)
}

func TestAdminAccountTestIsPinnedAndSanitizesUpstreamError(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.onModel = func(id string, w http.ResponseWriter, _ *http.Request) bool {
		if id == "alpha" {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"error":"upstream-secret-token password-alpha"}`)
			return true
		}
		return false
	}
	cfg := proxyTestConfig(u, "alpha", "beta")
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	s := adminTestServer(t, cfg, adminTestKey)
	resp := adminTestRequest(t, s, "POST", "/api/admin/accounts/alpha/test", "", adminTestKey, map[string]string{"X-Account-ID": "beta", "X-Website-ID": "not-the-selected-site"})
	if resp.status < 400 {
		t.Fatalf("account test silently failed over: %s", resp.body)
	}
	for _, secret := range []string{"upstream-secret-token", "password-alpha", "alpha-token-1"} {
		if strings.Contains(resp.body, secret) {
			t.Errorf("account test leaked %q", secret)
		}
	}
	logins, records := u.snapshot()
	if logins["beta"] != 0 || len(records) != 1 || records[0].ID != "alpha" {
		t.Fatalf("account test was not pinned: %v %v", logins, records)
	}
}

func TestAdminAllDisabledConfigurationAllowed(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := Config{APIKey: testProxyKey, Websites: []WebsiteConfig{{ID: "east", BaseURL: u.server.URL, Disabled: true}}, Accounts: []AccountConfig{{ID: "alpha", WebsiteID: "east", Email: "alpha@example.test", Password: "password-alpha", Disabled: true}}}
	s := adminTestServer(t, cfg, adminTestKey)
	state := adminTestReadState(t, s)
	if len(state.Accounts) != 1 || state.Accounts[0].Enabled || state.Accounts[0].Available || len(state.Websites) != 1 || state.Websites[0].Enabled {
		t.Fatalf("disabled state incorrect: %+v", state)
	}
	requireProxyStatus(t, adminTestRequest(t, s, "GET", "/v1/models", "", testProxyKey, nil), 503)
}

func TestAdminEnabledMutationsPublishAndPersist(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := Config{APIKey: testProxyKey, ConfigPath: filepath.Join(t.TempDir(), "config.json")}
	s := adminTestServer(t, cfg, adminTestKey)
	adminTestAddWebsite(t, s, "east", u.server.URL)
	adminTestAddAccount(t, s, "alpha", "east")
	for _, kind := range []string{"accounts", "websites"} {
		id := "alpha"
		body := map[string]any{"id": "alpha", "website_id": "east", "name": "Alpha", "email": "alpha@example.test"}
		if kind == "websites" {
			id = "east"
			body = map[string]any{"id": "east", "name": "East", "base_url": u.server.URL}
		}
		for _, enabled := range []bool{false, true} {
			body["enabled"] = enabled
			adminTestMutation(t, s, "PUT", "/api/admin/"+kind+"/"+id, body)
			state := adminTestReadState(t, s)
			got := state.Accounts[0].Enabled
			if kind == "websites" {
				got = state.Websites[0].Enabled
			}
			if got != enabled || state.Accounts[0].Available != enabled {
				t.Errorf("%s enabled=%t state=%+v", kind, enabled, state)
			}
			raw, err := os.ReadFile(cfg.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			var stored struct {
				Accounts []AccountConfig `json:"accounts"`
				Websites []WebsiteConfig `json:"websites"`
			}
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			disabled := stored.Accounts[0].Disabled
			if kind == "websites" {
				disabled = stored.Websites[0].Disabled
			}
			if disabled == enabled {
				t.Errorf("%s enabled flag persisted incorrectly", kind)
			}
			want := 503
			if enabled {
				want = 200
			}
			requireProxyStatus(t, adminTestRequest(t, s, "GET", "/v1/models", "", testProxyKey, nil), want)
		}
	}
}

func TestAdminConcurrentCRUDDoesNotLoseUpdates(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := Config{APIKey: testProxyKey, ConfigPath: filepath.Join(t.TempDir(), "config.json")}
	s := adminTestServer(t, cfg, adminTestKey)
	adminTestAddWebsite(t, s, "east", u.server.URL)
	const count = 12
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			adminTestAddAccount(t, s, fmt.Sprintf("account-%02d", i), "east")
			_ = adminTestReadState(t, s)
		}(i)
	}
	wg.Wait()
	state := adminTestReadState(t, s)
	if len(state.Accounts) != count {
		t.Fatalf("concurrent creates lost updates: got=%d want=%d", len(state.Accounts), count)
	}
	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Accounts []AccountConfig `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil || len(stored.Accounts) != count {
		t.Fatalf("persisted concurrent creates: accounts=%d err=%v", len(stored.Accounts), err)
	}
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			adminTestMutation(t, s, "DELETE", fmt.Sprintf("/api/admin/accounts/account-%02d", i), nil)
		}(i)
	}
	wg.Wait()
	if state := adminTestReadState(t, s); len(state.Accounts) != 0 {
		t.Errorf("concurrent deletes left %d accounts", len(state.Accounts))
	}
}

// TestAdminLibreChatAccountDropsCredentials guards the LibreChat contract: the
// backend has no email/password pair, so switching an account to it must clear
// any stored credentials instead of persisting dead secrets.
func TestAdminLibreChatAccountDropsCredentials(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "owui")
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	s := adminTestServer(t, cfg, adminTestKey)
	adminTestAddWebsite(t, s, "lc", u.server.URL)
	adminTestMutation(t, s, "PUT", "/api/admin/accounts/owui", map[string]any{
		"website_id": "lc",
		"backend":    "librechat",
		"api_key":    "k12345678",
		"email":      "bot@example.test",
		"password":   "hidden",
	})
	state := adminTestReadState(t, s)
	for _, account := range state.Accounts {
		if account.ID != "owui" {
			continue
		}
		if account.Email != "" || account.HasPassword {
			t.Fatalf("librechat account kept credentials: %+v", account)
		}
		if !account.HasAPIKey {
			t.Fatalf("librechat account lost its api key: %+v", account)
		}
		return
	}
	t.Fatalf("owui missing from state")
}

// TestAdminFaviconAvoidsUnauthorized keeps the browser's automatic favicon
// request out of the API path: without this it 401s and clutters the console.
func TestAdminFaviconAvoidsUnauthorized(t *testing.T) {
	u := newProxyTestUpstream(t)
	s := adminTestServer(t, proxyTestConfig(u, "acc"), adminTestKey)
	resp := adminTestRequest(t, s, http.MethodGet, "/favicon.ico", "", "", nil)
	if resp.status != http.StatusNoContent {
		t.Fatalf("favicon status = %d, want %d", resp.status, http.StatusNoContent)
	}
}

// TestWebUIShipsNoInlineStyle guards the CSP change: inline styles are allowed
// for extension compatibility, but the console itself must not need them.
func TestWebUIShipsNoInlineStyle(t *testing.T) {
	for _, name := range []string{"web/index.html", "web/app.js"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), "style=\"") || strings.Contains(string(data), ".style.") {
			t.Errorf("%s uses inline styles; keep CSP meaningful", name)
		}
	}
}
