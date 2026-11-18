package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webAssets embed.FS

type configDocument struct {
	Websites []WebsiteConfig `json:"websites"`
	Accounts []AccountConfig `json:"accounts"`
}
type Application struct {
	mu       sync.RWMutex
	cfg      Config
	proxy    *Proxy
	adminKey [32]byte
	guard    *adminGuard
	revision uint64
}

func NewApplication(cfg Config, adminKey string) (*Application, error) {
	if adminKey == "" {
		adminKey = cfg.APIKey
	}
	if len(adminKey) < minAPIKeyLength {
		return nil, fmt.Errorf("ADMIN_API_KEY must contain at least %d characters", minAPIKeyLength)
	}
	proxy, err := buildProxy(cfg, true)
	if err != nil {
		return nil, err
	}
	cfg.Websites = append([]WebsiteConfig{}, cfg.Websites...)
	cfg.Accounts = append([]AccountConfig{}, cfg.Accounts...)
	return &Application{cfg: cfg, proxy: proxy, adminKey: sha256.Sum256([]byte(adminKey)), guard: newAdminGuard(), revision: 1}, nil
}
func (a *Application) snapshot() (*Proxy, Config, uint64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.proxy, a.cfg, a.revision
}

// healthLoop starts probing the current proxy snapshot and keeps probing after
// every config reload by re-resolving the live proxy each tick.
func (a *Application) healthLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultProbeInterval
	}
	go func() {
		a.probeOnce(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.probeOnce(ctx)
			}
		}
	}()
}

func (a *Application) probeOnce(ctx context.Context) {
	proxy, _, _ := a.snapshot()
	proxy.runHealthProbes(ctx, false)
}

func (a *Application) CloseIdleConnections() { p, _, _ := a.snapshot(); p.CloseIdleConnections() }
func redactedProxy(raw string) string {
	if raw == "" || raw == "direct" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "configured"
	}
	u.User = nil
	return u.String()
}

// logBuffer keeps the most recent debug lines for the admin console, so an
// operator can see why an upstream rejected a request without shell access. It
// holds no credentials: the router never logs headers or keys.
type logBuffer struct {
	mu    sync.Mutex
	lines []string
}

const logBufferSize = 300

// debugLogs is the process-wide ring buffer served by /api/admin/logs.
var debugLogs = &logBuffer{}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Split on newlines so one record is one log line.
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		b.lines = append(b.lines, line)
	}
	if over := len(b.lines) - logBufferSize; over > 0 {
		b.lines = append([]string(nil), b.lines[over:]...)
	}
	return len(p), nil
}

func (b *logBuffer) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Return an empty slice rather than nil so the API always answers [].
	return append([]string{}, b.lines...)
}

// healthHandler exposes the latest probe results to the admin WebUI/API.
func (a *Application) healthHandler(w http.ResponseWriter, r *http.Request) {
	proxy, _, _ := a.snapshot()
	if r.Method == http.MethodPost || r.URL.Query().Get("probe") == "1" {
		proxy.runHealthProbes(r.Context(), true)
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": proxy.healthReport()})
}

// logsHandler returns recent debug lines. Logging is always mirrored into this
// ring buffer; it only fills up when DEBUG_LOG is enabled.
func (a *Application) logsHandler(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"lines": debugLogs.snapshot()})
}

func (a *Application) state(w http.ResponseWriter) {
	p, cfg, revision := a.snapshot()
	sites := make([]map[string]any, 0, len(cfg.Websites))
	accounts := make([]map[string]any, 0, len(cfg.Accounts))
	for _, s := range cfg.Websites {
		sites = append(sites, map[string]any{"id": s.ID, "name": displayName(s.ID, s.Name), "stored_name": s.Name, "base_url": s.BaseURL, "enabled": !s.Disabled, "proxy_url": redactedProxy(s.ProxyURL), "proxy_configured": s.ProxyURL != "", "timeout_ms": s.TimeoutMS})
	}
	for _, c := range cfg.Accounts {
		available := false
		cooldown := 0
		health := "unknown"
		for _, active := range p.accounts {
			if active.config.ID == c.ID && active.config.WebsiteID == c.WebsiteID {
				cooldown = accountCooldown(active)
				status, _, _, _ := active.health.snapshot()
				health = string(status)
				available = !active.config.Disabled && cooldown == 0 && status != healthFailed
				break
			}
		}
		accounts = append(accounts, map[string]any{"id": c.ID, "key": accountKey(c.WebsiteID, c.ID), "name": displayName(c.ID, c.Name), "stored_name": c.Name, "website_id": c.WebsiteID, "email": c.Email, "backend": string(c.Backend), "has_api_key": c.APIKey != "", "enabled": !c.Disabled, "has_password": c.Password != "", "proxy_url": redactedProxy(c.ProxyURL), "proxy_configured": c.ProxyURL != "", "available": available, "cooldown_seconds": cooldown, "health": health})
	}
	writeJSON(w, 200, map[string]any{"version": Version, "revision": revision, "persistent": cfg.ConfigPath != "", "websites": sites, "accounts": accounts, "api_base_path": "/v1"})
}
func decodeAdmin(w http.ResponseWriter, r *http.Request, out any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return &apiError{status: 400, message: "Content-Type must be application/json"}
	}
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			return &apiError{status: 413, message: "Admin request exceeds 1 MiB"}
		}
		return &apiError{status: 400, message: "Invalid JSON body"}
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return &apiError{status: 400, message: "Body must be a JSON object"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil {
		return &apiError{status: 400, message: "Invalid JSON body or unsupported field"}
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return &apiError{status: 400, message: "Body must contain exactly one JSON object"}
	}
	return nil
}
func persistDocument(path string, doc configDocument) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > 4<<20 {
		return errors.New("configuration exceeds 4 MiB")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { file.Close(); os.Remove(name) }()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// The rename is the commit point. A directory sync failure must not create a
	// divergence where disk committed but the running configuration did not.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// mutate serializes validation, durable commit and pointer publication. Requests
// already using an older snapshot continue uninterrupted, including SSE streams.
func (a *Application) mutate(change func(*configDocument) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.ConfigPath == "" {
		return &apiError{status: 503, message: "Set CONFIG_FILE to enable persistent WebUI edits"}
	}
	doc := configDocument{Websites: append([]WebsiteConfig{}, a.cfg.Websites...), Accounts: append([]AccountConfig{}, a.cfg.Accounts...)}
	if err := change(&doc); err != nil {
		return err
	}
	nextCfg := a.cfg
	nextCfg.Websites = doc.Websites
	nextCfg.Accounts = doc.Accounts
	next, err := buildProxy(nextCfg, true)
	if err != nil {
		return &apiError{status: 400, message: err.Error()}
	}
	// Preserve tokens, cooldown and connection pools for accounts whose effective
	// credentials, website and outbound transport have not changed.
	for i, newAccount := range next.accounts {
		for _, old := range a.proxy.accounts {
			if old.config == newAccount.config {
				newAccount.client.CloseIdleConnections()
				next.accounts[i] = old
				break
			}
		}
	}
	if err := persistDocument(a.cfg.ConfigPath, doc); err != nil {
		next.CloseIdleConnections()
		return &apiError{status: 500, message: "Could not save configuration; running configuration is unchanged"}
	}
	old := a.proxy
	a.proxy = next
	a.cfg = nextCfg
	a.revision++
	old.CloseIdleConnections()
	return nil
}

type websiteDraft struct {
	ID        string  `json:"id"`
	Name      *string `json:"name"`
	BaseURL   *string `json:"base_url"`
	ProxyURL  *string `json:"proxy_url"`
	Enabled   *bool   `json:"enabled"`
	TimeoutMS *int    `json:"timeout_ms"`
}
type accountDraft struct {
	ID        string  `json:"id"`
	WebsiteID *string `json:"website_id"`
	Name      *string `json:"name"`
	Email     *string `json:"email"`
	Password  *string `json:"password"`
	ProxyURL  *string `json:"proxy_url"`
	Enabled   *bool   `json:"enabled"`
	// Backend picks the upstream API family ("openwebui" or "librechat").
	Backend *string `json:"backend"`
	// APIKey is the LibreChat Agents API key; never echoed back in state.
	APIKey *string `json:"api_key"`
}

func (a *Application) websiteMutation(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		writeError(w, &apiError{status: 405, message: "Method not allowed"})
		return
	}
	if (r.Method == http.MethodPost && id != "") || (r.Method != http.MethodPost && id == "") {
		writeError(w, &apiError{status: 404, message: "Endpoint not found"})
		return
	}
	var draft websiteDraft
	if r.Method != http.MethodDelete {
		if err := decodeAdmin(w, r, &draft); err != nil {
			writeError(w, err)
			return
		}
	}
	if r.Method == http.MethodPost {
		id = draft.ID
	} else if draft.ID != "" && draft.ID != id {
		writeError(w, &apiError{status: 400, message: "Website ID is immutable"})
		return
	}
	err := a.mutate(func(doc *configDocument) error {
		index := -1
		for i, s := range doc.Websites {
			if s.ID == id {
				index = i
				break
			}
		}
		if r.Method == http.MethodPost && index >= 0 {
			return &apiError{status: 409, message: "Website ID already exists"}
		}
		if r.Method != http.MethodPost && index < 0 {
			return &apiError{status: 404, message: "Website not found"}
		}
		if r.Method == http.MethodDelete {
			for _, c := range doc.Accounts {
				if c.WebsiteID == id {
					return &apiError{status: 409, message: "Delete or move accounts belonging to this website first"}
				}
			}
			doc.Websites = append(doc.Websites[:index], doc.Websites[index+1:]...)
			return nil
		}
		site := WebsiteConfig{ID: id}
		if index >= 0 {
			site = doc.Websites[index]
		}
		if draft.Name != nil {
			site.Name = strings.TrimSpace(*draft.Name)
		}
		if draft.BaseURL != nil {
			site.BaseURL = strings.TrimRight(strings.TrimSpace(*draft.BaseURL), "/")
		}
		if draft.ProxyURL != nil {
			site.ProxyURL = strings.TrimSpace(*draft.ProxyURL)
		}
		if draft.Enabled != nil {
			site.Disabled = !*draft.Enabled
		}
		if draft.TimeoutMS != nil {
			site.TimeoutMS = *draft.TimeoutMS
		}
		if index >= 0 {
			doc.Websites[index] = site
		} else {
			doc.Websites = append(doc.Websites, site)
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	writeJSON(w, status, map[string]bool{"ok": true})
}
func (a *Application) accountMutation(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		writeError(w, &apiError{status: 405, message: "Method not allowed"})
		return
	}
	if (r.Method == http.MethodPost && id != "") || (r.Method != http.MethodPost && id == "") {
		writeError(w, &apiError{status: 404, message: "Endpoint not found"})
		return
	}
	var draft accountDraft
	if r.Method != http.MethodDelete {
		if err := decodeAdmin(w, r, &draft); err != nil {
			writeError(w, err)
			return
		}
	}
	if r.Method == http.MethodPost {
		id = draft.ID
	} else if draft.ID != "" {
		// `id` may carry the "<website>/<account-id>" key; only the bare account
		// part is immutable.
		_, bareID := splitAccountKey(id)
		if draft.ID != bareID {
			writeError(w, &apiError{status: 400, message: "Account ID is immutable"})
			return
		}
	}
	err := a.mutate(func(doc *configDocument) error {
		// Account IDs only need to be unique within one website, so the lookup
		// key is the composite "<website-id>/<account-id>".
		websiteID, accountID := splitAccountKey(id)
		index := -1
		matches := 0
		for i, c := range doc.Accounts {
			if c.ID == accountID && (websiteID == "" || c.WebsiteID == websiteID) {
				if index < 0 {
					index = i
				}
				matches++
			}
		}
		// A bare ID that exists in several websites would silently edit or
		// delete the wrong account, so it is refused instead of guessed.
		if websiteID == "" && matches > 1 {
			return &apiError{status: 409, message: "Account ID exists in more than one website; use <website-id>/<account-id>"}
		}
		if r.Method == http.MethodPost {
			// POST creates: the same ID may exist in another website, but not in
			// the target one.
			targetSite := ""
			if draft.WebsiteID != nil {
				targetSite = strings.TrimSpace(*draft.WebsiteID)
			}
			for _, c := range doc.Accounts {
				if c.ID == accountID && c.WebsiteID == targetSite {
					return &apiError{status: 409, message: "Account ID already exists in this website"}
				}
			}
			index = -1
		}
		if r.Method != http.MethodPost && index < 0 {
			return &apiError{status: 404, message: "Account not found"}
		}
		if r.Method == http.MethodDelete {
			doc.Accounts = append(doc.Accounts[:index], doc.Accounts[index+1:]...)
			return nil
		}
		account := AccountConfig{ID: accountID}
		if index >= 0 {
			account = doc.Accounts[index]
		}
		if draft.WebsiteID != nil {
			account.WebsiteID = *draft.WebsiteID
		}
		if draft.Name != nil {
			account.Name = strings.TrimSpace(*draft.Name)
		}
		if draft.Email != nil {
			account.Email = strings.TrimSpace(*draft.Email)
		}
		if draft.Password != nil && *draft.Password != "" {
			account.Password = *draft.Password
		}
		if draft.ProxyURL != nil {
			account.ProxyURL = strings.TrimSpace(*draft.ProxyURL)
		}
		if draft.Backend != nil {
			account.Backend = BackendKind(strings.TrimSpace(*draft.Backend))
		}
		if draft.APIKey != nil && strings.TrimSpace(*draft.APIKey) != "" {
			account.APIKey = strings.TrimSpace(*draft.APIKey)
		}
		// Switching an account to LibreChat clears the Open WebUI credentials so
		// no dead secret is left behind in the stored config.
		if normalizeBackend(account.Backend) == BackendLibreChat {
			account.Email = ""
			account.Password = ""
		}
		if draft.Enabled != nil {
			account.Disabled = !*draft.Enabled
		}
		if index >= 0 {
			doc.Accounts[index] = account
		} else {
			doc.Accounts = append(doc.Accounts, account)
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	writeJSON(w, status, map[string]bool{"ok": true})
}
func (a *Application) adminProxy(w http.ResponseWriter, r *http.Request, path string) {
	p, cfg, _ := a.snapshot()
	clone := r.Clone(r.Context())
	target := *r.URL
	target.Path = path
	target.RawQuery = ""
	clone.URL = &target
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	p.ServeHTTP(w, clone)
}
func (a *Application) testAccount(w http.ResponseWriter, r *http.Request, key string) {
	p, cfg, _ := a.snapshot()
	websiteID, id := splitAccountKey(key)
	var selected *account
	for _, c := range p.accounts {
		if c.config.ID == id && (websiteID == "" || c.config.WebsiteID == websiteID) {
			selected = c
			break
		}
	}
	if selected == nil {
		writeError(w, &apiError{status: 404, message: "Account not found"})
		return
	}
	// The operator asked for live evidence, so the check must not replay the
	// proxy's cached verdict: probe the upstream with fresh credentials and let
	// the result overwrite the stored health. Otherwise a transient failure
	// keeps the account "unavailable" for a full cooldown and the button looks
	// broken even while the upstream signs in fine.
	selected.probeMu.Lock()
	defer selected.probeMu.Unlock()
	if selected.config.Disabled {
		writeError(w, &apiError{status: 503, message: "Account is disabled"})
		return
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), p.probeTimeout())
	defer cancel()
	if err := p.probe(probeCtx, selected, true); err != nil {
		writeError(w, err)
		return
	}
	clone := r.Clone(r.Context())
	target := *r.URL
	target.Path = "/v1/models"
	target.RawQuery = ""
	clone.URL = &target
	clone.Method = http.MethodGet
	clone.Body = nil
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	clone.Header.Set("X-Account-ID", id)
	clone.Header.Set("X-Website-ID", selected.config.WebsiteID)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, clone)
	if recorder.Code != 200 {
		for _, name := range []string{"Content-Type", "Retry-After", "X-Account-ID", "X-Website-ID"} {
			if v := recorder.Header().Get(name); v != "" {
				w.Header().Set(name, v)
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
		return
	}
	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
		writeError(w, &apiError{status: 502, message: "Invalid model list"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "account_id": id, "website_id": selected.config.WebsiteID, "models": response.Data})
}

// remoteHost is the client identity used for the admin throttle. Behind a
// reverse proxy RemoteAddr is the proxy, so forwarded-for is honoured only when
// the peer is a loopback/private address; otherwise one attacker could rotate the
// header and defeat the limit.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsLoopback() || ip.IsPrivate()) {
		return host
	}
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return host
	}
	first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
	if net.ParseIP(first) != nil {
		return first
	}
	return host
}

func adminOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
}

// adminGuard throttles admin key guessing. The minimum key length is small
// (minAPIKeyLength), so a deployment with a short key is realistic; a stalled
// attacker should not get unlimited attempts. Failures are counted per remote
// host and lock the host out for a window that doubles up to a ceiling.
type adminGuard struct {
	mu       sync.Mutex
	failures map[string]*adminFailure
}

type adminFailure struct {
	count      int
	blockedTil time.Time
}

const (
	adminBurstLimit   = 5
	adminLockWindow   = time.Minute
	adminMaxLock      = 15 * time.Minute
	adminGuardSweepAt = 1024
)

func newAdminGuard() *adminGuard {
	return &adminGuard{failures: make(map[string]*adminFailure)}
}

// blocked reports whether this host must wait, and for how long.
func (g *adminGuard) blocked(host string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.failures[host]
	if !ok {
		return false, 0
	}
	if wait := time.Until(entry.blockedTil); wait > 0 {
		return true, wait
	}
	return false, 0
}

// record registers a failed attempt and returns the lockout applied, if any.
func (g *adminGuard) record(host string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failures == nil {
		g.failures = make(map[string]*adminFailure)
	}
	if len(g.failures) > adminGuardSweepAt {
		// Drop entries whose lockout has expired, so the map cannot grow forever.
		now := time.Now()
		for key, entry := range g.failures {
			if now.After(entry.blockedTil) && entry.count == 0 {
				delete(g.failures, key)
			}
		}
	}
	entry, ok := g.failures[host]
	if !ok {
		entry = &adminFailure{}
		g.failures[host] = entry
	}
	entry.count++
	if entry.count < adminBurstLimit {
		return 0
	}
	lock := adminLockWindow
	for i := adminBurstLimit; i < entry.count && lock < adminMaxLock; i++ {
		lock *= 2
	}
	if lock > adminMaxLock {
		lock = adminMaxLock
	}
	entry.blockedTil = time.Now().Add(lock)
	return lock
}

// reset clears the counter after a successful authentication.
func (g *adminGuard) reset(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.failures, host)
}

func (a *Application) authorized(r *http.Request) bool {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	hash := sha256.Sum256([]byte(parts[1]))
	return subtle.ConstantTimeCompare(hash[:], a.adminKey[:]) == 1
}
func serveWeb(w http.ResponseWriter, r *http.Request) bool {
	name := ""
	contentType := ""
	switch r.URL.Path {
	case "/":
		name = "index.html"
		contentType = "text/html; charset=utf-8"
	case "/assets/app.js":
		name = "app.js"
		contentType = "application/javascript; charset=utf-8"
	case "/assets/style.css":
		name = "style.css"
		contentType = "text/css; charset=utf-8"
	default:
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, &apiError{status: 405, message: "Method not allowed"})
		return true
	}
	data, err := webAssets.ReadFile("web/" + name)
	if err != nil {
		writeError(w, &apiError{status: 500, message: "WebUI asset unavailable"})
		return true
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	// The console ships one stylesheet and no inline style attributes, inline
	// <style> blocks or style property writes, so style-src needs no
	// 'unsafe-inline'. Nothing in the page creates style attributes at runtime
	// (visibility is driven by the hidden attribute), which keeps the policy
	// airtight without breaking the UI.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
	// HSTS only when the request arrived over TLS; sending it on plain HTTP is
	// ignored by browsers but would be a lie if a reverse proxy later serves HTTP.
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	return true
}
func (a *Application) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	// Browsers request /favicon.ico on every page; without an answer it falls
	// through to the API and shows up as a noisy 401 in the console.
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if serveWeb(w, r) {
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/admin/") {
		p, _, _ := a.snapshot()
		p.ServeHTTP(w, r)
		return
	}
	host := remoteHost(r)
	if blocked, wait := a.guard.blocked(host); blocked {
		seconds := int((wait + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeError(w, &apiError{status: 429, message: "Too many failed admin attempts; retry later", retryAfter: seconds})
		return
	}
	if !a.authorized(r) {
		if lock := a.guard.record(host); lock > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(lock/time.Second)))
		}
		writeError(w, &apiError{status: 401, message: "Invalid admin API key"})
		return
	}
	a.guard.reset(host)
	if !adminOriginAllowed(r) {
		writeError(w, &apiError{status: 403, message: "Cross-origin admin requests are not allowed"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	switch {
	case path == "model-cache" && (r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodDelete):
		a.modelCacheHandler(w, r)
	case path == "health" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		a.healthHandler(w, r)
	case path == "logs" && r.Method == http.MethodGet:
		a.logsHandler(w)
	case path == "usage" && r.Method == http.MethodGet:
		a.usageHandler(w)
	case path == "state" && r.Method == http.MethodGet:
		a.state(w)
	case path == "models" && r.Method == http.MethodGet:
		a.adminProxy(w, r, "/v1/models")
	case path == "chat/completions" && r.Method == http.MethodPost:
		a.adminProxy(w, r, "/v1/chat/completions")
	case path == "websites":
		a.websiteMutation(w, r, "")
	case strings.HasPrefix(path, "websites/") && !strings.Contains(strings.TrimPrefix(path, "websites/"), "/"):
		a.websiteMutation(w, r, strings.TrimPrefix(path, "websites/"))
	case path == "accounts":
		a.accountMutation(w, r, "")
	case strings.HasPrefix(path, "accounts/"):
		// Account paths accept either "<id>" (ambiguous across websites) or the
		// unambiguous "<website-id>/<account-id>".
		rest := strings.TrimPrefix(path, "accounts/")
		parts := strings.Split(rest, "/")
		wantsTest := r.Method == http.MethodPost && len(parts) > 1 && parts[len(parts)-1] == "test"
		if wantsTest {
			parts = parts[:len(parts)-1]
		}
		if len(parts) == 0 || len(parts) > 2 {
			writeError(w, &apiError{status: 404, message: "Endpoint not found"})
		} else if wantsTest {
			a.testAccount(w, r, strings.Join(parts, "/"))
		} else {
			a.accountMutation(w, r, strings.Join(parts, "/"))
		}
	default:
		writeError(w, &apiError{status: 404, message: "Endpoint not found"})
	}
}

// modelCacheLoop refreshes cached model lists on an interval until ctx ends.
func (a *Application) modelCacheLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		// Warm the cache immediately: a slow upstream (/api/models taking 21s)
		// should be paid for in the background at startup, not by the first client
		// that asks for models.
		proxy, _, _ := a.snapshot()
		proxy.refreshModelCache(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				proxy, _, _ := a.snapshot()
				proxy.refreshModelCache(ctx)
			}
		}
	}()
}

// modelCacheHandler exposes and clears cached model lists for the admin UI/API.
func (a *Application) modelCacheHandler(w http.ResponseWriter, r *http.Request) {
	proxy, _, _ := a.snapshot()
	if proxy.modelCache == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "entries": []any{}})
		return
	}
	if r.Method == http.MethodPost {
		proxy.refreshModelCache(r.Context())
	} else if r.Method == http.MethodDelete {
		for _, key := range proxy.modelCache.keys() {
			proxy.modelCache.drop(key)
		}
	}
	entries := make([]map[string]any, 0, len(proxy.modelCache.keys()))
	for _, key := range proxy.modelCache.keys() {
		if entry, ok := proxy.modelCache.describe(key); ok {
			entries = append(entries, entry)
		}
	}
	writeJSON(w, 200, map[string]any{
		"enabled": true,
		"ttl":     int(proxy.modelCache.ttl / time.Second),
		"entries": entries,
	})
}
