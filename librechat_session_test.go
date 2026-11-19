package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// libreChatFixture stands in for a LibreChat host: login, the chat turn POST that
// answers a streamId, and the SSE subscription that delivers the answer.
type libreChatFixture struct {
	server    *httptest.Server
	logins    atomic.Int32
	turns     atomic.Int32
	streams   atomic.Int32
	lastStart atomic.Value
	uaSeen    atomic.Value
	// finalText is what the SSE payload reports as the persisted answer.
	finalText string
	// loginStatus forces a login response code (403 = banned).
	loginStatus int
}

func newLibreChatFixture(t *testing.T, finalText string) *libreChatFixture {
	t.Helper()
	f := &libreChatFixture{finalText: finalText}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "" {
			f.uaSeen.Store(ua)
		}
		switch {
		case r.URL.Path == "/api/auth/login":
			f.logins.Add(1)
			if f.loginStatus != 0 {
				w.WriteHeader(f.loginStatus)
				_, _ = io.WriteString(w, `{"message":"Your account has been temporarily banned due to violations of our service."}`)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "refreshToken", Value: "r1", Path: "/"})
			_, _ = io.WriteString(w, `{"token":"jwt-1","user":{"id":"u1"}}`)
		case strings.HasPrefix(r.URL.Path, "/api/agents/chat/") && !strings.Contains(r.URL.Path, "/stream/"):
			f.turns.Add(1)
			raw, _ := io.ReadAll(r.Body)
			f.lastStart.Store(string(raw))
			if r.Header.Get("Authorization") != "Bearer jwt-1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
				return
			}
			_, _ = io.WriteString(w, `{"streamId":"stream-1"}`)
		case strings.HasPrefix(r.URL.Path, "/api/agents/chat/stream/"):
			f.streams.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			// Real shape: the SSE event name is always "message" and the meaning sits
			// in the payload's own "event" field.
			_, _ = io.WriteString(w, `event: message`+"\n"+`data: {"created":true,"message":{"text":"hi"},"streamId":"s1"}`+"\n\n")
			_, _ = io.WriteString(w, `event: message`+"\n"+`data: {"event":"on_message_delta","data":{"delta":{"content":[{"type":"text","text":"Hel"}]}}}`+"\n\n")
			_, _ = io.WriteString(w, `event: message`+"\n"+`data: {"event":"on_message_delta","data":{"delta":{"content":[{"type":"text","text":"lo"}]}}}`+"\n\n")
			_, _ = io.WriteString(w, `event: message`+"\n"+`data: {"event":"on_token_usage","data":{"input_tokens":7,"output_tokens":3}}`+"\n\n")
			payload, _ := json.Marshal(map[string]any{
				"final": true,
				"responseMessage": map[string]any{
					"content": map[string]any{"type": "text", "text": f.finalText},
				},
			})
			_, _ = io.WriteString(w, "event: message\ndata: "+string(payload)+"\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func libreChatSessionConfig(f *libreChatFixture) Config {
	return Config{
		APIKey: testProxyKey, Timeout: 5 * time.Second, Cooldown: time.Second,
		Websites: []WebsiteConfig{{ID: "site-lc", BaseURL: f.server.URL}},
		Accounts: []AccountConfig{{ID: "lc", WebsiteID: "site-lc", Backend: BackendLibreChat, Email: "a@b.test", Password: "pw"}},
	}
}

// A session account runs a turn through the web UI protocol: login, start the
// generation, then follow its SSE stream. The persisted final answer wins over the
// streamed fragments.
func TestLibreChatSessionChat(t *testing.T) {
	f := newLibreChatFixture(t, "Hello from the final snapshot")
	s := newProxyTestServer(t, libreChatSessionConfig(f))

	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusOK)
	requireProxyStatus(t, resp, http.StatusOK)
	if !strings.Contains(resp.body, "Hello from the final snapshot") {
		t.Errorf("final answer missing: %s", resp.body)
	}
	// Token usage from on_token_usage reaches the caller.
	if !strings.Contains(resp.body, `"total_tokens":10`) {
		t.Errorf("usage missing: %s", resp.body)
	}
	if f.logins.Load() != 1 || f.turns.Load() != 1 || f.streams.Load() != 1 {
		t.Errorf("calls: logins=%d turns=%d streams=%d", f.logins.Load(), f.turns.Load(), f.streams.Load())
	}
	// The turn body must use LibreChat's field names, not OpenAI's.
	start, _ := f.lastStart.Load().(string)
	// Shape captured from the real UI: no agent_id, endpoint-scoped route, and the
	// provider name in "endpoint".
	for _, want := range []string{`"text":"hi"`, `"endpointType":"custom"`, `"isCreatedByUser":true`, `"key":"never"`} {
		if !strings.Contains(start, want) {
			t.Errorf("turn body missing %s: %s", want, start)
		}
	}
	// A browser User-Agent is mandatory: without it LibreChat records a violation.
	if ua, _ := f.uaSeen.Load().(string); !strings.Contains(ua, "Mozilla/5.0") {
		t.Errorf("no browser User-Agent was sent: %q", ua)
	}
}

// The JWT is reused: a second turn must not log in again.
func TestLibreChatSessionReusesToken(t *testing.T) {
	f := newLibreChatFixture(t, "ok")
	s := newProxyTestServer(t, libreChatSessionConfig(f))
	for i := 0; i < 3; i++ {
		resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions",
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
		requireProxyStatus(t, resp, http.StatusOK)
	}
	if f.logins.Load() != 1 {
		t.Errorf("logins = %d, want 1 (token must be cached)", f.logins.Load())
	}
}

// A banned login must be recorded and never retried: every retry is another
// violation against the host's per-IP counter.
func TestLibreChatBannedLoginIsNotRetried(t *testing.T) {
	f := newLibreChatFixture(t, "unused")
	f.loginStatus = http.StatusForbidden
	cfg := libreChatSessionConfig(f)
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.libreChatToken(context.Background(), p.accounts[0])
	if err == nil || first != "" {
		t.Fatalf("banned login should fail: token=%q err=%v", first, err)
	}
	if !strings.Contains(err.Error(), "banned") {
		t.Errorf("error should carry the host's reason: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, secondErr := p.libreChatToken(context.Background(), p.accounts[0]); secondErr == nil {
			t.Fatal("a paused account still tried to authenticate")
		}
	}
	if got := f.logins.Load(); got != 1 {
		t.Errorf("login attempts = %d, want exactly 1", got)
	}
}

// Multi-turn history is flattened into a readable transcript; a single user turn is
// sent verbatim.
func TestSessionPromptFlattening(t *testing.T) {
	model, prompt, stream, err := chatPromptFromBody([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || model != "m" || prompt != "hi" || !stream {
		t.Fatalf("single turn: model=%q prompt=%q stream=%v err=%v", model, prompt, stream, err)
	}
	_, prompt, _, err = chatPromptFromBody([]byte(`{"model":"m","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"System: be terse", "Assistant: b", "User: a", "User: c"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("transcript missing %q: %s", want, prompt)
		}
	}
	if _, _, _, err := chatPromptFromBody([]byte(`{"model":"m","messages":[]}`)); err == nil {
		t.Error("empty messages accepted")
	}
}

// The user's own message echo arrives first and must never be counted as the
// answer; deltas accumulate in order and the persisted snapshot wins.
func TestLibreChatStreamParsing(t *testing.T) {
	stream := strings.Join([]string{
		`event: message` + "\n" + `data: {"created":true,"message":{"text":"Reply with exactly: PONG"},"streamId":"x"}`,
		`event: message` + "\n" + `data: {"event":"on_message_delta","data":{"delta":{"content":[{"type":"text","text":"P"}]}}}`,
		`event: message` + "\n" + `data: {"event":"on_message_delta","data":{"delta":{"content":[{"type":"text","text":"ONG"}]}}}`,
		`event: message` + "\n" + `data: {"event":"on_token_usage","data":{"input_tokens":10,"output_tokens":2}}`,
		`event: message` + "\n" + `data: {"final":true,"conversation":{"conversationId":"c"}}`,
	}, "\n\n") + "\n\n"

	turn, err := parseLibreChatStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "PONG" {
		t.Errorf("text = %q, want PONG (the echo must be ignored)", turn.Text)
	}
	if turn.PromptTokens != 10 || turn.CompletionTokens != 2 {
		t.Errorf("usage = %d/%d", turn.PromptTokens, turn.CompletionTokens)
	}
}

// A provider-scoped route is used, and the endpoint name follows the model family.
func TestLibreChatEndpointName(t *testing.T) {
	cases := map[string]string{
		"deepseek-chat":         "Deepseek",
		"gpt-6.1-sol":           "openAI",
		"claude-opus-5":         "anthropic",
		"gemini-3.8-flash":      "google",
		"something-unknown-xyz": "openAI",
	}
	for model, want := range cases {
		if got := libreChatEndpointName(model, nil); got != want {
			t.Errorf("libreChatEndpointName(%q) = %q, want %q", model, got, want)
		}
	}
}

// The model list is authoritative for the endpoint name: a provider the id
// heuristics do not know (Databricks, bedrock, assistants) must still route to its
// real endpoint instead of failing with endpoint_models_not_loaded.
func TestLibreChatEndpointFromModelList(t *testing.T) {
	known := map[string]string{
		"databricks-gpt-5-4-nano": "Databricks",
		"anthropic.claude-opus-4": "bedrock",
		"gpt-5.4":                 "assistants",
		"deepseek-chat":           "Deepseek",
	}
	cases := map[string]string{
		"databricks-gpt-5-4-nano": "Databricks",
		"anthropic.claude-opus-4": "bedrock",
		"gpt-5.4":                 "assistants",
		"deepseek-chat":           "Deepseek",
		"never-listed-model":      "openAI", // fallback only
	}
	for model, want := range cases {
		if got := libreChatEndpointName(model, known); got != want {
			t.Errorf("libreChatEndpointName(%q) = %q, want %q", model, got, want)
		}
	}
}
