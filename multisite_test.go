package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func multisiteTestConfig(a, b *proxyTestUpstream) Config {
	return Config{APIKey: testProxyKey, Timeout: time.Second, Cooldown: time.Second,
		Websites: []WebsiteConfig{{ID: "east", Name: "East", BaseURL: a.server.URL}, {ID: "west", Name: "West", BaseURL: b.server.URL}},
		Accounts: []AccountConfig{
			{ID: "alpha", WebsiteID: "east", Email: "alpha@example.test", Password: "password-alpha"},
			{ID: "beta", WebsiteID: "east", Email: "beta@example.test", Password: "password-beta"},
			{ID: "gamma", WebsiteID: "west", Email: "gamma@example.test", Password: "password-gamma"},
		},
	}
}

func multisiteTestRequest(t *testing.T, s *httptest.Server, method, path, body, site, account string) proxyTestResponse {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testProxyKey)
	req.Header.Set("Content-Type", "application/json")
	if site != "" {
		req.Header.Set("X-Website-ID", site)
	}
	if account != "" {
		req.Header.Set("X-Account-ID", account)
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

func TestMultisiteWebsiteSelectionAndIndependentRoundRobin(t *testing.T) {
	east, west := newProxyTestUpstream(t), newProxyTestUpstream(t)
	s := newProxyTestServer(t, multisiteTestConfig(east, west))
	for n := 0; n < 6; n++ {
		// Interleaving another site's requests must not advance east's cursor.
		for _, site := range []string{"east", "west"} {
			resp := multisiteTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, site, "")
			requireProxyStatus(t, resp, 200)
			want := "gamma"
			if site == "east" {
				want = []string{"alpha", "beta"}[n%2]
			}
			if resp.header.Get("X-Account-ID") != want || resp.header.Get("X-Website-ID") != site {
				t.Fatalf("site %s request %d headers=%v, want account=%s", site, n, resp.header, want)
			}
		}
	}
	for _, tc := range []struct {
		u   *proxyTestUpstream
		ids map[string]bool
	}{{east, map[string]bool{"alpha": true, "beta": true}}, {west, map[string]bool{"gamma": true}}} {
		_, records := tc.u.snapshot()
		for _, r := range records {
			if !tc.ids[r.ID] {
				t.Errorf("cross-site credential routing: %+v", r)
			}
		}
	}
}

func TestMultisitePinnedAccountAndInvalidSelectors(t *testing.T) {
	east, west := newProxyTestUpstream(t), newProxyTestUpstream(t)
	s := newProxyTestServer(t, multisiteTestConfig(east, west))
	for _, site := range []string{"", "west"} {
		resp := multisiteTestRequest(t, s, "GET", "/v1/models", "", site, "gamma")
		requireProxyStatus(t, resp, 200)
		if resp.header.Get("X-Website-ID") != "west" || resp.header.Get("X-Account-ID") != "gamma" {
			t.Fatalf("pin lost actual website: %v", resp.header)
		}
	}
	for _, tc := range []struct{ site, account string }{{"east", "gamma"}, {"missing", ""}, {"missing", "alpha"}, {"east", "missing"}} {
		requireProxyStatus(t, multisiteTestRequest(t, s, "GET", "/v1/models", "", tc.site, tc.account), 400)
	}
	el, er := east.snapshot()
	wl, wr := west.snapshot()
	if len(el) != 0 || len(er) != 0 || wl["gamma"] != 1 || len(wr) != 2 {
		t.Fatalf("invalid selectors reached upstream: east=%v/%v west=%v/%v", el, er, wl, wr)
	}
}

func TestMultisiteDisabledAccountsAndWebsitesSkipped(t *testing.T) {
	east, west := newProxyTestUpstream(t), newProxyTestUpstream(t)
	cfg := multisiteTestConfig(east, west)
	cfg.Accounts[0].Disabled = true
	cfg.Websites[1].Disabled = true
	s := newProxyTestServer(t, cfg)
	for n := 0; n < 4; n++ {
		resp := multisiteTestRequest(t, s, "GET", "/v1/models", "", "", "")
		requireProxyStatus(t, resp, 200)
		if resp.header.Get("X-Account-ID") != "beta" || resp.header.Get("X-Website-ID") != "east" {
			t.Fatalf("selected disabled config: %v", resp.header)
		}
	}
	for _, tc := range []struct{ site, account string }{{"", "alpha"}, {"", "gamma"}, {"west", ""}} {
		resp := multisiteTestRequest(t, s, "GET", "/v1/models", "", tc.site, tc.account)
		if resp.status >= 200 && resp.status < 300 {
			t.Errorf("disabled selector accepted: %+v", tc)
		}
	}
	logins, _ := east.snapshot()
	wl, wr := west.snapshot()
	if logins["alpha"] != 0 || len(wl) != 0 || len(wr) != 0 {
		t.Errorf("disabled credentials reached upstream: east=%v west=%v/%v", logins, wl, wr)
	}
}

func TestMultisiteFailoverStaysWithinSelectedWebsite(t *testing.T) {
	east, west := newProxyTestUpstream(t), newProxyTestUpstream(t)
	east.onModel = func(_ string, w http.ResponseWriter, _ *http.Request) bool { w.WriteHeader(503); return true }
	s := newProxyTestServer(t, multisiteTestConfig(east, west))
	resp := multisiteTestRequest(t, s, "GET", "/v1/models", "", "east", "")
	if resp.status < 400 {
		t.Fatalf("unhealthy site unexpectedly succeeded: %s", resp.body)
	}
	logins, records := west.snapshot()
	if len(logins) != 0 || len(records) != 0 {
		t.Fatalf("selected site failed over to another website: %v %v", logins, records)
	}
}

func TestMultisiteAccountIDsAreGloballyUnique(t *testing.T) {
	east, west := newProxyTestUpstream(t), newProxyTestUpstream(t)
	cfg := multisiteTestConfig(east, west)
	cfg.Accounts[2].ID = cfg.Accounts[0].ID
	if _, err := NewProxy(cfg); err == nil {
		t.Fatal("duplicate account ID across websites accepted")
	}
	cfg = multisiteTestConfig(east, west)
	cfg.Accounts[0].WebsiteID = "missing"
	if _, err := NewProxy(cfg); err == nil {
		t.Fatal("unknown account website accepted")
	}
}

func TestMultisiteWebsiteProxyDefaultAndAccountOverride(t *testing.T) {
	var hits [3]atomic.Int32
	proxies := make([]*httptest.Server, 3)
	for i := range proxies {
		i := i
		proxies[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[i].Add(1)
			if r.URL.Host != "site.example.invalid" {
				t.Errorf("wrong website base URL: %s", r.URL.String())
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/auths/signin" {
				var a map[string]string
				_ = json.NewDecoder(r.Body).Decode(&a)
				_ = json.NewEncoder(w).Encode(map[string]string{"token": "token-" + a["email"]})
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"site-model"}]}`)
		}))
		t.Cleanup(proxies[i].Close)
	}
	cfg := Config{APIKey: testProxyKey, Timeout: time.Second, OutboundProxy: proxies[0].URL,
		Websites: []WebsiteConfig{{ID: "east", BaseURL: "http://site.example.invalid", ProxyURL: proxies[1].URL}, {ID: "west", BaseURL: "http://site.example.invalid"}},
		Accounts: []AccountConfig{{ID: "alpha", WebsiteID: "east", Email: "alpha", Password: "p"}, {ID: "beta", WebsiteID: "east", Email: "beta", Password: "p", ProxyURL: proxies[2].URL}, {ID: "gamma", WebsiteID: "west", Email: "gamma", Password: "p"}},
	}
	s := newProxyTestServer(t, cfg)
	for _, id := range []string{"alpha", "beta", "gamma"} {
		requireProxyStatus(t, multisiteTestRequest(t, s, "GET", "/v1/models", "", "", id), 200)
	}
	for i := range hits {
		if hits[i].Load() != 2 {
			t.Errorf("proxy %d hits=%d, want signin+models", i, hits[i].Load())
		}
	}
}
