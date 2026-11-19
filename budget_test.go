package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const affordabilityRefusal = `{"error":{"message":"This request requires more credits, or fewer max_tokens. You requested up to 128000 tokens, but can only afford 106664. To increase, visit https://openrouter.ai/settings/credits and add more credits","code":402}}`

// The ceiling is stated in the refusal, so it can be read out of it.
func TestAffordableMaxTokens(t *testing.T) {
	if got := affordableMaxTokens([]byte(affordabilityRefusal)); got != 106664 {
		t.Errorf("affordableMaxTokens = %d, want 106664", got)
	}
	if got := affordableMaxTokens([]byte(`{"detail":"no credits remaining"}`)); got != 0 {
		t.Errorf("a refusal without a figure must give 0, got %d", got)
	}
	limit, ok := budgetRefusal([]byte(affordabilityRefusal))
	if !ok || limit != 106664 {
		t.Errorf("budgetRefusal = %d, %v", limit, ok)
	}
	// A generic credit error is not a budget refusal: lowering max_tokens cannot help.
	if _, ok := budgetRefusal([]byte(`{"detail":"You have no credits remaining. Add credits"}`)); ok {
		t.Error("generic credit error treated as a budget refusal")
	}
}

func TestClampMaxTokens(t *testing.T) {
	lowered, changed := clampMaxTokens([]byte(`{"model":"m","max_tokens":128000,"messages":[]}`), 1000)
	if !changed || !strings.Contains(string(lowered), `"max_tokens":1000`) {
		t.Errorf("clamp = %s (changed=%v)", lowered, changed)
	}
	// Already under the ceiling: nothing to do, and no pointless retry.
	if _, changed := clampMaxTokens([]byte(`{"model":"m","max_tokens":50}`), 1000); changed {
		t.Error("clamped a request that already fit")
	}
	// No budget at all: the model default is what cannot be afforded, so cap it.
	capped, changed := capMaxTokens([]byte(`{"model":"m"}`), 500)
	if !changed || !strings.Contains(string(capped), `"max_tokens":500`) {
		t.Errorf("cap = %s (changed=%v)", capped, changed)
	}
	var document map[string]any
	if json.Unmarshal(capped, &document) != nil || document["model"] != "m" {
		t.Errorf("cap lost other fields: %s", capped)
	}
}

// End to end: the first attempt is refused for budget, the second must go out with a
// smaller max_tokens and succeed.
func TestBudgetRefusalRetriedWithSmallerBudget(t *testing.T) {
	var attempts atomic.Int32
	var secondBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "signin") {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, affordabilityRefusal)
			return
		}
		secondBody.Store(string(raw))
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	cfg.Cooldown = time.Minute
	s := newProxyTestServer(t, cfg)
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","max_tokens":128000,"messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusOK)
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	body, _ := secondBody.Load().(string)
	if !strings.Contains(body, `"max_tokens":106664`) {
		t.Errorf("retry did not carry the affordable ceiling: %s", body)
	}
}

// A nested error object carrying a numeric code used to break the decode and lose
// the reason entirely — the caller then saw only "returned HTTP 402".
func TestUpstreamDetailSurvivesNumericCode(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"boom","code":402}}`:      "boom",
		`{"error":{"message":"boom"}}`:                 "boom",
		`{"detail":"plain detail"}`:                    "plain detail",
		`{"message":"flat message"}`:                   "flat message",
		`{"error":{"message":"x","metadata":{"a":1}}}`: "x",
	}
	for body, want := range cases {
		if got := upstreamDetail([]byte(body)); got != want {
			t.Errorf("upstreamDetail(%s) = %q, want %q", body, got, want)
		}
	}
}
