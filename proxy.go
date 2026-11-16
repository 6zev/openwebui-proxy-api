package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type WebsiteConfig struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	BaseURL  string `json:"base_url"`
	ProxyURL string `json:"proxy_url,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	// TimeoutMS overrides the global upstream timeout for this website's
	// accounts. Slow hosts (a model list that takes 20s) need a budget of their
	// own; zero or absent means inherit Config.Timeout.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}
type AccountConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	WebsiteID string `json:"website_id,omitempty"`
	Email     string `json:"email"`
	Password  string `json:"password"`
	BaseURL   string `json:"base_url,omitempty"`
	ProxyURL  string `json:"proxy_url,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	// Backend selects the upstream API family: "" or "openwebui" uses email +
	// password login, "librechat" uses the static LibreChat Agents API key.
	Backend BackendKind `json:"backend,omitempty"`
	// APIKey is the LibreChat Agents API key (Authorization: Bearer …).
	APIKey string `json:"api_key,omitempty"`
}
type Config struct {
	BaseURL, APIKey, DefaultModel, OutboundProxy string
	ConfigPath                                   string
	// ModelPrefix enables "<id>/<model>" IDs in model listings.
	ModelPrefix bool
	// DebugLog logs one line per routed request plus a bounded excerpt of an
	// upstream error body. It never logs credentials: auth headers and the
	// proxy keys are excluded by construction.
	DebugLog bool
	// ProbeTimeout overrides Timeout for health probes only; zero means the
	// regular upstream timeout applies.
	ProbeTimeout time.Duration
	// ModelCacheTTL keeps successful model listings for this long; zero disables
	// the cache entirely.
	ModelCacheTTL     time.Duration
	Websites          []WebsiteConfig
	Accounts          []AccountConfig
	Timeout, Cooldown time.Duration
}
type account struct {
	config AccountConfig
	// timeout is the effective upstream budget for this account: the website
	// override when present, otherwise the global one.
	timeout      time.Duration
	client       *http.Client
	mu           sync.Mutex
	token        string
	expires      time.Time
	loginBlocked time.Time
	// probeMu serialises operator-triggered probes for this account.
	probeMu       sync.Mutex
	cooldownMu    sync.Mutex
	cooldownUntil time.Time
	health        accountHealth
}
type Proxy struct {
	config        Config
	key           [32]byte
	accounts      []*account
	sites         map[string]WebsiteConfig
	mu            sync.Mutex
	nextByWebsite map[string]int
	// modelCache holds the last successful model list per account key.
	modelCache *modelCache
}
type apiError struct {
	status     int
	message    string
	retryAfter int
}

// apiErrorStored is an apiError whose verdict came from cached state (cooldown,
// login block, last probe result) instead of an upstream round-trip. A stored
// verdict never causes failover, because the selected account was not contacted
// yet; a real upstream failure still does.
type apiErrorStored struct{ *apiError }

func (e *apiErrorStored) Unwrap() error { return e.apiError }
func (e *apiErrorStored) Error() string { return e.apiError.message }

func storedError(status int, message string, retryAfter int) error {
	return &apiErrorStored{&apiError{status: status, message: message, retryAfter: retryAfter}}
}

func (e *apiError) Error() string { return e.message }

func validConfigID(id string) bool {
	return len(id) > 0 && len(id) <= 64 && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) < 0
}
func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 4096 && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

// minAPIKeyLength is intentionally low so a local install can use a short key.
// Run the proxy behind HTTPS and prefer a long random key when exposed.
const minAPIKeyLength = 5

// Display names are optional: fall back to the ID so lists stay readable.
func displayName(id, name string) string {
	if strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return id
}

func NewProxy(c Config) (*Proxy, error) { return buildProxy(c, false) }

// timeoutFor resolves the effective upstream budget: a website override wins,
// then the global timeout. Positive values only; buildProxy rejects negatives.
func timeoutFor(c Config, overrideMS int) time.Duration {
	if overrideMS > 0 {
		return time.Duration(overrideMS) * time.Millisecond
	}
	return c.Timeout
}
func buildProxy(c Config, allowEmpty bool) (*Proxy, error) {
	if len(c.APIKey) < minAPIKeyLength {
		return nil, fmt.Errorf("PROXY_API_KEY must contain at least %d characters", minAPIKeyLength)
	}
	if !allowEmpty && len(c.Accounts) == 0 {
		return nil, errors.New("configure at least one account")
	}
	if c.Timeout == 0 {
		c.Timeout = 300 * time.Second
	}
	if c.Cooldown == 0 {
		c.Cooldown = time.Minute
	}
	if c.Timeout < 0 || c.Cooldown < 0 {
		return nil, errors.New("timeouts and cooldowns must be positive")
	}
	if len(c.Websites) > 256 || len(c.Accounts) > 4096 {
		return nil, errors.New("configuration supports at most 256 websites and 4096 accounts")
	}
	c.Websites = append([]WebsiteConfig{}, c.Websites...)
	c.Accounts = append([]AccountConfig{}, c.Accounts...)
	p := &Proxy{config: c, key: sha256.Sum256([]byte(c.APIKey)), sites: make(map[string]WebsiteConfig), nextByWebsite: make(map[string]int)}
	if c.ModelCacheTTL > 0 {
		p.modelCache = newModelCache(c.ModelCacheTTL)
	}
	for _, s := range c.Websites {
		if !validConfigID(s.ID) {
			return nil, errors.New("website IDs must use 1-64 letters, digits, dots, hyphens or underscores")
		}
		if _, exists := p.sites[s.ID]; exists {
			return nil, fmt.Errorf("duplicate website ID: %s", s.ID)
		}
		if len(s.Name) > 160 || !validBaseURL(s.BaseURL) {
			return nil, fmt.Errorf("website %s requires a valid HTTP(S) URL (name is optional)", s.ID)
		}
		if s.TimeoutMS < 0 || s.TimeoutMS > 3600000 {
			return nil, fmt.Errorf("website %s: timeout_ms must be between 1 and 3600000", s.ID)
		}
		client, err := newOutboundClient(s.ProxyURL, timeoutFor(c, s.TimeoutMS))
		if err != nil {
			return nil, fmt.Errorf("website %s: invalid outbound proxy configuration", s.ID)
		}
		client.CloseIdleConnections()
		s.BaseURL = strings.TrimRight(s.BaseURL, "/")
		s.Name = displayName(s.ID, s.Name)
		p.sites[s.ID] = s
	}
	// Account IDs only have to be unique inside their website; the composite
	// "<website-id>/<account-id>" is the deployment-wide identity.
	ids := map[string]bool{}
	for _, a := range c.Accounts {
		if !validConfigID(a.ID) {
			return nil, errors.New("account IDs must use 1-64 letters, digits, dots, hyphens or underscores")
		}
		key := accountKey(a.WebsiteID, a.ID)
		if ids[key] {
			return nil, fmt.Errorf("duplicate account ID %q in website %q", a.ID, a.WebsiteID)
		}
		ids[key] = true
		if err := validateBackendAccount(a); err != nil {
			return nil, err
		}
		if a.Backend == "" {
			a.Backend = BackendOpenWebUI
		}
		a.Name = displayName(a.ID, a.Name)
		proxyURL := a.ProxyURL
		if a.WebsiteID != "" {
			s, exists := p.sites[a.WebsiteID]
			if !exists {
				return nil, fmt.Errorf("account %s references an unknown website", a.ID)
			}
			a.BaseURL = s.BaseURL
			a.Disabled = a.Disabled || s.Disabled
			if proxyURL == "" {
				proxyURL = s.ProxyURL
			}
		} else if a.BaseURL == "" {
			a.BaseURL = c.BaseURL
		}
		if !validBaseURL(a.BaseURL) {
			return nil, fmt.Errorf("account %s requires a valid HTTP(S) base URL without credentials/query/fragment", a.ID)
		}
		a.BaseURL = strings.TrimRight(a.BaseURL, "/")
		if proxyURL == "" {
			proxyURL = c.OutboundProxy
		}
		timeout := c.Timeout
		if a.WebsiteID != "" {
			timeout = timeoutFor(c, p.sites[a.WebsiteID].TimeoutMS)
		}
		client, err := newOutboundClient(proxyURL, timeout)
		if err != nil {
			return nil, fmt.Errorf("account %s: invalid outbound proxy configuration", a.ID)
		}
		a.ProxyURL = proxyURL
		p.accounts = append(p.accounts, &account{config: a, timeout: timeout, client: client})
	}
	return p, nil
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func writeError(w http.ResponseWriter, err error) {
	e := &apiError{status: 502, message: "Upstream connection failed"}
	if errors.Is(err, context.DeadlineExceeded) {
		e = &apiError{status: 504, message: "Upstream request timed out"}
	}
	var known *apiError
	if errors.As(err, &known) {
		e = known
	}
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	typ := "api_error"
	if e.status == 400 || e.status == 404 || e.status == 413 {
		typ = "invalid_request_error"
	}
	if e.status == 401 {
		typ = "authentication_error"
	}
	writeJSON(w, e.status, map[string]any{"error": map[string]any{"message": e.message, "type": typ, "code": strconv.Itoa(e.status)}})
}

// debugf logs only when DEBUG_LOG is enabled, so a normal deployment stays quiet.
func (p *Proxy) debugf(format string, args ...any) {
	if p.config.DebugLog {
		log.Printf(format, args...)
	}
}

// debugBody returns a bounded, single-line excerpt of an upstream error body for
// the debug log. The body is upstream-controlled, so control characters are
// stripped and the length is capped.
func debugBody(raw []byte) string {
	const limit = 1500
	text := strings.Join(strings.Fields(string(raw)), " ")
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}

func (p *Proxy) cooldown(a *account, d time.Duration) {
	a.cooldownMu.Lock()
	defer a.cooldownMu.Unlock()
	if until := time.Now().Add(d); until.After(a.cooldownUntil) {
		a.cooldownUntil = until
	}
}

// loginBlockSeconds exposes the remaining login block so health output can say
// why an account is unavailable even when its last probe looked fine.
func (a *account) loginBlockSeconds() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return remaining(a.loginBlocked)
}

// clearStaleBlocks drops every cached "do not use this account" verdict. It is
// called once real evidence arrives: a completed login or a completed probe.
func (a *account) clearStaleBlocks() {
	a.mu.Lock()
	a.loginBlocked = time.Time{}
	a.mu.Unlock()
	a.cooldownMu.Lock()
	a.cooldownUntil = time.Time{}
	a.cooldownMu.Unlock()
}

func accountCooldown(a *account) int {
	a.cooldownMu.Lock()
	defer a.cooldownMu.Unlock()
	return remaining(a.cooldownUntil)
}
func (p *Proxy) CloseIdleConnections() {
	for _, a := range p.accounts {
		a.client.CloseIdleConnections()
	}
}
func remaining(t time.Time) int {
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// requestTimeout is the budget for a routed request: the largest effective
// timeout among the accounts that may serve it, so a slow website is not cut
// short by a faster neighbour's limit before failover even gets a chance.
func (p *Proxy) requestTimeout(candidates []*account) time.Duration {
	limit := time.Duration(0)
	for _, a := range candidates {
		if a.timeout > limit {
			limit = a.timeout
		}
	}
	if limit <= 0 {
		return p.config.Timeout
	}
	return limit
}

func (p *Proxy) candidates(id string) ([]*account, error) { return p.candidatesFor(id, "") }
func underAny(path string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func (p *Proxy) candidatesFor(id, website string) ([]*account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if website != "" {
		s, ok := p.sites[website]
		if !ok {
			return nil, &apiError{400, "Unknown X-Website-ID", 0}
		}
		if s.Disabled {
			return nil, &apiError{503, "Selected website is disabled", 0}
		}
	}
	if id != "" {
		// Account IDs are unique per website, so (website, id) identifies one
		// account. Without a website the caller may use id alone, but only when
		// it is unambiguous.
		matches := make([]*account, 0, 1)
		for _, a := range p.accounts {
			if a.config.ID != id {
				continue
			}
			if website != "" && a.config.WebsiteID != website {
				continue
			}
			matches = append(matches, a)
		}
		if len(matches) == 0 {
			return nil, &apiError{400, "Unknown X-Account-ID", 0}
		}
		if len(matches) > 1 {
			return nil, &apiError{409, "Account ID is ambiguous across websites; send X-Website-ID as well", 0}
		}
		a := matches[0]
		if a.config.Disabled {
			return nil, &apiError{503, "Selected account or website is disabled", 0}
		}
		if n := accountCooldown(a); n > 0 {
			return nil, storedError(429, "Selected account is cooling down", n)
		}
		if status, _, reason, _ := a.health.snapshot(); status == healthFailed {
			return nil, storedError(503, "Selected account failed its health check: "+reason, 0)
		}
		return []*account{a}, nil
	}
	eligible := make([]*account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if a.config.Disabled || (website != "" && a.config.WebsiteID != website) {
			continue
		}
		// Skip accounts known to be broken; the health loop revives them.
		if status, _, _, _ := a.health.snapshot(); status == healthFailed {
			continue
		}
		eligible = append(eligible, a)
	}
	if len(eligible) == 0 {
		return nil, &apiError{503, "No enabled accounts configured for this route", 0}
	}
	start := p.nextByWebsite[website] % len(eligible)
	p.nextByWebsite[website] = (start + 1) % len(eligible)
	result := make([]*account, 0, len(eligible))
	wait := 0
	for i := 0; i < len(eligible); i++ {
		a := eligible[(start+i)%len(eligible)]
		n := accountCooldown(a)
		if n == 0 {
			result = append(result, a)
		} else if wait == 0 || n < wait {
			wait = n
		}
	}
	if len(result) == 0 {
		return nil, &apiError{429, "All accounts are cooling down", wait}
	}
	return result, nil
}
func (p *Proxy) login(ctx context.Context, a *account) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if a.token != "" && (a.expires.IsZero() || time.Until(a.expires) > 30*time.Second) {
		return a.token, nil
	}
	if time.Now().Before(a.loginBlocked) {
		return "", storedError(503, "Account login temporarily unavailable", remaining(a.loginBlocked))
	}
	loginCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	data, _ := json.Marshal(map[string]string{"email": a.config.Email, "password": a.config.Password})
	req, _ := http.NewRequestWithContext(loginCtx, http.MethodPost, a.config.BaseURL+"/api/v1/auths/signin", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			a.loginBlocked = time.Now().Add(p.config.Cooldown)
			p.cooldown(a, p.config.Cooldown)
		}
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d := p.config.Cooldown
		if resp.StatusCode == 429 {
			d = p.retryDuration(resp.Header.Get("Retry-After"))
		}
		a.loginBlocked = time.Now().Add(d)
		p.cooldown(a, d)
		if resp.StatusCode == 429 {
			return "", &apiError{429, "Open WebUI login rate limited", remaining(a.loginBlocked)}
		}
		return "", &apiError{502, "Open WebUI login failed; check credentials and password-login settings", 0}
	}
	var session struct {
		Token     string `json:"token"`
		ExpiresAt *int64 `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&session); err != nil || session.Token == "" {
		a.loginBlocked = time.Now().Add(p.config.Cooldown)
		p.cooldown(a, p.config.Cooldown)
		return "", &apiError{502, "Open WebUI did not return a valid login token", 0}
	}
	a.token = session.Token
	a.expires = time.Time{}
	if session.ExpiresAt != nil {
		a.expires = time.Unix(*session.ExpiresAt, 0)
	}
	return a.token, nil
}

// retryDuration turns Retry-After into a cooldown. It never shortens the
// configured cooldown: a bare Retry-After: 0 (or a past HTTP-date) means "wait
// zero seconds", and honouring that literally would put the throttled account
// straight back into rotation to be throttled again.
func (p *Proxy) retryDuration(s string) time.Duration {
	d := p.config.Cooldown
	candidate := time.Duration(0)
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32); err == nil && n > 0 {
		candidate = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(s); err == nil {
		if v := time.Until(t); v > 0 {
			candidate = v
		}
	}
	if candidate > d {
		return candidate
	}
	return d
}
func (p *Proxy) request(ctx context.Context, a *account, path, method string, body []byte) (*http.Response, error, bool) {
	return p.requestWithHeaders(ctx, a, path, method, body, nil)
}

// requestWithHeaders forwards to the account's backend. extra lets callers add
// protocol headers (for example Idempotency-Key for LibreChat agent events)
// without teaching this function about every surface. Static-key backends
// (LibreChat Agents API) never perform a login round-trip and never auto-retry
// a 401, because re-sending an idempotency-keyed event is the caller's call.
func (p *Proxy) requestWithHeaders(ctx context.Context, a *account, path, method string, body []byte, extra http.Header) (*http.Response, error, bool) {
	auth, err := backendAuthHeader(ctx, p, a)
	if err != nil {
		var e *apiError
		// Stored verdicts mean the selected account was never contacted, so
		// there is nothing to fail over from; a service-wide login rate limit is
		// not a reason to rotate accounts either.
		retry := ctx.Err() == nil && !isStoredErr(err) && !(errors.As(err, &e) && e.status == 429)
		return nil, err, retry
	}
	staticKey := a.config.Backend == BackendLibreChat
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, method, a.config.BaseURL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "*/*")
		for name, values := range extra {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
		started := time.Now()
		resp, err := a.client.Do(req)
		if err != nil {
			p.debugf("debug: %s %s account=%s website=%s transport-error=%v elapsed=%s", method, path, a.config.ID, a.config.WebsiteID, err, time.Since(started))
			if ctx.Err() == nil {
				p.cooldown(a, p.config.Cooldown)
			}
			return nil, err, false
		}
		p.debugf("debug: %s %s account=%s website=%s status=%d elapsed=%s", method, path, a.config.ID, a.config.WebsiteID, resp.StatusCode, time.Since(started))
		if resp.StatusCode == 401 && attempt == 0 && !staticKey {
			resp.Body.Close()
			a.mu.Lock()
			// Invalidate the cached token so login() actually re-authenticates;
			// auth carries the "Bearer " prefix while a.token holds the raw token.
			if a.token != "" && "Bearer "+a.token == auth {
				a.token = ""
			}
			a.mu.Unlock()
			var refreshed string
			refreshed, err = p.login(ctx, a)
			if err != nil {
				return nil, err, false
			}
			auth = "Bearer " + refreshed
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil, false
		}
		var errorExcerpt []byte
		if p.config.DebugLog {
			errorExcerpt, _ = io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			p.debugf("debug: upstream error body %s %s account=%s -> %s", method, path, a.config.ID, debugBody(errorExcerpt))
		}
		resp.Body.Close()
		e := &apiError{status: resp.StatusCode, message: fmt.Sprintf("%s returned HTTP %d", backendLabel(a.config.Backend), resp.StatusCode)}
		if resp.StatusCode == 429 {
			d := p.retryDuration(resp.Header.Get("Retry-After"))
			p.cooldown(a, d)
			e.retryAfter = int((d + time.Second - 1) / time.Second)
		}
		if resp.StatusCode >= 500 {
			p.cooldown(a, p.config.Cooldown)
			return nil, e, true
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			e.status = 502
			e.message = "Upstream returned an unexpected redirect"
		}
		return nil, e, false
	}
	return nil, &apiError{502, "Upstream authentication failed", 0}, false
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		writeError(w, &apiError{401, "Invalid proxy API key", 0})
		return
	}
	hash := sha256.Sum256([]byte(fields[1]))
	if subtle.ConstantTimeCompare(hash[:], p.key[:]) != 1 {
		writeError(w, &apiError{401, "Invalid proxy API key", 0})
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/health" || r.URL.Path == "/v1/healthz") {
		if r.URL.Query().Get("probe") == "1" {
			p.runHealthProbes(r.Context(), true)
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": p.healthReport()})
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/health/probe" {
		failures := p.runHealthProbes(r.Context(), true)
		writeJSON(w, 200, map[string]any{"object": "list", "failures": failures, "data": p.healthReport()})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/accounts" {
		items := make([]map[string]any, 0, len(p.accounts))
		for _, a := range p.accounts {
			n := accountCooldown(a)
			items = append(items, map[string]any{"id": a.config.ID, "key": accountKey(a.config.WebsiteID, a.config.ID), "website_id": a.config.WebsiteID, "enabled": !a.config.Disabled, "available": n == 0 && !a.config.Disabled, "cooldown_seconds": n})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": items})
		return
	}
	models := r.Method == http.MethodGet && r.URL.Path == "/v1/models"
	chat := r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions"
	// Every other documented Open WebUI surface (Anthropic Messages, Ollama
	// native, embeddings, tools, MCP, knowledge, files, admin) is forwarded to
	// the selected account so upstream keeps ownership of execution.
	if err := eventsHeaderGuard(p, r); err != nil {
		writeError(w, err)
		return
	}
	passThrough := false
	var surfaceTarget string
	if !models && !chat {
		// Open WebUI surface wins for its own paths; LibreChat agents paths are
		// only considered when the Open WebUI surface does not claim them.
		if target, ok := mapSurfacePath(r.URL.Path); ok {
			passThrough = true
			surfaceTarget = target
		} else if target, ok := libreChatUpstreamPath(r.URL.Path); ok {
			// Agent-events and other LibreChat-only routes have no Open WebUI
			// equivalent, so they fall back to the LibreChat surface.
			passThrough = true
			surfaceTarget = target
		}
	}
	if !models && !chat && !passThrough {
		writeError(w, &apiError{404, "Endpoint not found", 0})
		return
	}
	var body []byte
	if chat || passThrough {
		reader := http.MaxBytesReader(w, r.Body, 10<<20)
		defer reader.Close()
		var err error
		body, err = io.ReadAll(reader)
		if err != nil {
			var max *http.MaxBytesError
			if errors.As(err, &max) {
				writeError(w, &apiError{413, "Request exceeds 10 MiB", 0})
			} else {
				writeError(w, &apiError{400, "Invalid request body", 0})
			}
			return
		}
	}
	if passThrough {
		body = applyModelPrefix(p, r, body)
		p.debugSurfaceBody(r, body, surfaceTarget)
		p.forwardBody(w, r, body, surfaceTarget)
		return
	}
	if chat {
		var input map[string]json.RawMessage
		if json.Unmarshal(body, &input) != nil || input == nil {
			writeError(w, &apiError{400, "Body must be a JSON object", 0})
			return
		}
		var model string
		if raw, ok := input["model"]; ok {
			if json.Unmarshal(raw, &model) != nil {
				writeError(w, &apiError{400, "model must be a string", 0})
				return
			}
		}
		if model == "" && p.config.DefaultModel != "" {
			model = p.config.DefaultModel
			input["model"], _ = json.Marshal(model)
		}
		var messages []json.RawMessage
		if strings.TrimSpace(model) == "" || json.Unmarshal(input["messages"], &messages) != nil || messages == nil {
			writeError(w, &apiError{400, "model and messages[] are required", 0})
			return
		}
		// "<prefix>/<model>" lets an OpenAI client pick the account or website
		// without custom headers: prefix may be an account ID or a website ID.
		if prefix, rest, ok := splitModelPrefix(model); ok {
			if exists := p.prefixTarget(prefix); exists {
				model = rest
				input["model"], _ = json.Marshal(model)
				if r.Header.Get("X-Account-ID") == "" && r.Header.Get("X-Website-ID") == "" {
					if p.isAccountID(prefix) {
						r.Header.Set("X-Account-ID", prefix)
					} else {
						r.Header.Set("X-Website-ID", prefix)
					}
				}
			}
		}
		body, _ = json.Marshal(input)
	}
	candidates, err := p.candidatesFor(r.Header.Get("X-Account-ID"), r.Header.Get("X-Website-ID"))
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.requestTimeout(candidates))
	defer cancel()
	// Serve a fresh cached model list before touching (possibly very slow)
	// upstreams. Only /v1/models is cached; chat is never replayed from cache.
	if models && p.modelCache != nil {
		key := modelCacheKeyFor(candidates)
		if cached, fetchedAt, ok := p.modelCache.get(key); ok {
			w.Header().Set("X-Model-Cache", "hit")
			w.Header().Set("X-Model-Cache-Age", ageSeconds(fetchedAt))
			writeModelList(w, cached, p.modelPrefixFor(r, candidates))
			return
		}
		// Expired entry: serve it immediately and refresh behind the caller, so a
		// slow upstream is never on the request path. The caller sees
		// X-Model-Cache: stale and the age, and the next request gets a hit.
		if stale, fetchedAt, ok := p.modelCache.peek(key); ok {
			w.Header().Set("X-Model-Cache", "stale")
			w.Header().Set("X-Model-Cache-Age", ageSeconds(fetchedAt))
			writeModelList(w, stale, p.modelPrefixFor(r, candidates))
			if a := candidates[0]; a != nil {
				go p.refreshModelEntry(context.WithoutCancel(r.Context()), a)
			}
			return
		}
		w.Header().Set("X-Model-Cache", "miss")
	}
	// Open WebUI paths; LibreChat accounts translate these per account because
	// the two backends expose different route families for the same surface.
	path, method := "/api/chat/completions", http.MethodPost
	if models {
		path, method = "/api/models", http.MethodGet
	}
	if err := eventsRouteGuard(candidates, r); err != nil {
		writeError(w, err)
		return
	}
	var response *http.Response
	selectedPrefix := ""
	for _, a := range candidates {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		// MODEL_PREFIX=1 exposes "<website-id>/<model>" by default so a client
		// can route to the website without headers; an explicitly pinned account
		// (X-Account-ID) switches the prefix to the account id.
		if p.config.ModelPrefix {
			selectedPrefix = a.config.WebsiteID
			if r.Header.Get("X-Account-ID") != "" && a.config.WebsiteID != "" {
				selectedPrefix = a.config.ID
			}
		}
		accountPath := path
		extra := http.Header{}
		if a.config.Backend == BackendLibreChat {
			public := "/v1/chat/completions"
			if models {
				public = "/v1/models"
			}
			if chat {
				public = "/v1/chat/completions"
			}
			if mapped, ok := libreChatUpstreamPath(public); ok {
				accountPath = mapped
			} else {
				err = &apiError{404, "Endpoint not found", 0}
				break
			}
		}
		w.Header().Set("X-Account-ID", a.config.ID)
		w.Header().Set("X-Website-ID", a.config.WebsiteID)
		var retry bool
		response, err, retry = p.requestWithHeaders(ctx, a, accountPath, method, body, extra)
		if err == nil {
			break
		}
		if r.Header.Get("X-Account-ID") != "" || !retry {
			break
		}
	}
	if err != nil {
		if models && p.modelCache != nil {
			if stale, fetchedAt, ok := p.modelCache.peek(modelCacheKeyFor(candidates)); ok {
				// Every account failed, but a previously served list is better
				// than an error: the client can still pick a model and retry.
				w.Header().Set("X-Model-Cache", "stale-error")
				w.Header().Set("X-Model-Cache-Age", ageSeconds(fetchedAt))
				w.Header().Set("X-Cache-Error", "upstream-failed")
				writeModelList(w, stale, p.modelPrefixFor(r, candidates))
				return
			}
		}
		writeError(w, err)
		return
	}
	defer response.Body.Close()
	if models {
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
		if readErr != nil {
			writeError(w, readErr)
			return
		}
		if len(raw) > 8<<20 {
			writeError(w, &apiError{502, "Model list exceeds 8 MiB", 0})
			return
		}
		var items []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		}
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Data != nil {
			raw = envelope.Data
		}
		if json.Unmarshal(raw, &items) != nil || items == nil {
			writeError(w, &apiError{502, "Unexpected model-list response", 0})
			return
		}
		cached := make([]cachedModel, 0, len(items))
		for _, m := range items {
			if m.ID == "" {
				continue
			}
			if m.OwnedBy == "" {
				m.OwnedBy = "open-webui"
			}
			cached = append(cached, cachedModel{ID: m.ID, Created: m.Created, OwnedBy: m.OwnedBy})
		}
		if p.modelCache != nil {
			p.modelCache.put(modelCacheKeyFor(candidates), cached)
			w.Header().Set("X-Model-Cache", "miss")
		}
		writeModelList(w, cached, selectedPrefix)
		return
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(response.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	buffer := make([]byte, 32<<10)
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			if err != io.EOF { // Terminate truncated streams rather than falsely signaling a complete response.
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
}

// responsesUnsupported lists OpenAI Responses API fields that Open WebUI's
// /openai/responses handler is known to reject. Sending one of them makes the
// upstream answer 400 without saying which field caused it, which is the most
// common cause of an editor integration failing with "Open WebUI returned HTTP
// 400". The proxy still forwards the payload untouched; this is diagnostics.
var responsesUnsupported = []string{"reasoning", "include", "parallel_tool_calls", "truncation", "store", "background", "metadata", "max_tool_calls", "previous_response_id", "prompt_cache_key", "service_tier", "top_logprobs", "tool_choice"}

// debugSurfaceBody logs the shape of a passthrough body when debugging is on,
// and warns about Responses fields the upstream is likely to reject.
func (p *Proxy) debugSurfaceBody(r *http.Request, body []byte, target string) {
	if !p.config.DebugLog {
		return
	}
	var input map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &input) != nil {
		p.debugf("debug: %s %s upstream=%s body=%dB (not JSON)", r.Method, r.URL.Path, target, len(body))
		return
	}
	keys := make([]string, 0, len(input))
	suspicious := make([]string, 0, 4)
	for key := range input {
		keys = append(keys, key)
		for _, name := range responsesUnsupported {
			if key == name {
				suspicious = append(suspicious, key)
			}
		}
	}
	sort.Strings(keys)
	sort.Strings(suspicious)
	var model string
	_ = json.Unmarshal(input["model"], &model)
	p.debugf("debug: %s %s upstream=%s model=%q body=%dB keys=%v", r.Method, r.URL.Path, target, model, len(body), keys)
	if len(suspicious) > 0 {
		p.debugf("debug: WARNING %s carries %v; Open WebUI's /openai/responses rejects these with 400. Remove them in the client if the call fails.", r.URL.Path, suspicious)
	}
}

// forwardBody proxies an Open WebUI tool/embeddings/MCP request to one selected
// account. Upstream owns tool execution, web search and MCP authorization; the
// proxy only enforces routing, size limits and a sanitized error surface.
func (p *Proxy) forwardBody(w http.ResponseWriter, r *http.Request, body []byte, surfaceTarget string) {
	candidates, err := p.candidatesFor(r.Header.Get("X-Account-ID"), r.Header.Get("X-Website-ID"))
	if err != nil {
		writeError(w, err)
		return
	}
	upstreamPath := surfaceTarget
	if upstreamPath == "" {
		upstreamPath = r.URL.Path
	}
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.requestTimeout(candidates))
	defer cancel()
	var response *http.Response
	for _, a := range candidates {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		w.Header().Set("X-Account-ID", a.config.ID)
		w.Header().Set("X-Website-ID", a.config.WebsiteID)
		var retry bool
		accountPath := upstreamPath
		if a.config.Backend == BackendLibreChat {
			mapped, ok := libreChatUpstreamPath(r.URL.Path)
			if !ok {
				err = &apiError{404, "Endpoint not found", 0}
				break
			}
			accountPath = mapped
		}
		response, err, retry = p.requestWithHeaders(ctx, a, accountPath, r.Method, body, forwardHeaders(a, r))
		if err == nil {
			break
		}
		// Only pre-response failures may rotate; tool calls can have side effects.
		if r.Header.Get("X-Account-ID") != "" || !retry {
			break
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	defer response.Body.Close()
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	if _, copyErr := io.Copy(w, io.LimitReader(response.Body, 16<<20)); copyErr != nil {
		// The upstream died mid-body. Abort the connection so the client sees a
		// truncated transfer instead of a complete-looking short response.
		panic(http.ErrAbortHandler)
	}
}
