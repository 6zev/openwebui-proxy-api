package main

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A streaming Responses call must come back as Responses SSE events, and a
// streaming Anthropic call must be wrapped in the Anthropic event envelope.
func TestTranslateMissingStreaming(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.onChat = func(_ string, _ int, w http.ResponseWriter, _ *http.Request, _ map[string]any) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return true
	}
	cfg := proxyTestConfig(u, "alpha")
	cfg.TranslateMissing = true
	s := newProxyTestServer(t, cfg)
	for _, tc := range []struct{ path, body, want string }{
		{"/v1/responses", `{"model":"m","input":"hi","stream":true}`, "response.output_text.delta"},
		{"/v1/messages", `{"model":"m","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "content_block_delta"},
	} {
		req, _ := http.NewRequest(http.MethodPost, s.URL+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+testProxyKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body strings.Builder
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			body.WriteString(scanner.Text())
			body.WriteString("\n")
		}
		resp.Body.Close()
		if !strings.Contains(body.String(), tc.want) {
			t.Errorf("%s stream missing %q:\n%s", tc.path, tc.want, body.String())
		}
		if !strings.Contains(body.String(), "hel") {
			t.Errorf("%s stream lost content:\n%s", tc.path, body.String())
		}
	}
}
