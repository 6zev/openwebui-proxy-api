package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowModelsUpstream answers /api/models after a delay, counting calls.
func slowModelsUpstream(t *testing.T, delay time.Duration, hits *atomic.Int32) *proxyTestUpstream {
	t.Helper()
	u := newProxyTestUpstream(t)
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		hits.Add(1)
		time.Sleep(delay)
		_, _ = w.Write([]byte(`{"data":[{"id":"slow-model","created":1,"owned_by":"x"}]}`))
	})
	return u
}

func TestModelCacheServesRepeatedListings(t *testing.T) {
	var hits atomic.Int32
	u := slowModelsUpstream(t, 300*time.Millisecond, &hits)
	cfg := proxyTestConfig(u, "acc")
	cfg.ModelCacheTTL = time.Minute
	s := newProxyTestServer(t, cfg)

	start := time.Now()
	first := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, first, 200)
	firstCost := time.Since(start)
	if first.header.Get("X-Model-Cache") != "miss" {
		t.Errorf("first response cache header = %q, want miss", first.header.Get("X-Model-Cache"))
	}

	start = time.Now()
	second := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, second, 200)
	secondCost := time.Since(start)
	if second.header.Get("X-Model-Cache") != "hit" {
		t.Errorf("second response cache header = %q, want hit", second.header.Get("X-Model-Cache"))
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (second served from cache)", hits.Load())
	}
	if secondCost > firstCost/2 {
		t.Errorf("cached listing took %v, upstream took %v", secondCost, firstCost)
	}
	if !contains(second.body, "slow-model") {
		t.Errorf("cached body lost the model: %s", second.body)
	}
}

func TestModelCacheDisabled(t *testing.T) {
	var hits atomic.Int32
	u := slowModelsUpstream(t, 0, &hits)
	cfg := proxyTestConfig(u, "acc")
	cfg.ModelCacheTTL = 0
	s := newProxyTestServer(t, cfg)
	for i := 0; i < 3; i++ {
		resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
		requireProxyStatus(t, resp, 200)
		if resp.header.Get("X-Model-Cache") != "" {
			t.Errorf("cache header present while disabled: %q", resp.header.Get("X-Model-Cache"))
		}
	}
	if hits.Load() != 3 {
		t.Errorf("upstream hits = %d, want 3", hits.Load())
	}
}

// An expired entry is served immediately as "stale" while a background refresh
// runs, so a slow upstream is never on the caller's clock. The next request sees
// the refreshed list as a hit.
func TestModelCacheServesStaleWhileRefreshing(t *testing.T) {
	var hits atomic.Int32
	u := slowModelsUpstream(t, 0, &hits)
	cfg := proxyTestConfig(u, "acc")
	cfg.ModelCacheTTL = 50 * time.Millisecond
	// The adaptive rule stretches an expensive entry; use a fixed cache here so
	// the test measures staleness, not the extension.
	cfg.DisableAdaptiveCache = true
	s := newProxyTestServer(t, cfg)
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, ""), 200)
	time.Sleep(90 * time.Millisecond)
	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if got := resp.header.Get("X-Model-Cache"); got != "stale" {
		t.Errorf("expired entry reported %q, want stale", got)
	}
	// Background refresh must land without another client request.
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() < 2 {
		t.Fatalf("expired entry was never refreshed; upstream hits = %d", hits.Load())
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		resp = proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
		if resp.header.Get("X-Model-Cache") == "hit" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := resp.header.Get("X-Model-Cache"); got != "hit" {
		t.Errorf("refreshed entry reported %q, want hit", got)
	}
}

// Refresher default: unset MODEL_CACHE_REFRESH_SECONDS must not disable it.
func TestModelCacheRefreshIntervalDefaultsToHalfTTL(t *testing.T) {
	t.Setenv("MODEL_CACHE_REFRESH_SECONDS", "")
	if got := modelCacheRefreshInterval(300 * time.Second); got != 150*time.Second {
		t.Errorf("default interval = %s, want 150s", got)
	}
	t.Setenv("MODEL_CACHE_REFRESH_SECONDS", "0")
	if got := modelCacheRefreshInterval(300 * time.Second); got != 0 {
		t.Errorf("explicit 0 should disable, got %s", got)
	}
	t.Setenv("MODEL_CACHE_REFRESH_SECONDS", "20")
	if got := modelCacheRefreshInterval(300 * time.Second); got != 20*time.Second {
		t.Errorf("configured interval = %s, want 20s", got)
	}
	t.Setenv("MODEL_CACHE_REFRESH_SECONDS", "")
	if got := modelCacheRefreshInterval(500 * time.Millisecond); got != time.Second {
		t.Errorf("tiny ttl interval = %s, want 1s floor", got)
	}
}

// When every account fails, a previously served list is better than an error.
func TestModelCacheServesStaleOnUpstreamError(t *testing.T) {
	var broken atomic.Bool
	u := newProxyTestUpstream(t)
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		if broken.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m","created":1,"owned_by":"x"}]}`))
	})
	cfg := proxyTestConfig(u, "acc")
	cfg.ModelCacheTTL = 50 * time.Millisecond
	cfg.DisableAdaptiveCache = true
	s := newProxyTestServer(t, cfg)
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, ""), 200)
	broken.Store(true)
	time.Sleep(90 * time.Millisecond)
	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if got := resp.header.Get("X-Model-Cache"); got != "stale" {
		t.Errorf("cache header = %q, want stale", got)
	}
	if !strings.Contains(resp.body, `"m"`) {
		t.Errorf("stale list missing models: %s", resp.body)
	}
}

func TestModelCacheIsolatesAccounts(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m","created":1,"owned_by":"x"}]}`))
	})
	cfg := proxyTestConfig(u, "one", "two")
	cfg.ModelCacheTTL = time.Minute
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := p.candidatesFor("", "")
	if err != nil {
		t.Fatal(err)
	}
	key := modelCacheKeyFor(candidates)
	if key == "" {
		t.Fatal("empty cache key")
	}
	if key != "one" && key != "two" {
		t.Errorf("cache key = %q, want one of the account ids", key)
	}
}

func TestProbeUsesConfiguredTimeout(t *testing.T) {
	p := &Proxy{config: Config{Timeout: 300 * time.Second, ProbeTimeout: 45 * time.Second}}
	if got := p.probeTimeout(); got != 45*time.Second {
		t.Errorf("probeTimeout = %v, want 45s", got)
	}
	p.config.ProbeTimeout = 0
	if got := p.probeTimeout(); got != 300*time.Second {
		t.Errorf("probeTimeout without override = %v, want upstream timeout", got)
	}
}

// TestProbeDoesNotFailSlowUpstream covers the reported bug: a models endpoint
// that takes longer than the old hard 20s ceiling must still be healthy when the
// configured timeout is larger.
func TestProbeDoesNotFailSlowUpstream(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		time.Sleep(1200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	cfg := proxyTestConfig(u, "acc")
	cfg.Timeout = 10 * time.Second
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.probe(context.Background(), p.accounts[0], false); err != nil {
		t.Fatalf("slow but healthy upstream reported a failure: %v", err)
	}
	if status, _, _, _ := p.accounts[0].health.snapshot(); status != healthOK {
		t.Errorf("health status = %s, want ok", status)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = httptest.NewServer

// A list that took 21s to fetch must not be re-fetched every 5 minutes: the TTL
// grows with the cost of fetching it, capped by MODEL_CACHE_MAX_SECONDS.
func TestAdaptiveModelCacheExtendsExpensiveEntries(t *testing.T) {
	cache := newAdaptiveModelCache(5*time.Minute, time.Hour)
	cache.putCost("w/a", []cachedModel{{ID: "m"}}, 21*time.Second)
	if got := cache.ttlFor("w/a"); got != 630*time.Second {
		t.Errorf("adaptive ttl = %s, want 630s (30x the 21s cost)", got)
	}
	// A cheap list keeps the base TTL.
	cache.putCost("w/b", []cachedModel{{ID: "m"}}, 800*time.Millisecond)
	if got := cache.ttlFor("w/b"); got != 5*time.Minute {
		t.Errorf("cheap ttl = %s, want the base ttl", got)
	}
	// The extension is capped by MODEL_CACHE_MAX_SECONDS.
	cache.putCost("w/c", []cachedModel{{ID: "m"}}, 40*time.Second) // 40s x 30 = 20m, under the cap
	if got := cache.ttlFor("w/c"); got != 20*time.Minute {
		t.Errorf("scaled ttl = %s, want 20m", got)
	}
	cache.putCost("w/d", []cachedModel{{ID: "m"}}, 5*time.Minute) // 5m x 30 = 2h30m -> capped
	if got := cache.ttlFor("w/d"); got != time.Hour {
		t.Errorf("capped ttl = %s, want 1h", got)
	}
}

// Without adaptation the TTL stays exactly as configured.
func TestModelCacheFixedTTLUnchanged(t *testing.T) {
	cache := newModelCache(2 * time.Minute)
	cache.putCost("w/a", []cachedModel{{ID: "m"}}, 30*time.Second)
	if got := cache.ttlFor("w/a"); got != 2*time.Minute {
		t.Errorf("fixed ttl = %s, want 2m", got)
	}
}

// A rejected password must be reported once, not on every sweep: one real log had
// 195 identical "refresh failed" lines that buried everything else.
func TestRefreshFailureLoggingIsRateLimited(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	p, err := NewProxy(proxyTestConfig(newProxyTestUpstream(t), "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	p.config.DebugLog = true
	account := p.accounts[0]
	loginErr := errors.New("Open WebUI login failed; check credentials and password-login settings")
	for i := 0; i < 20; i++ {
		p.recordRefreshFailure(account, loginErr)
	}
	if got := strings.Count(buf.String(), "refresh failed"); got != 1 {
		t.Errorf("logged %d failures within one window, want 1:\n%s", got, buf.String())
	}
	// The single line reports the counter value when it was emitted (1), and the
	// counter keeps growing silently afterwards.
	if !strings.Contains(buf.String(), "consecutive=1") {
		t.Errorf("failure counter missing from the line:\n%s", buf.String())
	}
	account.refreshMu.Lock()
	silent := account.refreshFailures
	account.refreshMu.Unlock()
	if silent != 20 {
		t.Errorf("counter after silent failures = %d, want 20", silent)
	}
	// Recovery is reported and resets the counter.
	p.recordRefreshFailure(account, nil)
	if !strings.Contains(buf.String(), "recovered") {
		t.Errorf("recovery not reported:\n%s", buf.String())
	}
	account.refreshMu.Lock()
	failures := account.refreshFailures
	account.refreshMu.Unlock()
	if failures != 0 {
		t.Errorf("counter not reset: %d", failures)
	}
}

// MODEL_CACHE_SECONDS=0 disables the cache instead of failing to start.
func TestModelCacheSecondsZeroDisablesCache(t *testing.T) {
	t.Setenv("MODEL_CACHE_SECONDS", "0")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("MODEL_CACHE_SECONDS=0 should be accepted: %v", err)
	}
	if cfg.ModelCacheTTL != 0 {
		t.Errorf("ttl = %v, want 0 (disabled)", cfg.ModelCacheTTL)
	}
	t.Setenv("MODEL_CACHE_SECONDS", "-5")
	if _, err := configFromEnv(); err == nil {
		t.Error("negative value should still be rejected")
	}
}
