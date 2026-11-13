package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// BackendKind selects which upstream product an account talks to. Both speak
// OpenAI-shaped HTTP, but they authenticate differently and mount their
// routes under different prefixes.
type BackendKind string

const (
	// BackendOpenWebUI is the historical behaviour: email/password login at
	// /api/v1/auths/signin with a short-lived bearer session token.
	BackendOpenWebUI BackendKind = "openwebui"
	// BackendLibreChat talks to the LibreChat Agents API. A static Agent API
	// key is sent as a bearer token on every request; there is no login and no
	// refresh, so an account never holds session state for it.
	BackendLibreChat BackendKind = "librechat"
)

// minLibreChatAPIKeyLength keeps obviously placeholder keys out of config. A
// LibreChat Agent API key is far longer in practice; the floor only needs to
// reject empty/typo values while staying easy to test.
const minLibreChatAPIKeyLength = 8

// libreChatPrefix is the mount point of LibreChat's Agents API. Proxy-facing
// /v1/... paths are translated under it.
const libreChatPrefix = "/api/agents/v1"

// libreChatEventIdempotencyKey is the header LibreChat requires on every
// Agent Events write.
const libreChatEventIdempotencyKey = "Idempotency-Key"

// normalizeBackend folds the empty backend into the default so every caller
// compares against exactly two values.
func normalizeBackend(kind BackendKind) BackendKind {
	if kind == "" {
		return BackendOpenWebUI
	}
	return kind
}

// validateBackendAccount checks the credentials an account needs for its
// backend. It never mutates the account.
func validateBackendAccount(a AccountConfig) error {
	switch normalizeBackend(a.Backend) {
	case BackendOpenWebUI:
		// Open WebUI logs in with an email/password pair.
		if strings.TrimSpace(a.Email) == "" || a.Password == "" {
			return errors.New("openwebui accounts require email and password")
		}
		if len(a.Email) > 320 || len(a.Password) > 8192 {
			return errors.New("openwebui account email or password is too long")
		}
		return nil
	case BackendLibreChat:
		// LibreChat authenticates every request with a static Agent API key;
		// a stored password would never be sent and only widens the secret
		// surface, so it is rejected instead of silently ignored.
		if a.Password != "" {
			return errors.New("librechat accounts do not use a password; set api_key instead")
		}
		if len(a.APIKey) < minLibreChatAPIKeyLength {
			return errors.New("librechat accounts require an api_key")
		}
		if len(a.APIKey) > 8192 {
			return errors.New("librechat account api_key is too long")
		}
		if strings.ContainsFunc(a.APIKey, func(r rune) bool {
			return r <= ' ' || r >= 0x7f
		}) {
			return errors.New("librechat account api_key must be printable without spaces")
		}
		if len(strings.TrimSpace(a.Email)) > 320 {
			return errors.New("librechat account email is too long")
		}
		return nil
	default:
		return errors.New("unsupported backend; use openwebui or librechat")
	}
}

// newBackendClient builds the outbound client for an account. Proxy settings
// are resolved before the account reaches this point, so the raw proxy string
// is passed straight through; the literal "direct" keeps its existing meaning
// of "ignore environment proxies".
func newBackendClient(a AccountConfig, timeout time.Duration) (*http.Client, error) {
	return newOutboundClient(a.ProxyURL, timeout)
}

// resolveAuth returns the Authorization header value for one upstream request.
// LibreChat keys are static, so no network call happens; Open WebUI keeps the
// existing cached-login behaviour.
//
// A free function, not a method: proxy.go owns *account, so this shape cannot
// collide with anything the Lead declares there. The method form below simply
// delegates to it.
func backendAuthHeader(ctx context.Context, p *Proxy, a *account) (string, error) {
	switch normalizeBackend(a.config.Backend) {
	case BackendLibreChat:
		if a.config.APIKey == "" {
			return "", &apiError{502, "LibreChat account is missing its Agent API key", 0}
		}
		return "Bearer " + a.config.APIKey, nil
	case BackendOpenWebUI:
		token, err := p.login(ctx, a)
		if err != nil {
			return "", err
		}
		return "Bearer " + token, nil
	default:
		return "", &apiError{502, "Unsupported backend for account " + a.config.ID, 0}
	}
}

// resolveAuth is the method form of backendAuthHeader for call sites that
// already hold the account.
func (a *account) resolveAuth(ctx context.Context, p *Proxy) (string, error) {
	return backendAuthHeader(ctx, p, a)
}

// libreChatRoute is one entry of the LibreChat route table. An empty to means
// "forward verbatim", mirroring surface.go's convention.
type libreChatRoute struct {
	from string
	to   string
	// events marks routes that write Agent Events and therefore need the
	// Idempotency-Key header.
	events bool
}

// libreChatRoutes is an explicit allow-list: only documented Agents API
// surfaces are translated, everything else is not a LibreChat route at all.
// Exact entries win over prefix entries.
var libreChatRoutes = []libreChatRoute{
	{from: "/v1/chat/completions", to: libreChatPrefix + "/chat/completions"},
	{from: "/v1/responses", to: libreChatPrefix + "/responses"},
	{from: "/v1/responses/", to: libreChatPrefix + "/responses/"},
	{from: "/v1/models", to: libreChatPrefix + "/models"},
	{from: "/v1/models/", to: libreChatPrefix + "/models/"},
	{from: "/v1/events", to: libreChatPrefix + "/events", events: true},
	{from: "/v1/events/", to: libreChatPrefix + "/events/", events: true},
	{from: "/v1/agents", to: libreChatPrefix + "/agents"},
	{from: "/v1/agents/", to: libreChatPrefix + "/agents/"},
	{from: "/v1/skills", to: libreChatPrefix + "/skills"},
	{from: "/v1/skills/", to: libreChatPrefix + "/skills/"},
}

// libreChatUpstreamPath maps a proxy-facing path onto the LibreChat Agents
// API and reports whether the second return value marks an events route that
// needs an Idempotency-Key.
// libreChatRoute is one public-path → upstream-path mapping.
func libreChatRouteFor(path string) (libreChatRoute, bool) {
	for _, route := range libreChatRoutes {
		if route.from == path {
			return route, true
		}
	}
	for _, route := range libreChatRoutes {
		if !strings.HasSuffix(route.from, "/") || !strings.HasPrefix(path, route.from) {
			continue
		}
		return route, true
	}
	return libreChatRoute{}, false
}

func libreChatUpstreamPath(path string) (string, bool) {
	for _, route := range libreChatRoutes {
		if route.from == path {
			// The second return value means "route found"; the events flag is a
			// property of the route, not the success signal.
			return route.to, true
		}
	}
	for _, route := range libreChatRoutes {
		if !strings.HasSuffix(route.from, "/") {
			continue
		}
		if !strings.HasPrefix(path, route.from) {
			continue
		}
		if path == strings.TrimSuffix(route.from, "/")+"/" {
			// "/v1/models/" alone is a prefix with an empty remainder; treat it
			// as the exact collection route instead of an empty id.
			return route.to, true
		}
		return route.to + strings.TrimPrefix(path, route.from), true
	}
	return "", false
}

// validIdempotencyKey accepts 1 to 256 characters from LibreChat's documented
// alphabet: letters, digits and the characters . _ ~ : / + = -
func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > 256 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '~' || c == ':' || c == '/' || c == '+' || c == '=' || c == '-':
		default:
			return false
		}
	}
	return true
}

// requireIdempotencyKey enforces LibreChat's Agent Events contract before the
// request leaves the proxy. Non-POST requests and non-events routes pass.
func requireIdempotencyKey(method, path string, h http.Header) error {
	if method != http.MethodPost {
		return nil
	}
	route, found := libreChatRouteFor(path)
	if !found || !route.events {
		return nil
	}
	var key string
	if h != nil {
		key = strings.TrimSpace(h.Get(libreChatEventIdempotencyKey))
	}
	if key == "" {
		return &apiError{400, "Idempotency-Key header is required for events", 0}
	}
	if !validIdempotencyKey(key) {
		return &apiError{400, "Idempotency-Key must be 1-256 characters of A-Za-z0-9._~:/+=-", 0}
	}
	return nil
}

// forwardHeaders carries protocol headers that must survive the proxy hop.
// LibreChat agent events require an Idempotency-Key on the upstream call, and
// it must be the caller's key so retries stay idempotent.
func forwardHeaders(a *account, r *http.Request) http.Header {
	headers := http.Header{}
	if a.config.Backend != BackendLibreChat {
		return headers
	}
	if value := r.Header.Get(libreChatEventIdempotencyKey); value != "" && underAny(r.URL.Path, "/v1/events") {
		headers.Set(libreChatEventIdempotencyKey, value)
	}
	return headers
}

// eventsRouteGuard rejects an agent-events request that lacks a valid
// Idempotency-Key before any account is contacted. A request may be served by
// several accounts on retry, so the header shape is validated once up front.
func eventsRouteGuard(candidates []*account, r *http.Request) error {
	if !underAny(r.URL.Path, "/v1/events") {
		return nil
	}
	for _, a := range candidates {
		if a.config.Backend == BackendLibreChat {
			return requireIdempotencyKey(r.Method, r.URL.Path, r.Header)
		}
	}
	return nil
}

// backendLabel names the backend in sanitized error messages.
func backendLabel(kind BackendKind) string {
	if kind == BackendLibreChat {
		return "LibreChat"
	}
	return "Open WebUI"
}

// eventsHeaderGuard rejects an agent-events POST that lacks a valid
// Idempotency-Key before routing, for any LibreChat account in the pool.
func eventsHeaderGuard(p *Proxy, r *http.Request) error {
	if !underAny(r.URL.Path, "/v1/events") {
		return nil
	}
	for _, a := range p.accounts {
		if a.config.Backend == BackendLibreChat {
			return requireIdempotencyKey(r.Method, r.URL.Path, r.Header)
		}
	}
	return nil
}
