package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// healthUpstream serves models for healthy accounts and 401 for broken ones.
func healthUpstream(t *testing.T, broken map[string]bool) *proxyTestUpstream {
	t.Helper()
	u := newProxyTestUpstream(t)
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer bad") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	})
	return u
}

func TestHealthProbeMarksDeadAccountAndSkipsIt(t *testing.T) {
	u := healthUpstream(t, nil)
	cfg := proxyTestConfig(u, "good", "bad")
	cfg.Cooldown = time.Second
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Force the "bad" account into a failed state.
	p.accounts[1].health.record(&apiError{401, "credentials rejected", 0}, 0)
	report := p.healthReport()
	if report[1]["status"] != string(healthFailed) {
		t.Fatalf("bad account status = %v", report[1]["status"])
	}
	if report[1]["available"] != false {
		t.Errorf("failed account still advertised available")
	}
	for i := 0; i < 5; i++ {
		candidates, err := p.candidatesFor("", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range candidates {
			if a.config.ID == "bad" {
				t.Fatalf("failed account returned in rotation: %v", a.config.ID)
			}
		}
	}
	if _, err := p.candidatesFor("bad", ""); err == nil {
		t.Error("pinned failed account was accepted")
	}
}

func TestHealthProbesRecoverAndRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	u := newProxyTestUpstream(t)
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	})
	p, err := NewProxy(proxyTestConfig(u, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if failures := p.runHealthProbes(context.Background()); failures != 1 {
		t.Fatalf("failures = %d, want 1", failures)
	}
	if status, _, _, _ := p.accounts[0].health.snapshot(); status != healthFailed {
		t.Fatalf("status = %s, want failed", status)
	}
	fail.Store(false)
	if failures := p.runHealthProbes(context.Background()); failures != 0 {
		t.Fatalf("recovery failures = %d, want 0", failures)
	}
	if status, _, _, _ := p.accounts[0].health.snapshot(); status != healthOK {
		t.Fatalf("status = %s, want ok", status)
	}
}

func TestHealthEndpointShapes(t *testing.T) {
	u := healthUpstream(t, nil)
	p, err := NewProxy(proxyTestConfig(u, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	p.runHealthProbes(context.Background())
	server := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	// Probes ran against a different instance; just assert the JSON contract.
	_ = p
	resp := proxyTestRequest(t, server, http.MethodGet, "/v1/health", "", testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	var body struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp.body), &body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body.Object != "list" || len(body.Data) != 1 {
		t.Fatalf("unexpected health payload: %s", resp.body)
	}
	for _, key := range []string{"id", "status", "available", "backend"} {
		if _, ok := body.Data[0][key]; !ok {
			t.Errorf("health entry missing %q: %v", key, body.Data[0])
		}
	}
	probe := proxyTestRequest(t, server, http.MethodPost, "/v1/health/probe", "", testProxyKey, "")
	requireProxyStatus(t, probe, 200)
	if !strings.Contains(probe.body, `"failures":0`) {
		t.Errorf("probe result = %s", probe.body)
	}
	if unauth := proxyTestRequest(t, server, http.MethodGet, "/v1/health", "", "", ""); unauth.status != 401 {
		t.Errorf("health without key = %d", unauth.status)
	}
}

func TestHealthSanitizesErrors(t *testing.T) {
	if got := sanitizeProbeError(&apiError{401, "credentials rejected", 0}); got != "credentials rejected" {
		t.Errorf("apiError passthrough = %q", got)
	}
	err := sanitizeProbeError(io.ErrUnexpectedEOF)
	if strings.Contains(err, "token") || strings.Contains(err, "Bearer") {
		t.Errorf("unsafe sanitized error: %q", err)
	}
}
