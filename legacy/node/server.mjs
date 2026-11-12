import http from 'node:http';
import { createHash, timingSafeEqual } from 'node:crypto';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { pathToFileURL } from 'node:url';

class ProxyError extends Error {
  constructor(status, message, type = 'api_error') {
    super(message); this.status = status; this.type = type;
  }
}
const digest = (value) => createHash('sha256').update(value).digest();
const json = (res, status, body) => {
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(body));
};

export function createProxy(config) {
  const base = new URL(config.baseUrl);
  if (!['http:', 'https:'].includes(base.protocol) || base.username || base.password || base.search || base.hash) {
    throw new Error('OPENWEBUI_URL must be an HTTP(S) URL without credentials, query or fragment');
  }
  if (!config.email || !config.password || !config.apiKey || config.apiKey.length < 24) {
    throw new Error('Set OPENWEBUI_EMAIL, OPENWEBUI_PASSWORD and PROXY_API_KEY (at least 24 characters)');
  }
  const root = base.href.replace(/\/$/, '');
  const keyHash = digest(config.apiKey);
  const timeoutMs = config.timeoutMs ?? 300000;
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) throw new Error('UPSTREAM_TIMEOUT_MS must be positive');
  let session = null;
  let loginPending = null;
  let loginBlockedUntil = 0;

  async function login() {
    if (session && session.expires > Date.now() + 30000) return session.token;
    if (loginPending) return loginPending;
    if (Date.now() < loginBlockedUntil) throw new ProxyError(503, 'Upstream login temporarily unavailable; retry shortly');
    loginPending = (async () => {
      const response = await fetch(`${root}/api/v1/auths/signin`, {
        method: 'POST', redirect: 'error',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: config.email, password: config.password }),
        signal: AbortSignal.timeout(Math.min(timeoutMs, 15000)),
      });
      if (!response.ok) {
        await response.body?.cancel();
        throw new ProxyError(502, 'Open WebUI login failed; check credentials and password-login settings');
      }
      const data = await response.json();
      if (typeof data.token !== 'string' || !data.token) throw new ProxyError(502, 'Open WebUI did not return a login token');
      session = { token: data.token, expires: data.expires_at ? Number(data.expires_at) * 1000 : Infinity };
      return session.token;
    })();
    try { return await loginPending; }
    catch (error) { loginBlockedUntil = Date.now() + 5000; throw error; }
    finally { loginPending = null; }
  }

  async function upstream(path, method, body, signal) {
    let token = await login();
    const send = () => fetch(`${root}${path}`, {
      method, redirect: 'error', signal,
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', Accept: '*/*' },
      body,
    });
    let response = await send();
    if (response.status === 401) {
      await response.body?.cancel();
      if (session?.token === token) session = null;
      token = await login();
      response = await send();
    }
    return response;
  }

  const server = http.createServer(async (req, res) => {
    const controller = new AbortController();
    const abort = () => { if (!res.writableEnded) controller.abort(); };
    res.on('close', abort);
    const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(timeoutMs)]);
    try {
      const path = new URL(req.url, 'http://proxy.local').pathname;
      if (req.method === 'GET' && path === '/healthz') return json(res, 200, { status: 'ok' });
      const match = /^Bearer (.+)$/i.exec(req.headers.authorization ?? '');
      if (!match || !timingSafeEqual(digest(match[1]), keyHash)) {
        throw new ProxyError(401, 'Invalid proxy API key', 'authentication_error');
      }
      const models = req.method === 'GET' && path === '/v1/models';
      const chat = req.method === 'POST' && path === '/v1/chat/completions';
      if (!models && !chat) throw new ProxyError(404, 'Endpoint not found', 'invalid_request_error');
      let body;
      if (chat) {
        const chunks = []; let size = 0;
        for await (const chunk of req) {
          size += chunk.length;
          if (size > 10 * 1024 * 1024) throw new ProxyError(413, 'Request exceeds 10 MiB', 'invalid_request_error');
          chunks.push(chunk);
        }
        let input;
        try { input = JSON.parse(Buffer.concat(chunks).toString('utf8')); }
        catch { throw new ProxyError(400, 'Body must be valid JSON', 'invalid_request_error'); }
        if (!input || typeof input !== 'object' || Array.isArray(input)) throw new ProxyError(400, 'Body must be a JSON object', 'invalid_request_error');
        if (!input.model && config.defaultModel) input.model = config.defaultModel;
        if (typeof input.model !== 'string' || !input.model.trim() || !Array.isArray(input.messages)) {
          throw new ProxyError(400, 'model and messages[] are required', 'invalid_request_error');
        }
        body = JSON.stringify(input);
      }
      const response = await upstream(models ? '/api/models' : '/api/chat/completions', models ? 'GET' : 'POST', body, signal);
      if (!response.ok) {
        // Never expose upstream error bodies: they may contain internal credentials or configuration.
        await response.body?.cancel();
        throw new ProxyError(response.status, `Open WebUI returned HTTP ${response.status}`);
      }
      if (models) {
        const data = await response.json();
        const items = Array.isArray(data) ? data : data.data;
        if (!Array.isArray(items)) throw new ProxyError(502, 'Unexpected model-list response');
        return json(res, 200, { object: 'list', data: items.map((m) => ({ id: m.id, object: 'model', created: m.created ?? 0, owned_by: m.owned_by ?? 'open-webui' })) });
      }
      res.writeHead(response.status, {
        'Content-Type': response.headers.get('content-type') ?? 'application/json',
        'Cache-Control': 'no-cache, no-transform', 'X-Accel-Buffering': 'no',
      });
      res.flushHeaders();
      if (response.body) await pipeline(Readable.fromWeb(response.body), res);
      else res.end();
    } catch (error) {
      if (res.destroyed) return;
      if (res.headersSent) { res.destroy(); return; }
      const timedOut = signal.aborted && !controller.signal.aborted;
      const status = error instanceof ProxyError ? error.status : timedOut ? 504 : 502;
      json(res, status, { error: {
        message: error instanceof ProxyError ? error.message : timedOut ? 'Upstream request timed out' : 'Upstream connection failed',
        type: error instanceof ProxyError ? error.type : 'api_error', code: String(status),
      } });
    } finally { res.off('close', abort); }
  });
  server.requestTimeout = 60000;
  server.headersTimeout = 30000;
  return server;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const server = createProxy({
      baseUrl: process.env.OPENWEBUI_URL,
      email: process.env.OPENWEBUI_EMAIL,
      password: process.env.OPENWEBUI_PASSWORD,
      apiKey: process.env.PROXY_API_KEY,
      defaultModel: process.env.DEFAULT_MODEL,
      timeoutMs: Number(process.env.UPSTREAM_TIMEOUT_MS || 300000),
    });
    const port = Number(process.env.PORT || 8080);
    server.listen(port, process.env.HOST || '127.0.0.1', () => console.log(`Open WebUI proxy listening on port ${port}`));
    for (const event of ['SIGINT', 'SIGTERM']) process.on(event, () => {
      server.close(() => process.exit(0));
      setTimeout(() => process.exit(0), 5000).unref();
    });
  } catch (error) {
    console.error(`Configuration error: ${error.message}`); process.exit(1);
  }
}
