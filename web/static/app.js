/* TermDock dashboard — vanilla JS. Terminal handling lives in the shared
 * module terminal.js (also used by the /t/{id} popup page). */
import { TermDockTerminal } from '/terminal.js';

'use strict';

const S = {
  user: null,
  orgs: [],
  currentOrg: 'all',
  sessions: [],
  tabs: new Map(),      // sessionId -> tab state
  termStates: new Map(), // sessionId -> terminal state ('waiting'|'running'|...)
};

const $ = (id) => document.getElementById(id);

/* ---------------- API ---------------- */

async function api(method, path, body) {
  const opts = { method, credentials: 'same-origin', headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    return { ok: false, status: 0, data: { error: 'network error: ' + e.message } };
  }
  if (res.status === 401 && !path.startsWith('/api/auth/')) {
    showAuth();
    return { ok: false, status: 401, data: { error: 'not authenticated' } };
  }
  let data = null;
  try { data = await res.json(); } catch (e) { /* empty body */ }
  return { ok: res.ok, status: res.status, data };
}

let toastTimer = null;
function toast(msg, ms = 3500) {
  const el = $('toast');
  el.textContent = msg;
  el.classList.remove('hidden');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add('hidden'), ms);
}

function apiError(r, fallback) {
  return (r.data && r.data.error) || fallback || 'request failed';
}

/* ---------------- Auth ---------------- */

function showAuth() {
  S.user = null;
  $('app-view').classList.add('hidden');
  $('auth-view').classList.remove('hidden');
}

function showApp() {
  $('auth-view').classList.add('hidden');
  $('app-view').classList.remove('hidden');
}

function initAuth() {
  $('tab-login').onclick = () => {
    $('tab-login').classList.add('active');
    $('tab-register').classList.remove('active');
    $('login-form').classList.remove('hidden');
    $('register-form').classList.add('hidden');
  };
  $('tab-register').onclick = () => {
    $('tab-register').classList.add('active');
    $('tab-login').classList.remove('active');
    $('register-form').classList.remove('hidden');
    $('login-form').classList.add('hidden');
  };
  $('login-form').onsubmit = async (e) => {
    e.preventDefault();
    const r = await api('POST', '/api/auth/login', {
      username: $('login-username').value.trim(),
      password: $('login-password').value,
    });
    if (!r.ok) return authFail(r);
    await boot(r.data);
  };
  $('register-form').onsubmit = async (e) => {
    e.preventDefault();
    const r = await api('POST', '/api/auth/register', {
      username: $('register-username').value.trim(),
      password: $('register-password').value,
    });
    if (!r.ok) return authFail(r);
    await boot(r.data);
  };
  $('logout-btn').onclick = async () => {
    await api('POST', '/api/auth/logout');
    for (const id of [...S.tabs.keys()]) closeTab(id);
    showAuth();
  };
}

function authFail(r) {
  const el = $('auth-error');
  el.textContent = apiError(r, 'authentication failed');
  el.classList.remove('hidden');
}

async function boot(user) {
  S.user = user;
  $('user-label').textContent = user.username + (user.is_admin ? ' (admin)' : '');
  showApp();
  await loadOrgs();
  await loadSessions();
  startPolling();
}

async function init() {
  initAuth();
  const r = await api('GET', '/api/auth/me');
  if (r.ok) await boot(r.data);
  else showAuth();
}

/* ---------------- Orgs ---------------- */

async function loadOrgs() {
  const r = await api('GET', '/api/orgs');
  if (!r.ok) return toast('Failed to load orgs: ' + apiError(r));
  S.orgs = r.data;
  renderOrgSwitcher();
}

function renderOrgSwitcher() {
  const sel = $('org-switcher');
  sel.innerHTML = '';
  const all = document.createElement('option');
  all.value = 'all';
  all.textContent = 'All orgs';
  sel.appendChild(all);
  for (const o of S.orgs) {
    const opt = document.createElement('option');
    opt.value = o.id;
    opt.textContent = o.name;
    sel.appendChild(opt);
  }
  sel.value = S.currentOrg;
  sel.onchange = () => { S.currentOrg = sel.value; renderSessionList(); };
  $('new-org-btn').onclick = async () => {
    const name = prompt('New organization name:');
    if (!name || !name.trim()) return;
    const r = await api('POST', '/api/orgs', { name: name.trim() });
    if (!r.ok) return toast('Create org failed: ' + apiError(r));
    S.currentOrg = r.data.id;
    await loadOrgs();
  };
}

/* ---------------- Sessions list ---------------- */

async function loadSessions() {
  const r = await api('GET', '/api/sessions');
  if (!r.ok) return toast('Failed to load sessions: ' + apiError(r));
  S.sessions = r.data;
  renderSessionList();
  syncTabs();
}

function visibleSessions() {
  if (S.currentOrg === 'all') return S.sessions;
  return S.sessions.filter((s) => s.org_id === S.currentOrg);
}

function statusLabel(s) {
  if (s.status === 'creating' || s.status === 'pulling') return 'starting…';
  return s.status;
}

function renderSessionList() {
  const list = $('session-list');
  list.innerHTML = '';
  const sessions = visibleSessions();
  if (!sessions.length) {
    list.innerHTML = '<p class="muted">No terminals yet.</p>';
    return;
  }
  for (const s of sessions) {
    const item = document.createElement('div');
    item.className = 'session-item';
    const roleTag = s.my_role === 'viewer' ? ' · view-only' : '';
    const termState = S.termStates.get(s.id);
    const actTag = termState === 'running' ? ' · <span class="act-running">● running</span>' : '';
    item.innerHTML = `<div class="name"><span class="dot ${s.status}"></span><span></span></div>
      <div class="meta">${statusLabel(s)}${actTag}${roleTag} · ${s.image}</div>`;
    item.querySelector('.name span:last-child').textContent = s.name;
    item.onclick = () => openTab(s.id);
    list.appendChild(item);
  }
}

/* Keep open tabs in sync with session state (status text, toolbar). */
function syncTabs() {
  for (const [id, tab] of S.tabs) {
    const s = S.sessions.find((x) => x.id === id);
    if (!s) { closeTab(id); continue; } // session was deleted
    tab.session = s;
    tab.tabEl.querySelector('.tab-name').textContent = s.name;
    const dot = tab.tabEl.querySelector('.dot');
    if (dot) dot.className = 'dot ' + s.status;
    renderToolbar(tab);
  }
}

/* ---------------- Tabs + terminal ---------------- */

function openTab(sessionId) {
  if (S.tabs.has(sessionId)) {
    activateTab(sessionId);
    return;
  }
  const s = S.sessions.find((x) => x.id === sessionId);
  if (!s) return;

  $('empty-state').style.display = 'none';

  const tabEl = document.createElement('div');
  tabEl.className = 'tab';
  tabEl.innerHTML = `<span class="dot ${s.status}"></span><span class="tab-name"></span>
    <button class="tab-close" title="Close tab">×</button>`;
  tabEl.querySelector('.tab-name').textContent = s.name;
  tabEl.onclick = (e) => {
    if (e.target.classList.contains('tab-close')) closeTab(sessionId);
    else activateTab(sessionId);
  };

  const panelEl = document.createElement('div');
  panelEl.className = 'tab-panel';
  panelEl.innerHTML = `
    <div class="viewer-banner hidden">View only — you are watching this terminal.</div>
    <div class="toolbar">
      <span class="td-badge-slot"></span>
      <span class="status-text"></span>
      <div class="spacer"></div>
      <button class="btn small ghost share-btn">Share</button>
      <button class="btn small ghost popout-btn" title="Open in a popup window">⧉ Pop out</button>
      <button class="btn small ghost toggle-btn"></button>
      <button class="btn small danger delete-btn">Delete</button>
    </div>
    <div class="terminal-wrap"></div>`;

  $('tab-bar').appendChild(tabEl);
  $('tab-panels').appendChild(panelEl);

  const tab = { id: sessionId, session: s, tabEl, panelEl, tterm: null };
  S.tabs.set(sessionId, tab);

  panelEl.querySelector('.share-btn').onclick = () => openShareModal(sessionId);
  // window.open directly in the click handler so popup blockers allow it.
  panelEl.querySelector('.popout-btn').onclick = () => {
    window.open(`/t/${sessionId}`, '_blank', 'width=1000,height=700');
  };
  panelEl.querySelector('.toggle-btn').onclick = () => toggleSession(tab);
  panelEl.querySelector('.delete-btn').onclick = () => deleteSession(tab);

  activateTab(sessionId);
  attachTab(tab);
}

function activateTab(sessionId) {
  for (const [id, t] of S.tabs) {
    const active = id === sessionId;
    t.tabEl.classList.toggle('active', active);
    t.panelEl.classList.toggle('active', active);
    if (active && t.tterm) setTimeout(() => t.tterm.refit(), 30);
  }
}

// Closing a tab only detaches the websocket — the session, its container,
// and its shell keep running. Reopening re-attaches to the same shell.
function closeTab(sessionId) {
  const tab = S.tabs.get(sessionId);
  if (!tab) return;
  if (tab.tterm) tab.tterm.close();
  tab.tabEl.remove();
  tab.panelEl.remove();
  S.tabs.delete(sessionId);
  S.termStates.delete(sessionId);
  if (!S.tabs.size) $('empty-state').style.display = '';
  renderSessionList();
}

function renderToolbar(tab) {
  const s = tab.session;
  const statusEl = tab.panelEl.querySelector('.status-text');
  const toggleBtn = tab.panelEl.querySelector('.toggle-btn');
  statusEl.textContent = `${s.image} · ${s.shell} · ${s.cpu} CPU · ${s.memory_mb} MB`;
  if (s.status_msg && s.status === 'error') statusEl.textContent += ` — ${s.status_msg}`;
  toggleBtn.disabled = false;
  if (s.status === 'running') {
    toggleBtn.textContent = 'Stop';
  } else if (s.status === 'stopped' || s.status === 'error') {
    toggleBtn.textContent = s.status === 'stopped' ? 'Resume' : 'Start';
  } else {
    toggleBtn.textContent = 'Starting…';
    toggleBtn.disabled = true;
  }
}

async function toggleSession(tab) {
  const s = tab.session;
  if (s.status === 'running') {
    const r = await api('POST', `/api/sessions/${s.id}/stop`);
    if (!r.ok) toast('Stop failed: ' + apiError(r));
  } else if (s.status === 'stopped' || s.status === 'error') {
    const r = await api('POST', `/api/sessions/${s.id}/start`);
    if (!r.ok) {
      toast('Start failed: ' + apiError(r));
    } else {
      attachTab(tab); // re-arm the "wait until running" loop; fresh shell, same volume
    }
  }
  await loadSessions();
}

async function deleteSession(tab) {
  if (!confirm(`Delete terminal "${tab.session.name}"? Its container and /workspace volume will be removed.`)) return;
  const r = await api('DELETE', `/api/sessions/${tab.session.id}`);
  if (!r.ok) return toast('Delete failed: ' + apiError(r));
  closeTab(tab.id);
  await loadSessions();
}

// (Re)attach a TermDockTerminal to the tab. Safe to call again after Start.
function attachTab(tab) {
  if (tab.tterm) tab.tterm.close();
  const wrap = tab.panelEl.querySelector('.terminal-wrap');
  wrap.innerHTML = '';
  const tterm = new TermDockTerminal({
    sessionId: tab.id,
    api,
    notify: (msg) => toast(msg),
    updateTitle: false, // dashboard keeps its own title
    onState: (state) => {
      S.termStates.set(tab.id, state);
      renderSessionList(); // refresh the sidebar activity marker
    },
    onRole: (role) => {
      tab.panelEl.querySelector('.viewer-banner').classList.toggle('hidden', role !== 'viewer');
    },
  });
  tab.tterm = tterm;
  tterm.open(wrap, tab.panelEl.querySelector('.td-badge-slot'));
}

/* ---------------- New session modal ---------------- */

function initNewSessionModal() {
  $('new-session-btn').onclick = () => {
    const sel = $('ns-org');
    sel.innerHTML = '';
    for (const o of S.orgs) {
      const opt = document.createElement('option');
      opt.value = o.id;
      opt.textContent = o.name;
      sel.appendChild(opt);
    }
    if (S.currentOrg !== 'all') sel.value = S.currentOrg;
    $('ns-error').classList.add('hidden');
    $('new-session-modal').classList.remove('hidden');
  };
  $('ns-cancel').onclick = () => $('new-session-modal').classList.add('hidden');
  $('new-session-form').onsubmit = async (e) => {
    e.preventDefault();
    const idleVal = parseInt($('ns-idle').value, 10);
    const body = {
      name: $('ns-name').value.trim(),
      org_id: $('ns-org').value,
      image: $('ns-image').value.trim() || undefined,
      shell: $('ns-shell').value.trim() || undefined,
      cpu: parseFloat($('ns-cpu').value) || undefined,
      memory_mb: parseInt($('ns-memory').value, 10) || undefined,
      idle_minutes: Number.isNaN(idleVal) ? undefined : idleVal,
    };
    const r = await api('POST', '/api/sessions', body);
    if (!r.ok) {
      const el = $('ns-error');
      el.textContent = apiError(r, 'create failed');
      el.classList.remove('hidden');
      return;
    }
    $('new-session-modal').classList.add('hidden');
    $('ns-name').value = '';
    await loadSessions();
    openTab(r.data.id); // opens immediately; the terminal attaches once status=running
    toast(`Terminal "${r.data.name}" is starting…`);
  };
}

/* ---------------- Share modal ---------------- */

let shareSessionId = null;

function openShareModal(sessionId) {
  shareSessionId = sessionId;
  const s = S.sessions.find((x) => x.id === sessionId);
  $('share-session-name').textContent = s ? `Sharing "${s.name}"` : '';
  $('share-error').classList.add('hidden');
  $('share-username').value = '';
  $('share-modal').classList.remove('hidden');
  loadShares();
}

async function loadShares() {
  const r = await api('GET', `/api/sessions/${shareSessionId}/shares`);
  const list = $('share-list');
  list.innerHTML = '';
  if (!r.ok) {
    list.innerHTML = `<p class="error">${apiError(r)}</p>`;
    return;
  }
  if (!r.data.length) {
    list.innerHTML = '<p class="muted">Not shared with anyone yet.</p>';
    return;
  }
  for (const sh of r.data) {
    const row = document.createElement('div');
    row.className = 'share-row';
    const name = document.createElement('span');
    name.textContent = sh.username || sh.user_id;
    name.style.flex = '1';
    const tag = document.createElement('span');
    tag.className = 'role-tag ' + sh.role;
    tag.textContent = sh.role;
    const revoke = document.createElement('button');
    revoke.className = 'btn small danger';
    revoke.textContent = 'Revoke';
    revoke.onclick = async () => {
      if (!confirm(`Revoke ${name.textContent}'s access? Their terminal window will close immediately.`)) return;
      const rr = await api('DELETE', `/api/sessions/${shareSessionId}/shares/${sh.id}`);
      if (!rr.ok) return toast('Revoke failed: ' + apiError(rr));
      toast('Access revoked');
      loadShares();
    };
    row.append(name, tag, revoke);
    list.appendChild(row);
  }
}

function initShareModal() {
  $('share-close').onclick = () => $('share-modal').classList.add('hidden');
  $('share-form').onsubmit = async (e) => {
    e.preventDefault();
    const r = await api('POST', `/api/sessions/${shareSessionId}/shares`, {
      username: $('share-username').value.trim(),
      role: $('share-role').value,
    });
    if (!r.ok) {
      const el = $('share-error');
      el.textContent = apiError(r, 'share failed');
      el.classList.remove('hidden');
      return;
    }
    $('share-username').value = '';
    $('share-error').classList.add('hidden');
    toast('Shared');
    loadShares();
  };
}

/* ---------------- Polling ---------------- */

let pollTimer = null;
function startPolling() {
  if (pollTimer) return;
  pollTimer = setInterval(loadSessions, 5000);
}

/* ---------------- Boot ---------------- */

document.addEventListener('DOMContentLoaded', () => {
  initNewSessionModal();
  initShareModal();
  window.addEventListener('resize', () => {
    for (const [id, t] of S.tabs) {
      if (t.panelEl.classList.contains('active') && t.tterm) t.tterm.refit();
    }
  });
  init();
});
