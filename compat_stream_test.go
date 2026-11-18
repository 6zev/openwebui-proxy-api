package main

import (
	"bufio"
	"encoding/json"
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

// When the upstream reports no usage, a terminal event must not claim zero tokens:
// clients show "0 tokens" as if nothing was generated. A character-based estimate
// is reported instead, and real usage always wins.
func TestTerminalUsageNeverClaimsZeroWithoutData(t *testing.T) {
	state, opening := newStreamState(surfaceAnthropic, "m")
	for _, event := range opening {
		if strings.Contains(string(event), "message_start") {
			continue
		}
	}
	feed := func(payload string) {
		if _, _, err := state.convertChatStream([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	feed(`{"id":"c","choices":[{"delta":{"content":"a fairly long answer worth counting"}}]}`)
	feed(`{"id":"c","choices":[{"delta":{},"finish_reason":"stop"}]}`)

	events := state.finalEvents("stop")
	var usage map[string]any
	for _, event := range events {
		// An SSE frame is "event: <name>\ndata: <json>\n\n"; parse the data line.
		for _, line := range strings.Split(string(event), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var decoded struct {
				Type  string         `json:"type"`
				Usage map[string]any `json:"usage"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &decoded); err != nil {
				continue
			}
			if decoded.Type == "message_delta" || decoded.Usage != nil {
				usage = decoded.Usage
			}
		}
	}
	if usage == nil {
		t.Fatal("no usage in message_delta")
	}
	if completion, _ := usage["output_tokens"].(float64); completion <= 0 {
		t.Errorf("output_tokens = %v, want a positive estimate", usage["output_tokens"])
	}

	// Real numbers from the upstream take precedence over the estimate.
	state2, _ := newStreamState(surfaceAnthropic, "m")
	if _, _, err := state2.convertChatStream([]byte(`{"id":"c","choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)); err != nil {
		t.Fatal(err)
	}
	prompt, completion := state2.usageCounts()
	if prompt != 11 || completion != 3 {
		t.Errorf("usage = %d/%d, want 11/3 from the upstream", prompt, completion)
	}
}
