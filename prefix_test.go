package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSplitModelPrefix(t *testing.T) {
	cases := []struct {
		model, prefix, rest string
		ok                  bool
	}{
		{"acc-1/openai/gpt-5.6", "acc-1", "openai/gpt-5.6", true},
		{"openai/gpt-5.6", "openai", "gpt-5.6", true},
		{"nomodel", "", "", false},
		{"/leading", "", "", false},
		{"trailing/", "", "", false},
		{"a/b/c/d", "a", "b/c/d", true},
	}
	for _, tc := range cases {
		prefix, rest, ok := splitModelPrefix(tc.model)
		if prefix != tc.prefix || rest != tc.rest || ok != tc.ok {
			t.Errorf("splitModelPrefix(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.model, prefix, rest, ok, tc.prefix, tc.rest, tc.ok)
		}
	}
}

func TestModelPrefixRoutesWithoutOverridingHeaders(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "alpha", "beta")
	cfg.ModelPrefix = true
	s := newProxyTestServer(t, cfg)

	// Listing exposes "<website-or-account>/<model>".
	list := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, list, 200)
	if !strings.Contains(list.body, "alpha/test-model") && !strings.Contains(list.body, "beta/test-model") {
		t.Errorf("model listing missing prefix: %s", list.body)
	}

	// "beta/test-model" pins account beta and strips the prefix upstream.
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"beta/test-model","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if got := resp.header.Get("X-Account-ID"); got != "beta" {
		t.Errorf("prefix routing picked %q", got)
	}
	_, records := u.snapshot()
	last := records[len(records)-1]
	if last.Body["model"] != "test-model" {
		t.Errorf("upstream model = %v, want stripped id", last.Body["model"])
	}

	// A real model ID containing a slash is untouched when the prefix is unknown.
	resp = proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"openai/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "alpha")
	requireProxyStatus(t, resp, 200)
	_, records = u.snapshot()
	last = records[len(records)-1]
	if last.Body["model"] != "openai/gpt-5.6-luna" {
		t.Errorf("unprefixed model rewritten: %v", last.Body["model"])
	}
	if last.ID != "alpha" {
		t.Errorf("explicit header overridden: %s", last.ID)
	}
}

func TestModelPrefixDisabledKeepsPlainIDs(t *testing.T) {
	u := newProxyTestUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	list := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, list, 200)
	if strings.Contains(list.body, "alpha/test-model") {
		t.Errorf("prefix applied without opt-in: %s", list.body)
	}
}

func TestModelPrefixAppliesToPassthroughBodies(t *testing.T) {
	u, _ := surfaceUpstream(t)
	cfg := proxyTestConfig(u, "alpha", "beta")
	cfg.ModelPrefix = true
	s := newProxyTestServer(t, cfg)
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/responses", `{"model":"beta/agent_x","input":"hi"}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if got := resp.header.Get("X-Account-ID"); got != "beta" {
		t.Errorf("passthrough prefix routing picked %q", got)
	}
}

func TestSurfaceUpstreamAccounts(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	p, err := NewProxy(proxyTestConfig(u, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p.healthReport())
	if strings.Contains(string(raw), "Bearer") {
		t.Errorf("health report leaked auth: %s", raw)
	}
}
