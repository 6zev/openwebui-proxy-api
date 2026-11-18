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

// All four surfaces must end up with real usage. Chat completions gets the flag on
// its outbound copy; the translated Responses and Anthropic paths build their own
// internal chat request, and it must carry the flag too — otherwise their terminal
// events report an estimate instead of the upstream's counts.
func TestTranslatedSurfacesRequestUsage(t *testing.T) {
	var sawInclude atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if options, ok := body["stream_options"].(map[string]any); ok {
			if include, ok := options["include_usage"].(bool); ok && include {
				sawInclude.Add(1)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	cfg.TranslateMissing = true
	s := newProxyTestServer(t, cfg)

	cases := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{"responses", "/v1/responses", `{"model":"m","input":"hi","stream":true}`},
		{"anthropic", "/v1/messages", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`},
	}
	for _, test := range cases {
		before := sawInclude.Load()
		resp := proxyTestRequest(t, s, http.MethodPost, test.path, test.body, testProxyKey, "")
		requireProxyStatus(t, resp, http.StatusOK)
		if sawInclude.Load() == before {
			t.Errorf("%s: upstream was not asked for usage", test.name)
		}
		if !strings.Contains(resp.body, "13") && !strings.Contains(resp.body, `"prompt_tokens":9`) && !strings.Contains(resp.body, `"input_tokens":9`) {
			t.Errorf("%s: usage not surfaced to the client: %s", test.name, resp.body)
		}
	}
}

// The passthrough guarantee still holds: a pinned LibreChat-style payload that is
// forwarded byte-for-byte must not gain stream_options.
func TestPassthroughResponsesPayloadUntouched(t *testing.T) {
	var got atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		got.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","output":[]}`)
	}))
	defer upstream.Close()
	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	// Translation off: /v1/responses is forwarded as-is.
	s := newProxyTestServer(t, cfg)
	payload := `{"model":"m","input":"hi","stream":true}`
	resp := proxyTestRequest(t, s, http.MethodPost, "/openai/responses", payload, testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusOK)
	if stored, _ := got.Load().(string); stored != payload {
		t.Errorf("passthrough rewrote the payload: %s", stored)
	}
}
