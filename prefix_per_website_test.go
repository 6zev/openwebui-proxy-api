package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Each model must be qualified by the website that actually serves it. Taking the
// prefix from candidates[0] stamped one website's name onto every website's
// models, which produced ids like "ai.baselgovernance.org/gpt-5.6-luna" for a
// model only the other site had — clicking it hit a 400 "Model not found".
func TestModelListPrefixesEachModelWithItsOwnWebsite(t *testing.T) {
	mk := func(models string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auths/signin" {
				_, _ = io.WriteString(w, `{"token":"t"}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":`+models+`}`)
		}))
	}
	serverA := mk(`[{"id":"gpt-5.6-luna"},{"id":"openai/gpt-5.6-luna"},{"id":"only-a"}]`)
	defer serverA.Close()
	serverB := mk(`[{"id":"claude-sonnet-5"}]`)
	defer serverB.Close()

	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second,
		ModelCacheTTL: time.Minute, ModelPrefix: true,
		Websites: []WebsiteConfig{
			{ID: "site-a", BaseURL: serverA.URL},
			{ID: "site-b", BaseURL: serverB.URL},
		},
		Accounts: []AccountConfig{
			{ID: "acc-a", WebsiteID: "site-a", Email: "a@example.test", Password: "pa"},
			{ID: "acc-b", WebsiteID: "site-b", Email: "b@example.test", Password: "pb"},
		},
	}
	s := newProxyTestServer(t, cfg)

	var body string
	for i := 0; i < 4; i++ {
		resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
		requireProxyStatus(t, resp, 200)
		body = resp.body
	}

	for _, want := range []string{
		`"site-a/gpt-5.6-luna"`,
		`"site-a/openai/gpt-5.6-luna"`,
		`"site-a/only-a"`,
		`"site-b/claude-sonnet-5"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("listing missing %s:\n%s", want, body)
		}
	}
	// The bug: site-b's model labelled as site-a, or site-a's model labelled site-b.
	for _, bad := range []string{`"site-b/gpt-5.6-luna"`, `"site-b/only-a"`, `"site-a/claude-sonnet-5"`} {
		if strings.Contains(body, bad) {
			t.Errorf("model labelled with the wrong website: %s", bad)
		}
	}
}

// A single-account deployment still prefixes, using that account's website.
func TestModelListPrefixesSingleAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"solo"}]}`)
	}))
	defer upstream.Close()
	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.ModelPrefix = true
	cfg.Websites = []WebsiteConfig{{ID: "site-a", BaseURL: upstream.URL}}
	cfg.Accounts = []AccountConfig{{ID: "acc-a", WebsiteID: "site-a", Email: "a@example.test", Password: "p"}}
	cfg.Timeout = 5 * time.Second
	s := newProxyTestServer(t, cfg)
	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if !strings.Contains(resp.body, `"site-a/solo"`) {
		t.Errorf("expected site-a/solo, got %s", resp.body)
	}
}
