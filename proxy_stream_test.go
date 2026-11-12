package main

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProxyStreamFlushAndClientCancellation(t *testing.T) {
	u := newProxyTestUpstream(t)
	canceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	u.onChat = func(_ string, _ int, w http.ResponseWriter, r *http.Request, _ map[string]any) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
		return true
	}
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
	req, _ := http.NewRequest("POST", s.URL+"/v1/chat/completions", strings.NewReader(testChatBody))
	req.Header.Set("Authorization", "Bearer "+testProxyKey)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data: first\n" {
		resp.Body.Close()
		t.Fatalf("first event not flushed before upstream completed: %q %v", first, err)
	}
	_ = resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client close did not cancel upstream")
	}
	_, records := u.snapshot()
	if len(records) != 1 {
		t.Errorf("stream cancellation retried other account")
	}
}

func TestProxyBrokenStreamDoesNotFailover(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.onChat = func(_ string, _ int, w http.ResponseWriter, _ *http.Request, _ map[string]any) bool {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return true
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\nd\r\ndata: first\n\n\r\n")
		_ = rw.Flush() // No terminating zero chunk: simulate a broken upstream stream.
		return true
	}
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
	req, _ := http.NewRequest("POST", s.URL+"/v1/chat/completions", strings.NewReader(testChatBody))
	req.Header.Set("Authorization", "Bearer "+testProxyKey)
	resp, err := s.Client().Do(req)
	if err == nil {
		_, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr == nil {
			t.Error("truncated stream incorrectly completed successfully")
		}
	}
	logins, records := u.snapshot()
	if len(records) != 1 || logins["beta"] != 0 {
		t.Errorf("broken stream caused failover: %v %v", logins, records)
	}
}
