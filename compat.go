package main

// Compatibility layer for upstreams that only implement part of the API.
//
// The design mirrors what magpie and new-api do: translate at the edge, keep the
// proxy itself a router. Nothing here invents data. A translation is only used
// when it is lossless for the request, and an approximation is marked so callers
// can tell. The goal is narrow: a client speaking an OpenAI/anthropic shape must
// still reach an Open WebUI instance that only serves chat completions.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// surface decides which translation an upstream needs.
type surfaceKind string

const (
	// surfaceNative means the upstream already speaks the requested shape.
	surfaceNative surfaceKind = "native"
	// surfaceResponses means the request is an OpenAI Responses call that has to
	// be carried over chat completions.
	surfaceResponses surfaceKind = "chat-from-responses"
	// surfaceAnthropic means the request is an Anthropic Messages call that has
	// to be carried over chat completions.
	surfaceAnthropic surfaceKind = "chat-from-anthropic"
)

// unsupportedFields lists request fields a translation cannot carry. Returning
// them instead of silently dropping them is the whole point: the caller learns
// exactly what it loses.
func unsupportedFields(keys []string, drop ...string) []string {
	blocked := make(map[string]bool, len(drop))
	for _, name := range drop {
		blocked[name] = true
	}
	out := make([]string, 0, 4)
	for _, key := range keys {
		if blocked[key] {
			out = append(out, key)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Anthropic Messages -> OpenAI chat completions
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    interface{}    `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// anthropicDroppedFields are request fields this translation cannot represent.
var anthropicDroppedFields = []string{"thinking", "context_management", "output_config", "metadata", "service_tier", "mcp_servers", "container", "betas"}

// chatFromAnthropic converts an Anthropic Messages request into an OpenAI chat
// completions request. It reports the fields it had to drop, so the caller can
// surface them instead of pretending the call was faithful.
func chatFromAnthropic(body []byte) ([]byte, []string, error) {
	var in anthropicRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	dropped := unsupportedFields(keys, anthropicDroppedFields...)

	out := map[string]any{"model": in.Model}
	if in.MaxTokens > 0 {
		out["max_tokens"] = in.MaxTokens
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
	if in.Stream {
		out["stream"] = true
	}
	messages := make([]chatMessage, 0, len(in.Messages)+1)
	if system := anthropicText(in.System); system != "" {
		messages = append(messages, chatMessage{Role: "system", Content: system})
	}
	for _, message := range in.Messages {
		converted, err := chatMessagesFromAnthropic(message)
		if err != nil {
			return nil, nil, err
		}
		messages = append(messages, converted...)
	}
	out["messages"] = messages
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, tool := range in.Tools {
			schema := tool.InputSchema
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  schema,
				},
			})
		}
		out["tools"] = tools
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return encoded, dropped, nil
}

// anthropicText flattens a system prompt that may be a string or a list of
// text blocks.
func anthropicText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type == "" || block.Type == "text" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// chatMessagesFromAnthropic converts one Anthropic message. A single Anthropic
// message can hold several text blocks, textual tool results and tool_use
// blocks; each of those becomes its own chat message, which is the only faithful
// mapping the chat schema offers.
func chatMessagesFromAnthropic(message anthropicMessage) ([]chatMessage, error) {
	role := "user"
	if message.Role == "assistant" {
		role = "assistant"
	}
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []chatMessage{{Role: role, Content: text}}, nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, err
	}
	out := make([]chatMessage, 0, len(blocks)+1)
	var texts []string
	var calls []chatToolCall
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		case "tool_use":
			arguments := "{}"
			if len(block.Input) > 0 && string(block.Input) != "null" {
				arguments = string(block.Input)
			}
			calls = append(calls, chatToolCall{ID: block.ID, Type: "function", Function: chatFunction{Name: block.Name, Arguments: arguments}})
		case "tool_result":
			result := anthropicText(block.Content)
			if block.IsError {
				result = "error: " + result
			}
			out = append(out, chatMessage{Role: "tool", ToolCallID: block.ToolUseID, Content: result})
		}
	}
	if len(texts) > 0 || len(calls) > 0 {
		entry := chatMessage{Role: role}
		if len(texts) > 0 {
			entry.Content = strings.Join(texts, "\n\n")
		}
		if len(calls) > 0 {
			entry.ToolCalls = calls
			if entry.Content == nil {
				entry.Content = ""
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// anthropicFromChat converts an OpenAI chat completion into an Anthropic
// Messages response.
func anthropicFromChat(body []byte, model string) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []chatToolCall  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	content := make([]map[string]any, 0, 2)
	stopReason := "end_turn"
	if len(in.Choices) > 0 {
		choice := in.Choices[0]
		if text := rawText(choice.Message.Content); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, call := range choice.Message.ToolCalls {
			var input json.RawMessage = json.RawMessage(`{}`)
			if strings.TrimSpace(call.Function.Arguments) != "" {
				input = json.RawMessage(call.Function.Arguments)
				if !json.Valid(input) {
					input = json.RawMessage(`{}`)
				}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
			stopReason = "tool_use"
		}
		switch choice.FinishReason {
		case "length":
			stopReason = "max_tokens"
		case "tool_calls", "function_call":
			stopReason = "tool_use"
		}
	}
	out := map[string]any{
		"id":            in.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  in.Usage["prompt_tokens"],
			"output_tokens": in.Usage["completion_tokens"],
		},
	}
	return json.Marshal(out)
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		joined := make([]string, 0, len(parts))
		for _, part := range parts {
			if part.Text != "" {
				joined = append(joined, part.Text)
			}
		}
		return strings.Join(joined, "\n\n")
	}
	return ""
}

// ---------------------------------------------------------------------------
// OpenAI Responses -> chat completions
// ---------------------------------------------------------------------------

// responsesDroppedFields are Responses request fields the chat schema cannot
// carry. Items that carry real content (input, tools, instructions) are
// converted instead.
var responsesDroppedFields = []string{"reasoning", "include", "store", "metadata", "parallel_tool_calls", "previous_response_id", "truncation", "background", "top_logprobs", "tool_choice", "service_tier", "max_tool_calls", "prompt_cache_key", "text", "user"}

// chatFromResponses converts a Responses request into chat completions.
func chatFromResponses(body []byte) ([]byte, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	dropped := unsupportedFields(keys, responsesDroppedFields...)
	var model string
	_ = json.Unmarshal(raw["model"], &model)
	out := map[string]any{"model": model}
	if maxTokens := firstInt(raw, "max_output_tokens", "max_completion_tokens", "max_tokens"); maxTokens > 0 {
		out["max_tokens"] = maxTokens
	}
	var stream bool
	if json.Unmarshal(raw["stream"], &stream) == nil && stream {
		out["stream"] = true
	}
	messages := make([]chatMessage, 0, 4)
	if instructions := strings.TrimSpace(rawString(raw["instructions"])); instructions != "" {
		messages = append(messages, chatMessage{Role: "system", Content: instructions})
	}
	items, err := responsesInputItems(raw["input"], dropped)
	if err != nil {
		return nil, nil, err
	}
	messages = append(messages, items...)
	out["messages"] = messages
	if tools, err := responsesTools(raw["tools"]); err != nil {
		return nil, nil, err
	} else if len(tools) > 0 {
		out["tools"] = tools
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return encoded, dropped, nil
}

func firstInt(raw map[string]json.RawMessage, names ...string) int {
	for _, name := range names {
		if value, ok := raw[name]; ok {
			var number int
			if json.Unmarshal(value, &number) == nil && number > 0 {
				return number
			}
		}
	}
	return 0
}

func rawString(raw json.RawMessage) string {
	var text string
	if len(raw) > 0 && json.Unmarshal(raw, &text) == nil {
		return text
	}
	return ""
}

// responsesInputItems normalises the Responses `input` field, which may be a
// plain string or a list of items (message, function_call, function_call_output,
// reasoning…).
func responsesInputItems(raw json.RawMessage, dropped []string) ([]chatMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []chatMessage{{Role: "user", Content: text}}, nil
	}
	var items []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]chatMessage, 0, len(items))
	for _, item := range items {
		kind := item.Type
		if kind == "" {
			kind = "message"
		}
		switch kind {
		case "message":
			role := item.Role
			if role == "developer" {
				role = "system"
			}
			if role == "" {
				role = "user"
			}
			out = append(out, chatMessage{Role: role, Content: responsesText(item.Content)})
		case "function_call":
			if len(out) == 0 || out[len(out)-1].Role != "assistant" {
				out = append(out, chatMessage{Role: "assistant", Content: ""})
			}
			last := &out[len(out)-1]
			last.ToolCalls = append(last.ToolCalls, chatToolCall{ID: item.CallID, Type: "function", Function: chatFunction{Name: item.Name, Arguments: item.Arguments}})
		case "function_call_output":
			out = append(out, chatMessage{Role: "tool", ToolCallID: item.CallID, Content: responsesText(item.Output)})
		default:
			// reasoning and other non-content items cannot be replayed to a chat
			// completion; drop them silently but report the field name.
			dropped = append(dropped, "input."+kind)
		}
	}
	return out, nil
}

// responsesText flattens Responses content (string or input_text/output_text
// parts) into plain text.
func responsesText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type    string  `json:"type"`
		Text    string  `json:"text"`
		Refusal *string `json:"refusal"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		joined := make([]string, 0, len(parts))
		for _, part := range parts {
			switch {
			case part.Text != "":
				joined = append(joined, part.Text)
			case part.Refusal != nil:
				joined = append(joined, *part.Refusal)
			}
		}
		return strings.Join(joined, "\n\n")
	}
	return ""
}

// responsesTools converts Responses tools (flat function shape) into chat tools.
func responsesTools(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Function    *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		name, description, parameters := tool.Name, tool.Description, tool.Parameters
		if tool.Type == "" || tool.Type == "function" {
			if tool.Function != nil {
				name, description, parameters = tool.Function.Name, tool.Function.Description, tool.Function.Parameters
			}
			if name == "" {
				continue
			}
			if len(parameters) == 0 || string(parameters) == "null" {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out = append(out, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": description,
					"parameters":  parameters,
				},
			})
			continue
		}
		// Hosted tools (web_search, file_search, code_interpreter…) run inside
		// the vendor, never inside a chat completion. Dropping them quietly is
		// what makes those calls fail far from here.
		return nil, errors.New("hosted tool type is not supported by chat completions: " + tool.Type)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Streaming translation: chat completions SSE -> Responses SSE / Anthropic SSE
// ---------------------------------------------------------------------------

// convertChatStream rewrites an SSE stream produced by chat completions into the
// event shape the caller asked for. The function returns the [DONE] sentinel
// plus the body written for each chat event.
func convertChatStream(target surfaceKind, model string, chatEvent []byte) ([][]byte, bool, error) {
	if bytes.Equal(bytes.TrimSpace(chatEvent), []byte("[DONE]")) {
		return nil, true, nil
	}
	var chunk struct {
		ID      string `json:"id"`
		Choices []struct {
			Delta struct {
				Content   string         `json:"content"`
				ToolCalls []chatToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chatEvent, &chunk); err != nil {
		return nil, false, nil // A non-JSON comment or heartbeat passes through unchanged.
	}
	var out [][]byte
	switch target {
	case surfaceAnthropic:
		if len(chunk.Choices) == 0 {
			return nil, false, nil
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			out = append(out, sseEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": delta.Content},
			}))
		}
		if chunk.Choices[0].FinishReason != "" {
			reason := "end_turn"
			if chunk.Choices[0].FinishReason == "length" {
				reason = "max_tokens"
			}
			if chunk.Choices[0].FinishReason == "tool_calls" {
				reason = "tool_use"
			}
			out = append(out, sseEvent("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": 0},
			}))
		}
	case surfaceResponses:
		if len(chunk.Choices) == 0 {
			return nil, false, nil
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			out = append(out, sseEvent("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": chunk.ID, "output_index": 0,
				"content_index": 0, "delta": delta.Content,
			}))
		}
		if chunk.Choices[0].FinishReason != "" {
			out = append(out, sseEvent("response.completed", map[string]any{
				"type":     "response.completed",
				"response": map[string]any{"id": chunk.ID, "object": "response", "status": "completed", "model": model},
			}))
		}
	}
	return out, false, nil
}

// sseEvent renders one server-sent event with a JSON payload.
func sseEvent(name string, payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var out bytes.Buffer
	out.WriteString("event: ")
	out.WriteString(name)
	out.WriteString("\n")
	out.WriteString("data: ")
	out.Write(encoded)
	out.WriteString("\n\n")
	return out.Bytes()
}

// splitSSEEvents parses a chat completion SSE stream tolerantly: blank lines end
// an event, "data:" lines carry the payload, comments are ignored.
func splitSSEEvents(reader io.Reader, emit func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		return emit([]byte(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// responsesFromChat converts a chat completion into an OpenAI Responses object.
func responsesFromChat(body []byte, model string) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []chatToolCall  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	output := make([]map[string]any, 0, 2)
	status := "completed"
	if len(in.Choices) > 0 {
		choice := in.Choices[0]
		if text := rawText(choice.Message.Content); text != "" {
			output = append(output, map[string]any{
				"type": "message", "id": in.ID, "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
			})
		}
		for _, call := range choice.Message.ToolCalls {
			output = append(output, map[string]any{
				"type": "function_call", "id": call.ID, "call_id": call.ID,
				"name": call.Function.Name, "arguments": call.Function.Arguments, "status": "completed",
			})
		}
		if choice.FinishReason == "length" {
			status = "incomplete"
		}
	}
	out := map[string]any{
		"id": in.ID, "object": "response", "created_at": in.Created, "model": model,
		"status": status, "output": output,
		"usage": map[string]any{
			"input_tokens":  in.Usage["prompt_tokens"],
			"output_tokens": in.Usage["completion_tokens"],
			"total_tokens":  in.Usage["total_tokens"],
		},
	}
	return json.Marshal(out)
}
