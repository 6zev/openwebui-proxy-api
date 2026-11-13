package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// libreChatStub mimics the LibreChat Agents API surface.
type libreChatStub struct {
	paths   []string
	headers []http.Header
	bodies  []string
	u       *proxyTestUpstream
}

func newLibreChatStub(t *testing.T) *libreChatStub {
	t.Helper()
	stub := &libreChatStub{u: newProxyTestUpstream(t)}
	stub.u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		stub.paths = append(stub.paths, r.URL.Path)
		stub.headers = append(stub.headers, r.Header.Clone())
		stub.bodies = append(stub.bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/agents/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"agent_abc123"}]}`)
		case r.URL.Path == "/api/agents/v1/chat/completions":
			_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
		case r.URL.Path == "/api/agents/v1/responses":
			_, _ = io.WriteString(w, `{"object":"response","status":"completed"}`)
		case strings.HasPrefix(r.URL.Path, "/api/agents/v1/events"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"deliveryId":"d1"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		}
	})
	return stub
}

func libreChatConfig(u *proxyTestUpstream, site string) Config {
	cfg := proxyTestConfig(u, "")
	cfg.Websites = []WebsiteConfig{{ID: site, Name: "LC", BaseURL: u.server.URL}}
	cfg.Accounts = []AccountConfig{{ID: "agent", WebsiteID: site, Backend: BackendLibreChat, APIKey: "k12345678"}}
	return cfg
}

func TestLibreChatBackendLiveWiring(t *testing.T) {
	stub := newLibreChatStub(t)
	s := newProxyTestServer(t, libreChatConfig(stub.u, "lc"))

	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if !strings.Contains(resp.body, "agent_abc123") {
		t.Errorf("agent list not forwarded: %s", resp.body)
	}

	resp = proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"agent_abc123","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)

	resp = proxyTestRequest(t, s, http.MethodPost, "/v1/responses", `{"model":"agent_abc123","input":"hi"}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)

	// Chat and models translate correctly; events need an Idempotency-Key.
	// (Guarded separately in TestLibreChatEventsWithoutKeyRejected.)

	want := []string{"/api/agents/v1/models", "/api/agents/v1/chat/completions", "/api/agents/v1/responses"}
	if strings.Join(stub.paths, ",") != strings.Join(want, ",") {
		t.Errorf("upstream paths = %v, want %v", stub.paths, want)
	}
	for _, h := range stub.headers {
		if got := h.Get("Authorization"); got != "Bearer k12345678" {
			t.Errorf("authorization = %q, want static agent key", got)
		}
	}
	for _, path := range stub.paths {
		if path == "/api/v1/auths/signin" {
			t.Errorf("LibreChat account performed an Open WebUI login")
		}
	}
}

func TestLibreChatEventsWithoutKeyRejected(t *testing.T) {
	stub := newLibreChatStub(t)
	s := newProxyTestServer(t, libreChatConfig(stub.u, "lc"))
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/events", `{"mode":"fire"}`, testProxyKey, "")
	requireProxyStatus(t, resp, 400)
	if len(stub.paths) != 0 {
		t.Errorf("event without idempotency key reached upstream: %v", stub.paths)
	}
}

func TestLibreChatEventsWithIdempotencyKey(t *testing.T) {
	stub := newLibreChatStub(t)
	s := newProxyTestServer(t, libreChatConfig(stub.u, "lc"))
	request, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/events", strings.NewReader(`{"mode":"fire"}`))
	request.Header.Set("Authorization", "Bearer "+testProxyKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "webhook-42-resource-7")
	response, err := s.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if len(stub.paths) != 1 || stub.paths[0] != "/api/agents/v1/events" {
		t.Fatalf("upstream paths = %v", stub.paths)
	}
	if got := stub.headers[0].Get("Idempotency-Key"); got != "webhook-42-resource-7" {
		t.Errorf("Idempotency-Key not forwarded: %q", got)
	}
}

func TestMixedBackendsPickCorrectFamily(t *testing.T) {
	stub := newLibreChatStub(t)
	cfg := libreChatConfig(stub.u, "lc")
	cfg.Accounts = append(cfg.Accounts, AccountConfig{ID: "owui", WebsiteID: "lc", Email: "a@example.test", Password: "p"})
	s := newProxyTestServer(t, cfg)

	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "agent")
	requireProxyStatus(t, resp, 200)
	if stub.paths[0] != "/api/agents/v1/models" {
		t.Errorf("pinned librechat went to %q", stub.paths[0])
	}

	// An Open WebUI account on the same website must use its own route family.
	resp = proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "owui")
	if resp.status != 200 && resp.status != 502 {
		t.Errorf("pinned openwebui returned %d", resp.status)
	}
	for _, path := range stub.paths {
		if strings.HasPrefix(path, "/api/agents/") && path != "/api/agents/v1/models" {
			t.Errorf("openwebui account used LibreChat route %q", path)
		}
	}
}

func TestLibreChatUnknownSurfaceRejected(t *testing.T) {
	stub := newLibreChatStub(t)
	s := newProxyTestServer(t, libreChatConfig(stub.u, "lc"))
	for _, path := range []string{"/v1/messages", "/v2/anything"} {
		resp := proxyTestRequest(t, s, http.MethodPost, path, "{}", testProxyKey, "")
		requireProxyStatus(t, resp, 404)
	}
}

func TestLibreChatResponsesBodyUnchanged(t *testing.T) {
	stub := newLibreChatStub(t)
	s := newProxyTestServer(t, libreChatConfig(stub.u, "lc"))
	payload := `{"model":"agent_abc123","input":"What changed?","stream":true}`
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/responses", payload, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if stub.bodies[0] != payload {
		t.Errorf("payload rewritten: %s", stub.bodies[0])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stub.bodies[0]), &decoded); err != nil || decoded["input"] != "What changed?" {
		t.Errorf("input field missing: %v", decoded)
	}
}
