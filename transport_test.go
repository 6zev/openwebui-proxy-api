package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboundHTTPProxyPerAccount(t *testing.T) {
	var count [2]atomic.Int32
	var proxies [2]*httptest.Server
	for i := 0; i < 2; i++ {
		i := i
		proxies[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count[i].Add(1)
			wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxyuser:proxypass"))
			if r.Header.Get("Proxy-Authorization") != wantAuth {
				t.Error("missing proxy auth")
			}
			if r.URL.Host != "unresolvable.example.invalid" {
				t.Errorf("wrong proxy target: %s", r.URL.Host)
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/auths/signin" {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				want := []string{"alpha", "beta"}[i] + "@example.test"
				if body["email"] != want {
					t.Errorf("account leaked across proxies")
				}
				_, _ = io.WriteString(w, `{"token":"token"}`)
			} else {
				_, _ = io.WriteString(w, `{"data":[{"id":"proxy-model"}]}`)
			}
		}))
		t.Cleanup(proxies[i].Close)
	}
	withCredentials := func(raw string) string { return strings.Replace(raw, "http://", "http://proxyuser:proxypass@", 1) }
	cfg := Config{BaseURL: "http://unresolvable.example.invalid", APIKey: testProxyKey, Timeout: time.Second, OutboundProxy: withCredentials(proxies[0].URL), Accounts: []AccountConfig{
		{ID: "alpha", Email: "alpha@example.test", Password: "p"},
		{ID: "beta", Email: "beta@example.test", Password: "p", ProxyURL: withCredentials(proxies[1].URL)},
	}}
	s := newProxyTestServer(t, cfg)
	for _, id := range []string{"alpha", "beta"} {
		requireProxyStatus(t, proxyTestRequest(t, s, "GET", "/v1/models", "", testProxyKey, id), 200)
	}
	for i := 0; i < 2; i++ {
		if count[i].Load() != 2 {
			t.Errorf("proxy %d got %d requests, want login and models", i, count[i].Load())
		}
	}
}

func TestOutboundSOCKS5(t *testing.T) {
	u := newProxyTestUpstream(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var connections atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(conn)
				header := make([]byte, 2)
				if _, err := io.ReadFull(reader, header); err != nil {
					return
				}
				if header[0] != 5 {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(reader, methods); err != nil {
					return
				}
				_, _ = conn.Write([]byte{5, 0})
				cmd := make([]byte, 4)
				if _, err := io.ReadFull(reader, cmd); err != nil {
					return
				}
				var host string
				switch cmd[3] {
				case 1:
					ip := make([]byte, 4)
					if _, err := io.ReadFull(reader, ip); err != nil {
						return
					}
					host = net.IP(ip).String()
				case 3:
					n, err := reader.ReadByte()
					if err != nil {
						return
					}
					b := make([]byte, int(n))
					if _, err := io.ReadFull(reader, b); err != nil {
						return
					}
					host = string(b)
				default:
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(reader, port); err != nil {
					return
				}
				dest, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1]))), time.Second)
				if err != nil {
					return
				}
				defer dest.Close()
				connections.Add(1)
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				done := make(chan struct{})
				go func() { _, _ = io.Copy(dest, reader); _ = dest.Close(); close(done) }()
				_, _ = io.Copy(conn, dest)
				_ = conn.Close()
				<-done
			}()
		}
	}()
	cfg := proxyTestConfig(u, "alpha")
	cfg.OutboundProxy = "socks5h://" + listener.Addr().String()
	s := newProxyTestServer(t, cfg)
	requireProxyStatus(t, proxyTestRequest(t, s, "GET", "/v1/models", "", testProxyKey, "alpha"), 200)
	if connections.Load() == 0 {
		t.Error("SOCKS5 was not used")
	}
}

func TestOutboundProxyValidation(t *testing.T) {
	for _, raw := range []string{"ftp://host:21", "http://host", "http://host:8080/path", "socks5://:1234", "http://host:1?x=1"} {
		if _, err := newOutboundClient(raw, time.Second); err == nil {
			t.Errorf("accepted invalid proxy %s", raw)
		}
	}
	client, err := newOutboundClient("direct", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Error("direct must disable environment proxy")
	}
}
