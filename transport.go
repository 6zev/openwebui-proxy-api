package main

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Go's HTTP transport supports HTTP(S) CONNECT and SOCKS5 proxies, including
// username/password authentication. Proxy credentials never go to the origin.
func newOutboundClient(raw string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	transport.MaxIdleConnsPerHost = 32
	switch {
	case raw == "direct":
		transport.Proxy = nil
	case raw == "":
		transport.Proxy = http.ProxyFromEnvironment
	default:
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("invalid proxy URL")
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, errors.New("unsupported proxy scheme")
		}
		// SOCKS endpoints require a port; explicit ports also avoid scheme-specific surprises.
		if u.Port() == "" {
			return nil, errors.New("proxy URL requires a port")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
