package main

import (
	"bytes"
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
		writeError(w, &apiError{status: 400, message: "Cannot translate request: " + err.Error()})
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
		writeError(w, &apiError{status: 502, message: "Upstream response was cut off"})
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
	state, opening := newStreamState(kind, model)
	for _, event := range opening {
		if _, err := w.Write(event); err != nil {
			return
		}
	}
	_ = rc.Flush()
	finished := false
	write := func(events [][]byte) error {
		for _, event := range events {
			if _, err := w.Write(event); err != nil {
				return err
			}
		}
		return rc.Flush()
	}
	err := splitSSEEvents(response.Body, func(event []byte) error {
		chunks, done, convErr := state.convertChatStream(event)
		if convErr != nil {
			return convErr
		}
		if len(chunks) > 0 && !finished {
			// Terminal events arrive with the last finish_reason chunk; remember
			// that so [DONE] does not repeat them.
			for _, chunk := range chunks {
				if bytes.Contains(chunk, []byte("\"type\":\"response.completed\"")) || bytes.Contains(chunk, []byte("\"type\":\"message_stop\"")) {
					finished = true
				}
			}
		}
		if err := write(chunks); err != nil {
			return err
		}
		if done {
			// [DONE] is the end of the stream. Close the message exactly once:
			// if a finish_reason already produced the terminal event, emitting
			// another response.completed/message_stop makes clients treat the
			// answer as unfinished and retry the whole request.
			if finished {
				return nil
			}
			finished = true
			return write(state.finalEvents("stop"))
		}
		return nil
	})
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = rc.Flush()
}
