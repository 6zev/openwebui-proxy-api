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

// streamState carries what the translation needs across events: which content
// block is open, which tool call is being streamed, and the token usage the
// final event must report.
//
// A chat completion streams all text first, then one tool call per stream with a
// stable index. Anthropic instead interleaves blocks, so a text block has to be
// closed before a tool_use block opens; each tool call gets its own block and its
// JSON arguments are forwarded as input_json_delta. Without this a client that
// dispatches tools sees malformed calls or loses every call but the first.
type streamState struct {
	target    surfaceKind
	model     string
	messageID string
	textOpen  bool
	nextBlock int
	tools     map[int]*toolBlockState
	// itemOpen tracks the Responses output item, which needs its own add/done
	// events around the text deltas.
	itemOpen  bool
	textBuf   strings.Builder
	done      bool
	promptTok int
	compleTok int
	totalTok  int
	createdAt int64
}

type toolBlockState struct {
	block   int    // Anthropic content block index
	id      string // tool call id, as the client must echo it back
	name    string
	started bool // content_block_start already emitted
	// arguments accumulates the streamed JSON so the terminal Responses payload
	// can carry a finished function_call item.
	arguments strings.Builder
}

// chatStreamChunk is the subset of a chat completion SSE chunk this layer reads.
type chatStreamChunk struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string           `json:"content"`
			ToolCalls []streamToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			ToolCalls []streamToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// streamToolCall keeps the index openai streams per tool call; id and name are
// usually only present on the first fragment.
type streamToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// newStreamState seeds the state and emits the opening events for Anthropic,
// whose clients wait for message_start/content_block_start before any delta.
func newStreamState(target surfaceKind, model string) (*streamState, [][]byte) {
	st := &streamState{target: target, model: model, tools: make(map[int]*toolBlockState)}
	if target == surfaceAnthropic {
		opening := [][]byte{sseEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_compat", "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})}
		return st, opening
	}
	return st, nil
}

// itemID names the output item a Responses client must track. Response ids are
// reused across chunks by upstream, so the item id is derived once per stream.
func (st *streamState) itemID(index int) string {
	if st.messageID == "" {
		st.messageID = "resp_compat"
	}
	return st.messageID + "-item-" + itoa(index)
}

// openResponsesItem emits the Responses envelope for the message item the first
// time text arrives: response.created plus response.output_item.added and
// response.content_part.added. Without these a Responses client sees deltas for
// an item it never learned about and simply retries.
func (st *streamState) openResponsesItem() [][]byte {
	if st.itemOpen {
		return nil
	}
	st.itemOpen = true
	response := map[string]any{
		"id": st.messageID, "object": "response", "status": "in_progress",
		"model": st.model, "created_at": st.createdAt, "output": []any{},
	}
	item := map[string]any{
		"type": "message", "id": st.itemID(0), "role": "assistant", "status": "in_progress",
		"content": []any{},
	}
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	return [][]byte{
		sseEvent("response.created", map[string]any{"type": "response.created", "response": response}),
		sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item}),
		sseEvent("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": st.itemID(0),
			"output_index": 0, "content_index": 0, "part": part,
		}),
	}
}

// openTextBlock emits content_block_start for the text block, once.
func (st *streamState) openTextBlock() [][]byte {
	if st.textOpen {
		return nil
	}
	st.textOpen = true
	st.nextBlock = 1
	return [][]byte{sseEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})}
}

// convertChatStream translates one chat completion event into zero or more
// events in the caller's dialect.
func (st *streamState) convertChatStream(chatEvent []byte) ([][]byte, bool, error) {
	trimmed := bytes.TrimSpace(chatEvent)
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		st.done = true
		return nil, true, nil
	}
	var chunk chatStreamChunk
	if err := json.Unmarshal(trimmed, &chunk); err != nil {
		return nil, false, nil // comments and keep-alives carry nothing to translate
	}
	if chunk.ID != "" && st.messageID == "" {
		st.messageID = chunk.ID
	}
	if chunk.Created != 0 && st.createdAt == 0 {
		st.createdAt = chunk.Created
	}
	if chunk.Usage != nil {
		// Reported on the final chunk, or its own chunk when the client asked
		// for stream_options.include_usage.
		st.promptTok = chunk.Usage.PromptTokens
		st.compleTok = chunk.Usage.CompletionTokens
		st.totalTok = chunk.Usage.TotalTokens
	}
	var out [][]byte
	// Non-choices chunks (pure usage report) still have to advance nothing.
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		delta := choice.Delta
		for _, call := range delta.ToolCalls {
			out = append(out, st.toolDeltas(call)...)
		}
		if delta.Content != "" {
			switch st.target {
			case surfaceAnthropic:
				out = append(out, st.openTextBlock()...)
				out = append(out, sseEvent("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": 0,
					"delta": map[string]any{"type": "text_delta", "text": delta.Content},
				}))
			case surfaceResponses:
				out = append(out, st.openResponsesItem()...)
				st.textBuf.WriteString(delta.Content)
				out = append(out, sseEvent("response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": st.itemID(0), "output_index": 0,
					"content_index": 0, "delta": delta.Content,
				}))
			}
		}
		if choice.FinishReason != "" {
			// Some upstreams only send the whole answer in message.tool_calls.
			for _, call := range choice.Message.ToolCalls {
				out = append(out, st.toolDeltas(call)...)
			}
			out = append(out, st.finalEvents(choice.FinishReason)...)
		}
	}
	return out, st.done, nil
}

// toolDeltas turns one streamed tool-call fragment into the Anthropic events that
// start and feed a tool_use block. The openai call index decides the block, so
// parallel tool calls stay separate even when ids arrive later.
func (st *streamState) toolDeltas(call streamToolCall) [][]byte {
	index := 0
	if call.Index != nil {
		index = *call.Index
	}
	if st.target != surfaceAnthropic {
		// The Responses dialect reports tool calls as completed items, but the
		// fragments still have to be recorded: the terminal payload must carry
		// the finished function_call or the client sees no tool call at all.
		state := st.toolState(index)
		if state.id == "" && call.ID != "" {
			state.id = call.ID
		}
		if state.name == "" && call.Function.Name != "" {
			state.name = call.Function.Name
		}
		if call.Function.Arguments != "" {
			state.arguments.WriteString(call.Function.Arguments)
		}
		return nil
	}
	var out [][]byte
	state, ok := st.tools[index]
	if !ok {
		// A new tool call closes the text block first: Anthropic blocks cannot
		// interleave, so text must end before tool_use begins.
		if st.textOpen {
			out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
			st.textOpen = false
		}
		state = &toolBlockState{block: st.nextBlock}
		st.nextBlock++
		st.tools[index] = state
	}
	if state.id == "" && call.ID != "" {
		state.id = call.ID
	}
	if state.name == "" && call.Function.Name != "" {
		state.name = call.Function.Name
	}
	if !state.started && (state.name != "" || state.id != "") {
		id := state.id
		if id == "" {
			// A client still needs an id to echo back; derive a stable one.
			id = "toolu_compat_" + itoa(index)
		}
		out = append(out, sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": state.block,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": state.name, "input": map[string]any{}},
		}))
		state.started = true
	}
	if call.Function.Arguments != "" {
		state.arguments.WriteString(call.Function.Arguments)
	}
	if state.started && call.Function.Arguments != "" {
		out = append(out, sseEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": state.block,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments},
		}))
	}
	return out
}

// itoa is strconv.Itoa for the one place this file needs it, keeping the import
// list to encoding/json and friends.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		pos--
		digits[pos] = '-'
	}
	return string(digits[pos:])
}

// finalEvents closes every open block and reports the real token usage.
func (st *streamState) finalEvents(finishReason string) [][]byte {
	var out [][]byte
	if st.target == surfaceAnthropic {
		if st.textOpen {
			out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
			st.textOpen = false
		}
		// Close tool blocks in the order they opened.
		blocks := make([]int, 0, len(st.tools))
		for _, state := range st.tools {
			blocks = append(blocks, state.block)
		}
		sortInts(blocks)
		for _, block := range blocks {
			out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": block}))
		}
		stop := "end_turn"
		switch finishReason {
		case "length":
			stop = "max_tokens"
		case "tool_calls", "function_call":
			stop = "tool_use"
		}
		out = append(out, sseEvent("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
			"usage": map[string]any{"input_tokens": st.promptTok, "output_tokens": st.compleTok},
		}))
		out = append(out, sseEvent("message_stop", map[string]any{"type": "message_stop"}))
		return out
	}
	// Responses: close the item and its content part, then report the real
	// usage on the terminal event.
	var completedOutput []map[string]any
	if st.itemOpen {
		st.textOpen = false
		out = append(out, sseEvent("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": st.itemID(0),
			"output_index": 0, "content_index": 0, "text": st.textBuf.String(),
		}))
		out = append(out, sseEvent("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": st.itemID(0), "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": st.textBuf.String(), "annotations": []any{}},
		}))
		finalItem := map[string]any{
			"type": "message", "id": st.itemID(0), "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": st.textBuf.String(), "annotations": []any{}}},
		}
		out = append(out, sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": finalItem,
		}))
		completedOutput = append(completedOutput, finalItem)
		st.itemOpen = false
	}
	for _, block := range st.sortedTools() {
		arguments := block.arguments.String()
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		id := block.id
		if id == "" {
			id = "call_compat_" + itoa(block.block)
		}
		completedOutput = append(completedOutput, map[string]any{
			"type": "function_call", "id": id, "call_id": id,
			"name": block.name, "arguments": arguments, "status": "completed",
		})
	}
	status := "completed"
	if finishReason == "length" {
		status = "incomplete"
	}
	if completedOutput == nil {
		completedOutput = []map[string]any{}
	}
	out = append(out, sseEvent("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": st.messageID, "object": "response", "status": status, "model": st.model,
			"created_at": st.createdAt, "output": completedOutput,
			"usage": map[string]any{
				"input_tokens": st.promptTok, "output_tokens": st.compleTok, "total_tokens": st.totalTok,
			},
		},
	}))
	return out
}

// toolState returns the tracked state for one openai call index, creating it if
// this is the first fragment seen.
func (st *streamState) toolState(index int) *toolBlockState {
	if state, ok := st.tools[index]; ok {
		return state
	}
	state := &toolBlockState{block: st.nextBlock}
	st.nextBlock++
	st.tools[index] = state
	return state
}

// sortedTools returns the tool blocks in the order they opened.
func (st *streamState) sortedTools() []*toolBlockState {
	indexes := make([]int, 0, len(st.tools))
	for index := range st.tools {
		indexes = append(indexes, index)
	}
	sortInts(indexes)
	out := make([]*toolBlockState, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, st.tools[index])
	}
	return out
}

func sortInts(items []int) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
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
