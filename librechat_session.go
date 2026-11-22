package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// LibreChat session authentication and chat-UI transport.
//
// Why this exists: LibreChat's Agents API (/api/agents/v1/*) only reaches *agents*
// — it is not a plain model API. To talk to a model the way the browser does, the
// proxy signs in with email/password, receives a JWT plus a refresh cookie, and
// drives the chat UI's own endpoints:
//
//	POST /api/agents/chat/completions        -> {streamId, ...}  (a job is created)
//	GET  /api/agents/chat/stream/<streamId>  -> SSE events       (replay + live)
//
// Two things make this safe to automate:
//   - every request carries a browser User-Agent, because uaParser records a
//     violation for anything else and enough violations ban the IP (BAN_DURATION);
//   - a rejected login is never retried: it is recorded and the account is left
//     alone for a while, so no attempt loop can accumulate violations.
//
// A session account is text-only: LibreChat builds the run's agent from the stored
// endpoint configuration, so a tools list in the request body is stripped before
// the model is called. Rather than answer without the tool calls the client is
// waiting for, a request carrying tools is refused with 400 and the caller is told
// to use an account configured with an api_key — the Agents API route, where tools
// are translated and honoured.

const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

const (
	// libreChatSessionTTL renews a JWT slightly before its 15-minute expiry.
	libreChatSessionTTL = 12 * time.Minute
	// libreChatRejectBackoff leaves a rejected account alone. Retrying inside this
	// window only adds violations against the host's per-IP counter.
	libreChatRejectBackoff = 30 * time.Minute
	// libreChatNewConvo is LibreChat's placeholder for "start a conversation".
	libreChatNewConvo = "new"
	// libreChatEphemeralAgent runs a model without a saved agent definition.
	libreChatEphemeralAgent = "ephemeral"
	// libreChatNoParent is LibreChat's placeholder for "first message".
	libreChatNoParent = "00000000-0000-0000-0000-000000000000"
)

// libreChatTimezone matches what the browser sends. UTC keeps the value
// deterministic; LibreChat only nests the turn under a date bucket.
const libreChatTimezone = "UTC"

// newLibreChatID returns a v4-shaped identifier. LibreChat requires a string that
// looks like a message id; it does not validate the RFC 4122 variant bits.
func newLibreChatID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// libreChatEndpointName is the endpoint the model picker would have selected.
//
// The authoritative source is the model list: LibreChat's /api/models groups ids
// under provider names, and that group IS the endpoint. Guessing from the id alone
// fails on deployments with providers the guess does not know (Deepseek,
// Databricks, bedrock, assistants...), which answer endpoint_models_not_loaded.
// The provider captured with the model list is used first; the id heuristics stay
// only as a fallback for a model that never appeared in the list.
func libreChatEndpointName(model string, knownProviders map[string]string) string {
	if provider, ok := knownProviders[model]; ok && provider != "" {
		return provider
	}
	switch {
	case strings.Contains(model, "deepseek"):
		return "Deepseek"
	case strings.HasPrefix(model, "claude"), strings.Contains(model, "anthropic"):
		return "anthropic"
	case strings.Contains(model, "gemini"):
		return "google"
	case strings.Contains(model, "gpt"), strings.Contains(model, "o1"), strings.Contains(model, "o3"):
		return "openAI"
	}
	return "openAI"
}

// libreChatProviderIndex maps model id -> provider for the account about to serve.
//
// The cache may legitimately be cold or disabled (MODEL_CACHE_SECONDS=0), and a
// widget-only deployment would then guess the endpoint and fail with
// endpoint_models_not_loaded. So a cold cache triggers a real listing here: the
// answer is small, and it is the difference between working and not working.
func (p *Proxy) libreChatProviderIndex(ctx context.Context, a *account) map[string]string {
	index := map[string]string{}
	models := []cachedModel(nil)
	if p.modelCache != nil {
		if cached, _, ok := p.modelCache.peek(accountKey(a.config.WebsiteID, a.config.ID)); ok {
			models = cached
		}
	}
	if models == nil {
		if fetched, err := p.fetchModelList(ctx, a); err == nil {
			models = fetched
		}
	}
	for _, cached := range models {
		if cached.ID != "" && cached.OwnedBy != "" {
			index[cached.ID] = cached.OwnedBy
		}
	}
	return index
}

// libreChatSession is the per-account login state.
type libreChatSession struct {
	mu       sync.Mutex
	token    string
	expires  time.Time
	rejected time.Time
	reason   string
	jar      http.CookieJar
}

func (a *account) libreChatSessionFor() *libreChatSession {
	a.sessionOnce.Do(func() {
		jar, _ := cookiejar.New(nil)
		a.session = &libreChatSession{jar: jar}
	})
	return a.session
}

// blocked reports whether the account must not be contacted yet.
func (s *libreChatSession) blocked() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejected.IsZero() {
		return false, ""
	}
	if time.Since(s.rejected) < libreChatRejectBackoff {
		return true, s.reason
	}
	s.rejected = time.Time{}
	s.reason = ""
	return false, ""
}

func (s *libreChatSession) markRejected(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected = time.Now()
	s.reason = reason
	s.token = ""
	s.expires = time.Time{}
}

func (s *libreChatSession) cachedToken() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" || time.Now().After(s.expires) {
		return "", false
	}
	return s.token, true
}

func (s *libreChatSession) storeToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	s.expires = time.Now().Add(libreChatSessionTTL)
	s.rejected = time.Time{}
	s.reason = ""
}

func (s *libreChatSession) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
	s.expires = time.Time{}
}

// libreChatToken returns a JWT, reusing the cached one, then refresh, then a
// single login attempt.
func (p *Proxy) libreChatToken(ctx context.Context, a *account) (string, error) {
	session := a.libreChatSessionFor()
	if blocked, reason := session.blocked(); blocked {
		return "", &apiError{status: 502, message: fmt.Sprintf("LibreChat account %s is paused: %s", a.config.ID, reason)}
	}
	if token, ok := session.cachedToken(); ok {
		return token, nil
	}
	if token, err := p.libreChatRefresh(ctx, a, session); err == nil && token != "" {
		return token, nil
	}
	return p.libreChatLogin(ctx, a, session)
}

// libreChatRefresh exchanges the refresh cookie for a new JWT. It is silent: any
// failure falls through to a login.
func (p *Proxy) libreChatRefresh(ctx context.Context, a *account, session *libreChatSession) (string, error) {
	base, err := url.Parse(a.config.BaseURL)
	if err != nil || session.jar == nil {
		return "", errors.New("no cookie jar")
	}
	cookies := session.jar.Cookies(base)
	if len(cookies) == 0 {
		return "", errors.New("no refresh cookie")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.config.BaseURL, "/")+"/api/auth/refresh", nil)
	if err != nil {
		return "", err
	}
	p.decorateLibreChatRequest(request)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh answered %d", response.StatusCode)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Token == "" {
		return "", errors.New("refresh returned no token")
	}
	session.storeToken(payload.Token)
	p.debugf("debug: librechat session refreshed account=%s", a.config.ID)
	return payload.Token, nil
}

// libreChatLogin signs in once. A rejection is recorded, never retried.
func (p *Proxy) libreChatLogin(ctx context.Context, a *account, session *libreChatSession) (string, error) {
	body, err := json.Marshal(map[string]string{"email": a.config.Email, "password": a.config.Password})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.config.BaseURL, "/")+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	p.decorateLibreChatRequest(request)
	response, err := a.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if session.jar != nil {
		if base, perr := url.Parse(a.config.BaseURL); perr == nil {
			if cookies := response.Cookies(); len(cookies) > 0 {
				session.jar.SetCookies(base, cookies)
			}
		}
	}
	if response.StatusCode == http.StatusForbidden {
		// "temporarily banned due to violations of our service". Each retry is
		// another violation, so the account is parked instead.
		detail := sanitizeDetail(upstreamDetail(raw))
		if detail == "" {
			detail = "login refused (403)"
		}
		session.markRejected(detail)
		p.debugf("debug: librechat login refused account=%s status=403 detail=%q — paused, no retry", a.config.ID, detail)
		return "", &apiError{status: 403, message: "LibreChat rejected the login: " + detail}
	}
	if response.StatusCode != http.StatusOK {
		detail := sanitizeDetail(upstreamDetail(raw))
		if detail == "" {
			detail = fmt.Sprintf("login answered %d", response.StatusCode)
		}
		session.markRejected(detail)
		return "", &apiError{status: response.StatusCode, message: "LibreChat login failed: " + detail}
	}
	var payload struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Token == "" {
		session.markRejected("login returned no token")
		return "", errors.New("LibreChat login returned no token")
	}
	session.storeToken(payload.Token)
	p.debugf("debug: librechat login ok account=%s website=%s", a.config.ID, a.config.WebsiteID)
	return payload.Token, nil
}

// decorateLibreChatRequest sets the headers a browser would send. The User-Agent
// is mandatory: without it LibreChat records a violation.
func (p *Proxy) decorateLibreChatRequest(request *http.Request) {
	request.Header.Set("User-Agent", browserUserAgent)
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// requireSameOrigin compares Origin against DOMAIN_CLIENT/DOMAIN_SERVER. The
	// account's own base URL is the origin a browser would present, so it is derived
	// automatically; LIBRECHAT_ORIGIN overrides it when the UI is served from a
	// different host than the API.
	origin := p.originFor(request.URL)
	if origin != "" {
		request.Header.Set("Origin", origin)
		request.Header.Set("Referer", origin+"/")
	}
}

// originFor returns the Origin a browser would send for this URL.
//
// "Làm giả request đến từ chính web ui" is exactly what the server checks: Origin
// must match the deployment's own client URL. The base URL is that value in the
// common case (API and UI on one host), so no configuration is needed; an explicit
// override exists for split deployments.
func (p *Proxy) originFor(target *url.URL) string {
	if override := strings.TrimSpace(p.config.LibreChatOrigin); override != "" {
		return strings.TrimRight(override, "/")
	}
	if target == nil {
		return ""
	}
	origin := &url.URL{Scheme: target.Scheme, Host: target.Host}
	return origin.String()
}

// libreChatTurn is one chat turn: assistant text plus any tool-call side channel.
type libreChatTurn struct {
	Text             string
	PromptTokens     int
	CompletionTokens int
}

// libreChatChat runs one turn through the chat UI and returns the assistant text.
//
// LibreChat answers the POST with a streamId and delivers the turn over a separate
// SSE subscription, so this performs both halves. It never retries a login
// rejection; a normal upstream failure surfaces as an apiError.
func (p *Proxy) libreChatChat(ctx context.Context, a *account, prompt string, model string, params map[string]any) (*libreChatTurn, error) {
	token, err := p.libreChatToken(ctx, a)
	if err != nil {
		return nil, err
	}
	streamID, err := p.libreChatStartTurn(ctx, a, token, prompt, model, params)
	if err != nil {
		return nil, err
	}
	return p.libreChatFollowTurn(ctx, a, streamID)
}

// libreChatStartTurn posts the turn and returns its stream id.
func (p *Proxy) libreChatStartTurn(ctx context.Context, a *account, token, prompt, model string, params map[string]any) (string, error) {
	// Shape captured from the real web UI (DevTools):
	//   POST /api/agents/chat/<endpoint>   with endpointType "custom"
	//   {text, sender, clientTimestamp, isCreatedByUser, parentMessageId,
	//    messageId, error, endpoint, endpointType, model, key, modelDisplayLabel,
	//    isTemporary, isRegenerate, isContinued, timezone, clientRequestId}
	// This is NOT the agents endpoint: there is no agent_id and no
	// endpointOption. The endpoint name comes from the caller ("Deepseek",
	// "openAI", ...); "custom" tells LibreChat the model is a provider slug.
	now := time.Now().UTC().Format("2006-01-02T15:04:05")
	endpointName := libreChatEndpointName(model, p.libreChatProviderIndex(ctx, a))
	payload := map[string]any{
		"text":              prompt,
		"sender":            "User",
		"clientTimestamp":   now,
		"isCreatedByUser":   true,
		"parentMessageId":   libreChatNoParent,
		"messageId":         newLibreChatID(),
		"error":             false,
		"endpoint":          endpointName,
		"endpointType":      "custom",
		"model":             model,
		"key":               "never",
		"modelDisplayLabel": endpointName,
		"isTemporary":       false,
		"isRegenerate":      false,
		"isContinued":       false,
		"timezone":          libreChatTimezone,
		"clientRequestId":   newLibreChatID(),
	}
	// Sampling parameters travel in endpointOption.model_parameters: LibreChat's
	// agent build turns that object into the agent's llmConfig, and its modelKwargs
	// are handed to the provider SDK unchanged. Omitting this block means the caller's
	// temperature / max_tokens / top_p are silently ignored — the request would still
	// succeed, just not as asked.
	if len(params) > 0 {
		payload["endpointOption"] = map[string]any{
			"model_parameters": params,
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	// The web UI posts to /api/agents/chat/<endpoint>, not the collection route.
	url := strings.TrimRight(a.config.BaseURL, "/") + "/api/agents/chat/" + url.PathEscape(endpointName)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	p.decorateLibreChatRequest(request)
	p.debugf("debug: librechat chat request account=%s path=%s sent=%s", a.config.ID, request.URL.Path, debugBody(body))
	response, err := a.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		detail := sanitizeDetail(upstreamDetail(raw))
		if detail == "" {
			detail = fmt.Sprintf("chat answered %d", response.StatusCode)
		}
		p.debugf("debug: librechat chat rejected account=%s status=%d body=%s", a.config.ID, response.StatusCode, debugBody(raw))
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			a.libreChatSessionFor().invalidate()
		}
		return "", &apiError{status: response.StatusCode, message: "LibreChat chat failed: " + detail}
	}
	var started struct {
		StreamID string `json:"streamId"`
	}
	if json.Unmarshal(raw, &started) != nil || started.StreamID == "" {
		return "", errors.New("LibreChat chat returned no streamId")
	}
	return started.StreamID, nil
}

// libreChatFollowTurn subscribes to the turn's SSE stream and collects the text.
func (p *Proxy) libreChatFollowTurn(ctx context.Context, a *account, streamID string) (*libreChatTurn, error) {
	token, ok := a.libreChatSessionFor().cachedToken()
	if !ok {
		return nil, errors.New("LibreChat session expired before the stream was read")
	}
	endpoint := strings.TrimRight(a.config.BaseURL, "/") + "/api/agents/chat/stream/" + url.PathEscape(streamID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/event-stream")
	p.decorateLibreChatRequest(request)
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return nil, &apiError{status: response.StatusCode, message: "LibreChat stream failed: " + sanitizeDetail(upstreamDetail(raw))}
	}
	return parseLibreChatStream(response.Body)
}

// debugLibreChatStream dumps raw stream payloads when troubleshooting a shape
// change; it is off unless the environment asks for it.
var (
	debugLibreChatStream           = os.Getenv("LIBRECHAT_STREAM_DEBUG") == "1"
	debugStreamSink      io.Writer = os.Stderr
)

// parseLibreChatStream reads LibreChat's agent SSE stream and collects the answer.
//
// The SSE event NAME is always "message"; the meaning lives in the JSON payload's
// own "event" field (on_message_delta, on_token_usage, on_run_step...). Only the
// payload matters here, and the final snapshot wins over accumulated deltas because
// it holds what LibreChat actually persisted.
func parseLibreChatStream(body io.Reader) (*libreChatTurn, error) {
	turn := &libreChatTurn{}
	var text strings.Builder
	var finalText string
	decoder := &libreChatEventReader{reader: body}
	for {
		_, payload, err := decoder.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if turn.Text != "" || finalText != "" {
				break
			}
			return nil, err
		}
		// Stream shape (captured from the live UI):
		//   {"event":"on_message_delta","data":{"delta":{"content":[{"type":"text","text":"P"}]}}}
		//   {"event":"on_token_usage","data":{"input_tokens":..,"output_tokens":..}}
		//   {"final":true,"conversation":{...},"responseMessage":{...}}
		// The first frame is {"created":true,"message":{...},"streamId":"..."} — the
		// echo of the user's own message, which must NOT be counted as the answer.
		var envelope struct {
			Event           string          `json:"event"`
			Created         bool            `json:"created"`
			Final           bool            `json:"final"`
			Error           json.RawMessage `json:"error"`
			Data            json.RawMessage `json:"data"`
			ResponseMessage *struct {
				Content *struct {
					Text string `json:"text"`
				} `json:"content"`
				Text string `json:"text"`
			} `json:"responseMessage"`
		}
		if json.Unmarshal([]byte(payload), &envelope) != nil {
			continue
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "false" && string(envelope.Error) != "null" {
			detail := detailText(envelope.Error)
			if detail != "" {
				// A stream-level refusal still names its cause; carry the flag so the
				// caller can stop advertising this model.
				return nil, &apiError{status: 502, message: "LibreChat stream error: " + sanitizeDetail(detail), providerDead: isProviderUnavailable([]byte(detail))}
			}
		}
		switch envelope.Event {
		case "on_message_delta":
			var delta struct {
				Delta struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"delta"`
			}
			if json.Unmarshal(envelope.Data, &delta) == nil {
				for _, part := range delta.Delta.Content {
					if part.Type == "text" || part.Type == "" {
						text.WriteString(part.Text)
					}
				}
			}
		case "on_token_usage":
			// Token counts are forwarded to the caller when present.
			var usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}
			if json.Unmarshal(envelope.Data, &usage) == nil {
				turn.PromptTokens = usage.InputTokens
				turn.CompletionTokens = usage.OutputTokens
			}
		}
		// The persisted snapshot is authoritative when the stream was cut short.
		if envelope.ResponseMessage != nil {
			if envelope.ResponseMessage.Content != nil && envelope.ResponseMessage.Content.Text != "" {
				finalText = envelope.ResponseMessage.Content.Text
			} else if envelope.ResponseMessage.Text != "" {
				finalText = envelope.ResponseMessage.Text
			}
		}
	}
	if finalText != "" {
		turn.Text = finalText
	} else {
		turn.Text = text.String()
	}
	return turn, nil
}

// libreChatEventReader splits an SSE stream into (event, data) pairs.
type libreChatEventReader struct {
	reader io.Reader
	buffer []byte
}

// Next returns the next event name and its data payload.
func (r *libreChatEventReader) Next() (string, string, error) {
	var name string
	var data strings.Builder
	for {
		line, err := r.readLine()
		if err != nil {
			if data.Len() > 0 {
				return name, data.String(), nil
			}
			return "", "", err
		}
		if line == "" {
			if data.Len() > 0 || name != "" {
				return name, data.String(), nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}

func (r *libreChatEventReader) readLine() (string, error) {
	for {
		if index := bytes.IndexByte(r.buffer, '\n'); index >= 0 {
			line := string(r.buffer[:index])
			r.buffer = r.buffer[index+1:]
			return strings.TrimSuffix(line, "\r"), nil
		}
		chunk := make([]byte, 4096)
		read, err := r.reader.Read(chunk)
		if read > 0 {
			r.buffer = append(r.buffer, chunk[:read]...)
			continue
		}
		if err != nil {
			if len(r.buffer) > 0 {
				line := string(r.buffer)
				r.buffer = nil
				return line, nil
			}
			return "", err
		}
	}
}
