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
	"net/http"
	"net/url"
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
}
type Config struct {
	BaseURL, APIKey, DefaultModel, OutboundProxy string
	ConfigPath                                   string
	Websites                                     []WebsiteConfig
	Accounts                                     []AccountConfig
	Timeout, Cooldown                            time.Duration
}
type account struct {
	config        AccountConfig
	client        *http.Client
	mu            sync.Mutex
	token         string
	expires       time.Time
	loginBlocked  time.Time
	cooldownMu    sync.Mutex
	cooldownUntil time.Time
}
type Proxy struct {
	config        Config
	key           [32]byte
	accounts      []*account
	sites         map[string]WebsiteConfig
	mu            sync.Mutex
	nextByWebsite map[string]int
}
type apiError struct {
	status     int
	message    string
	retryAfter int
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
		client, err := newOutboundClient(s.ProxyURL, c.Timeout)
		if err != nil {
			return nil, fmt.Errorf("website %s: invalid outbound proxy configuration", s.ID)
		}
		client.CloseIdleConnections()
		s.BaseURL = strings.TrimRight(s.BaseURL, "/")
		s.Name = displayName(s.ID, s.Name)
		p.sites[s.ID] = s
	}
	ids := map[string]bool{}
	for _, a := range c.Accounts {
		if !validConfigID(a.ID) {
			return nil, errors.New("account IDs must use 1-64 letters, digits, dots, hyphens or underscores")
		}
		if ids[a.ID] {
			return nil, fmt.Errorf("duplicate account ID: %s", a.ID)
		}
		ids[a.ID] = true
		if a.Email == "" || a.Password == "" || len(a.Email) > 320 || len(a.Password) > 8192 || len(a.Name) > 160 {
			return nil, fmt.Errorf("account %s requires email and password (display name is optional)", a.ID)
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
		client, err := newOutboundClient(proxyURL, c.Timeout)
		if err != nil {
			return nil, fmt.Errorf("account %s: invalid outbound proxy configuration", a.ID)
		}
		a.ProxyURL = proxyURL
		p.accounts = append(p.accounts, &account{config: a, client: client})
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
func (p *Proxy) cooldown(a *account, d time.Duration) {
	a.cooldownMu.Lock()
	defer a.cooldownMu.Unlock()
	if until := time.Now().Add(d); until.After(a.cooldownUntil) {
		a.cooldownUntil = until
	}
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
		for _, a := range p.accounts {
			if a.config.ID == id {
				if website != "" && a.config.WebsiteID != website {
					return nil, &apiError{400, "Selected account does not belong to selected website", 0}
				}
				if a.config.Disabled {
					return nil, &apiError{503, "Selected account or website is disabled", 0}
				}
				if n := accountCooldown(a); n > 0 {
					return nil, &apiError{429, "Selected account is cooling down", n}
				}
				return []*account{a}, nil
			}
		}
		return nil, &apiError{400, "Unknown X-Account-ID", 0}
	}
	eligible := make([]*account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if !a.config.Disabled && (website == "" || a.config.WebsiteID == website) {
			eligible = append(eligible, a)
		}
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
		return "", &apiError{503, "Account login temporarily unavailable", remaining(a.loginBlocked)}
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
func (p *Proxy) retryDuration(s string) time.Duration {
	d := p.config.Cooldown
	if n, err := strconv.ParseInt(s, 10, 32); err == nil && n > 0 {
		if v := time.Duration(n) * time.Second; v > d {
			d = v
		}
	} else if t, err := http.ParseTime(s); err == nil {
		if v := time.Until(t); v > d {
			d = v
		}
	}
	return d
}
func (p *Proxy) request(ctx context.Context, a *account, path, method string, body []byte) (*http.Response, error, bool) {
	token, err := p.login(ctx, a)
	if err != nil {
		var e *apiError
		// A service-wide login rate limit is not a reason to rotate accounts.
		retry := ctx.Err() == nil && !(errors.As(err, &e) && e.status == 429)
		return nil, err, retry
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, method, a.config.BaseURL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "*/*")
		resp, err := a.client.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				p.cooldown(a, p.config.Cooldown)
			}
			return nil, err, false
		}
		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			a.mu.Lock()
			if a.token == token {
				a.token = ""
			}
			a.mu.Unlock()
			token, err = p.login(ctx, a)
			if err != nil {
				return nil, err, false
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil, false
		}
		resp.Body.Close()
		e := &apiError{status: resp.StatusCode, message: fmt.Sprintf("Open WebUI returned HTTP %d", resp.StatusCode)}
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
			e.message = "Open WebUI returned an unexpected redirect"
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
	if r.Method == http.MethodGet && r.URL.Path == "/v1/accounts" {
		items := make([]map[string]any, 0, len(p.accounts))
		for _, a := range p.accounts {
			n := accountCooldown(a)
			items = append(items, map[string]any{"id": a.config.ID, "website_id": a.config.WebsiteID, "enabled": !a.config.Disabled, "available": n == 0 && !a.config.Disabled, "cooldown_seconds": n})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": items})
		return
	}
	models := r.Method == http.MethodGet && r.URL.Path == "/v1/models"
	chat := r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions"
	// OpenAI-compatible embeddings plus upstream Open WebUI tool surfaces
	// (tool servers / MCP gateways / tool-calling pipelines). JSON is forwarded
	// to the selected account so Open WebUI keeps ownership of tool execution,
	// web search and MCP policy.
	passThrough := false
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
		passThrough = true
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && underAny(r.URL.Path,
		"/v1/tools/", "/v1/mcp/", "/v1/knowledge/", "/v1/files/"):
		passThrough = true
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/tools" || r.URL.Path == "/v1/mcp" || r.URL.Path == "/v1/tools/list"):
		passThrough = true
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
		p.forwardBody(w, r, body)
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
		body, _ = json.Marshal(input)
	}
	candidates, err := p.candidatesFor(r.Header.Get("X-Account-ID"), r.Header.Get("X-Website-ID"))
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.config.Timeout)
	defer cancel()
	path, method := "/api/chat/completions", http.MethodPost
	if models {
		path, method = "/api/models", http.MethodGet
	}
	var response *http.Response
	for _, a := range candidates {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		w.Header().Set("X-Account-ID", a.config.ID)
		w.Header().Set("X-Website-ID", a.config.WebsiteID)
		var retry bool
		response, err, retry = p.request(ctx, a, path, method, body)
		if err == nil {
			break
		}
		if r.Header.Get("X-Account-ID") != "" || !retry {
			break
		}
	}
	if err != nil {
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
		out := make([]map[string]any, 0, len(items))
		for _, m := range items {
			if m.ID == "" {
				continue
			}
			if m.OwnedBy == "" {
				m.OwnedBy = "open-webui"
			}
			out = append(out, map[string]any{"id": m.ID, "object": "model", "created": m.Created, "owned_by": m.OwnedBy})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": out})
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

// forwardBody proxies an Open WebUI tool/embeddings/MCP request to one selected
// account. Upstream owns tool execution, web search and MCP authorization; the
// proxy only enforces routing, size limits and a sanitized error surface.
func (p *Proxy) forwardBody(w http.ResponseWriter, r *http.Request, body []byte) {
	candidates, err := p.candidatesFor(r.Header.Get("X-Account-ID"), r.Header.Get("X-Website-ID"))
	if err != nil {
		writeError(w, err)
		return
	}
	upstreamPath := r.URL.Path
	if !strings.HasPrefix(upstreamPath, "/api/") {
		if r.URL.Path == "/v1/embeddings" {
			upstreamPath = "/api/embeddings"
		} else {
			upstreamPath = "/api" + r.URL.Path
			if r.URL.Path == "/v1/mcp" {
				upstreamPath = "/api/v1/mcp/servers"
			}
		}
	}
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.config.Timeout)
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
		response, err, retry = p.request(ctx, a, upstreamPath, r.Method, body)
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
	_, _ = io.Copy(w, io.LimitReader(response.Body, 16<<20))
}
