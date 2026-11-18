package main

import (
	"context"
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
	// Each website's entry keeps its own prefix: "site-a/shared" and
	// "site-b/shared" are different reachable models, so both are listed.
	for _, want := range []string{`"site-a/shared"`, `"site-b/shared"`} {
		if !strings.Contains(third.body, want) {
			t.Errorf("listing missing %s: %s", want, third.body)
		}
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

// The listing must not depend on which account happened to be warm: a cache that
// holds only one website's entry must be completed before answering, otherwise
// callers see website A on one call and website B on the next.
func TestModelListNeverServedPartial(t *testing.T) {
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
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Warm only one account, as a single early request or a refresher tick would.
	p.refreshModelEntry(context.Background(), p.accounts[0])
	if list := p.mergedModelList(p.accounts); len(list.missing) != 1 {
		t.Fatalf("expected one missing account, got %d", len(list.missing))
	}

	s := newProxyTestServer(t, cfg)
	for i := 0; i < 4; i++ {
		resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
		requireProxyStatus(t, resp, 200)
		if !strings.Contains(resp.body, "only-a") || !strings.Contains(resp.body, "only-b") {
			t.Fatalf("call %d served a partial listing: %s", i, resp.body)
		}
	}
}
