package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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
	// requireSameOrigin compares Origin against DOMAIN_CLIENT/DOMAIN_SERVER. A
	// request without Origin passes; forging one would guess at someone's config.
	if origin := strings.TrimSpace(p.config.LibreChatOrigin); origin != "" {
		request.Header.Set("Origin", origin)
		request.Header.Set("Referer", origin+"/")
	}
}

// libreChatTurn is one chat turn: assistant text plus any tool-call side channel.
type libreChatTurn struct {
	Text string
}

// libreChatChat runs one turn through the chat UI and returns the assistant text.
//
// LibreChat answers the POST with a streamId and delivers the turn over a separate
// SSE subscription, so this performs both halves. It never retries a login
// rejection; a normal upstream failure surfaces as an apiError.
func (p *Proxy) libreChatChat(ctx context.Context, a *account, prompt string, model string) (*libreChatTurn, error) {
	token, err := p.libreChatToken(ctx, a)
	if err != nil {
		return nil, err
	}
	streamID, err := p.libreChatStartTurn(ctx, a, token, prompt, model)
	if err != nil {
		return nil, err
	}
	return p.libreChatFollowTurn(ctx, a, streamID)
}

// libreChatStartTurn posts the turn and returns its stream id.
func (p *Proxy) libreChatStartTurn(ctx context.Context, a *account, token, prompt, model string) (string, error) {
	payload := map[string]any{
		"text":            prompt,
		"conversationId":  libreChatNewConvo,
		"parentMessageId": libreChatNoParent,
		"endpoint":        "agents",
		"endpointOption": map[string]any{
			"agent_id":         libreChatEphemeralAgent,
			"model":            model,
			"model_parameters": map[string]any{},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.config.BaseURL, "/")+"/api/agents/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	p.decorateLibreChatRequest(request)
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

// parseLibreChatStream reads LibreChat's agent SSE stream and collects the answer.
//
// Event names in the wild: "message" with payloads that carry either a text
// fragment, a final snapshot ({"final":true,"responseMessage":{...}}), or an error
// envelope ({"error":...}). The final snapshot is authoritative — it holds the
// persisted response — so its text wins over the accumulated deltas.
func parseLibreChatStream(body io.Reader) (*libreChatTurn, error) {
	turn := &libreChatTurn{}
	var text strings.Builder
	var finalText string
	decoder := &libreChatEventReader{reader: body}
	for {
		name, payload, err := decoder.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if turn.Text != "" || finalText != "" {
				break
			}
			return nil, err
		}
		var envelope struct {
			Error           string `json:"error"`
			Message         string `json:"message"`
			Text            string `json:"text"`
			Final           bool   `json:"final"`
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
		if envelope.Error != "" {
			return nil, &apiError{status: 502, message: "LibreChat stream error: " + sanitizeDetail(envelope.Error)}
		}
		if envelope.ResponseMessage != nil {
			if envelope.ResponseMessage.Content != nil && envelope.ResponseMessage.Content.Text != "" {
				finalText = envelope.ResponseMessage.Content.Text
			} else if envelope.ResponseMessage.Text != "" {
				finalText = envelope.ResponseMessage.Text
			}
		}
		_ = name // event names vary between LibreChat versions; the payload carries the meaning
		if envelope.Text != "" && envelope.Message == "" {
			text.WriteString(envelope.Text)
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
