package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A login must look like it came from the instance's own web UI: Open WebUI behind
// a CSRF proxy and LibreChat's requireSameOrigin both compare Origin.
func TestOriginDerivedFromBaseURL(t *testing.T) {
	p := &Proxy{config: Config{}}
	target, _ := url.Parse("https://114.34.242.158:1443/api/auth/login")
	if got := p.originFor(target); got != "https://114.34.242.158:1443" {
		t.Errorf("origin = %q", got)
	}
	if got := p.originFor(nil); got != "" {
		t.Errorf("nil target should give no origin: %q", got)
	}
	// An explicit override wins, trailing slash removed.
	overridden := &Proxy{config: Config{LibreChatOrigin: " https://chat.example.com/ "}}
	if got := overridden.originFor(target); got != "https://chat.example.com" {
		t.Errorf("override = %q", got)
	}
}

// The LibreChat login request carries Origin, Referer and a browser User-Agent.
func TestLibreChatLoginHeaders(t *testing.T) {
	request, _ := http.NewRequest(http.MethodPost, "https://lc.example.com/api/auth/login", nil)
	p := &Proxy{config: Config{}}
	p.decorateLibreChatRequest(request)
	if got := request.Header.Get("Origin"); got != "https://lc.example.com" {
		t.Errorf("Origin = %q", got)
	}
	if got := request.Header.Get("Referer"); got != "https://lc.example.com/" {
		t.Errorf("Referer = %q", got)
	}
	if got := request.Header.Get("User-Agent"); !strings.Contains(got, "Mozilla/5.0") {
		t.Errorf("User-Agent = %q", got)
	}
}
