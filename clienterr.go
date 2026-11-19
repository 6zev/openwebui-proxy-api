package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// upstreamDetail extracts a human-readable reason from an upstream error body.
//
// Open WebUI (and most OpenAI-compatible servers) answer with
//
//	{"detail":"You have no credits remaining. Add credits to continue ..."}
//
// but the proxy used to discard that and reply "<Backend> returned HTTP 400",
// which tells the caller nothing actionable: a spent credit balance looked
// identical to a malformed request. The detail is now forwarded.
func upstreamDetail(raw []byte) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return ""
	}
	var envelope struct {
		Detail  json.RawMessage `json:"detail"`
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		// LibreChat reports machine-readable failures as {"type":"no_user_key"} or
		// {"type":"illegal_model_request","info":"..."} — no detail, no error.
		Type string `json:"type"`
		Info string `json:"info"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return ""
	}
	if text := detailText(envelope.Detail); text != "" {
		return text
	}
	if text := detailText(envelope.Error); text != "" {
		return text
	}
	if envelope.Type != "" {
		if envelope.Info != "" {
			return sanitizeDetail(envelope.Type + " " + envelope.Info)
		}
		return sanitizeDetail(envelope.Type)
	}
	// LibreChat answers 403 with {"message":"Your account has been temporarily
	// banned due to violations of our service."} — no detail, no error object.
	return sanitizeDetail(envelope.Message)
}

// detailText accepts the shapes seen in the wild: a plain string, an object with
// message/detail, or a list of validation errors.
func detailText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return sanitizeDetail(text)
	}
	var object struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(raw, &object) == nil {
		if object.Message != "" {
			return sanitizeDetail(object.Message)
		}
		if object.Detail != "" {
			return sanitizeDetail(object.Detail)
		}
	}
	var list []struct {
		Msg  string `json:"msg"`
		Loc  []any  `json:"loc"`
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		parts := make([]string, 0, len(list))
		for _, item := range list {
			if item.Msg != "" {
				parts = append(parts, item.Msg)
			}
		}
		if len(parts) > 0 {
			return sanitizeDetail(strings.Join(parts, "; "))
		}
	}
	return ""
}

// sanitizeDetail keeps an upstream message on one line and bounded, because it is
// upstream-controlled text that ends up in a client-facing error.
func sanitizeDetail(text string) string {
	const limit = 400
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}

// readUpstreamError drains a bounded amount of an error body so the reason can be
// forwarded even when debug logging is off.
func readUpstreamError(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, 8<<10))
}

// ensureStreamUsage asks the upstream for token usage on a streaming chat call.
//
// OpenAI-compatible servers only report usage in a stream when the request sets
// stream_options.include_usage. Without it a streaming client shows no token
// count and no tokens-per-second, which is exactly what mobile agents (OpenMinis,
// Kelivo) display. The flag is added to the proxy's outbound copy only, so the
// caller's payload is untouched; a request that already sets it keeps its value.
func ensureStreamUsage(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return body
	}
	var stream bool
	if raw, ok := request["stream"]; !ok || json.Unmarshal(raw, &stream) != nil || !stream {
		return body
	}
	var options map[string]json.RawMessage
	if raw, ok := request["stream_options"]; ok {
		if json.Unmarshal(raw, &options) != nil {
			options = nil
		}
	}
	if options == nil {
		options = map[string]json.RawMessage{}
	}
	if _, set := options["include_usage"]; set {
		return body
	}
	options["include_usage"] = json.RawMessage("true")
	encoded, err := json.Marshal(options)
	if err != nil {
		return body
	}
	request["stream_options"] = encoded
	updated, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return updated
}
