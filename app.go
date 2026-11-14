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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	return &Application{cfg: cfg, proxy: proxy, adminKey: sha256.Sum256([]byte(adminKey)), revision: 1}, nil
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
	proxy.runHealthProbes(ctx)
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

// healthHandler exposes the latest probe results to the admin WebUI/API.
func (a *Application) healthHandler(w http.ResponseWriter, r *http.Request) {
	proxy, _, _ := a.snapshot()
	if r.Method == http.MethodPost || r.URL.Query().Get("probe") == "1" {
		proxy.runHealthProbes(r.Context())
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": proxy.healthReport()})
}

func (a *Application) state(w http.ResponseWriter) {
	p, cfg, revision := a.snapshot()
	sites := make([]map[string]any, 0, len(cfg.Websites))
	accounts := make([]map[string]any, 0, len(cfg.Accounts))
	for _, s := range cfg.Websites {
		sites = append(sites, map[string]any{"id": s.ID, "name": displayName(s.ID, s.Name), "base_url": s.BaseURL, "enabled": !s.Disabled, "proxy_url": redactedProxy(s.ProxyURL), "proxy_configured": s.ProxyURL != ""})
	}
	for _, c := range cfg.Accounts {
		available := false
		cooldown := 0
		health := "unknown"
		for _, active := range p.accounts {
			if active.config.ID == c.ID {
				cooldown = accountCooldown(active)
				status, _, _, _ := active.health.snapshot()
				health = string(status)
				available = !active.config.Disabled && cooldown == 0 && status != healthFailed
				break
			}
		}
		accounts = append(accounts, map[string]any{"id": c.ID, "name": displayName(c.ID, c.Name), "website_id": c.WebsiteID, "email": c.Email, "backend": string(c.Backend), "has_api_key": c.APIKey != "", "enabled": !c.Disabled, "has_password": c.Password != "", "proxy_url": redactedProxy(c.ProxyURL), "proxy_configured": c.ProxyURL != "", "available": available, "cooldown_seconds": cooldown, "health": health})
	}
	writeJSON(w, 200, map[string]any{"version": Version, "revision": revision, "persistent": cfg.ConfigPath != "", "websites": sites, "accounts": accounts, "api_base_path": "/v1"})
}
func decodeAdmin(w http.ResponseWriter, r *http.Request, out any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return &apiError{400, "Content-Type must be application/json", 0}
	}
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			return &apiError{413, "Admin request exceeds 1 MiB", 0}
		}
		return &apiError{400, "Invalid JSON body", 0}
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return &apiError{400, "Body must be a JSON object", 0}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil {
		return &apiError{400, "Invalid JSON body or unsupported field", 0}
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return &apiError{400, "Body must contain exactly one JSON object", 0}
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
		return &apiError{503, "Set CONFIG_FILE to enable persistent WebUI edits", 0}
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
		return &apiError{400, err.Error(), 0}
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
		return &apiError{500, "Could not save configuration; running configuration is unchanged", 0}
	}
	old := a.proxy
	a.proxy = next
	a.cfg = nextCfg
	a.revision++
	old.CloseIdleConnections()
	return nil
}

type websiteDraft struct {
	ID       string  `json:"id"`
	Name     *string `json:"name"`
	BaseURL  *string `json:"base_url"`
	ProxyURL *string `json:"proxy_url"`
	Enabled  *bool   `json:"enabled"`
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
		writeError(w, &apiError{405, "Method not allowed", 0})
		return
	}
	if (r.Method == http.MethodPost && id != "") || (r.Method != http.MethodPost && id == "") {
		writeError(w, &apiError{404, "Endpoint not found", 0})
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
		writeError(w, &apiError{400, "Website ID is immutable", 0})
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
			return &apiError{409, "Website ID already exists", 0}
		}
		if r.Method != http.MethodPost && index < 0 {
			return &apiError{404, "Website not found", 0}
		}
		if r.Method == http.MethodDelete {
			for _, c := range doc.Accounts {
				if c.WebsiteID == id {
					return &apiError{409, "Delete or move accounts belonging to this website first", 0}
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
		writeError(w, &apiError{405, "Method not allowed", 0})
		return
	}
	if (r.Method == http.MethodPost && id != "") || (r.Method != http.MethodPost && id == "") {
		writeError(w, &apiError{404, "Endpoint not found", 0})
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
	} else if draft.ID != "" && draft.ID != id {
		writeError(w, &apiError{400, "Account ID is immutable", 0})
		return
	}
	err := a.mutate(func(doc *configDocument) error {
		index := -1
		for i, c := range doc.Accounts {
			if c.ID == id {
				index = i
				break
			}
		}
		if r.Method == http.MethodPost && index >= 0 {
			return &apiError{409, "Account ID already exists", 0}
		}
		if r.Method != http.MethodPost && index < 0 {
			return &apiError{404, "Account not found", 0}
		}
		if r.Method == http.MethodDelete {
			doc.Accounts = append(doc.Accounts[:index], doc.Accounts[index+1:]...)
			return nil
		}
		account := AccountConfig{ID: id}
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
func (a *Application) testAccount(w http.ResponseWriter, r *http.Request, id string) {
	p, cfg, _ := a.snapshot()
	var selected *account
	for _, c := range p.accounts {
		if c.config.ID == id {
			selected = c
			break
		}
	}
	if selected == nil {
		writeError(w, &apiError{404, "Account not found", 0})
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
		writeError(w, &apiError{502, "Invalid model list", 0})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "account_id": id, "website_id": selected.config.WebsiteID, "models": response.Data})
}
func adminOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
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
		writeError(w, &apiError{405, "Method not allowed", 0})
		return true
	}
	data, err := webAssets.ReadFile("web/" + name)
	if err != nil {
		writeError(w, &apiError{500, "WebUI asset unavailable", 0})
		return true
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	return true
}
func (a *Application) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if serveWeb(w, r) {
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/admin/") {
		p, _, _ := a.snapshot()
		p.ServeHTTP(w, r)
		return
	}
	if !a.authorized(r) {
		writeError(w, &apiError{401, "Invalid admin API key", 0})
		return
	}
	if !adminOriginAllowed(r) {
		writeError(w, &apiError{403, "Cross-origin admin requests are not allowed", 0})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	switch {
	case path == "health" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		a.healthHandler(w, r)
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
		parts := strings.Split(strings.TrimPrefix(path, "accounts/"), "/")
		if len(parts) == 2 && parts[1] == "test" && r.Method == http.MethodPost {
			a.testAccount(w, r, parts[0])
		} else if len(parts) == 1 {
			a.accountMutation(w, r, parts[0])
		} else {
			writeError(w, &apiError{404, "Endpoint not found", 0})
		}
	default:
		writeError(w, &apiError{404, "Endpoint not found", 0})
	}
}
