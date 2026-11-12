package main

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestMapSurfacePath(t *testing.T) {
	cases := []struct {
		path   string
		target string
		ok     bool
	}{
		{"/v1/embeddings", "/api/embeddings", true},
		{"/v1/embeddings/2", "", true},
		{"/v1/messages", "/api/v1/messages", true},
		{"/v1/messages/count_tokens", "/api/v1/messages/count_tokens", true},
		{"/v1/responses", "", false},
		{"/v1/tools/list", "/api/v1/tools/list", true},
		{"/v1/tools/id/abc/specs", "", true},
		{"/v1/mcp", "/api/v1/mcp/servers", true},
		{"/v1/mcp/servers", "/api/v1/mcp/servers", true},
		{"/v1/knowledge/1/file/add", "", true},
		{"/v1/files/", "", true},
		{"/ollama/api/tags", "", true},
		{"/ollama/v1/embeddings/0", "", true},
		{"/api/v1/models/all", "", true},
		{"/api/version", "", true},
		{"/v2/chat", "", false},
		{"/random", "", false},
		{"/", "", false},
	}
	for _, tc := range cases {
		target, ok := mapSurfacePath(tc.path)
		if ok != tc.ok || target != tc.target {
			t.Errorf("mapSurfacePath(%q) = (%q, %t), want (%q, %t)", tc.path, target, ok, tc.target, tc.ok)
		}
	}
}

// surfaceUpstream records requests for the passthrough surface tests.
func surfaceUpstream(t *testing.T) (*proxyTestUpstream, map[string]string) {
	t.Helper()
	u := newProxyTestUpstream(t)
	seen := map[string]string{}
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		body := ""
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			body = string(raw)
		}
		seen[r.Method+" "+r.URL.Path] = body
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/messages":
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant"}`)
		case "/ollama/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"llama"}]}`)
		case "/api/v1/models/all":
			_, _ = io.WriteString(w, `{"data":[{"id":"x"}]}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	})
	return u, seen
}

func TestProxyForwardsAnthropicOllamaAndAdminSurfaces(t *testing.T) {
	u, seen := surfaceUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	cases := []struct{ method, path, body, wantPath string }{
		{"POST", "/v1/messages", `{"model":"m","max_tokens":10,"messages":[]}`, "/api/v1/messages"},

		{"GET", "/ollama/api/tags", "", "/ollama/api/tags"},
		{"GET", "/api/v1/models/all", "", "/api/v1/models/all"},
		{"GET", "/api/version", "", "/api/version"},
		{"POST", "/v1/embeddings/1", `{"input":"x"}`, "/v1/embeddings/1"},
	}
	for _, tc := range cases {
		resp := proxyTestRequest(t, s, tc.method, tc.path, tc.body, testProxyKey, "")
		requireProxyStatus(t, resp, 200)
		if _, ok := seen[tc.method+" "+tc.wantPath]; !ok {
			t.Errorf("%s %s did not reach %s; seen=%v", tc.method, tc.path, tc.wantPath, seen)
		}
		if tc.body != "" && seen[tc.method+" "+tc.wantPath] != tc.body {
			t.Errorf("body altered for %s: %s", tc.path, seen[tc.method+" "+tc.wantPath])
		}
	}
}

func TestProxyAnthropicAndOllamaResponsesPreserved(t *testing.T) {
	u, _ := surfaceUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))

	resp := proxyTestRequest(t, s, "POST", "/v1/messages", `{"model":"m","max_tokens":5,"messages":[]}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	var anthropic map[string]any
	if err := json.Unmarshal([]byte(resp.body), &anthropic); err != nil || anthropic["type"] != "message" {
		t.Errorf("Anthropic response not preserved: %s", resp.body)
	}

	resp = proxyTestRequest(t, s, "GET", "/ollama/api/tags", "", testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if !strings.Contains(resp.body, "llama") {
		t.Errorf("Ollama response not preserved: %s", resp.body)
	}
}

func TestProxyRejectsUnknownSurface(t *testing.T) {
	u, _ := surfaceUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	for _, path := range []string{"/v2/chat/completions", "/foo", "/v1/unknown-surface"} {
		method := http.MethodPost
		if path == "/foo" {
			method = http.MethodGet
		}
		resp := proxyTestRequest(t, s, method, path, "{}", testProxyKey, "")
		requireProxyStatus(t, resp, 404)
	}
}

func TestProxySurfacePinsAccount(t *testing.T) {
	u, seen := surfaceUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
	resp := proxyTestRequest(t, s, "GET", "/ollama/api/tags", "", testProxyKey, "beta")
	requireProxyStatus(t, resp, 200)
	if got := resp.header.Get("X-Account-ID"); got != "beta" {
		t.Errorf("pinned account header = %q", got)
	}
	if len(seen) != 1 {
		t.Errorf("expected a single upstream call, saw %v", seen)
	}
}

func TestSurfaceAllowListMatchesDocs(t *testing.T) {
	// Guard the advertised surface so a refactor cannot silently drop support.
	// chat/completions and models have dedicated proxy paths, not generic ones.
	required := []string{"/v1/embeddings", "/v1/messages", "/v1/messages/count_tokens", "/v1/tools/list", "/v1/mcp/servers", "/ollama/api/tags", "/api/v1/models/all"}
	for _, path := range required {
		if _, ok := mapSurfacePath(path); !ok {
			t.Errorf("advertised surface %q is not routed", path)
		}
	}
	if got := reflect.TypeOf(mapSurfacePath); got.Kind() != reflect.Func {
		t.Fatal("mapSurfacePath must stay a function")
	}
}
