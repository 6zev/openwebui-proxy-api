package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Upstream model ids contain slashes ("openai/gpt-5"). The prefix the proxy adds
// must be the only one stripped: prefixing such an id twice produced
// "site-a/openai/gpt-5", and stripping "openai" as if it were a website corrupted
// the model into a name the upstream does not know ("Model not found").
func TestSlashedModelIDsAreNotDoublePrefixed(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auths/signin":
			_, _ = io.WriteString(w, `{"token":"t"}`)
		case "/api/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"openai/gpt-5"},{"id":"anthropic/claude-sonnet-5-5"}]}`)
		case "/api/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			seen = append(seen, body.Model)
			_, _ = io.WriteString(w, `{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		}
	}))
	defer upstream.Close()

	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.ModelPrefix = true
	cfg.Websites = []WebsiteConfig{{ID: "site-a", BaseURL: upstream.URL}}
	cfg.Accounts = []AccountConfig{{ID: "acc-a", WebsiteID: "site-a", Email: "a@example.test", Password: "p"}}
	cfg.Timeout = 5 * time.Second
	s := newProxyTestServer(t, cfg)

	list := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, list, 200)
	for _, want := range []string{`"site-a/openai/gpt-5"`, `"site-a/anthropic/claude-sonnet-5-5"`} {
		if !strings.Contains(list.body, want) {
			t.Errorf("listing missing %s: %s", want, list.body)
		}
	}
	if strings.Contains(list.body, "site-a/site-a") {
		t.Errorf("double prefix in listing: %s", list.body)
	}

	// Chatting with the prefixed id must reach the upstream as the bare model.
	body := `{"model":"site-a/openai/gpt-5","messages":[{"role":"user","content":"hi"}]}`
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", body, testProxyKey, ""), 200)
	if len(seen) != 1 || seen[0] != "openai/gpt-5" {
		t.Fatalf("upstream received model %v, want openai/gpt-5", seen)
	}
}

// A model whose first segment merely looks like a configured id must not be
// mistaken for a routing prefix when that segment is not a real website/account.
func TestUnknownFirstSegmentIsNotTreatedAsPrefix(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body.Model)
		_, _ = io.WriteString(w, `{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	s := newProxyTestServer(t, cfg)
	body := `{"model":"openai/gpt-5","messages":[{"role":"user","content":"hi"}]}`
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", body, testProxyKey, ""), 200)
	if len(seen) != 1 || seen[0] != "openai/gpt-5" {
		t.Fatalf("upstream received %v, want the untouched id", seen)
	}
}
