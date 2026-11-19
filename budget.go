package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Budget refusals are the one upstream error that names its own fix.
//
// OpenRouter (and providers behind Open WebUI that proxy it) answer:
//
//	402 {"error":{"message":"This request requires more credits, or fewer
//	     max_tokens. You requested up to 128000 tokens, but can only afford
//	     106664. ..."}}
//
// The ceiling is spelled out in the message, so the proxy can ask again with a
// max_tokens the account can actually afford instead of handing the caller a 402
// it could have avoided. Nothing else about the request changes.

// affordBudgetRe matches the affordable ceiling in a budget refusal.
var affordBudgetRe = regexp.MustCompile(`(?i)can only afford\s+(\d+)`)

// affordableMaxTokens extracts the ceiling an upstream says it can serve. It
// returns 0 when the message carries no such figure.
func affordableMaxTokens(body []byte) int {
	detail := upstreamDetail(body)
	if detail == "" {
		detail = string(body)
	}
	match := affordBudgetRe.FindStringSubmatch(detail)
	if len(match) != 2 {
		return 0
	}
	value, err := strconv.Atoi(match[1])
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

// clampMaxTokens rewrites the output-token budget in a request body. It reports
// whether anything changed, so the caller can avoid a pointless retry.
//
// Both spellings are handled: chat completions uses max_tokens, and the newer
// max_completion_tokens wins when a client sends both — same rule the provider.
func clampMaxTokens(body []byte, limit int) ([]byte, bool) {
	if len(body) == 0 || limit <= 0 {
		return body, false
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(body, &document) != nil {
		return body, false
	}
	changed := false
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		raw, ok := document[key]
		if !ok {
			continue
		}
		var current int
		if json.Unmarshal(raw, &current) != nil || current <= limit {
			continue
		}
		encoded, err := json.Marshal(limit)
		if err != nil {
			continue
		}
		document[key] = encoded
		changed = true
	}
	if !changed {
		return body, false
	}
	rewritten, err := json.Marshal(document)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

// capMaxTokens sets an output-token cap on a body that has none. A refusal can name
// a ceiling for a request that never asked for one — the model default is what the
// account cannot afford — so the cap has to be added, not just lowered.
func capMaxTokens(body []byte, limit int) ([]byte, bool) {
	if len(body) == 0 || limit <= 0 {
		return body, false
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(body, &document) != nil {
		return body, false
	}
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if raw, ok := document[key]; ok && len(raw) > 0 && string(raw) != "null" {
			return body, false
		}
	}
	encoded, err := json.Marshal(limit)
	if err != nil {
		return body, false
	}
	document["max_tokens"] = encoded
	rewritten, err := json.Marshal(document)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

// budgetRefusal reports whether the body is a "fewer max_tokens" refusal rather
// than a generic credit problem, and returns the affordable ceiling.
func budgetRefusal(body []byte) (int, bool) {
	detail := strings.ToLower(upstreamDetail(body))
	if !strings.Contains(detail, "max_tokens") && !strings.Contains(detail, "max_output_tokens") {
		return 0, false
	}
	limit := affordableMaxTokens(body)
	if limit <= 0 {
		return 0, false
	}
	return limit, true
}
