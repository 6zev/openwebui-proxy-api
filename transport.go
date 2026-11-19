package main

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Go's HTTP transport supports HTTP(S) CONNECT and SOCKS5 proxies, including
// username/password authentication. Proxy credentials never go to the origin.
// newOutboundClient builds the HTTP client for an account.
//
// insecure skips certificate verification. It exists for self-hosted instances
// reached by bare IP (a cert without an IP SAN cannot validate), and it is
// opt-in per website: nothing is skipped unless the operator asks.
func newOutboundClient(raw string, timeout time.Duration, insecure ...bool) (*http.Client, error) {
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
	if len(insecure) > 0 && insecure[0] {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
