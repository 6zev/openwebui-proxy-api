package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testProxyKey = "test-proxy-secret-at-least-24-characters"
const testChatBody = `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`

type proxyTestRecord struct {
	ID, Path, Authorization string
	Body                    map[string]any
}

type proxyTestUpstream struct {
	server  *httptest.Server
	mu      sync.Mutex
	logins  map[string]int
	records []proxyTestRecord
	onLogin func(string, int, http.ResponseWriter, *http.Request) bool
	onChat  func(string, int, http.ResponseWriter, *http.Request, map[string]any) bool
	onModel func(string, http.ResponseWriter, *http.Request) bool
}

func newProxyTestUpstream(t *testing.T) *proxyTestUpstream {
	t.Helper()
	u := &proxyTestUpstream{logins: make(map[string]int)}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auths/signin" {
			var body struct{ Email, Password string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode signin: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			id := strings.TrimSuffix(body.Email, "@example.test")
			if body.Password != "password-"+id {
				t.Errorf("account %q received incorrect password", id)
			}
			u.mu.Lock()
			u.logins[id]++
			n := u.logins[id]
			u.mu.Unlock()
			if u.onLogin != nil && u.onLogin(id, n, w, r) {
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("%s-token-%d", id, n), "expires_at": time.Now().Add(time.Hour).Unix()})
			return
		}
		if r.URL.Path != "/api/chat/completions" && r.URL.Path != "/api/models" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		auth := r.Header.Get("Authorization")
		id := strings.Split(strings.TrimPrefix(auth, "Bearer "), "-token-")[0]
		if id == "" || !strings.HasPrefix(auth, "Bearer "+id+"-token-") {
			t.Errorf("invalid upstream authorization %q", auth)
		}
		var body map[string]any
		if r.URL.Path == "/api/chat/completions" {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upstream chat: %v", err)
			}
		}
		u.mu.Lock()
		u.records = append(u.records, proxyTestRecord{ID: id, Path: r.URL.Path, Authorization: auth, Body: body})
		n := 0
		for _, record := range u.records {
			if record.ID == id && record.Path == r.URL.Path {
				n++
			}
		}
		u.mu.Unlock()
		if r.URL.Path == "/api/models" {
			if u.onModel != nil && u.onModel(id, w, r) {
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"test-model","name":"Display name"}]}`)
			return
		}
		if u.onChat != nil && u.onChat(id, n, w, r, body) {
			return
		}
		_, _ = io.WriteString(w, `{"id":"chat-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *proxyTestUpstream) snapshot() (map[string]int, []proxyTestRecord) {
	u.mu.Lock()
	defer u.mu.Unlock()
	logins := make(map[string]int, len(u.logins))
	for id, n := range u.logins {
		logins[id] = n
	}
	return logins, append([]proxyTestRecord(nil), u.records...)
}

func proxyTestConfig(u *proxyTestUpstream, ids ...string) Config {
	cfg := Config{BaseURL: u.server.URL, APIKey: testProxyKey, DefaultModel: "default-model", Timeout: time.Second, Cooldown: time.Second}
	for _, id := range ids {
		cfg.Accounts = append(cfg.Accounts, AccountConfig{ID: id, Email: id + "@example.test", Password: "password-" + id})
	}
	return cfg
}

func newProxyTestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	s := httptest.NewServer(p)
	t.Cleanup(s.Close)
	return s
}

type proxyTestResponse struct {
	status int
	header http.Header
	body   string
}

func proxyTestRequest(t *testing.T, s *httptest.Server, method, path, body, key, account string) proxyTestResponse {
	t.Helper()
	r, err := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if account != "" {
		r.Header.Set("X-Account-ID", account)
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read proxy response: %v", err)
	}
	return proxyTestResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: string(data)}
}

func requireProxyStatus(t *testing.T, response proxyTestResponse, want int) {
	t.Helper()
	if response.status != want {
		t.Fatalf("status = %d, want %d; body = %s", response.status, want, response.body)
	}
}

func TestProxyRoundRobinAndTokenIsolation(t *testing.T) {
	for _, ids := range [][]string{{"alpha", "beta"}, {"alpha", "beta", "gamma"}} {
		t.Run(strings.Join(ids, "-"), func(t *testing.T) {
			u := newProxyTestUpstream(t)
			s := newProxyTestServer(t, proxyTestConfig(u, ids...))
			for n := 0; n < len(ids)*3; n++ {
				resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, "")
				requireProxyStatus(t, resp, http.StatusOK)
				if got := resp.header.Get("X-Account-ID"); got != ids[n%len(ids)] {
					t.Fatalf("request %d chose %q, want %q", n, got, ids[n%len(ids)])
				}
			}
			logins, records := u.snapshot()
			for _, id := range ids {
				if logins[id] != 1 {
					t.Errorf("%s logins = %d, want 1", id, logins[id])
				}
			}
			for n, record := range records {
				want := ids[n%len(ids)]
				if record.ID != want || record.Authorization != "Bearer "+want+"-token-1" {
					t.Errorf("request %d mixed account credentials: %+v", n, record)
				}
			}
		})
	}
}

func TestProxyPinnedAndUnknownAccount(t *testing.T) {
	u := newProxyTestUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
	for n := 0; n < 3; n++ {
		resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, "beta")
		requireProxyStatus(t, resp, http.StatusOK)
		if resp.header.Get("X-Account-ID") != "beta" {
			t.Errorf("pinned response selected %q", resp.header.Get("X-Account-ID"))
		}
	}
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, "missing")
	requireProxyStatus(t, resp, http.StatusBadRequest)
	logins, records := u.snapshot()
	if logins["alpha"] != 0 || logins["beta"] != 1 || len(records) != 3 {
		t.Errorf("unexpected account usage: logins=%v records=%v", logins, records)
	}
}

func TestProxyFailoverPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		loginFails bool
		status     int
		pinned     bool
		want       int
		wantIDs    []string
	}{
		{name: "login failure failover", loginFails: true, want: 200, wantIDs: []string{"beta"}},
		{name: "upstream 500 failover", status: 500, want: 200, wantIDs: []string{"alpha", "beta"}},
		{name: "upstream 503 failover", status: 503, want: 200, wantIDs: []string{"alpha", "beta"}},
		{name: "pinned login failure", loginFails: true, pinned: true, want: 502},
		{name: "pinned 500", status: 500, pinned: true, want: 500, wantIDs: []string{"alpha"}},
		{name: "403 no retry", status: 403, want: 403, wantIDs: []string{"alpha"}},
		{name: "429 no cross retry", status: 429, want: 429, wantIDs: []string{"alpha"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newProxyTestUpstream(t)
			u.onLogin = func(id string, _ int, w http.ResponseWriter, _ *http.Request) bool {
				if id == "alpha" && tc.loginFails {
					w.WriteHeader(http.StatusUnauthorized)
					return true
				}
				return false
			}
			u.onChat = func(id string, _ int, w http.ResponseWriter, _ *http.Request, _ map[string]any) bool {
				if id == "alpha" && tc.status != 0 {
					w.Header().Set("Retry-After", "3")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":"upstream private diagnostic"}`)
					return true
				}
				return false
			}
			s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
			pin := ""
			if tc.pinned {
				pin = "alpha"
			}
			resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, pin)
			requireProxyStatus(t, resp, tc.want)
			logins, records := u.snapshot()
			var got []string
			for _, record := range records {
				got = append(got, record.ID)
			}
			if !reflect.DeepEqual(got, tc.wantIDs) {
				t.Errorf("upstream accounts = %v, want %v", got, tc.wantIDs)
			}
			if tc.want != 200 && logins["beta"] != 0 {
				t.Errorf("error unexpectedly logged into beta: %v", logins)
			}
			if tc.want == 200 && resp.header.Get("X-Account-ID") != "beta" {
				t.Errorf("successful failover header = %q", resp.header.Get("X-Account-ID"))
			}
		})
	}
}

func TestProxyCooldownAndRetryAfter(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.onChat = func(id string, _ int, w http.ResponseWriter, _ *http.Request, _ map[string]any) bool {
		if id == "alpha" {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	}
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusTooManyRequests)
	if resp.header.Get("X-Account-ID") != "alpha" {
		t.Errorf("429 account header = %q", resp.header.Get("X-Account-ID"))
	}
	for n := 0; n < 2; n++ {
		resp = proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, "")
		requireProxyStatus(t, resp, http.StatusOK)
		if resp.header.Get("X-Account-ID") != "beta" {
			t.Errorf("cooling account was selected: %q", resp.header.Get("X-Account-ID"))
		}
	}
	// Retry-After exceeds Config.Cooldown: alpha must remain cooling after one second.
	time.Sleep(1100 * time.Millisecond)
	resp = proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, "alpha")
	requireProxyStatus(t, resp, http.StatusTooManyRequests)
	_, records := u.snapshot()
	if len(records) != 3 {
		t.Errorf("cooldown request reached upstream; records=%v", records)
	}
}

func TestProxyReauthentication(t *testing.T) {
	for _, alwaysUnauthorized := range []bool{false, true} {
		t.Run(fmt.Sprintf("always401=%t", alwaysUnauthorized), func(t *testing.T) {
			u := newProxyTestUpstream(t)
			u.onChat = func(_ string, n int, w http.ResponseWriter, _ *http.Request, _ map[string]any) bool {
				if n == 1 || alwaysUnauthorized {
					w.WriteHeader(http.StatusUnauthorized)
					return true
				}
				return false
			}
			s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
			resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, "")
			want := http.StatusOK
			if alwaysUnauthorized {
				want = http.StatusUnauthorized
			}
			requireProxyStatus(t, resp, want)
			logins, records := u.snapshot()
			if logins["alpha"] != 2 || logins["beta"] != 0 || len(records) != 2 {
				t.Fatalf("reauthentication not limited to same account once: logins=%v records=%v", logins, records)
			}
			if records[0].Authorization != "Bearer alpha-token-1" || records[1].Authorization != "Bearer alpha-token-2" {
				t.Errorf("reauthentication did not replace token: %v", records)
			}
		})
	}
}

func TestProxyPublicHealthAndAuthentication(t *testing.T) {
	u := newProxyTestUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodGet, "/healthz", "", "", ""), http.StatusOK)
	for _, path := range []string{"/v1/accounts", "/v1/models", "/v1/chat/completions"} {
		for _, key := range []string{"", "wrong", strings.Repeat("x", len(testProxyKey))} {
			method, body := http.MethodGet, ""
			if path == "/v1/chat/completions" {
				method, body = http.MethodPost, testChatBody
			}
			requireProxyStatus(t, proxyTestRequest(t, s, method, path, body, key, ""), http.StatusUnauthorized)
		}
	}
	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/accounts", "", testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusOK)
	if !json.Valid([]byte(resp.body)) {
		t.Fatalf("account listing is not JSON: %s", resp.body)
	}
	for _, id := range []string{"alpha", "beta"} {
		if !strings.Contains(resp.body, `"`+id+`"`) {
			t.Errorf("account list omitted %q: %s", id, resp.body)
		}
	}
	for _, secret := range []string{"@example.test", "password-alpha", "password-beta", testProxyKey, "-token-"} {
		if strings.Contains(resp.body, secret) {
			t.Errorf("account listing exposed secret %q", secret)
		}
	}
	if !strings.Contains(strings.ToLower(resp.body), "cooldown") {
		t.Errorf("account listing omitted cooldown information: %s", resp.body)
	}
	logins, records := u.snapshot()
	if len(logins) != 0 || len(records) != 0 {
		t.Errorf("health/auth/accounts unexpectedly contacted upstream: %v %v", logins, records)
	}
}

func TestProxyModelsNormalizedFromOneAccount(t *testing.T) {
	for _, upstreamBody := range []string{`[{"id":"model-one","name":"One"}]`, `{"data":[{"id":"model-one","created":123,"owned_by":"owner"}]}`} {
		t.Run(upstreamBody, func(t *testing.T) {
			u := newProxyTestUpstream(t)
			u.onModel = func(_ string, w http.ResponseWriter, _ *http.Request) bool {
				_, _ = io.WriteString(w, upstreamBody)
				return true
			}
			s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
			resp := proxyTestRequest(t, s, http.MethodGet, "/v1/models", "", testProxyKey, "")
			requireProxyStatus(t, resp, http.StatusOK)
			var body struct {
				Object string `json:"object"`
				Data   []struct {
					ID      string `json:"id"`
					Object  string `json:"object"`
					Created int64  `json:"created"`
					OwnedBy string `json:"owned_by"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(resp.body), &body); err != nil {
				t.Fatalf("decode models: %v", err)
			}
			if body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != "model-one" || body.Data[0].Object != "model" || body.Data[0].OwnedBy == "" {
				t.Errorf("models not normalized: %s", resp.body)
			}
			logins, records := u.snapshot()
			if len(logins) != 1 || len(records) != 1 || records[0].Path != "/api/models" {
				t.Errorf("models queried multiple accounts: %v %v", logins, records)
			}
			if len(records) == 1 && resp.header.Get("X-Account-ID") != records[0].ID {
				t.Errorf("models selected account header mismatch")
			}
		})
	}
}

func TestProxyJSONOptionsAndDefaultModel(t *testing.T) {
	u := newProxyTestUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	input := `{"messages":[{"role":"user","content":"hello"}],"temperature":0.25,"max_tokens":17,"stream":false,"tools":[{"type":"function","function":{"name":"test"}}],"tool_choice":"auto","stop":["END"],"seed":42,"response_format":{"type":"json_object"}}`
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", input, testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusOK)
	var expected map[string]any
	_ = json.Unmarshal([]byte(input), &expected)
	expected["model"] = "default-model"
	_, records := u.snapshot()
	if len(records) != 1 || !reflect.DeepEqual(records[0].Body, expected) {
		t.Errorf("forwarded JSON changed options: records=%v want=%v", records, expected)
	}
	if !strings.Contains(resp.body, `"chat.completion"`) {
		t.Errorf("upstream JSON response not forwarded: %s", resp.body)
	}
}

func TestProxySSE(t *testing.T) {
	const events = "data: {\"id\":\"chat-stream\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"
	u := newProxyTestUpstream(t)
	u.onChat = func(_ string, _ int, w http.ResponseWriter, _ *http.Request, body map[string]any) bool {
		if body["stream"] != true {
			t.Errorf("stream option not forwarded: %v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, events)
		w.(http.Flusher).Flush()
		return true
	}
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", `{"model":"test-model","messages":[],"stream":true}`, testProxyKey, "")
	requireProxyStatus(t, resp, http.StatusOK)
	if !strings.HasPrefix(resp.header.Get("Content-Type"), "text/event-stream") || resp.body != events {
		t.Errorf("SSE changed: content-type=%q body=%q", resp.header.Get("Content-Type"), resp.body)
	}
	if resp.header.Get("X-Account-ID") != "alpha" {
		t.Errorf("SSE omitted selected account")
	}
}

func TestProxyInvalidBodies(t *testing.T) {
	u := newProxyTestUpstream(t)
	cfg := proxyTestConfig(u, "alpha")
	cfg.DefaultModel = ""
	s := newProxyTestServer(t, cfg)
	for _, body := range []string{"", "{", "null", "[]", `"text"`, `{"model":"m"}`, `{"model":"m","messages":null}`, `{"model":"m","messages":{}}`, `{"model":"m","messages":"text"}`, `{"model":12,"messages":[]}`, `{"model":" ","messages":[]}`, `{"messages":[]}`} {
		t.Run(body, func(t *testing.T) {
			requireProxyStatus(t, proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", body, testProxyKey, ""), http.StatusBadRequest)
		})
	}
	body := `{"model":"m","messages":[],"padding":"` + strings.Repeat("x", 10*1024*1024) + `"}`
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", body, testProxyKey, ""), http.StatusRequestEntityTooLarge)
	logins, records := u.snapshot()
	if len(logins) != 0 || len(records) != 0 {
		t.Errorf("invalid request contacted upstream: %v %v", logins, records)
	}
}

func TestProxyTimeout(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.onChat = func(_ string, _ int, _ http.ResponseWriter, r *http.Request, _ map[string]any) bool {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
		return true
	}
	cfg := proxyTestConfig(u, "alpha", "beta")
	cfg.Timeout = 40 * time.Millisecond
	s := newProxyTestServer(t, cfg)
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, ""), http.StatusGatewayTimeout)
	// The proxy must never re-send generation after a timeout. The upstream may
	// record the call slightly after the client timeout fires, so poll briefly
	// instead of asserting an exact instantaneous count.
	deadline := time.Now().Add(2 * time.Second)
	var records []proxyTestRecord
	for time.Now().Before(deadline) {
		_, records = u.snapshot()
		if len(records) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(records) != 1 || records[0].ID != "alpha" {
		t.Errorf("timeout retried generation: %v", records)
	}
}

func TestProxyChatNetworkFailureDoesNotFailover(t *testing.T) {
	u := newProxyTestUpstream(t)
	u.onChat = func(_ string, _ int, w http.ResponseWriter, _ *http.Request, _ map[string]any) bool {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack upstream connection: %v", err)
			return true
		}
		_ = conn.Close()
		return true
	}
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha", "beta"))
	requireProxyStatus(t, proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", testChatBody, testProxyKey, ""), http.StatusBadGateway)
	logins, records := u.snapshot()
	if len(records) != 1 || logins["beta"] != 0 {
		t.Errorf("network error caused duplicate generation/failover: %v %v", logins, records)
	}
}

func TestProxyConcurrentLoginCache(t *testing.T) {
	for _, ids := range [][]string{{"alpha"}, {"alpha", "beta"}} {
		t.Run(strings.Join(ids, "-"), func(t *testing.T) {
			u := newProxyTestUpstream(t)
			u.onLogin = func(_ string, _ int, _ http.ResponseWriter, _ *http.Request) bool {
				time.Sleep(30 * time.Millisecond)
				return false
			}
			s := newProxyTestServer(t, proxyTestConfig(u, ids...))
			const count = 32
			start := make(chan struct{})
			errs := make(chan error, count)
			var wg sync.WaitGroup
			for n := 0; n < count; n++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					req, err := http.NewRequest(http.MethodPost, s.URL+"/v1/chat/completions", strings.NewReader(testChatBody))
					if err != nil {
						errs <- err
						return
					}
					req.Header.Set("Authorization", "Bearer "+testProxyKey)
					req.Header.Set("Content-Type", "application/json")
					resp, err := s.Client().Do(req)
					if err != nil {
						errs <- err
						return
					}
					_, readErr := io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if readErr != nil {
						errs <- readErr
					} else if resp.StatusCode != http.StatusOK {
						errs <- fmt.Errorf("concurrent request returned %d", resp.StatusCode)
					}
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			logins, records := u.snapshot()
			for _, id := range ids {
				if logins[id] != 1 {
					t.Errorf("shared login cache: %s logged in %d times, want 1", id, logins[id])
				}
			}
			if len(records) != count {
				t.Errorf("chat count = %d, want %d", len(records), count)
			}
			for _, record := range records {
				if record.Authorization != "Bearer "+record.ID+"-token-1" {
					t.Errorf("concurrent token isolation failed: %v", record)
				}
			}
		})
	}
}

func TestProxyPassThroughToolsAndEmbeddings(t *testing.T) {
	u := newProxyTestUpstream(t)
	var seen []proxyTestRecord
	u.onChat = func(_ string, _ int, w http.ResponseWriter, _ *http.Request, _ map[string]any) bool { return false }
	u.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auths/signin" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		u.mu.Lock()
		u.records = append(u.records, proxyTestRecord{ID: "t", Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body})
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/tools/servers":
			_, _ = io.WriteString(w, `{"data":[{"id":"mcp-1","type":"mcp"}]}`)
		case r.URL.Path == "/api/v1/embeddings" || r.URL.Path == "/api/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1]}]}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	})
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))

	resp := proxyTestRequest(t, s, http.MethodGet, "/v1/tools/servers", "", testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if !strings.Contains(resp.body, "mcp-1") {
		t.Errorf("tool server list not forwarded: %s", resp.body)
	}

	embedding := `{"model":"m","input":"hello"}`
	resp = proxyTestRequest(t, s, http.MethodPost, "/v1/embeddings", embedding, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	if !strings.Contains(resp.body, "embedding") {
		t.Errorf("embeddings not forwarded: %s", resp.body)
	}

	resp = proxyTestRequest(t, s, http.MethodPost, "/v1/mcp/servers", `{"name":"x"}`, testProxyKey, "")
	requireProxyStatus(t, resp, 200)

	_ = seen
	_, records := u.snapshot()
	var paths []string
	for _, r := range records {
		paths = append(paths, r.Path)
	}
	want := []string{"/api/v1/tools/servers", "/api/embeddings", "/api/v1/mcp/servers"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("upstream paths = %v, want %v", paths, want)
	}
	for _, r := range records {
		if r.Authorization != "Bearer t" {
			t.Errorf("tool request not authenticated with account token: %q", r.Authorization)
		}
	}
}

func TestProxyPassThroughRejectsUnknownRoutes(t *testing.T) {
	u := newProxyTestUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	for _, path := range []string{"/v1/random", "/v1/tools", "/api/tools/servers"} {
		method := http.MethodPost
		if path == "/v1/tools" {
			method = http.MethodPost
		}
		requireProxyStatus(t, proxyTestRequest(t, s, method, path, "{}", testProxyKey, ""), 404)
	}
}

func TestProxyChatForwardsToolDefinitions(t *testing.T) {
	u := newProxyTestUpstream(t)
	s := newProxyTestServer(t, proxyTestConfig(u, "alpha"))
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"calc","parameters":{"type":"object"}}}],"tool_choice":"auto","features":{"web_search":true,"code_interpreter":true,"image_generation":false}}`
	resp := proxyTestRequest(t, s, http.MethodPost, "/v1/chat/completions", body, testProxyKey, "")
	requireProxyStatus(t, resp, 200)
	_, records := u.snapshot()
	if len(records) != 1 {
		t.Fatalf("expected one upstream chat call, got %d", len(records))
	}
	got := records[0].Body
	if _, ok := got["tools"]; !ok {
		t.Error("tools definition dropped")
	}
	if got["tool_choice"] != "auto" {
		t.Errorf("tool_choice dropped: %v", got["tool_choice"])
	}
	features, ok := got["features"].(map[string]any)
	if !ok || features["web_search"] != true || features["code_interpreter"] != true {
		t.Errorf("features flags dropped: %v", got["features"])
	}
}
