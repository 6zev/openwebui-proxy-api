package main

import (
	"context"
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

// probe checks one account without touching the shared request path.
func (p *Proxy) probe(ctx context.Context, a *account) error {
	start := time.Now()
	auth, err := backendAuthHeader(ctx, p, a)
	if err != nil {
		a.health.record(err, time.Since(start))
		return err
	}
	path, ok := probePathFor(a.config.Backend)
	if !ok {
		err := &apiError{400, "backend has no health probe", 0}
		a.health.record(err, time.Since(start))
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, minDuration(p.config.Timeout, 20*time.Second))
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, a.config.BaseURL+path, nil)
	if err != nil {
		a.health.record(err, time.Since(start))
		return err
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

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 || a > b {
		return b
	}
	return a
}

// healthSnapshot is the JSON shape exposed by /v1/health and the admin API.
func (p *Proxy) healthReport() []map[string]any {
	report := make([]map[string]any, 0, len(p.accounts))
	for _, a := range p.accounts {
		status, checkedAt, errText, latency := a.health.snapshot()
		entry := map[string]any{
			"id":               a.config.ID,
			"website_id":       a.config.WebsiteID,
			"backend":          string(a.config.Backend),
			"enabled":          !a.config.Disabled,
			"status":           string(status),
			"available":        !a.config.Disabled && accountCooldown(a) == 0 && status != healthFailed,
			"cooldown_seconds": accountCooldown(a),
			"latency_ms":       int(latency / time.Millisecond),
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
func (p *Proxy) runHealthProbes(ctx context.Context) int {
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
			if err := p.probe(ctx, a); err != nil {
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
		p.runHealthProbes(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.runHealthProbes(ctx)
			}
		}
	}()
}
