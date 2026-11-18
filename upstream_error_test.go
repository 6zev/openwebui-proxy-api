package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The caller must see the upstream's own reason. A spent credit balance used to
// surface as a bare "Open WebUI returned HTTP 400", which reads like a malformed
// request.
func TestUpstreamErrorDetailIsForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"You have no credits remaining. Add credits to continue using the API."}`)
	}))
	defer upstream.Close()
	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	s := newProxyTestServer(t, cfg)

	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusBadRequest)
	if !strings.Contains(resp.body, "no credits remaining") {
		t.Errorf("upstream reason not forwarded: %s", resp.body)
	}
	if !strings.Contains(resp.body, `"upstream"`) {
		t.Errorf("structured upstream field missing: %s", resp.body)
	}
}

// The extractor must cope with the shapes upstreams actually send.
func TestUpstreamDetailShapes(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"detail string", `{"detail":"no credits"}`, "no credits"},
		{"error object", `{"error":{"message":"rate limited"}}`, "rate limited"},
		{"validation list", `{"detail":[{"msg":"tools: List should have at least 1 item"}]}`, "tools: List should have at least 1 item"},
		{"empty", ``, ""},
		{"not json", `<html>502</html>`, ""},
		{"json without reason", `{"ok":true}`, ""},
	}
	for _, test := range cases {
		if got := upstreamDetail([]byte(test.body)); got != test.want {
			t.Errorf("%s: got %q, want %q", test.name, got, test.want)
		}
	}
	// Upstream text is flattened and bounded before it reaches a client.
	long := `{"detail":"` + strings.Repeat("x ", 500) + `"}`
	if got := upstreamDetail([]byte(long)); strings.Contains(got, "\n") || len(got) > 410 {
		t.Errorf("detail not sanitised: len=%d", len(got))
	}
}

// A balance rejection belongs to the account: the next account must be tried.
func TestQuotaRejectionFailsOverToAnotherAccount(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"ta"}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"You have no credits remaining."}`)
	}))
	defer broken.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"tb"}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer healthy.Close()

	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: 30 * time.Second,
		Websites: []WebsiteConfig{
			{ID: "site-a", BaseURL: broken.URL},
			{ID: "site-b", BaseURL: healthy.URL},
		},
		Accounts: []AccountConfig{
			{ID: "acc-a", WebsiteID: "site-a", Email: "a@example.test", Password: "pa"},
			{ID: "acc-b", WebsiteID: "site-b", Email: "b@example.test", Password: "pb"},
		},
	}
	s := newProxyTestServer(t, cfg)

	// Whichever account round-robin picks first, the call must succeed on the
	// other one instead of returning the credit error.
	for i := 0; i < 4; i++ {
		resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
		requireProxyStatus(t, resp, http.StatusOK)
	}
	// Pinning the drained account must NOT fail over; the caller asked for it. The
	// account is cooling down after its quota rejection, so the honest answer is
	// 429 "cooling down" with Retry-After — never a silent switch to another site.
	pinned := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "acc-a")
	if pinned.status != http.StatusTooManyRequests && pinned.status != http.StatusBadRequest {
		t.Fatalf("pinned status = %d: %s", pinned.status, pinned.body)
	}
	if pinned.header.Get("Retry-After") == "" && pinned.status == http.StatusTooManyRequests {
		t.Error("cooling-down reply must carry Retry-After")
	}
}

// The console needs per-website request counts.
func TestUsageRecordsPerWebsite(t *testing.T) {
	stats := newUsageStats()
	now := int64(1000)
	stats.minute = func() int64 { return now }
	stats.record("site-a", 200, "m", "")
	stats.record("site-a", 200, "m", "")
	stats.record("site-a", 400, "m", "no credits")
	stats.record("site-b", 200, "m", "")

	data := stats.snapshot()
	if len(data) != 2 {
		t.Fatalf("sites = %d, want 2", len(data))
	}
	first := data[0]
	if first["website"] != "site-a" || first["success"].(int64) != 2 || first["failed"].(int64) != 1 {
		t.Errorf("site-a totals wrong: %+v", first)
	}
	if first["last_error"] != "no credits" {
		t.Errorf("last error missing: %+v", first)
	}
	minutes, _ := first["minutes"].([]usageBucket)
	if len(minutes) != usageWindow || minutes[usageWindow-1].Success != 2 {
		t.Errorf("bucket series wrong: len=%d series=%+v", len(minutes), minutes)
	}
	// Buckets older than the window are dropped.
	now += usageWindow + 5
	stats.record("site-a", 200, "m", "")
	if got := len(stats.bySite["site-a"]); got != 1 {
		t.Errorf("stale buckets kept: %d", got)
	}
}
