package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A round-robin deployment must advertise the union of every reachable account's
// models on every call, not whichever account round-robin happened to pick.
func TestModelListIsUnionAcrossWebsites(t *testing.T) {
	var hitsA, hitsB int
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"ta"}`)
			return
		}
		hitsA++
		_, _ = io.WriteString(w, `{"data":[{"id":"shared"},{"id":"only-a"}]}`)
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"tb"}`)
			return
		}
		hitsB++
		_, _ = io.WriteString(w, `{"data":[{"id":"shared"},{"id":"only-b"}]}`)
	}))
	defer upstreamB.Close()

	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second,
		ModelCacheTTL: time.Minute,
		Websites: []WebsiteConfig{
			{ID: "site-a", BaseURL: upstreamA.URL},
			{ID: "site-b", BaseURL: upstreamB.URL},
		},
		Accounts: []AccountConfig{
			{ID: "acc-a", WebsiteID: "site-a", Email: "a@example.test", Password: "pa"},
			{ID: "acc-b", WebsiteID: "site-b", Email: "b@example.test", Password: "pb"},
		},
	}
	s := newProxyTestServer(t, cfg)

	// First call live-fetches whichever account round-robin selects, then merges.
	first := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, first, 200)
	if hitsA == 0 && hitsB == 0 {
		t.Fatal("no upstream was queried")
	}
	// Second call fills in the other account (still one live fetch, one from cache).
	second := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, second, 200)

	// Third call must be served from cache alone and contain both websites.
	beforeA, beforeB := hitsA, hitsB
	third := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, third, 200)
	if hitsA != beforeA || hitsB != beforeB {
		t.Errorf("cache did not serve the merged list (hits %d/%d -> %d/%d)", beforeA, beforeB, hitsA, hitsB)
	}
	for _, want := range []string{"only-a", "only-b", "shared"} {
		if !strings.Contains(third.body, want) {
			t.Errorf("merged listing missing %q: %s", want, third.body)
		}
	}
	if strings.Count(third.body, `"shared"`) != 1 {
		t.Errorf("duplicate model leaked into the listing: %s", third.body)
	}
}

// A pinned account still returns its own models only.
func TestModelListPinnedAccountUnchanged(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"ta"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"only-a"}]}`)
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"tb"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"only-b"}]}`)
	}))
	defer upstreamB.Close()
	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second,
		Websites: []WebsiteConfig{{ID: "site-a", BaseURL: upstreamA.URL}, {ID: "site-b", BaseURL: upstreamB.URL}},
		Accounts: []AccountConfig{
			{ID: "acc-a", WebsiteID: "site-a", Email: "a@example.test", Password: "pa"},
			{ID: "acc-b", WebsiteID: "site-b", Email: "b@example.test", Password: "pb"},
		},
	}
	s := newProxyTestServer(t, cfg)
	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "acc-b")
	requireProxyStatus(t, resp, 200)
	if !strings.Contains(resp.body, "only-b") || strings.Contains(resp.body, "only-a") {
		t.Errorf("pinned listing wrong: %s", resp.body)
	}
}
