package main

import (
	"strings"
	"testing"
)

// Two streamed tool calls must open two distinct content blocks with their own
// indexes, ids and argument deltas — and the text block must close first.
func TestAnthropicStreamToolBlocksByIndex(t *testing.T) {
	state, opening := newStreamState(surfaceAnthropic, "claude")
	var events []string
	for _, event := range opening {
		events = append(events, string(event))
	}
	feed := func(payload string) {
		out, _, err := state.convertChatStream([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range out {
			events = append(events, string(event))
		}
	}
	feed(`{"id":"c1","choices":[{"delta":{"content":"thinking"}}]}`)
	feed(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`)
	feed(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`)
	feed(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"save","arguments":"{}"}}]}}]}`)
	feed(`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`)
	all := strings.Join(events, "")

	// Text block 0 opened and closed before the first tool block.
	if !strings.Contains(all, `"content_block":{"text":"","type":"text"},"index":0`) {
		t.Errorf("text block start missing:\n%s", all)
	}
	// Two tool_use blocks on indexes 1 and 2.
	if !strings.Contains(all, `"content_block":{"id":"call_a","input":{},"name":"lookup","type":"tool_use"},"index":1`) {
		t.Errorf("tool block 1 missing:\n%s", all)
	}
	if !strings.Contains(all, `"content_block":{"id":"call_b","input":{},"name":"save","type":"tool_use"},"index":2`) {
		t.Errorf("tool block 2 missing:\n%s", all)
	}
	// Arguments streamed as partial JSON on the right block.
	if !strings.Contains(all, `"delta":{"partial_json":"{\"q\":","type":"input_json_delta"},"index":1`) {
		t.Errorf("first arguments delta missing:\n%s", all)
	}
	if !strings.Contains(all, `"partial_json":"\"x\"}"`) {
		t.Errorf("second arguments delta missing:\n%s", all)
	}
	// Every opened block is closed exactly once, and usage is the real number.
	for _, index := range []string{"0", "1", "2"} {
		if strings.Count(all, `"index":`+index+`,"type":"content_block_stop"`) == 0 {
			t.Errorf("block %s never closed:\n%s", index, all)
		}
	}
	if !strings.Contains(all, `"input_tokens":11`) || !strings.Contains(all, `"output_tokens":4`) {
		t.Errorf("usage not forwarded:\n%s", all)
	}
	if !strings.Contains(all, `"stop_reason":"tool_use"`) {
		t.Errorf("stop reason missing:\n%s", all)
	}
	if !strings.Contains(all, "message_stop") {
		t.Errorf("message_stop missing:\n%s", all)
	}
}

// A Responses stream must report aggregated usage on response.completed.
func TestResponsesStreamReportsUsage(t *testing.T) {
	state, _ := newStreamState(surfaceResponses, "gpt")
	var events []string
	feed := func(payload string) {
		out, _, err := state.convertChatStream([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range out {
			events = append(events, string(event))
		}
	}
	feed(`{"id":"r1","created":1700000000,"choices":[{"delta":{"content":"hi"}}]}`)
	feed(`{"id":"r1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
	all := strings.Join(events, "")
	if !strings.Contains(all, `"input_tokens":9`) || !strings.Contains(all, `"output_tokens":2`) || !strings.Contains(all, `"total_tokens":11`) {
		t.Errorf("usage not forwarded:\n%s", all)
	}
	if !strings.Contains(all, `"status":"completed"`) || !strings.Contains(all, "response.completed") {
		t.Errorf("terminal event missing:\n%s", all)
	}
}

// Zero usage must still be reported as numbers, not omitted, so a client can
// distinguish "no tokens" from "no information".
func TestStreamUsageDefaultsToZero(t *testing.T) {
	state, _ := newStreamState(surfaceAnthropic, "m")
	out, _, err := state.convertChatStream([]byte(`{"id":"c","choices":[{"delta":{},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, event := range out {
		all += string(event)
	}
	if !strings.Contains(all, `"input_tokens":0`) || !strings.Contains(all, `"output_tokens":0`) {
		t.Errorf("usage absent:\n%s", all)
	}
}
