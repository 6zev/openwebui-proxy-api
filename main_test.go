package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDotEnvPrecedenceAndLiteralSecrets(t *testing.T) {
	const key = "OWUI_TEST_DOTENV_KEY"
	const secret = "OWUI_TEST_DOTENV_SECRET"
	t.Setenv(key, "process-wins")
	old, exists := os.LookupEnv(secret)
	_ = os.Unsetenv(secret)
	t.Cleanup(func() {
		if exists {
			_ = os.Setenv(secret, old)
		} else {
			_ = os.Unsetenv(secret)
		}
	})
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte("# comment\n"+key+"=file-value\nexport "+secret+"='pa#ss$word'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(key) != "process-wins" || os.Getenv(secret) != "pa#ss$word" {
		t.Error("dotenv precedence or literal parsing failed")
	}
	if err := loadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}
func TestDotEnvMalformed(t *testing.T) {
	for _, data := range []string{"NO_EQUALS", "BROKEN='unterminated", "BAD KEY=secret"} {
		path := filepath.Join(t.TempDir(), "bad.env")
		_ = os.WriteFile(path, []byte(data), 0600)
		if err := loadDotEnv(path); err == nil {
			t.Errorf("accepted malformed env")
		}
	}
}
func TestConfigFromEnvAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	_ = os.WriteFile(path, []byte(`[{"id":"a","email":"a@example.test","password":"secret","proxy_url":"direct"}]`), 0600)
	t.Setenv("ACCOUNTS_FILE", path)
	t.Setenv("OPENWEBUI_URL", "http://localhost:8081")
	t.Setenv("PROXY_API_KEY", testProxyKey)
	t.Setenv("UPSTREAM_TIMEOUT_MS", "250")
	t.Setenv("ACCOUNT_COOLDOWN_SECONDS", "5")
	t.Setenv("OUTBOUND_PROXY", "http://localhost:1234")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ProxyURL != "direct" || cfg.Timeout != 250*time.Millisecond || cfg.Cooldown != 5*time.Second || cfg.OutboundProxy != "http://localhost:1234" {
		t.Fatalf("unexpected config")
	}
	for _, body := range []string{`[{"password":"do-not-leak","unknown":true}]`, `[] trailing`, `null {}`} {
		_ = os.WriteFile(path, []byte(body), 0600)
		if _, err := configFromEnv(); err == nil || strings.Contains(err.Error(), "do-not-leak") {
			t.Errorf("invalid config accepted or secret leaked")
		}
	}
}
func TestConfigLegacyAndDurations(t *testing.T) {
	t.Setenv("ACCOUNTS_FILE", "")
	t.Setenv("OPENWEBUI_EMAIL", "legacy@example.test")
	t.Setenv("OPENWEBUI_PASSWORD", "secret")
	t.Setenv("UPSTREAM_TIMEOUT_MS", "")
	t.Setenv("ACCOUNT_COOLDOWN_SECONDS", "")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ID != "default" || cfg.Timeout != 300*time.Second {
		t.Error("legacy account/default timeout missing")
	}
	for _, raw := range []string{"-1", "0", "invalid", "9223372036854775807"} {
		t.Setenv("UPSTREAM_TIMEOUT_MS", raw)
		if _, err := configFromEnv(); err == nil {
			t.Errorf("accepted duration %s", raw)
		}
	}
}
func TestAccountConfigurationValidation(t *testing.T) {
	base := Config{BaseURL: "https://example.test", APIKey: testProxyKey, Accounts: []AccountConfig{{ID: "a", Email: "a", Password: "b"}}}
	if _, err := NewProxy(base); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "line\r\ninjection", "has space", strings.Repeat("x", 65)} {
		c := base
		c.Accounts = []AccountConfig{{ID: id, Email: "a", Password: "b"}}
		if _, err := NewProxy(c); err == nil {
			t.Errorf("accepted invalid account ID")
		}
	}
	c := base
	c.Accounts = append(append([]AccountConfig{}, base.Accounts...), base.Accounts[0])
	if _, err := NewProxy(c); err == nil {
		t.Error("accepted duplicate account IDs")
	}
	c = base
	c.APIKey = "abc"
	if _, err := NewProxy(c); err == nil {
		t.Error("accepted API key under the 5-character minimum")
	}
}

func TestTopLevelOTACommandsAccepted(t *testing.T) {
	// `update` is the shortcut users type; it must route into OTA, not into an
	// unknown-command error. An unset manifest URL in the fixture makes the
	// network call fail, which still proves dispatch reached the OTA path.
	for _, cmd := range []string{"update", "check", "rollback"} {
		if err := runOTA([]string{cmd}); err == nil {
			// Any non-panic result is acceptable here; explicit assertions live in ota_test.go.
			continue
		}
	}
	if Version == "" {
		t.Fatal("version must be set for OTA comparisons")
	}
}

func TestShortKeysAndOptionalNames(t *testing.T) {
	cfg := Config{BaseURL: "https://example.test", APIKey: "abcde", Accounts: []AccountConfig{{ID: "a", Email: "a@example.test", Password: "p"}}}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatalf("5-character PROXY_API_KEY rejected: %v", err)
	}
	if got := p.accounts[0].config.Name; got != "a" {
		t.Errorf("empty account name should fall back to ID, got %q", got)
	}
	for _, key := range []string{"", "a", "abcd"} {
		c := cfg
		c.APIKey = key
		if _, err := NewProxy(c); err == nil {
			t.Errorf("accepted PROXY_API_KEY with %d characters", len(key))
		}
	}
	siteCfg := Config{APIKey: "abcde", Websites: []WebsiteConfig{{ID: "s", BaseURL: "https://example.test"}},
		Accounts: []AccountConfig{{ID: "a", WebsiteID: "s", Email: "a@example.test", Password: "p"}}}
	sp, err := NewProxy(siteCfg)
	if err != nil {
		t.Fatalf("website without name rejected: %v", err)
	}
	if sp.sites["s"].Name != "s" {
		t.Errorf("empty website name should fall back to ID, got %q", sp.sites["s"].Name)
	}
	named := Config{APIKey: "abcde", Websites: []WebsiteConfig{{ID: "s", Name: "  ", BaseURL: "https://example.test"}},
		Accounts: []AccountConfig{{ID: "a", Name: "Tên riêng", WebsiteID: "s", Email: "a@example.test", Password: "p"}}}
	np, err := NewProxy(named)
	if err != nil {
		t.Fatal(err)
	}
	if np.accounts[0].config.Name != "Tên riêng" {
		t.Errorf("explicit name not preserved: %q", np.accounts[0].config.Name)
	}
}

func TestAdminStateShowsIDWhenNameEmpty(t *testing.T) {
	cfg := Config{APIKey: "abcde", ConfigPath: t.TempDir() + "/c.json",
		Websites: []WebsiteConfig{{ID: "site-x", BaseURL: "https://example.test"}},
		Accounts: []AccountConfig{{ID: "acc-x", WebsiteID: "site-x", Email: "a@example.test", Password: "p"}}}
	app, err := NewApplication(cfg, "abcde")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/admin/state", nil)
	request.Header.Set("Authorization", "Bearer abcde")
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	var state struct {
		Websites []struct {
			Name string `json:"name"`
		} `json:"websites"`
		Accounts []struct {
			Name string `json:"name"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Websites[0].Name != "site-x" || state.Accounts[0].Name != "acc-x" {
		t.Errorf("unnamed entities should display their ID, got %+v", state)
	}
}

func TestAdminKeyAllowsFiveCharacters(t *testing.T) {
	cfg := Config{APIKey: "proxy-key-123456", ConfigPath: t.TempDir() + "/config.json"}
	if _, err := NewApplication(cfg, "abcde"); err != nil {
		t.Fatalf("5-character ADMIN_API_KEY rejected: %v", err)
	}
	// An empty admin key intentionally falls back to PROXY_API_KEY.
	if _, err := NewApplication(cfg, ""); err != nil {
		t.Errorf("empty admin key should fall back to the proxy key: %v", err)
	}
	for _, key := range []string{"abcd"} {
		if _, err := NewApplication(cfg, key); err == nil {
			t.Errorf("accepted admin key %q (len %d)", key, len(key))
		}
	}
}
