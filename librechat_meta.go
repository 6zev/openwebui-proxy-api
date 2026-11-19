package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LibreChat's /api/models answers only ids. Everything a client would want to show
// beside a model — the endpoint's display label, its capabilities, whether it is a
// custom provider — lives in /api/endpoints instead. This file merges the two so a
// session deployment exposes the same richness as an OpenAI-shaped upstream.

// libreChatEndpointInfo is the part of an endpoint entry worth republishing.
type libreChatEndpointInfo struct {
	ModelDisplayLabel string   `json:"modelDisplayLabel"`
	Type              string   `json:"type"`
	Order             int      `json:"order"`
	Capabilities      []string `json:"capabilities"`
}

// libreChatEndpointCache memoises the endpoint map per account. The data changes
// only when an admin edits the deployment, so a long TTL with no refresh thread is
// the right shape: one extra request per account per hour.
type libreChatEndpointCache struct {
	mu      sync.Mutex
	entries map[string]libreChatEndpointEntry
}

type libreChatEndpointEntry struct {
	info      map[string]libreChatEndpointInfo
	fetchedAt time.Time
}

const libreChatEndpointTTL = time.Hour

func (c *libreChatEndpointCache) get(key string) (map[string]libreChatEndpointInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Since(entry.fetchedAt) > libreChatEndpointTTL {
		return nil, false
	}
	return entry.info, true
}

func (c *libreChatEndpointCache) put(key string, info map[string]libreChatEndpointInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]libreChatEndpointEntry{}
	}
	c.entries[key] = libreChatEndpointEntry{info: info, fetchedAt: time.Now()}
}

// libreChatEndpoints returns the endpoint map for an account, fetching it once and
// caching it. A failure is not fatal: the caller keeps the plain model list.
func (p *Proxy) libreChatEndpoints(ctx context.Context, a *account) map[string]libreChatEndpointInfo {
	if p.endpointInfo == nil {
		return nil
	}
	key := accountKey(a.config.WebsiteID, a.config.ID)
	if info, ok := p.endpointInfo.get(key); ok {
		return info
	}
	token, err := p.libreChatToken(ctx, a)
	if err != nil {
		return nil
	}
	info, err := p.fetchLibreChatEndpoints(ctx, a, token)
	if err != nil {
		p.debugf("debug: librechat endpoints unavailable account=%s err=%v", a.config.ID, err)
		return nil
	}
	p.endpointInfo.put(key, info)
	return info
}

// fetchLibreChatEndpoints reads and trims /api/endpoints.
func (p *Proxy) fetchLibreChatEndpoints(ctx context.Context, a *account, token string) (map[string]libreChatEndpointInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(a.config.BaseURL, "/")+libreChatEndpointsPath(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	p.decorateLibreChatRequest(request)
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &apiError{status: response.StatusCode, message: "LibreChat endpoints request failed"}
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	var document map[string]libreChatEndpointInfo
	if json.Unmarshal(raw, &document) != nil {
		return nil, &apiError{status: 502, message: "Unexpected endpoints response"}
	}
	return document, nil
}

// applyEndpointMetadata decorates the models of one account with the endpoint they
// belong to. The provider key IS the endpoint name, so the lookup is exact; models
// whose provider is unknown are left untouched rather than guessed at.
func applyEndpointMetadata(models []cachedModel, info map[string]libreChatEndpointInfo) {
	if len(info) == 0 {
		return
	}
	// Built-in endpoints (openAI, anthropic, google, bedrock, assistants...) are not
	// listed by /api/endpoints — that map only carries CUSTOM endpoints — but they
	// are still real, well-known providers. A short table fills in their label so the
	// listing is not half-annotated.
	builtin := map[string]libreChatEndpointInfo{
		"openAI":          {ModelDisplayLabel: "OpenAI"},
		"anthropic":       {ModelDisplayLabel: "Anthropic"},
		"google":          {ModelDisplayLabel: "Google"},
		"bedrock":         {ModelDisplayLabel: "AWS Bedrock"},
		"assistants":      {ModelDisplayLabel: "OpenAI Assistants"},
		"azureAssistants": {ModelDisplayLabel: "Azure Assistants"},
		"azureOpenAI":     {ModelDisplayLabel: "Azure OpenAI"},
		"openRouter":      {ModelDisplayLabel: "OpenRouter"},
		"deepseek":        {ModelDisplayLabel: "DeepSeek"},
		"xai":             {ModelDisplayLabel: "xAI"},
	}
	for i := range models {
		// Custom endpoints come from /api/endpoints, built-in ones from the table.
		// A custom endpoint that leaves modelDisplayLabel unset still deserves the
		// type/order it does carry, so the two sources are merged rather than
		// treated as alternatives.
		custom, hasCustom := info[models[i].OwnedBy]
		fallback, hasBuiltin := builtin[models[i].OwnedBy]
		if !hasCustom && !hasBuiltin {
			continue
		}
		entry := fallback
		if hasCustom {
			if custom.ModelDisplayLabel != "" {
				entry.ModelDisplayLabel = custom.ModelDisplayLabel
			}
			if custom.Type != "" {
				entry.Type = custom.Type
			}
			if len(custom.Capabilities) > 0 {
				entry.Capabilities = custom.Capabilities
			}
			if custom.Order != 0 {
				entry.Order = custom.Order
			}
		}
		extra := models[i].Extra
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		if entry.ModelDisplayLabel != "" {
			extra["x_endpoint_label"] = mustJSON(entry.ModelDisplayLabel)
		}
		if entry.Type != "" {
			extra["x_endpoint_type"] = mustJSON(entry.Type)
		}
		if len(entry.Capabilities) > 0 {
			extra["x_endpoint_capabilities"] = mustJSON(entry.Capabilities)
		}
		// A custom endpoint with no label still has a name: the provider key IS what
		// LibreChat shows when nothing else is configured.
		if _, ok := extra["x_endpoint_label"]; !ok && models[i].OwnedBy != "" {
			extra["x_endpoint_label"] = mustJSON(displayName(models[i].OwnedBy, ""))
		}
		models[i].Extra = extra
	}
}

// mustJSON encodes a value that is known to be encodable.
func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}
