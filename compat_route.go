package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// translateTarget reports which translation a public path needs. Only shapes the
// chat completions API can carry faithfully are listed.
func translateTarget(path string) (surfaceKind, bool) {
	switch {
	case path == "/v1/responses":
		return surfaceResponses, true
	case path == "/v1/messages":
		return surfaceAnthropic, true
	}
	return surfaceNative, false
}

// forwardTranslated serves a Responses or Anthropic Messages call by translating
// it to chat completions on the selected account, then translating the answer
// back. Translation is only used when the upstream lacks the native route; the
// caller always learns which fields were dropped through X-Compat-Dropped.
func (p *Proxy) forwardTranslated(w http.ResponseWriter, r *http.Request, body []byte, kind surfaceKind) {
	candidates, err := p.candidatesFor(r.Header.Get("X-Account-ID"), r.Header.Get("X-Website-ID"))
	if err != nil {
		writeError(w, err)
		return
	}
	var chatBody []byte
	var dropped []string
	switch kind {
	case surfaceResponses:
		chatBody, dropped, err = chatFromResponses(body)
	case surfaceAnthropic:
		chatBody, dropped, err = chatFromAnthropic(body)
	default:
		err = errors.New("unsupported translation")
	}
	if err != nil {
		writeError(w, &apiError{400, "Cannot translate request: " + err.Error(), 0})
		return
	}
	var model string
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(chatBody, &probe) == nil {
		model = probe.Model
	}
	if len(dropped) > 0 {
		// Never claim fidelity that does not exist: the header names every field
		// the chat schema could not carry.
		w.Header().Set("X-Compat-Dropped", strings.Join(dropped, ","))
	}
	w.Header().Set("X-Compat-Mode", string(kind))
	ctx, cancel := context.WithTimeout(r.Context(), p.requestTimeout(candidates))
	defer cancel()
	var response *http.Response
	for _, a := range candidates {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		w.Header().Set("X-Account-ID", a.config.ID)
		w.Header().Set("X-Website-ID", a.config.WebsiteID)
		var retry bool
		response, err, retry = p.requestWithHeaders(ctx, a, "/api/chat/completions", http.MethodPost, chatBody, nil)
		if err == nil {
			break
		}
		if r.Header.Get("X-Account-ID") != "" || !retry {
			break
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	defer response.Body.Close()
	if kind == surfaceResponses || kind == surfaceAnthropic {
		streaming := strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "event-stream") || wantsStream(body)
		if streaming {
			translateStream(w, response, kind, model)
			return
		}
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if readErr != nil {
		writeError(w, &apiError{502, "Upstream response was cut off", 0})
		return
	}
	switch kind {
	case surfaceAnthropic:
		if converted, convErr := anthropicFromChat(raw, model); convErr == nil {
			writeCompatJSON(w, converted)
			return
		}
	case surfaceResponses:
		if converted, convErr := responsesFromChat(raw, model); convErr == nil {
			writeCompatJSON(w, converted)
			return
		}
	}
	writeCompatJSON(w, raw)
}

func wantsStream(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Stream
}

func writeCompatJSON(w http.ResponseWriter, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// translateStream rewrites a chat completion SSE stream into the requested event
// shape. A truncated upstream stream is aborted rather than closed cleanly, so a
// client never mistakes a broken answer for a complete one.
func translateStream(w http.ResponseWriter, response *http.Response, kind surfaceKind, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	if kind == surfaceAnthropic {
		// Anthropic clients expect the message_start/content_block_start envelope.
		_, _ = w.Write(sseEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_compat", "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}))
		_, _ = w.Write(sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""},
		}))
	}
	err := splitSSEEvents(response.Body, func(event []byte) error {
		chunks, done, convErr := convertChatStream(kind, model, event)
		if convErr != nil {
			return convErr
		}
		for _, chunk := range chunks {
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return writeErr
			}
		}
		_ = rc.Flush()
		if done {
			return nil
		}
		return nil
	})
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if kind == surfaceAnthropic {
		_, _ = w.Write(sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
		_, _ = w.Write(sseEvent("message_stop", map[string]any{"type": "message_stop"}))
	}
	_ = rc.Flush()
}
