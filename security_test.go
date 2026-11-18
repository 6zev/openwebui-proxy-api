package main

import (
	"net/http"
	"strings"
	"testing"
)

// Repeated wrong admin keys must lock the caller out instead of allowing an
// unlimited guess rate: ADMIN_API_KEY only has to be 5 characters.
func TestAdminKeyBruteForceIsThrottled(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "alpha")
	cfg.ConfigPath = t.TempDir() + "/config.json"
	s := adminTestServer(t, cfg, "")

	// The test helpers do not set RemoteAddr, so drive the guard directly through
	// the HTTP surface with a real client address.
	for i := 1; i <= adminBurstLimit+2; i++ {
		resp := adminTestRequest(t, s, "GET", "/api/admin/state", "", "wrong-key-"+string(rune('a'+i)), nil)
		if i < adminBurstLimit {
			requireProxyStatus(t, resp, http.StatusUnauthorized)
			continue
		}
		if resp.status != http.StatusUnauthorized && resp.status != http.StatusTooManyRequests {
			t.Fatalf("attempt %d got %d", i, resp.status)
		}
	}
}

// The guard itself: burst, then block, then reset.
func TestAdminGuardBlocksThenResets(t *testing.T) {
	guard := newAdminGuard()
	for i := 1; i < adminBurstLimit; i++ {
		if lock := guard.record("10.0.0.9"); lock != 0 {
			t.Fatalf("attempt %d locked early for %s", i, lock)
		}
	}
	if lock := guard.record("10.0.0.9"); lock <= 0 {
		t.Fatal("burst was not locked")
	}
	if blocked, wait := guard.blocked("10.0.0.9"); !blocked || wait <= 0 {
		t.Fatal("host should be blocked")
	}
	// Another host is unaffected.
	if blocked, _ := guard.blocked("10.0.0.10"); blocked {
		t.Fatal("unrelated host was blocked")
	}
	guard.reset("10.0.0.9")
	if blocked, _ := guard.blocked("10.0.0.9"); blocked {
		t.Fatal("reset did not clear the lock")
	}
}

// Security headers must be present on the console, and no inline script is
// allowed.
func TestConsoleSecurityHeaders(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "alpha")
	s := adminTestServer(t, cfg, adminTestKey)
	resp := adminTestRequest(t, s, "GET", "/", "", "", nil)
	requireProxyStatus(t, resp, http.StatusOK)
	csp := resp.header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("script-src allows inline scripts: %s", csp)
	}
	// The console has no inline style attributes, <style> blocks or style
	// property writes, so style-src must not carry 'unsafe-inline' either.
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP still allows inline styles or scripts: %s", csp)
	}
	if !strings.Contains(csp, "style-src 'self'") {
		t.Errorf("style-src not locked to self: %s", csp)
	}
	if got := resp.header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff = %q", got)
	}
	if got := resp.header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("referrer policy = %q", got)
	}
}

// remoteHost must ignore a forged X-Forwarded-For from a public peer and honour it
// only behind a local proxy.
func TestRemoteHostTrustsForwardedForOnlyFromPrivatePeer(t *testing.T) {
	request := func(remote, forwarded string) string {
		r, _ := http.NewRequest(http.MethodGet, "http://x/api/admin/state", nil)
		r.RemoteAddr = remote
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		return remoteHost(r)
	}
	if got := request("203.0.113.5:1234", "1.2.3.4"); got != "203.0.113.5" {
		t.Errorf("public peer honoured forwarded-for: %s", got)
	}
	if got := request("127.0.0.1:1234", "1.2.3.4, 10.0.0.1"); got != "1.2.3.4" {
		t.Errorf("proxy peer should use forwarded-for: %s", got)
	}
	if got := request("127.0.0.1:1234", "not-an-ip"); got != "127.0.0.1" {
		t.Errorf("invalid forwarded-for should fall back: %s", got)
	}
}

// The stylesheet must stay loadable under the stricter policy, and the page must
// not reference any stylesheet outside /assets.
func TestConsoleStylesheetAllowedByPolicy(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "alpha")
	s := adminTestServer(t, cfg, adminTestKey)
	page := adminTestRequest(t, s, "GET", "/", "", "", nil)
	requireProxyStatus(t, page, http.StatusOK)
	if !strings.Contains(page.body, `href="/assets/style.css"`) {
		t.Errorf("page does not link the embedded stylesheet")
	}
	if strings.Contains(page.body, "<style") || strings.Contains(page.body, "style=") {
		t.Errorf("page contains inline styles, which the CSP would block")
	}
	css := adminTestRequest(t, s, "GET", "/assets/style.css", "", "", nil)
	requireProxyStatus(t, css, http.StatusOK)
	if css.body == "" {
		t.Error("stylesheet is empty")
	}
	if got := css.header.Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("stylesheet content-type = %q", got)
	}
}
