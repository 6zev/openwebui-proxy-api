'use strict';

(() => {
  const $ = (id) => document.getElementById(id);
  const pages = ['dashboard', 'websites', 'accounts', 'playground'];
  let key = '';
  let state = { websites: [], accounts: [] };
  let session = 0;
  let refreshSequence = 0;
  let editor = null;
  let deletion = null;
  let chatController = null;
  let modelController = null;
  let chatRun = 0;
  let modelRun = 0;
  let chatPending = false;
  let modelsPending = false;
  const testing = new Set();
  const requests = new Set();

  class APIError extends Error {
    constructor(message, status) {
      super(message);
      this.status = status;
    }
  }

  function sanitized(value) {
    let text = typeof value === 'string' ? value : 'Yêu cầu thất bại. Vui lòng thử lại.';
    if (key) text = text.split(key).join('[đã ẩn]');
    text = text.replace(/\b(?:https?|socks[45]h?):\/\/[^\s/@]+(?::[^\s/@]*)?@/gi, '[proxy đã ẩn]@');
    text = text.replace(/\bBearer\s+[^\s,;]+/gi, 'Bearer [đã ẩn]');
    text = text.replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g, '');
    return text.slice(0, 700);
  }

  function errorMessage(payload, status) {
    const message = payload && typeof payload === 'object'
      ? (payload.error && typeof payload.error === 'object' ? payload.error.message : payload.error) || payload.message
      : '';
    return sanitized(typeof message === 'string' ? message : `Yêu cầu thất bại (HTTP ${status}).`);
  }

  function showError(id, message) {
    $(id).textContent = sanitized(message);
    $(id).hidden = false;
  }

  function notice(message, error = false) {
    $('global-notice').textContent = sanitized(message);
    $('global-notice').classList.toggle('is-error', error);
    $('global-notice').hidden = false;
  }

  async function api(path, options = {}) {
    if (!key) throw new APIError('Vui lòng đăng nhập để tiếp tục.', 401);
    const currentSession = session;
    const controller = new AbortController();
    requests.add(controller);
    const externalSignal = options.signal;
    const abort = () => controller.abort();
    if (externalSignal) {
      if (externalSignal.aborted) controller.abort();
      else externalSignal.addEventListener('abort', abort, { once: true });
    }
    const headers = new Headers(options.headers || {});
    headers.set('Authorization', `Bearer ${key}`);
    if (options.body !== undefined) headers.set('Content-Type', 'application/json');
    try {
      const response = await fetch(path, { ...options, headers, signal: controller.signal, credentials: 'omit', cache: 'no-store', redirect: 'error' });
      if (currentSession !== session) throw new DOMException('Session ended', 'AbortError');
      if (response.status === 401) {
        logout();
        showError('login-error', 'Phiên đăng nhập đã hết hạn hoặc khóa API không đúng.');
        throw new APIError('Khóa API không đúng hoặc phiên đăng nhập đã hết hạn.', 401);
      }
      if (!response.ok) {
        let payload;
        try { payload = await response.json(); } catch { payload = null; }
        throw new APIError(errorMessage(payload, response.status), response.status);
      }
      if (options.raw) return response;
      let payload;
      try { payload = await response.json(); } catch { throw new APIError('Máy chủ trả về dữ liệu không hợp lệ.', response.status); }
      if (payload && payload.error) throw new APIError(errorMessage(payload, response.status), response.status);
      return payload;
    } catch (error) {
      if (error.name === 'AbortError' || error instanceof APIError) throw error;
      throw new APIError('Kết nối gián đoạn. Kiểm tra máy chủ và thử lại.', 0);
    } finally {
      requests.delete(controller);
      if (externalSignal) externalSignal.removeEventListener('abort', abort);
    }
  }

  function node(tag, className, text) {
    const el = document.createElement(tag);
    if (className) el.className = className;
    if (text !== undefined) el.textContent = String(text);
    return el;
  }

  function badge(text, kind = 'off') { return node('span', `badge ${kind}`, text); }
  function cell(...children) { const el = node('td'); el.append(...children); return el; }
  function websiteName(id) { return state.websites.find((site) => site.id === id)?.name || (id || '(legacy)'); }
  function option(value, text) { const el = node('option', '', text); el.value = value; return el; }
  function action(text, name, id, danger = false) {
    const el = node('button', `button ${danger ? 'danger' : 'secondary'}`, text);
    el.type = 'button';
    el.dataset.action = name;
    el.dataset.id = id;
    return el;
  }

  function status(account) {
    if (!account.enabled) return badge('Đã tắt');
    if (Number(account.cooldown_seconds) > 0) return badge(`Chờ ${Math.ceil(account.cooldown_seconds)} giây`, 'warn');
    return account.available ? badge('Sẵn sàng', 'good') : badge('Chưa sẵn sàng', 'warn');
  }

  function populate(select, items, initial, previous = select.value) {
    const options = initial ? [option(initial.value, initial.text)] : [];
    for (const item of items) options.push(option(item.id, item.name || item.id));
    select.replaceChildren(...options);
    if (options.some((item) => item.value === previous)) select.value = previous;
  }

  function navigate(page) {
    const current = pages.includes(page) ? page : 'dashboard';
    for (const name of pages) $('page-' + name).hidden = name !== current;
    for (const button of document.querySelectorAll('[data-page]')) {
      const active = button.dataset.page === current;
      button.classList.toggle('active', active);
      if (active) button.setAttribute('aria-current', 'page');
      else button.removeAttribute('aria-current');
    }
    $('page-title').textContent = current.charAt(0).toUpperCase() + current.slice(1);
  }

  function renderDashboard() {
    const activeSites = state.websites.filter((site) => site.enabled).length;
    const ready = state.accounts.filter((account) => account.enabled && account.available && !(Number(account.cooldown_seconds) > 0)).length;
    const cooling = state.accounts.filter((account) => account.enabled && Number(account.cooldown_seconds) > 0).length;
    const stats = [
      ['Tổng website', state.websites.length, `${activeSites} website đang bật`, '◎'],
      ['Tổng tài khoản', state.accounts.length, `${state.accounts.filter((account) => account.enabled).length} tài khoản đang bật`, '♙'],
      ['Tài khoản sẵn sàng', ready, 'Sẵn sàng nhận yêu cầu', '↗'],
      ['Đang nghỉ', cooling, 'Tự động phục hồi sau thời gian chờ', '◷']
    ];
    $('stats-grid').replaceChildren(...stats.map(([title, value, caption, icon]) => {
      const card = node('article', 'stat-card');
      const top = node('div', 'stat-top');
      top.append(node('span', '', title), node('span', 'stat-icon', icon));
      card.append(top, node('div', 'stat-value', value), node('div', 'stat-caption', caption));
      return card;
    }));
    const overview = $('dashboard-sites');
    overview.replaceChildren();
    if (!state.websites.length) {
      const empty = node('div', 'empty-state');
      empty.append(node('span', 'empty-icon', '◎'), node('h3', '', 'Không gian mới đang chờ bạn'), node('p', '', 'Thêm website đầu tiên trong mục Websites.'));
      overview.append(empty);
    }
    for (const site of state.websites.slice(0, 5)) {
      const row = node('div', 'site-row');
      const info = node('div', 'site-detail');
      const count = state.accounts.filter((account) => account.website_id === site.id).length;
      info.append(node('strong', '', site.name || site.id), node('small', '', `${site.id} · ${count} tài khoản`));
      row.append(node('span', 'site-avatar', (site.name || site.id || 'W').charAt(0).toUpperCase()), info, badge(site.enabled ? 'Đang bật' : 'Đã tắt', site.enabled ? 'good' : 'off'));
      overview.append(row);
    }
    $('api-base').value = location.origin + '/v1';
    $('persistence-note').textContent = state.persistent ? 'Cấu hình được lưu bền vững.' : 'Cấu hình đang chạy trong bộ nhớ; có thể mất khi khởi động lại.';
  }

  function renderWebsites() {
    const query = $('website-search').value.toLowerCase();
    const sites = state.websites.filter((site) => [site.id, site.name, site.base_url].some((value) => String(value || '').toLowerCase().includes(query)));
    $('website-rows').replaceChildren(...sites.map((site) => {
      const row = node('tr');
      const name = cell(node('strong', '', site.name || site.id), node('small', '', site.id));
      const controls = node('div', 'row-actions');
      controls.append(action('Sửa', 'edit-website', site.id), action('Xóa', 'delete-website', site.id, true));
      const count = state.accounts.filter((account) => account.website_id === site.id).length;
      row.append(name, cell(node('span', '', site.base_url || '—')), cell(node('span', '', count)), cell(badge(site.enabled ? 'Đang bật' : 'Đã tắt', site.enabled ? 'good' : 'off')), cell(node('span', 'muted', site.proxy_configured ? 'Đã cấu hình' : 'Mặc định')), cell(controls));
      return row;
    }));
    $('website-total').textContent = `${sites.length} / ${state.websites.length} website`;
    $('website-empty').hidden = sites.length !== 0;
    $('website-empty').querySelector('h3').textContent = state.websites.length ? 'Không tìm thấy website phù hợp' : 'Chưa có website nào';
  }

  function renderAccounts() {
    const query = $('account-search').value.toLowerCase();
    const filter = $('account-filter').value;
    const accounts = state.accounts.filter((account) => (!filter || account.website_id === filter || (filter === '__legacy__' && !account.website_id)) && [account.id, account.name, account.email].some((value) => String(value || '').toLowerCase().includes(query)));
    $('account-rows').replaceChildren(...accounts.map((account) => {
      const row = node('tr');
      const controls = node('div', 'row-actions');
      const test = action(testing.has(account.id) ? 'Đang thử…' : 'Kiểm tra', 'test-account', account.id);
      test.disabled = testing.has(account.id);
      controls.append(test, action('Sửa', 'edit-account', account.id), action('Xóa', 'delete-account', account.id, true));
      row.append(cell(node('strong', '', account.name || account.id), node('small', '', account.email || '—'), node('small', '', account.id)), cell(node('span', '', websiteName(account.website_id))), cell(status(account)), cell(node('span', '', account.has_password ? 'Mật khẩu: đã lưu' : 'Mật khẩu: chưa có'), node('small', '', account.proxy_configured ? 'Proxy: đã cấu hình' : 'Proxy: mặc định / kế thừa')), cell(controls));
      return row;
    }));
    $('account-empty').hidden = accounts.length !== 0;
    $('add-account-button').disabled = state.websites.length === 0;
    $('add-account-button').title = state.websites.length ? '' : 'Thêm một website trước khi tạo tài khoản.';
  }

  function clearModels() {
    modelRun += 1;
    if (modelController) modelController.abort();
    modelController = null;
    modelsPending = false;
    $('play-model').replaceChildren(option('', 'Tải danh sách mô hình trước'));
    syncPlayControls();
  }

  function populatePlayAccounts() {
    const id = $('play-website').value;
    populate($('play-account'), state.accounts.filter((account) => account.website_id === id), { value: '', text: 'Tự động chọn' });
  }

  function syncPlayControls() {
    const hasWebsite = Boolean($('play-website').value);
    $('load-models-button').disabled = !hasWebsite || modelsPending || chatPending;
    $('send-chat-button').disabled = !hasWebsite || !$('play-model').value || chatPending || modelsPending;
    $('stop-chat-button').disabled = !chatPending;
    $('play-website').disabled = chatPending;
    $('play-account').disabled = chatPending || !hasWebsite;
    $('play-model').disabled = chatPending || modelsPending;
  }

  function render() {
    $('nav-sites-count').textContent = state.websites.length;
    $('nav-accounts-count').textContent = state.accounts.length;
    $('revision-label').textContent = `rev ${state.revision ?? '—'}`;
    $('sidebar-version').textContent = state.version ? `Phiên bản ${state.version}` : 'WebUI Proxy';
    populate($('account-filter'), state.websites, { value: '', text: 'Tất cả website' });
    if (state.accounts.some((account) => !account.website_id)) $('account-filter').append(option('__legacy__', '(legacy)'));
    const previousSite = $('play-website').value;
    const previousAccount = $('play-account').value;
    populate($('play-website'), state.websites, state.websites.length ? null : { value: '', text: 'Chưa có website' });
    populatePlayAccounts();
    if (previousSite !== $('play-website').value || previousAccount !== $('play-account').value) clearModels();
    syncPlayControls();
    renderDashboard();
    renderWebsites();
    renderAccounts();
  }

  async function refresh() {
    const current = session;
    const sequence = ++refreshSequence;
    const result = await api('/api/admin/state');
    if (current !== session || sequence !== refreshSequence) return;
    if (!result || !Array.isArray(result.websites) || !Array.isArray(result.accounts)) throw new APIError('Dữ liệu trạng thái không hợp lệ.', 0);
    state = result;
    render();
  }

  function logout() {
    session += 1;
    chatRun += 1;
    modelRun += 1;
    if (chatController) chatController.abort();
    if (modelController) modelController.abort();
    for (const controller of requests) controller.abort();
    requests.clear();
    chatController = null;
    modelController = null;
    key = '';
    state = { websites: [], accounts: [] };
    testing.clear();
    editor = null;
    deletion = null;
    chatPending = false;
    modelsPending = false;
    $('api-key').value = '';
    $('entity-password').value = '';
    $('entity-proxy').value = '';
    $('play-prompt').value = '';
    $('chat-output').textContent = '';
    $('account-test-result').textContent = '';
    $('play-status').textContent = 'Chọn website rồi tải danh sách mô hình.';
    $('response-status').textContent = 'Sẵn sàng';
    $('response-empty').hidden = false;
    $('global-notice').hidden = true;
    $('editor-error').hidden = true;
    $('delete-error').hidden = true;
    $('login-error').hidden = true;
    $('editor-form').reset();
    for (const dialog of [$('editor-dialog'), $('delete-dialog')]) if (dialog.open) dialog.close();
    $('app-shell').hidden = true;
    $('login-screen').hidden = false;
    $('login-submit').disabled = false;
    $('refresh-button').disabled = false;
    $('save-entity-button').disabled = false;
    $('confirm-delete-button').disabled = false;
    $('play-model').replaceChildren(option('', 'Tải danh sách mô hình trước'));
    render();
    $('api-key').focus();
  }

  function closeEditor() {
    $('editor-dialog').close();
    $('entity-password').value = '';
    $('entity-proxy').value = '';
    editor = null;
  }

  function openEditor(kind, id) {
    const list = kind === 'website' ? state.websites : state.accounts;
    const entity = id ? list.find((item) => item.id === id) : null;
    if (id && !entity) return;
    if (kind === 'account' && !state.websites.length) { notice('Thêm website trước khi tạo tài khoản.', true); return; }
    editor = { kind, id: entity?.id || null };
    $('editor-form').reset();
    $('editor-error').hidden = true;
    $('editor-title').textContent = `${entity ? 'Chỉnh sửa' : 'Thêm'} ${kind === 'website' ? 'website' : 'tài khoản'}`;
    $('website-fields').hidden = kind !== 'website';
    $('account-fields').hidden = kind !== 'account';
    $('entity-url').required = kind === 'website';
    $('entity-url').disabled = kind !== 'website';
    for (const id of ['entity-website', 'entity-email', 'entity-password']) $(id).disabled = kind !== 'account';
    $('entity-website').required = kind === 'account';
    $('entity-email').required = kind === 'account';
    $('entity-password').required = kind === 'account' && !entity;
    $('password-help').textContent = entity ? 'Để trống để giữ mật khẩu hiện tại.' : 'Nhập mật khẩu đăng nhập tài khoản Open WebUI.';
    $('entity-id').value = entity?.id || '';
    $('entity-id').readOnly = Boolean(entity);
    $('entity-name').value = entity?.name || '';
    $('entity-url').value = entity?.base_url || '';
    $('entity-email').value = entity?.email || '';
    $('entity-password').value = '';
    $('entity-proxy').value = '';
    $('entity-proxy').disabled = false;
    $('entity-proxy').placeholder = entity?.proxy_configured ? '(đã cấu hình; để trống để giữ)' : 'http://user:password@host:port';
    $('clear-proxy-label').hidden = !entity;
    $('entity-enabled').checked = entity ? Boolean(entity.enabled) : true;
    populate($('entity-website'), state.websites, { value: '', text: 'Chọn một website đã đăng ký' }, entity?.website_id || '');
    $('editor-dialog').showModal();
    (entity ? $('entity-name') : $('entity-id')).focus();
  }

  async function saveEntity(event) {
    event.preventDefault();
    if (!editor || !$('editor-form').reportValidity()) return;
    const context = { ...editor };
    const current = session;
    const body = { name: $('entity-name').value.trim(), enabled: $('entity-enabled').checked };
    if (!body.name) { showError('editor-error', 'Vui lòng nhập tên hiển thị.'); return; }
    if (!context.id) body.id = $('entity-id').value;
    if (!/^[A-Za-z0-9_.-]{1,64}$/.test(context.id || body.id)) { showError('editor-error', 'ID không hợp lệ.'); return; }
    if (context.kind === 'website') {
      body.base_url = $('entity-url').value.trim();
      let url;
      try { url = new URL(body.base_url); } catch { showError('editor-error', 'Địa chỉ website không hợp lệ.'); return; }
      if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) { showError('editor-error', 'Dùng địa chỉ HTTP hoặc HTTPS không chứa thông tin đăng nhập.'); return; }
    } else {
      body.website_id = $('entity-website').value;
      if (!state.websites.some((site) => site.id === body.website_id)) { showError('editor-error', 'Chọn một website đã đăng ký.'); return; }
      body.email = $('entity-email').value.trim();
      if ($('entity-password').value) body.password = $('entity-password').value;
    }
    if (context.id && $('clear-proxy').checked) body.proxy_url = '';
    else if ($('entity-proxy').value.trim()) body.proxy_url = $('entity-proxy').value.trim();
    $('save-entity-button').disabled = true;
    $('editor-error').hidden = true;
    const collection = context.kind === 'website' ? 'websites' : 'accounts';
    try {
      await api(`/api/admin/${collection}${context.id ? '/' + encodeURIComponent(context.id) : ''}`, { method: context.id ? 'PUT' : 'POST', body: JSON.stringify(body) });
      if (current !== session) return;
      closeEditor();
      notice('Đã lưu cấu hình thành công.');
      try { await refresh(); } catch (error) { if (current === session) notice(`Đã lưu, nhưng chưa làm mới trạng thái: ${error.message}`, true); }
    } catch (error) {
      if (current === session && error.name !== 'AbortError') showError('editor-error', error.message);
    } finally {
      if (current === session) $('save-entity-button').disabled = false;
      delete body.password;
      delete body.proxy_url;
    }
  }

  function openDelete(kind, id) {
    const entity = (kind === 'website' ? state.websites : state.accounts).find((item) => item.id === id);
    if (!entity) return;
    deletion = { kind, id };
    $('delete-error').hidden = true;
    $('confirm-delete-button').disabled = false;
    $('delete-description').textContent = `Xóa ${kind === 'website' ? 'website' : 'tài khoản'} “${entity.name || entity.id}” (${entity.id})? ${kind === 'website' ? 'Hãy xóa các tài khoản thuộc website trước. ' : ''}Thao tác này không thể hoàn tác.`;
    $('delete-dialog').showModal();
    $('cancel-delete-button').focus();
  }

  async function confirmDelete() {
    if (!deletion) return;
    const context = { ...deletion };
    const current = session;
    $('confirm-delete-button').disabled = true;
    try {
      await api(`/api/admin/${context.kind === 'website' ? 'websites' : 'accounts'}/${encodeURIComponent(context.id)}`, { method: 'DELETE' });
      if (current !== session) return;
      $('delete-dialog').close();
      deletion = null;
      notice('Đã xóa cấu hình.');
      try { await refresh(); } catch (error) { if (current === session) notice(`Đã xóa, nhưng chưa làm mới trạng thái: ${error.message}`, true); }
    } catch (error) {
      if (current === session && error.name !== 'AbortError') showError('delete-error', error.status === 409 ? 'Website còn tài khoản liên kết. Xóa các tài khoản đó trước, sau đó thử lại.' : error.message);
    } finally { if (current === session) $('confirm-delete-button').disabled = false; }
  }

  async function testAccount(id) {
    if (testing.has(id)) return;
    const current = session;
    testing.add(id);
    renderAccounts();
    $('account-test-result').textContent = `Đang kiểm tra tài khoản ${id}…`;
    try {
      const result = await api(`/api/admin/accounts/${encodeURIComponent(id)}/test`, { method: 'POST' });
      if (current !== session) return;
      const count = Array.isArray(result.models) ? result.models.length : 0;
      $('account-test-result').textContent = `Kết nối thành công: ${id} · ${count} mô hình.`;
      await refresh();
    } catch (error) {
      if (current === session && error.name !== 'AbortError') {
        $('account-test-result').textContent = `Kiểm tra ${id}: ${sanitized(error.message)}`;
        notice(error.message, true);
      }
    } finally { if (current === session) { testing.delete(id); renderAccounts(); } }
  }

  function routingHeaders() {
    const headers = { 'X-Website-ID': $('play-website').value };
    if ($('play-account').value) headers['X-Account-ID'] = $('play-account').value;
    return headers;
  }

  async function loadModels() {
    if (!$('play-website').value || modelsPending || chatPending) return;
    const current = session;
    const run = ++modelRun;
    modelController = new AbortController();
    modelsPending = true;
    syncPlayControls();
    $('play-status').textContent = 'Đang tải danh sách mô hình…';
    try {
      const result = await api('/api/admin/models', { headers: routingHeaders(), signal: modelController.signal });
      if (current !== session || run !== modelRun) return;
      if (!Array.isArray(result.data)) throw new APIError('Danh sách mô hình không hợp lệ.', 0);
      const models = result.data.filter((model) => model && typeof model.id === 'string' && model.id);
      populate($('play-model'), models.map((model) => ({ id: model.id, name: model.id })), models.length ? null : { value: '', text: 'Không có mô hình khả dụng' });
      $('play-status').textContent = `Đã tải ${models.length} mô hình.`;
    } catch (error) {
      if (current === session && run === modelRun && error.name !== 'AbortError') {
        $('play-model').replaceChildren(option('', 'Tải danh sách mô hình trước'));
        $('play-status').textContent = sanitized(error.message);
      }
    } finally {
      if (current === session && run === modelRun) { modelsPending = false; modelController = null; syncPlayControls(); }
    }
  }

  // Incremental UTF-8 decoding and SSE line framing, including CRLF split across chunks.
  async function consumeSSE(response, signal, onContent) {
    if (!response.body) throw new APIError('Máy chủ không trả về luồng phản hồi.', 0);
    const reader = response.body.getReader();
    const decoder = new TextDecoder('utf-8');
    let buffer = '';
    let data = [];
    let done = false;
    const cancel = () => { reader.cancel().catch(() => {}); };
    signal.addEventListener('abort', cancel, { once: true });
    if (signal.aborted) cancel();
    function dispatch() {
      if (!data.length) return;
      const raw = data.join('\n');
      data = [];
      if (raw.trim() === '[DONE]') { done = true; return; }
      let payload;
      try { payload = JSON.parse(raw); } catch { throw new APIError('Gói dữ liệu SSE không hợp lệ.', 0); }
      if (payload && payload.error) throw new APIError(errorMessage(payload, 200), 200);
      const content = payload?.choices?.[0]?.delta?.content;
      if (typeof content === 'string') onContent(content);
    }
    function line(value) {
      if (value === '') { dispatch(); return; }
      if (value.startsWith(':')) return;
      const colon = value.indexOf(':');
      const field = colon < 0 ? value : value.slice(0, colon);
      let content = colon < 0 ? '' : value.slice(colon + 1);
      if (content.startsWith(' ')) content = content.slice(1);
      if (field === 'data') data.push(content);
    }
    function drain(final) {
      while (!done) {
        const match = /[\r\n]/.exec(buffer);
        if (!match) break;
        const index = match.index;
        if (!final && buffer[index] === '\r' && index === buffer.length - 1) break;
        const width = buffer[index] === '\r' && buffer[index + 1] === '\n' ? 2 : 1;
        const value = buffer.slice(0, index);
        buffer = buffer.slice(index + width);
        line(value);
      }
      if (final && !done) { if (buffer) line(buffer); buffer = ''; dispatch(); }
    }
    try {
      while (!done) {
        if (signal.aborted) throw new DOMException('Aborted', 'AbortError');
        const chunk = await reader.read();
        if (signal.aborted) throw new DOMException('Aborted', 'AbortError');
        if (chunk.done) { buffer += decoder.decode(); drain(true); break; }
        buffer += decoder.decode(chunk.value, { stream: true });
        drain(false);
      }
    } finally {
      signal.removeEventListener('abort', cancel);
      await reader.cancel().catch(() => {});
      reader.releaseLock();
    }
  }

  async function sendChat(event) {
    event.preventDefault();
    if (chatPending || !$('playground-form').reportValidity()) return;
    const prompt = $('play-prompt').value;
    if (!prompt.trim() || !$('play-model').value || !$('play-website').value) return;
    const current = session;
    const run = ++chatRun;
    const controller = new AbortController();
    chatController = controller;
    chatPending = true;
    $('chat-output').textContent = '';
    $('response-empty').hidden = true;
    $('response-status').textContent = 'Đang nhận…';
    $('play-status').textContent = 'Đang kết nối tới mô hình…';
    syncPlayControls();
    try {
      const response = await api('/api/admin/chat/completions', { method: 'POST', headers: routingHeaders(), signal: controller.signal, raw: true, body: JSON.stringify({ model: $('play-model').value, messages: [{ role: 'user', content: prompt }], stream: true }) });
      if (current !== session || run !== chatRun) return;
      if (!(response.headers.get('Content-Type') || '').toLowerCase().includes('text/event-stream')) {
        let result;
        try { result = await response.json(); } catch { result = null; }
        throw new APIError(result?.error ? errorMessage(result, response.status) : 'Máy chủ trả về định dạng khác SSE.', response.status);
      }
      await consumeSSE(response, controller.signal, (text) => {
        if (current !== session || run !== chatRun) return;
        $('chat-output').textContent += text;
        $('play-status').textContent = 'Đang nhận phản hồi…';
        $('chat-output').scrollTop = $('chat-output').scrollHeight;
      });
      if (current !== session || run !== chatRun) return;
      $('response-status').textContent = 'Hoàn tất';
      $('play-status').textContent = $('chat-output').textContent ? 'Đã nhận xong phản hồi.' : 'Luồng đã kết thúc nhưng không có nội dung văn bản.';
    } catch (error) {
      if (current !== session || run !== chatRun) return;
      const aborted = error.name === 'AbortError' || controller.signal.aborted;
      $('response-status').textContent = aborted ? 'Đã dừng' : 'Có lỗi';
      $('play-status').textContent = aborted ? 'Đã dừng yêu cầu. Nội dung nhận được vẫn được giữ lại.' : sanitized(error.message);
    } finally {
      if (current === session && run === chatRun) { chatPending = false; chatController = null; syncPlayControls(); }
    }
  }

  $('login-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    if ($('login-submit').disabled) return;
    key = $('api-key').value.trim();
    $('api-key').value = '';
    if (!key) { showError('login-error', 'Vui lòng nhập khóa API.'); return; }
    const current = ++session;
    $('login-submit').disabled = true;
    $('login-error').hidden = true;
    try {
      await refresh();
      if (current !== session) return;
      $('login-screen').hidden = true;
      $('app-shell').hidden = false;
      navigate(location.hash.slice(1));
      $('refresh-button').focus();
    } catch (error) {
      if (current === session) {
        const message = sanitized(error.message);
        logout();
        if (error.name !== 'AbortError') showError('login-error', message);
      }
    } finally { $('login-submit').disabled = false; }
  });
  $('logout-button').addEventListener('click', logout);
  $('refresh-button').addEventListener('click', async () => {
    const current = session;
    $('refresh-button').disabled = true;
    try { await refresh(); if (current === session) notice('Đã làm mới trạng thái hệ thống.'); }
    catch (error) { if (current === session && error.name !== 'AbortError') notice(error.message, true); }
    finally { if (current === session) $('refresh-button').disabled = false; }
  });
  for (const button of document.querySelectorAll('[data-page], [data-goto]')) button.addEventListener('click', () => {
    const page = button.dataset.page || button.dataset.goto;
    location.hash = page;
    navigate(page);
  });
  window.addEventListener('hashchange', () => { if (key) navigate(location.hash.slice(1)); });
  $('website-search').addEventListener('input', renderWebsites);
  $('account-search').addEventListener('input', renderAccounts);
  $('account-filter').addEventListener('change', renderAccounts);
  $('add-website-button').addEventListener('click', () => openEditor('website'));
  $('add-account-button').addEventListener('click', () => openEditor('account'));
  $('editor-form').addEventListener('submit', saveEntity);
  $('close-editor-button').addEventListener('click', closeEditor);
  $('cancel-editor-button').addEventListener('click', closeEditor);
  $('editor-dialog').addEventListener('close', () => { $('entity-password').value = ''; $('entity-proxy').value = ''; editor = null; });
  $('clear-proxy').addEventListener('change', () => { $('entity-proxy').disabled = $('clear-proxy').checked; if ($('clear-proxy').checked) $('entity-proxy').value = ''; });
  $('cancel-delete-button').addEventListener('click', () => { $('delete-dialog').close(); deletion = null; });
  $('delete-dialog').addEventListener('close', () => { deletion = null; });
  $('confirm-delete-button').addEventListener('click', confirmDelete);
  for (const id of ['website-rows', 'account-rows']) $(id).addEventListener('click', (event) => {
    const button = event.target.closest('button[data-action]');
    if (!button || button.disabled) return;
    const name = button.dataset.action;
    const entityId = button.dataset.id;
    if (name === 'test-account') { testAccount(entityId); return; }
    const [verb, kind] = name.split('-');
    if (verb === 'edit') openEditor(kind, entityId);
    if (verb === 'delete') openDelete(kind, entityId);
  });
  $('play-website').addEventListener('change', () => { populatePlayAccounts(); clearModels(); $('play-status').textContent = 'Website đã đổi. Tải lại danh sách mô hình.'; });
  $('play-account').addEventListener('change', () => { clearModels(); $('play-status').textContent = 'Tài khoản đã đổi. Tải lại danh sách mô hình.'; });
  $('play-model').addEventListener('change', syncPlayControls);
  $('load-models-button').addEventListener('click', loadModels);
  $('playground-form').addEventListener('submit', sendChat);
  $('stop-chat-button').addEventListener('click', () => { if (chatController) { chatController.abort(); $('stop-chat-button').disabled = true; $('play-status').textContent = 'Đang dừng yêu cầu…'; } });
  $('copy-api-button').addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(location.origin + '/v1'); notice('Đã sao chép API base URL.'); }
    catch { $('api-base').focus(); $('api-base').select(); notice('Chọn URL và nhấn Ctrl+C hoặc ⌘C để sao chép.'); }
  });
  navigate('dashboard');
  syncPlayControls();
})();
