package main

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Go's HTTP transport supports HTTP(S) CONNECT and SOCKS5 proxies, including
// username/password authentication. Proxy credentials never go to the origin.

// certificateCannotValidate reports whether a base URL is addressed by a bare IP
// literal. A public CA never issues IP SANs to such a host, so Go's verifier
// fails with "x509: cannot validate certificate for <ip> because it doesn't
// contain any IP SANs" and the upstream is unreachable. Verification is skipped
// automatically for those hosts only; DNS-named upstreams keep full verification
// unless the website opts into insecure_tls.
func certificateCannotValidate(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	// A bracketed IPv6 literal (or a zone-scoped one) never has a usable SAN.
	if strings.Contains(host, ":") {
		return true
	}
	return net.ParseIP(host) != nil
}

// newOutboundClient builds the HTTP client for an account.
//
// insecure skips certificate verification. It exists for self-hosted instances
// reached by bare IP (a cert without an IP SAN cannot validate), and it is
// opt-in per website; bare IP hosts are detected automatically (see
// certificateCannotValidate), nothing else is skipped unless the operator asks.
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
