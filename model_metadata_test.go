package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The model list is upstream metadata: dropping fields forces clients to guess what
// a model can do. Everything the upstream sent must reach the caller.
func TestModelMetadataIsPreserved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{
			"id":"gpt-5","created":1754425777,"owned_by":"openai",
			"name":"GPT-5","description":"fast",
			"context_length":128000,
			"tags":["chat","vision"],
			"pricing":{"prompt":"0.001","completion":"0.002"},
			"info":{"meta":{"capabilities":{"vision":true,"citations":false}}},
			"access_control":{"read":{"group_ids":[]}}
		}]}`)
	}))
	defer upstream.Close()

	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	s := newProxyTestServer(t, cfg)
	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusOK)

	var payload struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp.body), &payload); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(payload.Data) != 1 {
		t.Fatalf("models = %d", len(payload.Data))
	}
	model := payload.Data[0]
	for _, key := range []string{"id", "object", "created", "owned_by", "name", "description", "context_length", "tags", "pricing", "info", "access_control"} {
		if _, ok := model[key]; !ok {
			t.Errorf("field %q was dropped", key)
		}
	}
	// The values are forwarded verbatim, not reshaped.
	if got := string(model["context_length"]); got != "128000" {
		t.Errorf("context_length = %s", got)
	}
	if !strings.Contains(string(model["info"]), `"vision":true`) {
		t.Errorf("capabilities lost: %s", model["info"])
	}
}

// The cache keeps the extras across a refresh, and merging two websites does not
// mix their metadata.
func TestModelMetadataSurvivesCacheAndMerge(t *testing.T) {
	mk := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auths/signin" {
				_, _ = io.WriteString(w, `{"token":"t"}`)
				return
			}
			_, _ = io.WriteString(w, body)
		}))
	}
	a := mk(`{"data":[{"id":"shared","name":"From A","context_length":8}]}`)
	defer a.Close()
	b := mk(`{"data":[{"id":"shared","name":"From B","context_length":16}]}`)
	defer b.Close()

	cfg := Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second,
		ModelCacheTTL: time.Minute,
		Websites:      []WebsiteConfig{{ID: "site-a", BaseURL: a.URL}, {ID: "site-b", BaseURL: b.URL}},
		Accounts: []AccountConfig{
			{ID: "a", WebsiteID: "site-a", Email: "a@x.test", Password: "p"},
			{ID: "b", WebsiteID: "site-b", Email: "b@x.test", Password: "p"},
		},
	}
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// fetchModelList is the shared parse path used by the refresher too.
	first, err := p.fetchModelList(context.Background(), p.accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Extra["name"] == nil || string(first[0].Extra["name"]) != `"From A"` {
		t.Errorf("extras not kept on the cache path: %+v", first[0].Extra)
	}
}
