package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestModelCacheExpires(t *testing.T) {
	var hits atomic.Int32
	u := slowModelsUpstream(t, 0, &hits)
	cfg := proxyTestConfig(u, "acc")
	cfg.ModelCacheTTL = 50 * time.Millisecond
	s := newProxyTestServer(t, cfg)
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, ""), 200)
	time.Sleep(90 * time.Millisecond)
	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if resp.header.Get("X-Model-Cache") != "miss" {
		t.Errorf("expired entry reported %q, want miss", resp.header.Get("X-Model-Cache"))
	}
	if hits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2", hits.Load())
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
