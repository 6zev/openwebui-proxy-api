package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Health probing keeps a dead account out of rotation instead of discovering it
// on a user's request. Probes reuse the account's real auth path (Open WebUI
// login or LibreChat static key) and a cheap read-only endpoint.
const (
	healthProbePath      = "/v1/models"
	healthMaxBodyBytes   = 1 << 20
	defaultProbeInterval = 5 * time.Minute
)

type healthStatus string

const (
	healthUnknown healthStatus = "unknown"
	healthOK      healthStatus = "ok"
	healthFailed  healthStatus = "failed"
)

type accountHealth struct {
	mu        sync.Mutex
	status    healthStatus
	checkedAt time.Time
	err       string
	latency   time.Duration
}

func (h *accountHealth) snapshot() (healthStatus, time.Time, string, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status, h.checkedAt, h.err, h.latency
}

func (h *accountHealth) record(err error, latency time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checkedAt = time.Now()
	h.latency = latency
	if err != nil {
		h.status = healthFailed
		h.err = sanitizeProbeError(err)
		return
	}
	h.status = healthOK
	h.err = ""
}

// sanitizeProbeError strips anything that could carry credentials or internal
// hosts into API responses and logs.
func sanitizeProbeError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	var known *apiError
	if errors.As(err, &known) {
		return known.message
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "probe timed out"
	case strings.Contains(message, "connection refused"):
		return "connection refused"
	}
	return "probe failed"
}

// probeTimeout bounds a single probe. It deliberately does not shrink the
// configured timeout to a fixed ceiling: slow-but-healthy upstreams (a model
// list that takes 20s) would otherwise be marked broken forever.
func (p *Proxy) probeTimeout() time.Duration {
	if p.config.ProbeTimeout > 0 {
		return p.config.ProbeTimeout
	}
	return p.config.Timeout
}

// isStoredErr reports whether err came from cached state rather than from an
// upstream round-trip, which means the account was never actually contacted.
func isStoredErr(err error) bool {
	var stored *apiErrorStored
	return errors.As(err, &stored)
}

// probeLoginTimeout is the signin budget. A real signin against a reachable host
// answers in well under a second; anything slower is a slow or dead host, and
// the probe caller decides whether that counts as unavailable.
const probeLoginTimeout = 7 * time.Second

// probeAuth resolves credentials for a probe without letting cached negative
// state lie to the operator.
//
// Every probe is fresh evidence, so the interactive check must talk to the
// upstream even when the account was blocked by an earlier transient failure:
// otherwise a single blip freezes the account as unavailable for a whole
// cooldown and the "Kiểm tra" button just replays that stale verdict. Regular
// selection keeps using the cached block, so a permanently broken account is
// still not hammered on every request.
func (p *Proxy) probeAuth(ctx context.Context, a *account, deep bool) (string, error) {
	if deep {
		a.mu.Lock()
		a.loginBlocked = time.Time{}
		a.token = ""
		a.expires = time.Time{}
		a.mu.Unlock()
		return p.loginProbe(ctx, a)
	}
	return backendAuthHeader(ctx, p, a)
}

// loginProbe is login() with a probe-sized budget and no cooldown bookkeeping:
// the probe owns the verdict, so it must not leave a temporary block behind if
// the request is merely slow.
func (p *Proxy) loginProbe(ctx context.Context, a *account) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if a.token != "" && (a.expires.IsZero() || time.Until(a.expires) > 30*time.Second) {
		return a.token, nil
	}
	loginCtx, cancel := context.WithTimeout(ctx, p.probeLoginTimeout())
	defer cancel()
	data, _ := json.Marshal(map[string]string{"email": a.config.Email, "password": a.config.Password})
	req, err := http.NewRequestWithContext(loginCtx, http.MethodPost, a.config.BaseURL+"/api/v1/auths/signin", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			d := p.retryDuration(resp.Header.Get("Retry-After"))
			return "", &apiError{status: 429, message: "Open WebUI login rate limited", retryAfter: int((d + time.Second - 1) / time.Second)}
		}
		return "", &apiError{status: resp.StatusCode, message: "credentials rejected"}
	}
	var session struct {
		Token     string `json:"token"`
		ExpiresAt *int64 `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&session); err != nil || session.Token == "" {
		return "", &apiError{status: 502, message: "login returned no token"}
	}
	a.token = session.Token
	a.expires = time.Time{}
	if session.ExpiresAt != nil {
		a.expires = time.Unix(*session.ExpiresAt, 0)
	}
	return a.token, nil
}

// probe checks one account without touching the shared request path. deep is
// false for the background loop (respect cached blocks, stay cheap) and true for
// an explicit operator check.
func (p *Proxy) probe(ctx context.Context, a *account, deep bool) error {
	start := time.Now()
	auth, err := p.probeAuth(ctx, a, deep)
	if err != nil {
		a.health.record(err, time.Since(start))
		return err
	}
	if deep {
		// From here on the upstream really answered; a stale cooldown must not
		// keep routing this account away while the badge reads ok.
		a.clearStaleBlocks()
	}
	path, ok := probePathFor(a.config.Backend)
	if !ok {
		err := &apiError{400, "backend has no health probe", 0}
		a.health.record(err, time.Since(start))
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.probeTimeout())
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, a.config.BaseURL+path, nil)
	if err != nil {
		a.health.record(err, time.Since(start))
		return err
	}
	// The resolved credential already carries its scheme ("Bearer ...");
	// Open WebUI session tokens arrive without one, and Open WebUI's own
	// Authorization parser expects the scheme, so normalise here.
	if !strings.HasPrefix(auth, "Bearer ") {
		auth = "Bearer " + auth
	}
	request.Header.Set("Authorization", auth)
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		a.health.record(err, time.Since(start))
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, healthMaxBodyBytes))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		a.health.record(nil, time.Since(start))
		return nil
	}
	probeErr := &apiError{status: response.StatusCode, message: probeErrorText(response.StatusCode)}
	a.health.record(probeErr, time.Since(start))
	return probeErr
}

// probePathFor picks a read-only endpoint per backend family.
func probePathFor(kind BackendKind) (string, bool) {
	if kind == BackendLibreChat {
		path, ok := libreChatUpstreamPath("/v1/models")
		return path, ok
	}
	return "/api/models", true
}

func probeErrorText(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "credentials rejected"
	case status == http.StatusTooManyRequests:
		return "rate limited"
	case status >= 500:
		return "upstream error"
	}
	return "probe rejected"
}

// probeLoginTimeout keeps signin inside the probe budget without inheriting the
// full upstream timeout: a host that needs 20s to answer a model list usually
// signs in fast, and a host that cannot sign in within a few seconds is not
// usable anyway.
func (p *Proxy) probeLoginTimeout() time.Duration {
	budget := p.probeTimeout()
	if budget <= 0 || budget > probeLoginTimeout {
		return probeLoginTimeout
	}
	return budget
}

// healthSnapshot is the JSON shape exposed by /v1/health and the admin API.
func (p *Proxy) healthReport() []map[string]any {
	report := make([]map[string]any, 0, len(p.accounts))
	for _, a := range p.accounts {
		status, checkedAt, errText, latency := a.health.snapshot()
		blocked := a.loginBlockSeconds()
		entry := map[string]any{
			"id":                  a.config.ID,
			"website_id":          a.config.WebsiteID,
			"backend":             string(a.config.Backend),
			"enabled":             !a.config.Disabled,
			"status":              string(status),
			"available":           !a.config.Disabled && accountCooldown(a) == 0 && blocked == 0 && status != healthFailed,
			"cooldown_seconds":    accountCooldown(a),
			"login_block_seconds": blocked,
			"latency_ms":          int(latency / time.Millisecond),
		}
		if !checkedAt.IsZero() {
			entry["checked_at"] = checkedAt.UTC().Format(time.RFC3339)
		}
		if errText != "" {
			entry["error"] = errText
		}
		report = append(report, entry)
	}
	return report
}

// runHealthProbes probes every enabled account once and returns the failures.
// The background loop keeps using cached blocks; an explicit operator check
// passes force to spend a real login round-trip on every account.
func (p *Proxy) runHealthProbes(ctx context.Context, force bool) int {
	var failures int
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, a := range p.accounts {
		if a.config.Disabled {
			continue
		}
		wg.Add(1)
		go func(a *account) {
			defer wg.Done()
			if err := p.probe(ctx, a, force); err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	return failures
}

// startHealthLoop probes immediately and then on an interval until ctx ends.
func (p *Proxy) startHealthLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultProbeInterval
	}
	go func() {
		p.runHealthProbes(ctx, false)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.runHealthProbes(ctx, false)
			}
		}
	}()
}
