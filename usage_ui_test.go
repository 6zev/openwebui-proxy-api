package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The console must ship the usage card and the endpoint it reads.
func TestConsoleShipsUsageCard(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "alpha")
	s := adminTestServer(t, cfg, adminTestKey)

	page := adminTestRequest(t, s, "GET", "/", "", "", nil)
	requireProxyStatus(t, page, http.StatusOK)
	for _, want := range []string{"usage-output", "usage-refresh", "Lưu lượng theo website"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	js := adminTestRequest(t, s, "GET", "/assets/app.js", "", "", nil)
	requireProxyStatus(t, js, http.StatusOK)
	if !strings.Contains(js.body, "/api/admin/usage") {
		t.Error("app.js does not call the usage endpoint")
	}
	if !strings.Contains(js.body, "textContent") {
		t.Error("usage rendering should use textContent, not innerHTML, for upstream text")
	}

	usage := adminTestRequest(t, s, "GET", "/api/admin/usage", "", adminTestKey, nil)
	requireProxyStatus(t, usage, http.StatusOK)
	var payload struct {
		WindowMinutes int   `json:"window_minutes"`
		Data          []any `json:"data"`
	}
	if err := json.Unmarshal([]byte(usage.body), &payload); err != nil {
		t.Fatalf("usage payload not JSON: %v", err)
	}
	if payload.WindowMinutes != usageWindow {
		t.Errorf("window = %d, want %d", payload.WindowMinutes, usageWindow)
	}
}

// Requests must be counted against the website that served them.
func TestUsageEndpointReflectsTraffic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	s := adminTestServer(t, cfg, adminTestKey)

	proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"alpha/m","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"alpha/m","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")

	usage := adminTestRequest(t, s, "GET", "/api/admin/usage", "", adminTestKey, nil)
	requireProxyStatus(t, usage, http.StatusOK)
	if !strings.Contains(usage.body, `"success":2`) {
		t.Errorf("traffic not counted: %s", usage.body)
	}
}
