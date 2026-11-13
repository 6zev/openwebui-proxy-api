package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// splitModelPrefix splits "<prefix>/<model>" on the first slash. Open WebUI
// model IDs frequently contain slashes ("openai/gpt-5.6"), so a prefix is only
// honoured when it matches a configured account or website ID.
func splitModelPrefix(model string) (prefix, rest string, ok bool) {
	model = strings.TrimSpace(model)
	index := strings.Index(model, "/")
	if index <= 0 || index == len(model)-1 {
		return "", "", false
	}
	return model[:index], model[index+1:], true
}

// prefixTarget reports whether the prefix names an account or a website.
func (p *Proxy) prefixTarget(prefix string) bool {
	for _, a := range p.accounts {
		if a.config.ID == prefix {
			return true
		}
	}
	_, ok := p.sites[prefix]
	return ok
}

func (p *Proxy) isAccountID(prefix string) bool {
	for _, a := range p.accounts {
		if a.config.ID == prefix {
			return true
		}
	}
	return false
}

// applyModelPrefix rewrites the JSON body's model when the caller used the
// "<prefix>/<model>" form and sets the routing header if none was provided.
func applyModelPrefix(p *Proxy, r *http.Request, body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(body, &input) != nil || input == nil {
		return body
	}
	var model string
	if raw, ok := input["model"]; !ok || json.Unmarshal(raw, &model) != nil {
		return body
	}
	prefix, rest, ok := splitModelPrefix(model)
	if !ok || !p.prefixTarget(prefix) {
		return body
	}
	input["model"], _ = json.Marshal(rest)
	if r.Header.Get("X-Account-ID") == "" && r.Header.Get("X-Website-ID") == "" {
		if p.isAccountID(prefix) {
			r.Header.Set("X-Account-ID", prefix)
		} else {
			r.Header.Set("X-Website-ID", prefix)
		}
	}
	updated, err := json.Marshal(input)
	if err != nil {
		return body
	}
	return updated
}
