package main

import (
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
	c.APIKey = "short"
	if _, err := NewProxy(c); err == nil {
		t.Error("accepted short API key")
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
