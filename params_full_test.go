package main

import (
	"testing"
)

// Every control LibreChat exposes in its Model Parameters panel that has a meaning
// for an OpenAI-shaped upstream must survive the translation. Anything the proxy
// cannot honour must be visibly absent, never silently replaced by a default.
func TestAllModelParametersForwarded(t *testing.T) {
	_, _, _, params, err := chatPromptFromBodyWithParams([]byte(`{
		"model":"m",
		"temperature":0.7,"top_p":0.8,"top_k":40,"max_tokens":100,
		"frequency_penalty":0.1,"presence_penalty":0.2,"seed":9,
		"stop":["END"],
		"reasoning_effort":"high","reasoning_summary":"auto",
		"reasoning_mode":"pro","reasoning_context":"full",
		"verbosity":"medium","effort":"medium",
		"max_context_tokens":32000,"prompt_prefix":"be terse",
		"user":"u-1","web_search":true,"disable_streaming":false,
		"metadata":{"trace":"abc"},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"temperature": 0.7, "top_p": 0.8, "top_k": 40, "max_tokens": 100,
		"frequency_penalty": 0.1, "presence_penalty": 0.2, "seed": 9,
		"reasoning_effort": "high", "reasoning_summary": "auto",
		"reasoning_mode": "pro", "reasoning_context": "full",
		"verbosity": "medium", "effort": "medium",
		"maxContextTokens": 32000, "promptPrefix": "be terse", "user": "u-1",
		"web_search": true, "disableStreaming": false,
	}
	for key, value := range want {
		if params[key] != value {
			t.Errorf("%s = %v, want %v", key, params[key], value)
		}
	}
	if _, ok := params["metadata"]; !ok {
		t.Error("metadata dropped")
	}
}

// The UI writes literals when a control was never touched; those must not be sent as
// if the operator had chosen them.
func TestUnsetChoicesAreNotForwarded(t *testing.T) {
	_, _, _, params, err := chatPromptFromBodyWithParams([]byte(`{
		"model":"m",
		"reasoning_effort":"unset","reasoning_summary":"unset",
		"reasoning_mode":"unset","verbosity":"","effort":"",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reasoning_effort", "reasoning_summary", "reasoning_mode", "verbosity", "effort"} {
		if value, ok := params[key]; ok {
			t.Errorf("%s should be omitted when unset, got %v", key, value)
		}
	}
}

// "auto" is a real choice (let the provider decide), not "untouched".
func TestAutoIsARealChoice(t *testing.T) {
	_, _, _, params, err := chatPromptFromBodyWithParams([]byte(`{
		"model":"m","reasoning_effort":"auto","reasoning_summary":"auto",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if params["reasoning_effort"] != "auto" {
		t.Errorf("reasoning_effort auto dropped: %v", params["reasoning_effort"])
	}
	if params["reasoning_summary"] != "auto" {
		t.Errorf("reasoning_summary auto dropped: %v", params["reasoning_summary"])
	}
}
