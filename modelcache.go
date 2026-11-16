package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// modelCache keeps the last successful model list per account. Slow upstreams
// (a /models call that takes 20s) should not pay that cost on every request, and
// a transient failure should not wipe a list the client just used.
//
// The cache is keyed by the account identity, so two websites sharing an account
// ID never see each other's models.
type modelCacheEntry struct {
	mu        sync.Mutex
	models    []cachedModel
	fetchedAt time.Time
	// expiresAt is when the entry stops being served fresh.
	expiresAt time.Time
	// refreshing marks an in-flight background fetch so a burst of callers
	// triggers one upstream call instead of one per request.
	refreshing bool
}

type cachedModel struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelCache struct {
	mu      sync.Mutex
	entries map[string]*modelCacheEntry
	ttl     time.Duration
}

func newModelCache(ttl time.Duration) *modelCache {
	return &modelCache{entries: make(map[string]*modelCacheEntry), ttl: ttl}
}

func (c *modelCache) entry(key string) *modelCacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		entry = &modelCacheEntry{}
		c.entries[key] = entry
	}
	return entry
}

// get returns a cached list when it is still fresh. The bool reports a hit.
func (c *modelCache) get(key string) ([]cachedModel, time.Time, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, time.Time{}, false
	}
	entry := c.entry(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.models == nil || time.Now().After(entry.expiresAt) {
		return nil, time.Time{}, false
	}
	return entry.models, entry.fetchedAt, true
}

// beginRefresh claims the right to fetch this key in the background. It reports
// false when a refresh is already running, so bursts collapse into one fetch.
func (c *modelCache) beginRefresh(key string) (*modelCacheEntry, bool) {
	if c == nil {
		return nil, false
	}
	entry := c.entry(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.refreshing {
		return nil, false
	}
	entry.refreshing = true
	return entry, true
}

func (c *modelCache) endRefresh(entry *modelCacheEntry) {
	if entry == nil {
		return
	}
	entry.mu.Lock()
	entry.refreshing = false
	entry.mu.Unlock()
}

// peek returns a cached list even when expired, for stale-while-error fallback.
func (c *modelCache) peek(key string) ([]cachedModel, time.Time, bool) {
	if c == nil {
		return nil, time.Time{}, false
	}
	entry := c.entry(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.models == nil {
		return nil, time.Time{}, false
	}
	return entry.models, entry.fetchedAt, true
}

func (c *modelCache) put(key string, models []cachedModel) {
	if c == nil {
		return
	}
	entry := c.entry(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.models = models
	entry.fetchedAt = time.Now()
	entry.expiresAt = entry.fetchedAt.Add(c.ttl)
}

func (c *modelCache) drop(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// keys lists the cached account keys, for the admin endpoints.
func (c *modelCache) keys() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.entries))
	for key := range c.entries {
		keys = append(keys, key)
	}
	return keys
}

// describe reports one cached entry for the admin API.
func (c *modelCache) describe(key string) (map[string]any, bool) {
	if c == nil {
		return nil, false
	}
	entry := c.entry(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.models == nil {
		return nil, false
	}
	age := time.Since(entry.fetchedAt)
	return map[string]any{
		"key":         key,
		"models":      len(entry.models),
		"age_seconds": int(age / time.Second),
		"ttl_seconds": int(c.ttl / time.Second),
		"expired":     time.Now().After(entry.expiresAt),
		"refreshing":  entry.refreshing,
	}, true
}

// refreshModelCache re-fetches every cached key in the background. It is what
// keeps a slow upstream's list warm without blocking callers.
func (p *Proxy) refreshModelCache(ctx context.Context) {
	if p.modelCache == nil {
		return
	}
	for _, key := range p.modelCache.keys() {
		a := p.accountByKey(key)
		if a == nil || a.config.Disabled {
			continue
		}
		p.refreshModelEntry(ctx, a)
	}
}

// refreshModelEntry fetches one account's list and stores it. The per-account
// timeout is applied here, so a sweep over several websites does not share one
// deadline and quietly skip the slow ones at the end.
func (p *Proxy) refreshModelEntry(ctx context.Context, a *account) {
	entry, claimed := p.modelCache.beginRefresh(accountKey(a.config.WebsiteID, a.config.ID))
	if !claimed {
		return
	}
	defer p.modelCache.endRefresh(entry)
	fetchCtx, cancel := context.WithTimeout(ctx, p.probeTimeoutFor(a))
	defer cancel()
	if models, err := p.fetchModelList(fetchCtx, a); err == nil {
		p.modelCache.put(accountKey(a.config.WebsiteID, a.config.ID), models)
	} else {
		p.debugf("debug: model-cache refresh failed account=%s website=%s err=%v", a.config.ID, a.config.WebsiteID, err)
	}
}

// accountByKey resolves a "<website-id>/<account-id>" cache key.
func (p *Proxy) accountByKey(key string) *account {
	websiteID, accountID := splitAccountKey(key)
	for _, a := range p.accounts {
		if a.config.ID == accountID && (websiteID == "" || a.config.WebsiteID == websiteID) {
			return a
		}
	}
	return nil
}

// fetchModelList performs the upstream /models call and parses the list.
func (p *Proxy) fetchModelList(ctx context.Context, a *account) ([]cachedModel, error) {
	path := "/api/models"
	if a.config.Backend == BackendLibreChat {
		mapped, ok := libreChatUpstreamPath("/v1/models")
		if !ok {
			return nil, &apiError{404, "Endpoint not found", 0}
		}
		path = mapped
	}
	response, err, _ := p.requestWithHeaders(ctx, a, path, "GET", nil, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&envelope); err != nil {
		return nil, &apiError{502, "Unexpected model-list response", 0}
	}
	raw := envelope.Data
	if raw == nil {
		return nil, &apiError{502, "Unexpected model-list response", 0}
	}
	var items []cachedModel
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, &apiError{502, "Unexpected model-list response", 0}
	}
	return items, nil
}

// modelCacheKeyFor picks the cache key for a request. A pinned account gets its
// own entry; otherwise the first candidate is used, so a website-level request
// maps to whichever account round-robin would have served it.
func modelCacheKeyFor(candidates []*account) string {
	if len(candidates) == 0 {
		return ""
	}
	return accountKey(candidates[0].config.WebsiteID, candidates[0].config.ID)
}

// modelPrefixFor reports the "<id>/" prefix a consumer must send back, mirroring
// what the live listing would have produced.
func (p *Proxy) modelPrefixFor(r *http.Request, candidates []*account) string {
	if !p.config.ModelPrefix || len(candidates) == 0 {
		return ""
	}
	first := candidates[0]
	if r.Header.Get("X-Account-ID") != "" && first.config.WebsiteID != "" {
		return first.config.ID
	}
	return first.config.WebsiteID
}

// writeModelList renders the OpenAI model list, adding the routing prefix.
func writeModelList(w http.ResponseWriter, models []cachedModel, prefix string) {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		id := m.ID
		if prefix != "" {
			id = prefix + "/" + m.ID
		}
		out = append(out, map[string]any{"id": id, "object": "model", "created": m.Created, "owned_by": m.OwnedBy})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": out})
}

func ageSeconds(t time.Time) string {
	return strconv.FormatInt(int64(time.Since(t)/time.Second), 10)
}
