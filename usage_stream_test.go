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

// A streaming chat call must ask for usage, otherwise the client shows no token
// count and no tokens-per-second (OpenMinis and Kelivo both display them).
func TestStreamChatRequestsUsage(t *testing.T) {
	var sawInclude atomic.Bool
	var otherKeys atomic.Value
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
				sawInclude.Store(true)
			}
		}
		otherKeys.Store(len(body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	cfg.Timeout = 5 * time.Second
	s := newProxyTestServer(t, cfg)

	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`, testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusOK)
	if !sawInclude.Load() {
		t.Error("proxy did not add stream_options.include_usage")
	}
	if !strings.Contains(resp.body, `"usage"`) {
		t.Errorf("usage missing from stream: %s", resp.body)
	}
}

// The helper must not touch non-streaming calls, nor override a caller's value.
func TestEnsureStreamUsageRespectsCaller(t *testing.T) {
	cases := []struct {
		name, in    string
		wantInclude *bool
		wantSame    bool
	}{
		{name: "non-stream untouched", in: `{"model":"m"}`, wantSame: true},
		{name: "stream false untouched", in: `{"stream":false}`, wantSame: true},
		{name: "stream true gets flag", in: `{"stream":true}`, wantInclude: boolPtr(true)},
		{name: "existing false kept", in: `{"stream":true,"stream_options":{"include_usage":false}}`, wantInclude: boolPtr(false)},
		{name: "not json untouched", in: `not json`, wantSame: true},
	}
	for _, test := range cases {
		got := ensureStreamUsage([]byte(test.in))
		if test.wantSame {
			if string(got) != test.in {
				t.Errorf("%s: body changed to %s", test.name, got)
			}
			continue
		}
		var body struct {
			StreamOptions struct {
				IncludeUsage *bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.Unmarshal(got, &body); err != nil {
			t.Errorf("%s: invalid JSON %s", test.name, got)
			continue
		}
		if body.StreamOptions.IncludeUsage == nil || *body.StreamOptions.IncludeUsage != *test.wantInclude {
			t.Errorf("%s: include_usage = %v, want %v", test.name, body.StreamOptions.IncludeUsage, *test.wantInclude)
		}
	}
}

func boolPtr(value bool) *bool { return &value }
