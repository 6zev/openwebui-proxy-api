import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { once } from 'node:events';
import { createProxy } from '../server.mjs';

const KEY = 'test-key-with-more-than-24-characters';
async function fixture(t, options = {}) {
  const state = { logins: 0, calls: 0, bodies: [], tokens: [] };
  const upstream = http.createServer(async (req, res) => {
    let raw = ''; for await (const chunk of req) raw += chunk;
    const body = raw ? JSON.parse(raw) : null;
    res.setHeader('Content-Type', 'application/json');
    if (req.url === '/api/v1/auths/signin') {
      state.logins++;
      assert.equal(body.email, 'user@example.com');
      assert.equal(body.password, 'password');
      if (options.badLogin) { res.writeHead(403); res.end('{"secret":"hidden"}'); return; }
      res.end(JSON.stringify({ token: `token-${state.logins}`, expires_at: Math.floor(Date.now()/1000) + 3600 }));
      return;
    }
    state.calls++; state.tokens.push(req.headers.authorization);
    if (options.reauth && state.calls === 1) { res.writeHead(401); res.end('{}'); return; }
    if (options.forbidden) { res.writeHead(403); res.end('{"secret":"hidden"}'); return; }
    if (options.slow) { return; }
    if (req.url === '/api/models') { res.end('{"data":[{"id":"mock-model"}]}'); return; }
    assert.equal(req.url, '/api/chat/completions');
    state.bodies.push(body);
    if (body.stream) {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      res.write('data: {"id":"mock","choices":[{"delta":{"content":"Xin"}}]}\n\n');
      res.end('data: [DONE]\n\n');
    } else res.end(JSON.stringify({ id: 'mock', object: 'chat.completion', choices: [{ message: { role: 'assistant', content: 'Xin chào' }, finish_reason: 'stop' }] }));
  });
  upstream.listen(0, '127.0.0.1'); await once(upstream, 'listening');
  const proxy = createProxy({ baseUrl: `http://127.0.0.1:${upstream.address().port}`, email: 'user@example.com', password: 'password', apiKey: KEY, defaultModel: 'mock-model', timeoutMs: options.timeoutMs ?? 5000 });
  proxy.listen(0, '127.0.0.1'); await once(proxy, 'listening');
  t.after(async () => {
    await Promise.all([proxy, upstream].map((server) => new Promise((resolve) => { server.close(resolve); server.closeAllConnections(); })));
  });
  const url = `http://127.0.0.1:${proxy.address().port}`;
  const request = (path, body, key = KEY) => fetch(url + path, { method: body === undefined ? 'GET' : 'POST', headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
  return { state, request, url };
}

test('health does not log in; API key is mandatory', async (t) => {
  const { request, state } = await fixture(t);
  assert.equal((await request('/healthz')).status, 200);
  assert.equal((await request('/v1/models', undefined, 'wrong')).status, 401);
  assert.equal(state.logins, 0);
});

test('models normalize and cache login across concurrent requests', async (t) => {
  const { request, state } = await fixture(t);
  const responses = await Promise.all(Array.from({length: 8}, () => request('/v1/models')));
  for (const response of responses) {
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { object: 'list', data: [{ id: 'mock-model', object: 'model', created: 0, owned_by: 'open-webui' }] });
  }
  assert.equal(state.logins, 1);
});

test('chat forwards options and applies default model', async (t) => {
  const { request, state } = await fixture(t);
  const response = await request('/v1/chat/completions', { messages: [{ role: 'user', content: 'Hi' }], temperature: 0.2 });
  assert.equal(response.status, 200);
  assert.equal((await response.json()).choices[0].message.content, 'Xin chào');
  assert.equal(state.bodies[0].temperature, 0.2);
  assert.equal(state.bodies[0].model, 'mock-model');
});

test('SSE streams preserve chunks and DONE marker', async (t) => {
  const { request } = await fixture(t);
  const response = await request('/v1/chat/completions', { model: 'mock-model', messages: [], stream: true });
  assert.match(response.headers.get('content-type'), /text\/event-stream/);
  assert.equal(await response.text(), 'data: {"id":"mock","choices":[{"delta":{"content":"Xin"}}]}\n\ndata: [DONE]\n\n');
});

test('401 retries once with a new login token', async (t) => {
  const { request, state } = await fixture(t, { reauth: true });
  assert.equal((await request('/v1/models')).status, 200);
  assert.equal(state.logins, 2);
  assert.deepEqual(state.tokens, ['Bearer token-1', 'Bearer token-2']);
});

test('403 does not retry or leak upstream details', async (t) => {
  const { request, state } = await fixture(t, { forbidden: true });
  const response = await request('/v1/models');
  assert.equal(response.status, 403);
  assert.doesNotMatch(await response.text(), /hidden/);
  assert.equal(state.logins, 1);
  assert.equal(state.calls, 1);
});

test('invalid bodies return 400 and unknown endpoints return 404', async (t) => {
  const { request, url } = await fixture(t);
  for (const body of [null, [], {}, { messages: 'bad' }]) {
    assert.equal((await request('/v1/chat/completions', body)).status, 400);
  }
  const bad = await fetch(url + '/v1/chat/completions', { method: 'POST', headers: { Authorization: `Bearer ${KEY}` }, body: '{' });
  assert.equal(bad.status, 400);
  assert.equal((await request('/v1/unknown')).status, 404);
});

test('failed login is sanitized and has cooldown', async (t) => {
  const { request, state } = await fixture(t, { badLogin: true });
  const response = await request('/v1/models');
  assert.equal(response.status, 502);
  assert.doesNotMatch(await response.text(), /hidden/);
  assert.equal((await request('/v1/models')).status, 503);
  assert.equal(state.logins, 1);
});

test('upstream timeout returns 504', async (t) => {
  const { request } = await fixture(t, { slow: true, timeoutMs: 100 });
  assert.equal((await request('/v1/models')).status, 504);
});

test('short proxy keys fail at startup', () => {
  assert.throws(() => createProxy({ baseUrl: 'http://localhost', email: 'a', password: 'b', apiKey: 'short' }), /24 characters/);
});
