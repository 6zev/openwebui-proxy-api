package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateBackendAccountMatrix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account AccountConfig
		wantErr bool
	}{
		{name: "empty backend with email and password", account: AccountConfig{ID: "a", Email: "a@example.test", Password: "pw"}},
		{name: "openwebui with email and password", account: AccountConfig{ID: "a", Backend: BackendOpenWebUI, Email: "a@example.test", Password: "pw"}},
		{name: "openwebui missing email", account: AccountConfig{ID: "a", Email: " ", Password: "pw"}, wantErr: true},
		{name: "openwebui missing password", account: AccountConfig{ID: "a", Email: "a@example.test"}, wantErr: true},
		{name: "openwebui with api key only", account: AccountConfig{ID: "a", APIKey: "k12345678"}, wantErr: true},
		{name: "librechat with api key", account: AccountConfig{ID: "a", Backend: BackendLibreChat, APIKey: "k12345678"}},
		{name: "librechat with api key and display email", account: AccountConfig{ID: "a", Backend: BackendLibreChat, APIKey: "k12345678", Email: "bot@example.test"}},
		{name: "librechat missing api key", account: AccountConfig{ID: "a", Backend: BackendLibreChat, Email: "bot@example.test"}, wantErr: true},
		{name: "librechat short api key", account: AccountConfig{ID: "a", Backend: BackendLibreChat, APIKey: "k123456"}, wantErr: true},
		{name: "librechat api key at minimum length", account: AccountConfig{ID: "a", Backend: BackendLibreChat, APIKey: "k1234567"}},
		{name: "librechat api key with space", account: AccountConfig{ID: "a", Backend: BackendLibreChat, APIKey: "k1234 678"}, wantErr: true},
		{name: "librechat with password", account: AccountConfig{ID: "a", Backend: BackendLibreChat, APIKey: "k12345678", Password: "pw"}, wantErr: true},
		{name: "unknown backend", account: AccountConfig{ID: "a", Backend: BackendKind("ollama"), Email: "a@example.test", Password: "pw", APIKey: "k12345678"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBackendAccount(tc.account)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateBackendAccount(%+v) error = %v, wantErr %v", tc.account, err, tc.wantErr)
			}
		})
	}
}

func TestLibreChatUpstreamPathTable(t *testing.T) {
	for _, tc := range []struct {
		path       string
		wantPath   string
		wantEvents bool
	}{
		{path: "/v1/chat/completions", wantPath: "/api/agents/v1/chat/completions"},
		{path: "/v1/chat/completions/", wantPath: "", wantEvents: false},
		{path: "/v1/responses", wantPath: "/api/agents/v1/responses"},
		{path: "/v1/responses/", wantPath: "/api/agents/v1/responses/"},
		{path: "/v1/responses/resp_123", wantPath: "/api/agents/v1/responses/resp_123"},
		{path: "/v1/responses/resp_123/events", wantPath: "/api/agents/v1/responses/resp_123/events", wantEvents: false},
		{path: "/v1/models", wantPath: "/api/agents/v1/models"},
		{path: "/v1/models/", wantPath: "/api/agents/v1/models/"},
		{path: "/v1/models/agent_abc123", wantPath: "/api/agents/v1/models/agent_abc123"},
		{path: "/v1/events", wantPath: "/api/agents/v1/events", wantEvents: true},
		{path: "/v1/events/", wantPath: "/api/agents/v1/events/", wantEvents: true},
		{path: "/v1/events/bindings", wantPath: "/api/agents/v1/events/bindings", wantEvents: true},
		{path: "/v1/events/evt_42", wantPath: "/api/agents/v1/events/evt_42", wantEvents: true},
		{path: "/v1/agents", wantPath: "/api/agents/v1/agents"},
		{path: "/v1/agents/", wantPath: "/api/agents/v1/agents/"},
		{path: "/v1/agents/agent_abc123", wantPath: "/api/agents/v1/agents/agent_abc123"},
		{path: "/v1/agents/agent_abc123/files", wantPath: "/api/agents/v1/agents/agent_abc123/files"},
		{path: "/v1/skills", wantPath: "/api/agents/v1/skills"},
		{path: "/v1/skills/", wantPath: "/api/agents/v1/skills/"},
		{path: "/v1/skills/skill_1", wantPath: "/api/agents/v1/skills/skill_1"},
		// Unknown surfaces are not LibreChat routes.
		{path: "/", wantPath: ""},
		{path: "/healthz", wantPath: ""},
		{path: "/v1/embeddings", wantPath: ""},
		{path: "/v1/modelsfoo", wantPath: ""},
		{path: "/v1/agentsfoo", wantPath: ""},
		{path: "/v2/models", wantPath: ""},
		{path: "/api/agents/v1/chat/completions", wantPath: ""},
		{path: "/v1/chat/completions/extra", wantPath: ""},
	} {
		gotPath, found := libreChatUpstreamPath(tc.path)
		wantFound := tc.wantPath != ""
		if gotPath != tc.wantPath || found != wantFound {
			t.Errorf("libreChatUpstreamPath(%q) = (%q, %v), want (%q, %v)", tc.path, gotPath, found, tc.wantPath, wantFound)
		}
		// The events flag stays a property of the route.
		if wantFound {
			if route, ok := libreChatRouteFor(tc.path); !ok || route.events != tc.wantEvents {
				t.Errorf("libreChatRouteFor(%q).events = %v, want %v", tc.path, route.events, tc.wantEvents)
			}
		}
	}
}

func TestRequireIdempotencyKey(t *testing.T) {
	header := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("Idempotency-Key", v)
		}
		return h
	}
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		header  http.Header
		wantErr string
	}{
		{name: "post events with key", method: http.MethodPost, path: "/v1/events", header: header("webhook-42-resource-7")},
		{name: "post binding with key", method: http.MethodPost, path: "/v1/events/bindings", header: header("championship-7-player-hanae")},
		{name: "post events without key", method: http.MethodPost, path: "/v1/events", header: header(""), wantErr: "Idempotency-Key header is required for events"},
		{name: "post events with blank key", method: http.MethodPost, path: "/v1/events", header: header("   "), wantErr: "Idempotency-Key header is required for events"},
		{name: "post events nil header", method: http.MethodPost, path: "/v1/events", header: nil, wantErr: "Idempotency-Key header is required for events"},
		{name: "post events invalid character", method: http.MethodPost, path: "/v1/events", header: header("bad key!"), wantErr: "Idempotency-Key must be 1-256 characters of A-Za-z0-9._~:/+=-"},
		{name: "post events too long", method: http.MethodPost, path: "/v1/events", header: header(strings.Repeat("a", 257)), wantErr: "Idempotency-Key must be 1-256 characters of A-Za-z0-9._~:/+=-"},
		{name: "post events max length", method: http.MethodPost, path: "/v1/events", header: header(strings.Repeat("a", 256))},
		{name: "get delivery does not need key", method: http.MethodGet, path: "/v1/events/evt_42", header: header("")},
		{name: "post chat does not need key", method: http.MethodPost, path: "/v1/chat/completions", header: header("")},
		{name: "post unknown path does not need key", method: http.MethodPost, path: "/v1/unknown", header: header("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireIdempotencyKey(tc.method, tc.path, tc.header)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("requireIdempotencyKey(%s %s) error = %v, want nil", tc.method, tc.path, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("requireIdempotencyKey(%s %s) = nil, want %q", tc.method, tc.path, tc.wantErr)
			}
			e, ok := err.(*apiError)
			if !ok {
				t.Fatalf("error %T is not *apiError: %v", err, err)
			}
			if e.status != 400 || e.message != tc.wantErr {
				t.Fatalf("got %d %q, want 400 %q", e.status, e.message, tc.wantErr)
			}
		})
	}
}

// libreChatTestAccount builds a proxy with a single librechat account pointing
// at baseURL plus plain accounts created with newOutboundClient, so the test
// does not depend on how buildProxy validates backends.
func libreChatTestAccount(t *testing.T, baseURL, id, key string) *Proxy {
	t.Helper()
	client, err := newBackendClient(AccountConfig{}, time.Second)
	if err != nil {
		t.Fatalf("newBackendClient: %v", err)
	}
	t.Cleanup(func() { client.CloseIdleConnections() })
	return &Proxy{accounts: []*account{{
		config: AccountConfig{ID: id, Backend: BackendLibreChat, APIKey: key, BaseURL: baseURL},
		client: client,
	}}}
}

func TestLibreChatResolveAuthIsOffline(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/agents/v1/chat/completions" {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := libreChatTestAccount(t, server.URL, "lc", "k12345678")
	header, err := backendAuthHeader(context.Background(), p, p.accounts[0])
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if header != "Bearer k12345678" {
		t.Fatalf("resolveAuth = %q, want %q", header, "Bearer k12345678")
	}
	mu.Lock()
	recorded := append([]string(nil), paths...)
	mu.Unlock()
	if len(recorded) != 0 {
		t.Fatalf("resolveAuth made network calls: %v", recorded)
	}

	// Second call repeats the same static header, again without network I/O.
	if again, err := backendAuthHeader(context.Background(), p, p.accounts[0]); err != nil || again != header {
		t.Fatalf("repeat resolveAuth = (%q, %v), want (%q, nil)", again, err, header)
	}
	if p.accounts[0].token != "" {
		t.Fatalf("librechat account stored a session token %q", p.accounts[0].token)
	}
}

func TestLibreChatResolveAuthErrors(t *testing.T) {
	p := libreChatTestAccount(t, "http://127.0.0.1:1", "lc", "")
	if _, err := backendAuthHeader(context.Background(), p, p.accounts[0]); err == nil {
		t.Fatal("resolveAuth with an empty API key returned no error")
	}

	unknown := &account{config: AccountConfig{ID: "x", Backend: BackendKind("ollama")}, client: http.DefaultClient}
	if _, err := backendAuthHeader(context.Background(), p, unknown); err == nil {
		t.Fatal("resolveAuth with an unknown backend returned no error")
	}
}

// TestLibreChatAuthHeaderEndToEnd proves the two halves compose: the handler
// would use backendAuthHeader for Authorization, so a live request built with
// that header against a mapped LibreChat route must carry the Agent API key and
// must never touch /api/v1/auths/signin.
func TestLibreChatAuthHeaderEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var paths, auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auths/signin" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":"login is not part of the LibreChat Agents API"}`))
			return
		}
		if r.URL.Path != "/api/agents/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unexpected path"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-lc","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	cfg := Config{APIKey: testProxyKey, Timeout: time.Second, Cooldown: time.Second,
		Accounts: []AccountConfig{{ID: "lc", Backend: BackendLibreChat, APIKey: "k12345678", BaseURL: server.URL}}}
	client, err := newBackendClient(cfg.Accounts[0], cfg.Timeout)
	if err != nil {
		t.Fatalf("newBackendClient: %v", err)
	}
	defer client.CloseIdleConnections()
	p := &Proxy{config: cfg, key: sha256.Sum256([]byte(cfg.APIKey)),
		accounts: []*account{{config: cfg.Accounts[0], client: client}},
		sites:    map[string]WebsiteConfig{}, nextByWebsite: map[string]int{}}

	header, err := backendAuthHeader(context.Background(), p, p.accounts[0])
	if err != nil {
		t.Fatalf("backendAuthHeader: %v", err)
	}
	upstreamPath, found := libreChatUpstreamPath("/v1/chat/completions")
	if upstreamPath == "" || !found {
		t.Fatalf("libreChatUpstreamPath(/v1/chat/completions) = (%q, %v)", upstreamPath, found)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+upstreamPath, strings.NewReader(`{"model":"agent_abc123","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", header)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	gotAuths := append([]string(nil), auths...)
	mu.Unlock()
	if len(gotPaths) != 1 || gotPaths[0] != "/api/agents/v1/chat/completions" {
		t.Fatalf("upstream paths = %v, want exactly [/api/agents/v1/chat/completions]", gotPaths)
	}
	if gotAuths[0] != "Bearer k12345678" {
		t.Fatalf("upstream authorization = %q, want %q", gotAuths[0], "Bearer k12345678")
	}
}

// TestLibreChatRouteHelpersUnchangedForOpenWebUI guards the refactor: the
// default backend keeps the Open WebUI login path.
func TestLibreChatRouteHelpersUnchangedForOpenWebUI(t *testing.T) {
	if normalizeBackend("") != BackendOpenWebUI {
		t.Fatalf("empty backend defaulted to %q", normalizeBackend(""))
	}
	if path, found := libreChatUpstreamPath("/v1/responses"); path != "/api/agents/v1/responses" || !found {
		t.Fatalf("responses mapping broken: (%q, %v)", path, found)
	}
	if err := validateBackendAccount(AccountConfig{ID: "owe", Email: "a@example.test", Password: "pw"}); err != nil {
		t.Fatalf("default backend rejected email/password account: %v", err)
	}
}
