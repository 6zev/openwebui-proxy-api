package main

import (
	"context"
	"encoding/json"
	"io"
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
	// lastCost is how long the last successful fetch took, used to keep an
	// expensive list cached longer.
	lastCost time.Duration
}

type cachedModel struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Extra carries every other field the upstream sent (name, description,
	// capabilities, pricing, tags, context length...). The model list is upstream
	// metadata: dropping it forced clients to guess what a model can do.
	Extra map[string]json.RawMessage `json:"-"`
	// Website is set while merging: each model belongs to the website whose
	// account listed it, so the rendered id can be qualified per model instead of
	// stamping one website's prefix onto the whole union.
	Website string `json:"-"`
}

type modelCache struct {
	mu      sync.Mutex
	entries map[string]*modelCacheEntry
	ttl     time.Duration
	// adaptive extends the TTL of an account whose upstream is expensive to
	// query: re-fetching a 21s model list every 5 minutes is pure waste, and the
	// list rarely changes. Up to maxTTL.
	adaptive bool
	maxTTL   time.Duration
}

// adaptiveFactorMultiplier stretches an entry's lifetime to at least this many
// times the time the upstream took to produce it. A 21s list therefore stays for
// a few minutes of its own, while a cheap list still respects the base TTL as a
// floor: the rule is "at least K times the cost, never less than the TTL".
const adaptiveFactorMultiplier = 30

// ttlFor reports the effective TTL for one key. An entry that took long to fetch
// keeps its list proportionally longer (10× the fetch cost, capped), so slow
// upstreams stop paying their cost on a fixed schedule.
func (c *modelCache) ttlFor(key string) time.Duration {
	if c == nil || !c.adaptive {
		if c == nil {
			return 0
		}
		return c.ttl
	}
	entry := c.entry(key)
	entry.mu.Lock()
	cost := entry.lastCost
	entry.mu.Unlock()
	if cost <= 0 {
		return c.ttl
	}
	// time.Duration counts nanoseconds, so the multiplier has to be a Duration
	// too: cost*10 is ten nanoseconds, not ten times the cost.
	scaled := cost * adaptiveFactorMultiplier
	if scaled < c.ttl {
		scaled = c.ttl
	}
	if c.maxTTL > 0 && scaled > c.maxTTL {
		scaled = c.maxTTL
	}
	return scaled
}

func newModelCache(ttl time.Duration) *modelCache {
	return &modelCache{entries: make(map[string]*modelCacheEntry), ttl: ttl}
}

// newAdaptiveModelCache extends entries that were expensive to fetch. maxTTL
// caps the extension; zero means no cap.
func newAdaptiveModelCache(ttl, maxTTL time.Duration) *modelCache {
	cache := newModelCache(ttl)
	cache.adaptive = true
	cache.maxTTL = maxTTL
	return cache
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

func (c *modelCache) put(key string, models []cachedModel) { c.putCost(key, models, 0) }

// putCost stores a list together with the time it took to fetch it, and sizes
// the entry's lifetime from that cost when adaptation is on.
func (c *modelCache) putCost(key string, models []cachedModel, cost time.Duration) {
	if c == nil {
		return
	}
	entry := c.entry(key)
	ttl := c.ttl
	if c.adaptive && cost > 0 {
		ttl = cost * adaptiveFactorMultiplier
		if ttl < c.ttl {
			ttl = c.ttl
		}
		if c.maxTTL > 0 && ttl > c.maxTTL {
			ttl = c.maxTTL
		}
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if cost > 0 {
		entry.lastCost = cost
	}
	entry.models = models
	entry.fetchedAt = time.Now()
	entry.expiresAt = entry.fetchedAt.Add(ttl)
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
		"cost_ms":     int(entry.lastCost / time.Millisecond),
		"expired":     time.Now().After(entry.expiresAt),
		"refreshing":  entry.refreshing,
	}, true
}

// refreshModelCache re-fetches every cached key in the background. It is what
// keeps a slow upstream's list warm without blocking callers.
// refreshModelCache keeps every enabled account's list warm. It walks the
// configured accounts rather than the cache keys: an entry created only on the
// first client request would otherwise never be pre-warmed, and a freshly
// configured account would stay cold until someone listed models.
func (p *Proxy) refreshModelCache(ctx context.Context) {
	if p.modelCache == nil {
		return
	}
	var wg sync.WaitGroup
	for _, a := range p.accounts {
		if a.config.Disabled {
			continue
		}
		if status, _, _, _ := a.health.snapshot(); status == healthFailed {
			// A failed account is retried by the health loop; refreshing here too
			// would double the traffic against a host already known to be down.
			continue
		}
		wg.Add(1)
		go func(a *account) {
			defer wg.Done()
			p.refreshModelEntry(ctx, a)
		}(a)
	}
	wg.Wait()
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
	started := time.Now()
	if models, err := p.fetchModelList(fetchCtx, a); err == nil {
		p.modelCache.putCost(accountKey(a.config.WebsiteID, a.config.ID), models, time.Since(started))
		p.recordRefreshFailure(a, nil)
	} else {
		p.recordRefreshFailure(a, err)
	}
}

// refreshFailureEvery rate-limits the repeated "refresh failed" line. A single
// rejected password used to produce one line every two and a half minutes for
// hours (195 lines in one log), which buries every other finding.
const refreshFailureEvery = 10 * time.Minute

// recordRefreshFailure logs a failure at most once per refreshFailureEvery per
// account, and reports how many consecutive failures it has seen, so a broken
// account is loud once instead of spamming.
func (p *Proxy) recordRefreshFailure(a *account, err error) {
	a.refreshMu.Lock()
	if err == nil {
		if a.refreshFailures > 0 {
			p.debugf("debug: model-cache refresh recovered account=%s website=%s after %d failures", a.config.ID, a.config.WebsiteID, a.refreshFailures)
		}
		a.refreshFailures = 0
		a.refreshLogged = time.Time{}
		a.refreshMu.Unlock()
		return
	}
	a.refreshFailures++
	count := a.refreshFailures
	logged := a.refreshLogged
	if !logged.IsZero() && time.Since(logged) < refreshFailureEvery {
		a.refreshMu.Unlock()
		return
	}
	a.refreshLogged = time.Now()
	a.refreshMu.Unlock()
	p.debugf("debug: model-cache refresh failed account=%s website=%s consecutive=%d err=%v", a.config.ID, a.config.WebsiteID, count, err)
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
// fetchModelList performs the upstream /models call and parses the list.
// The returned models carry no Website tag: it is assigned while merging, so a
// cached batch stays the account's own data.
// modelListPathFor picks the models route for an account. A LibreChat session
// account reads the web UI's /api/models (a session JWT is refused by the Agents
// API); everything else uses the shared mapping.
func modelListPathFor(a *account) string {
	if a.config.Backend == BackendLibreChat && a.config.APIKey == "" {
		return libreChatSessionModelPath()
	}
	return "/api/models"
}

// visibleModels drops models the account already refused. The filter runs at render
// time, not while merging, because a rejection happens AFTER the list was cached —
// filtering during the merge would only take effect on the next refresh.
func (p *Proxy) visibleModels(candidates []*account, models []cachedModel) []cachedModel {
	if !p.config.HideUnavailableModels || len(models) == 0 {
		return models
	}
	hidden := make(map[string]bool, 8)
	for _, a := range candidates {
		for _, model := range models {
			if cooled, _ := p.modelCooled(a, model.ID); cooled {
				hidden[model.ID] = true
			}
		}
	}
	if len(hidden) == 0 {
		return models
	}
	kept := make([]cachedModel, 0, len(models))
	for _, model := range models {
		if !hidden[model.ID] {
			kept = append(kept, model)
		}
	}
	return kept
}

// parseAnyModelList accepts both shapes a model list arrives in: the OpenAI
// {"data":[...]} envelope and LibreChat's provider map. One entry point so the
// request path and the refresher cannot disagree.
func parseAnyModelList(document []byte) ([]cachedModel, error) {
	if items := parseProviderMap(document); items != nil {
		return items, nil
	}
	return parseModelList(document)
}

func (p *Proxy) fetchModelList(ctx context.Context, a *account) ([]cachedModel, error) {
	path := modelListPathFor(a)
	response, err, _ := p.requestWithHeaders(ctx, a, path, "GET", nil, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	rawDocument, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	models, err := parseAnyModelList(rawDocument)
	if err != nil {
		return nil, err
	}
	// A session deployment lists ids only. Decorate them with the endpoint's display
	// label and capabilities so a client sees the same detail an OpenAI-shaped
	// upstream would have sent.
	if a.config.Backend == BackendLibreChat && a.config.APIKey == "" {
		applyEndpointMetadata(models, p.libreChatEndpoints(ctx, a))
	}
	return models, nil
}

// parseProviderMap flattens LibreChat's /api/models shape. It returns nil when the
// document is not a provider map, so callers fall back to the OpenAI shape.
func parseProviderMap(document []byte) []cachedModel {
	var providers map[string]json.RawMessage
	if json.Unmarshal(document, &providers) != nil || len(providers) == 0 {
		return nil
	}
	if _, hasData := providers["data"]; hasData {
		return nil
	}
	items := make([]cachedModel, 0, 32)
	seen := make(map[string]bool, 32)
	providerNames := make([]string, 0, len(providers))
	for name := range providers {
		providerNames = append(providerNames, name)
	}
	sortStrings(providerNames)
	for _, provider := range providerNames {
		var ids []string
		if json.Unmarshal(providers[provider], &ids) != nil {
			continue
		}
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			items = append(items, cachedModel{ID: id, OwnedBy: provider})
		}
	}
	if len(items) == 0 {
		return nil
	}
	return items
}

// parseModelList turns an upstream model-list body into cached models, keeping every
// field the struct does not name. Both the background refresher and the request path
// go through here: they used to parse separately, and only one of them kept extras.
func parseModelList(document []byte) ([]cachedModel, error) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	raw := document
	if json.Unmarshal(document, &envelope) == nil && envelope.Data != nil {
		raw = envelope.Data
	}
	var items []cachedModel
	if json.Unmarshal(raw, &items) != nil {
		return nil, &apiError{status: 502, message: "Unexpected model-list response"}
	}
	if items == nil {
		return nil, &apiError{status: 502, message: "Unexpected model-list response"}
	}
	filtered := make([]cachedModel, 0, len(items))
	for _, item := range items {
		if item.ID == "" {
			continue
		}
		if item.OwnedBy == "" {
			item.OwnedBy = "open-webui"
		}
		filtered = append(filtered, item)
	}
	attachModelExtras(raw, filtered)
	return filtered, nil
}

// attachModelExtras keeps every field of each upstream model that the struct does
// not name. Without this a caller sees only id/created/owned_by, so capabilities,
// pricing, context length and display names never reach the client.
func attachModelExtras(raw json.RawMessage, items []cachedModel) {
	var documents []map[string]json.RawMessage
	if json.Unmarshal(raw, &documents) != nil {
		return
	}
	for i := range items {
		if i >= len(documents) || documents[i] == nil {
			continue
		}
		extra := make(map[string]json.RawMessage, len(documents[i]))
		kept := false
		for key, value := range documents[i] {
			switch key {
			case "id", "created", "owned_by":
				continue
			}
			extra[key] = value
			kept = true
		}
		if kept {
			items[i].Extra = extra
		}
	}
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

// writeModelList renders the OpenAI model list. With prefixing on, each model is
// qualified by the website that owns it: a single prefix taken from candidates[0]
// would label every website's models with one website's name, which is how
// "ai.baselgovernance.org/gpt-5.6-luna" appeared for a model only kaethegpt
// serves. fallback is used for models that carry no website (single-account and
// pinned listings).
func writeModelList(w http.ResponseWriter, models []cachedModel, fallback string) {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		id := m.ID
		prefix := m.Website
		if prefix == "" {
			prefix = fallback
		}
		if prefix != "" {
			id = prefix + "/" + m.ID
		}
		entry := make(map[string]any, len(m.Extra)+4)
		for key, value := range m.Extra {
			// Raw messages are re-emitted verbatim; a field the caller ignores costs
			// nothing, a field that was dropped cannot be recovered.
			entry[key] = value
		}
		entry["id"] = id
		entry["object"] = "model"
		entry["created"] = m.Created
		entry["owned_by"] = m.OwnedBy
		out = append(out, entry)
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": out})
}

func ageSeconds(t time.Time) string {
	return strconv.FormatInt(int64(time.Since(t)/time.Second), 10)
}

// mergedModelList builds the listing a caller should see: the union of every
// candidate account's models, de-duplicated by model id.
//
// The old code served candidates[0] only, and round-robin rotates that index on
// every request — so a deployment with two websites advertised one site's models
// on one call and the other site's on the next. A chat selects its account from
// the same candidate set, so any model shown here must be reachable.
//
// state is "" when there is nothing cached at all (a miss upstream), "hit" when
// every candidate has a fresh entry, and "stale" when at least one entry is
// expired (the caller still gets a usable list and a refresh runs behind it).
// modelListState describes what a merged listing could cover.
type modelListState struct {
	models   []cachedModel
	fetched  time.Time
	state    string // "" when nothing at all is cached
	missing  []*account
	anyStale bool
}

// mergedModelList builds the listing a caller should see: the union of every
// candidate account's models.
//
// Candidates without a cached entry are reported in missing rather than silently
// skipped: the union would otherwise depend on which account happened to be
// warmed, which is exactly how the listing flapped between two websites.
func (p *Proxy) mergedModelList(candidates []*account) modelListState {
	result := modelListState{}
	if p.modelCache == nil || len(candidates) == 0 {
		return result
	}
	merged := make([]cachedModel, 0, 64)
	// A model id that exists on several websites must be listed once per owner:
	// the caller routes by model id, so collapsing two websites' "gpt-4o" into one
	// entry silently hides the second site and sends chats to whichever account
	// round-robin picks — which then answers "Model not found" for a model that
	// really does exist elsewhere.
	seen := make(map[string]bool, 64)
	covered := 0
	for _, a := range candidates {
		key := accountKey(a.config.WebsiteID, a.config.ID)
		models, fetchedAt, ok := p.modelCache.get(key)
		if !ok {
			if staleModels, staleAt, staleOK := p.modelCache.peek(key); staleOK {
				models, fetchedAt = staleModels, staleAt
				result.anyStale = true
				ok = true
			}
		}
		if !ok {
			result.missing = append(result.missing, a)
			continue
		}
		covered++
		if fetchedAt.After(result.fetched) {
			result.fetched = fetchedAt
		}
		for _, model := range models {
			// Qualify by website while merging so two sites that list the same model
			// both survive the union. A NUL byte cannot appear in an id, so the
			// qualifier can never collide with a real model name or leak into output.
			qualifier := ""
			if a.config.WebsiteID != "" {
				qualifier = a.config.WebsiteID + "\x00"
			}
			if seen[qualifier+model.ID] {
				continue
			}
			seen[qualifier+model.ID] = true
			model.Website = a.config.WebsiteID
			merged = append(merged, model)
		}
	}
	if covered == 0 {
		return result
	}
	result.models = merged
	switch {
	case result.anyStale:
		result.state = "stale"
	case len(result.missing) > 0:
		// Partially warm: callers still get what they can reach, but the cache is
		// flagged partial and the missing accounts are fetched behind them.
		result.state = "partial"
	default:
		result.state = "hit"
	}
	return result
}

// refreshAllCandidates refreshes every candidate's entry in the background, so a
// stale union becomes a fresh union for the next caller.
func (p *Proxy) refreshAllCandidates(ctx context.Context, candidates []*account) {
	for _, a := range candidates {
		if a.config.Disabled {
			continue
		}
		p.refreshModelEntry(ctx, a)
	}
}

// primeCandidateLists fetches every candidate that has no cached entry, in
// parallel. It is used on a cold start so the first model listing is complete
// instead of showing only the account round-robin happened to pick.
func (p *Proxy) primeCandidateLists(ctx context.Context, candidates []*account) {
	if p.modelCache == nil {
		return
	}
	var wg sync.WaitGroup
	for _, a := range candidates {
		key := accountKey(a.config.WebsiteID, a.config.ID)
		if _, _, ok := p.modelCache.get(key); ok {
			continue
		}
		if _, _, ok := p.modelCache.peek(key); ok {
			continue
		}
		if a.config.Disabled {
			continue
		}
		wg.Add(1)
		go func(a *account) {
			defer wg.Done()
			p.refreshModelEntry(ctx, a)
		}(a)
	}
	wg.Wait()
}
