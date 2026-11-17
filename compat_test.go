package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Anthropic Messages requests must become chat completions with system prompts,
// tool_use/tool_result pairs preserved and unsupported fields reported.
func TestChatFromAnthropic(t *testing.T) {
	body := `{
		"model":"claude-sonnet-5-5","max_tokens":1024,"stream":true,
		"system":[{"type":"text","text":"be brief"}],
		"thinking":{"type":"enabled","budget_tokens":1024},
		"metadata":{"user_id":"x"},
		"tools":[{"name":"lookup","description":"find","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"found"}]}]}
		]
	}`
	out, dropped, err := chatFromAnthropic([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(dropped, ",") != "metadata,thinking" {
		t.Errorf("dropped = %v", dropped)
	}
	var chat struct {
		Model    string           `json:"model"`
		Stream   bool             `json:"stream"`
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("converted body is invalid JSON: %v\n%s", err, out)
	}
	if chat.Model != "claude-sonnet-5-5" || !chat.Stream || len(chat.Messages) != 4 || len(chat.Tools) != 1 {
		t.Fatalf("unexpected conversion: %s", out)
	}
	if chat.Messages[0]["role"] != "system" || chat.Messages[0]["content"] != "be brief" {
		t.Errorf("system prompt lost: %v", chat.Messages[0])
	}
	if chat.Messages[1]["role"] != "user" {
		t.Errorf("user message lost: %v", chat.Messages[1])
	}
	// text + tool_use merge into a single assistant message, then the tool result.
	assistant := chat.Messages[2]
	if assistant["role"] != "assistant" || assistant["tool_calls"] == nil {
		t.Errorf("assistant tool call lost: %v", assistant)
	}
	if chat.Messages[3]["role"] != "tool" || chat.Messages[3]["tool_call_id"] != "tu_1" || chat.Messages[3]["content"] != "found" {
		t.Errorf("tool result lost: %v", chat.Messages[3])
	}
}

// A chat completion must come back in Anthropic shape, tool calls included.
func TestAnthropicFromChat(t *testing.T) {
	body := `{"id":"c1","choices":[{"message":{"content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	out, err := anthropicFromChat([]byte(body), "claude-sonnet-5-5")
	if err != nil {
		t.Fatal(err)
	}
	var message struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &message); err != nil {
		t.Fatalf("invalid output %s", out)
	}
	if message.Type != "message" || message.Role != "assistant" || len(message.Content) != 2 {
		t.Fatalf("unexpected shape: %s", out)
	}
	if message.Content[0].Text != "hello" || message.Content[1].Name != "lookup" || message.StopReason != "tool_use" {
		t.Errorf("content/stop reason wrong: %s", out)
	}
	if message.Usage.InputTokens != 7 || message.Usage.OutputTokens != 3 {
		t.Errorf("usage wrong: %s", out)
	}
}

// Responses input items map onto chat messages, including function call replay.
func TestChatFromResponses(t *testing.T) {
	body := `{"model":"gpt","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"call_9","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_9","output":"done"}
	],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}],
	"reasoning":{"effort":"low"},"store":false}`
	out, dropped, err := chatFromResponses([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(dropped, ",") != "reasoning,store" {
		t.Errorf("dropped = %v", dropped)
	}
	var chat struct {
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("invalid JSON %s", out)
	}
	if len(chat.Messages) != 3 || chat.Messages[0]["role"] != "user" {
		t.Fatalf("messages wrong: %s", out)
	}
	if chat.Messages[1]["role"] != "assistant" || len(chat.Tools) != 1 {
		t.Errorf("function call or tool lost: %s", out)
	}
	if chat.Messages[2]["role"] != "tool" || chat.Messages[2]["tool_call_id"] != "call_9" {
		t.Errorf("function output lost: %s", out)
	}
}

// Hosted Responses tools cannot run inside a chat completion, so the call must
// fail loudly instead of silently dropping the tool.
func TestResponsesHostedToolRejected(t *testing.T) {
	body := `{"model":"gpt","input":"hi","tools":[{"type":"web_search"}]}`
	if _, _, err := chatFromResponses([]byte(body)); err == nil {
		t.Fatal("hosted tool was accepted")
	}
}

// With TRANSLATE_MISSING on, /v1/responses and /v1/messages are served over chat
// completions and the response comes back in the requested shape.
func TestTranslateMissingEndToEnd(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.onChat = func(_ string, _ int, w http.ResponseWriter, _ *http.Request, body map[string]any) bool {
		if body["model"] != "m" {
			t.Errorf("upstream model = %v", body["model"])
		}
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
		return true
	}
	cfg := proxyTestConfig(u, "alpha")
	cfg.TranslateMissing = true
	s := newProxyTestServer(t, cfg)

	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/responses", `{"model":"m","input":"hi","reasoning":{"effort":"low"}}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if resp.header.Get("X-Compat-Mode") == "" {
		t.Errorf("missing X-Compat-Mode header")
	}
	if got := resp.header.Get("X-Compat-Dropped"); got != "reasoning" {
		t.Errorf("X-Compat-Dropped = %q", got)
	}
	if !strings.Contains(resp.body, `"output_text"`) {
		t.Errorf("responses shape missing: %s", resp.body)
	}

	resp = proxyTestRequest(t, s, http.MethodPost, "/v1/messages", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if !strings.Contains(resp.body, `"type":"message"`) || !strings.Contains(resp.body, `"text":"ok"`) {
		t.Errorf("anthropic shape missing: %s", resp.body)
	}
}

// Without the flag the native passthrough stays untouched.
func TestTranslateMissingDisabledByDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		if r.URL.Path != "/openai/responses" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"r1"}`)
	}))
	defer upstream.Close()
	cfg := proxyTestConfig(&proxyTestUpstream{server: upstream}, "alpha")
	s := newProxyTestServer(t, cfg)
	resp := proxyTestRequest(t, s, http.MethodPost, "/openai/responses", `{"model":"m","input":"hi"}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if resp.header.Get("X-Compat-Mode") != "" {
		t.Errorf("translation header present while disabled")
	}
}
