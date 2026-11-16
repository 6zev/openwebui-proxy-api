package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	// Keep the admin ring buffer fed in tests, exactly like main does.
	log.SetOutput(io.MultiWriter(&buf, debugLogs))
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

// Debug logging must record the routed upstream call and the rejected body, and
// must never leak credentials.
func TestDebugLogCapturesUpstreamError(t *testing.T) {
	buf := captureLog(t)
	u := newProxyTestUpstream(t)
	u.onChat = func(_ string, _ int, w http.ResponseWriter, _ *http.Request, _ map[string]any) bool {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"reasoning is not supported"}`)
		return true
	}
	cfg := proxyTestConfig(u, "alpha")
	cfg.DebugLog = true
	s := newProxyTestServer(t, cfg)
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, ""), http.StatusBadRequest)
	out := buf.String()
	for _, want := range []string{"debug: POST /api/chat/completions", "status=400", "reasoning is not supported"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug log missing %q; got:\n%s", want, out)
		}
	}
	for _, secret := range []string{testProxyKey, "password-alpha", "alpha-token-1"} {
		if strings.Contains(out, secret) {
			t.Errorf("debug log leaked %q", secret)
		}
	}
}

// Without DEBUG_LOG the router must stay silent.
func TestDebugLogOffIsQuiet(t *testing.T) {
	buf := captureLog(t)
	u := newProxyTestUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, ""), http.StatusOK)
	if strings.Contains(buf.String(), "debug:") {
		t.Errorf("debug lines logged while disabled:\n%s", buf.String())
	}
}

// A Responses payload carrying fields Open WebUI rejects must be called out,
// because upstream only answers 400.
func TestDebugWarnsAboutUnsupportedResponsesFields(t *testing.T) {
	buf := captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.DebugLog = true
	s := newProxyTestServer(t, cfg)
	body := `{"model":"m","input":"hi","reasoning":{"effort":"low"},"store":false}`
	proxyTestRequest(t, s, http.MethodPost, "/openai/responses", body, testProxyKey, "")
	out := buf.String()
	if !strings.Contains(out, "WARNING /openai/responses") || !strings.Contains(out, "reasoning") || !strings.Contains(out, "store") {
		t.Errorf("missing Responses warning; got:\n%s", out)
	}
}

// The admin console reads the ring buffer through /api/admin/logs.
func TestAdminLogsEndpoint(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "alpha")
	cfg.ConfigPath = t.TempDir() + "/config.json"
	cfg.DebugLog = true
	app, err := NewApplication(cfg, adminTestKey)
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(app)
	defer proxyServer.Close()
	requireProxyStatus(t, proxyTestRequest(t, proxyServer, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, ""), http.StatusOK)
	adminServer := httptest.NewServer(app)
	defer adminServer.Close()
	resp := adminTestRequest(t, adminServer, "GET", "/api/admin/logs", "", adminTestKey, nil)
	requireProxyStatus(t, resp, http.StatusOK)
	if !strings.Contains(resp.body, "lines") || !strings.Contains(resp.body, "/api/chat/completions") {
		t.Errorf("logs endpoint body = %s", resp.body)
	}
	if strings.Contains(resp.body, testProxyKey) {
		t.Error("logs endpoint leaked the proxy key")
	}
}
