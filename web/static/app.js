/* TermDock dashboard — vanilla JS. Terminal handling lives in the shared
 * module terminal.js (also used by the /t/{id} popup page). */
import { TermDockTerminal } from '/terminal.js';
import { DesktopViewer } from '/desktop.js';

'use strict';

const S = {
  user: null,
  orgs: [],
  currentOrg: 'all',
  sessions: [],
  tabs: new Map(),      // "sessionId:termId" -> tab state
  termStates: new Map(), // "sessionId:termId" -> terminal state ('waiting'|'running'|...)
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
  $('admin-btn').classList.toggle('hidden', !user.is_admin);
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
  if (s.status === 'creating' || s.status === 'pulling' || s.status === 'building') return 'starting…';
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
    // Active if any of the session's terminals is running a command.
    let active = false;
    for (const [k, v] of S.termStates) {
      if (k.startsWith(s.id + ':') && v === 'running') { active = true; break; }
    }
    const actTag = active ? ' · <span class="act-running">● running</span>' : '';
    const dispTag = s.display === 'desktop' ? '🖥️ ' : '';
    item.innerHTML = `<div class="name"><span class="dot ${s.status}"></span><span></span></div>
      <div class="meta">${statusLabel(s)}${actTag}${roleTag} · ${s.image}</div>`;
    item.querySelector('.name span:last-child').textContent = dispTag + s.name;
    item.onclick = () => openTab(s.id);
    list.appendChild(item);
  }
}

/* Keep open tabs in sync with session state (status text, toolbar). */
function tabKey(sessionId, termId) {
  return sessionId + ':' + (termId || 'main');
}

function syncTabs() {
  for (const [key, tab] of S.tabs) {
    const s = S.sessions.find((x) => x.id === tab.sessionId);
    if (!s) { closeTab(key); continue; } // session was deleted
    tab.session = s;
    tab.tabEl.querySelector('.tab-name').textContent = tab.label;
    const dot = tab.tabEl.querySelector('.dot');
    if (dot) dot.className = 'dot ' + s.status;
    renderToolbar(tab);
  }
}

/* ---------------- Tabs + terminal ---------------- */

// Open (or focus) a terminal tab. termId defaults to the session's persistent
// "main" terminal; extra terminals get client-generated IDs, each backed by
// its own shell (docker exec) in the same container. Desktop sessions open a
// single VNC tab instead (no terminal tabs).
function openTab(sessionId, termId, opts) {
  const s = S.sessions.find((x) => x.id === sessionId);
  if (!s) return;
  // Desktop sessions open the VNC view by default; a specific termId means
  // "terminal tab into this container's shell".
  if (s.display === 'desktop' && !termId) return openDesktopTab(s, opts || {});
  termId = termId || 'main';
  opts = opts || {};
  const key = tabKey(sessionId, termId);
  if (S.tabs.has(key)) {
    if (opts.activate !== false) activateTab(key);
    return;
  }

  $('empty-state').style.display = 'none';

  // Provisional label; the server-assigned seq refines it (name / name (2)…).
  const tabEl = document.createElement('div');
  tabEl.className = 'tab';
  tabEl.innerHTML = `<span class="dot ${s.status}"></span><span class="tab-name"></span>
    <button class="tab-close" title="Close tab">×</button>`;
  tabEl.querySelector('.tab-name').textContent = termId === 'main' ? s.name : `${s.name} …`;
  tabEl.onclick = (e) => {
    if (e.target.classList.contains('tab-close')) closeTab(key);
    else activateTab(key);
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
      <button class="btn small ghost reconnect-btn hidden">Reconnect</button>
      <button class="btn small ghost toggle-btn"></button>
      <button class="btn small danger delete-btn">Delete</button>
    </div>
    <div class="terminal-wrap"></div>`;

  $('tab-bar').insertBefore(tabEl, $('new-term-btn'));
  $('tab-panels').appendChild(panelEl);

  const tab = { key, sessionId, termId, label: tabEl.querySelector('.tab-name').textContent, session: s, tabEl, panelEl, tterm: null };
  S.tabs.set(key, tab);

  panelEl.querySelector('.share-btn').onclick = () => openShareModal(sessionId);
  // window.open directly in the click handler so popup blockers allow it.
  panelEl.querySelector('.popout-btn').onclick = () => {
    const q = termId === 'main' ? '' : `?term=${encodeURIComponent(termId)}`;
    window.open(`/t/${sessionId}${q}`, '_blank', 'width=1000,height=700');
  };
  panelEl.querySelector('.toggle-btn').onclick = () => toggleSession(tab);
  panelEl.querySelector('.reconnect-btn').onclick = () => {
    try {
      attachTab(tab); // fresh terminal object; resets the auto-reconnect budget
    } catch (e) {
      console.error('reconnect failed', e);
      toast('Reconnect failed: ' + (e && e.message ? e.message : e));
    }
  };
  panelEl.querySelector('.delete-btn').onclick = () => deleteSession(tab);

  if (opts.activate !== false) activateTab(key);
  try {
    attachTab(tab);
  } catch (e) {
    console.error('failed to attach terminal', e);
    tab.panelEl.querySelector('.terminal-wrap').innerHTML =
      `<div style="padding:24px;color:#ff7b72">Couldn't open the terminal view: ${(e && e.message) || e}<br>Close this tab and click the session in the sidebar to retry.</div>`;
  }
}

// Open (or focus) a desktop session's VNC tab. One tab per session —
// desktops have no terminal tabs.
function openDesktopTab(s, opts) {
  const key = tabKey(s.id, 'main');
  if (S.tabs.has(key)) {
    if (opts.activate !== false) activateTab(key);
    return;
  }
  $('empty-state').style.display = 'none';

  const tabEl = document.createElement('div');
  tabEl.className = 'tab';
  tabEl.innerHTML = `<span class="dot ${s.status}"></span><span class="tab-name">🖥️ </span>
    <button class="tab-close" title="Close tab">×</button>`;
  tabEl.querySelector('.tab-name').appendChild(document.createTextNode(s.name));
  tabEl.onclick = (e) => {
    if (e.target.classList.contains('tab-close')) closeTab(key);
    else activateTab(key);
  };

  const panelEl = document.createElement('div');
  panelEl.className = 'tab-panel';
  panelEl.innerHTML = `
    <div class="viewer-banner hidden">View only — you are watching this desktop.</div>
    <div class="toolbar">
      <span class="td-badge-slot"></span>
      <span class="status-text"></span>
      <div class="spacer"></div>
      <button class="btn small ghost share-btn">Share</button>
      <button class="btn small ghost popout-btn" title="Open in a popup window">⧉ Pop out</button>
      <button class="btn small ghost toggle-btn"></button>
      <button class="btn small danger delete-btn">Delete</button>
    </div>
    <div class="desktop-wrap"></div>`;

  $('tab-bar').insertBefore(tabEl, $('new-term-btn'));
  $('tab-panels').appendChild(panelEl);

  const tab = { key, sessionId: s.id, termId: 'main', label: s.name, session: s, tabEl, panelEl, dview: null, isDesktop: true };
  S.tabs.set(key, tab);

  panelEl.querySelector('.share-btn').onclick = () => openShareModal(s.id);
  panelEl.querySelector('.popout-btn').onclick = () => {
    window.open(`/d/${s.id}`, '_blank', 'width=1280,height=800');
  };
  panelEl.querySelector('.toggle-btn').onclick = () => toggleSession(tab);
  panelEl.querySelector('.delete-btn').onclick = () => deleteSession(tab);

  if (s.my_role === 'viewer') {
    panelEl.querySelector('.viewer-banner').classList.remove('hidden');
  }
  if (opts.activate !== false) activateTab(key);
  attachDesktop(tab);
}

// Connect the VNC viewer for a desktop tab. Waits for the session to be
// running first (provision starts the X stack before marking running).
function attachDesktop(tab) {
  const wrap = tab.panelEl.querySelector('.desktop-wrap');
  if (tab.dview) tab.dview.destroy();
  const role = tab.session.my_role || 'owner';
  const viewer = new DesktopViewer(wrap, tab.sessionId, role);
  tab.dview = viewer;
  viewer.onStatus = (text) => {
    const el = tab.panelEl.querySelector('.status-text');
    if (el && text) el.textContent = `${tab.session.image} · VNC · ${text}`;
  };
  if (tab.session.status === 'running') {
    viewer.connect();
  } else {
    viewer.setStatus('Waiting for desktop to start…', false);
    const t = setInterval(() => {
      const s = S.sessions.find((x) => x.id === tab.sessionId);
      if (!s || !S.tabs.has(tab.key)) { clearInterval(t); return; }
      tab.session = s;
      if (s.status === 'running') { clearInterval(t); viewer.connect(); }
      else if (s.status === 'error') {
        clearInterval(t);
        viewer.setStatus('Error: ' + (s.status_msg || 'failed to start'), false);
      }
    }, 2000);
  }
}

// The server assigns each terminal a stable seq (1 = main, 2,3,… = extras);
// label tabs "name" / "name (2)" / … so the dashboard and popups agree.
async function refreshTabLabel(tab) {
  try {
    const r = await api('GET', `/api/sessions/${tab.sessionId}/terms`);
    if (!r.ok) return;
    const t = (r.data.terms || []).find((x) => x.id === tab.termId);
    if (!t) return;
    tab.label = t.seq === 1 ? tab.session.name : `${tab.session.name} (${t.seq})`;
    tab.tabEl.querySelector('.tab-name').textContent = tab.label;
  } catch (e) { /* keep the provisional label */ }
}

function activateTab(key) {
  for (const [id, t] of S.tabs) {
    const active = id === key;
    t.tabEl.classList.toggle('active', active);
    t.panelEl.classList.toggle('active', active);
    if (active && t.tterm) setTimeout(() => t.tterm.refit(), 30);
  }
}

// Closing a tab detaches the websocket. The main terminal's shell keeps
// running server-side (reopening re-attaches to the same shell); an extra
// terminal's shell is torn down once its last viewer leaves.
function closeTab(key) {
  const tab = S.tabs.get(key);
  if (!tab) return;
  if (tab.tterm) tab.tterm.close();
  if (tab.dview) tab.dview.destroy();
  tab.tabEl.remove();
  tab.panelEl.remove();
  S.tabs.delete(key);
  S.termStates.delete(key);
  if (!S.tabs.size) $('empty-state').style.display = '';
  renderSessionList();
}

// Open a new extra terminal (its own shell) in the active tab's container.
function newTerminalTab() {
  const active = [...S.tabs.values()].find((t) => t.tabEl.classList.contains('active'));
  if (!active) return toast('Open a terminal first, then add another tab to its container.');
  const s = S.sessions.find((x) => x.id === active.sessionId);
  if (!s) return;
  if (s.my_role === 'viewer') return toast('Viewers cannot open new terminals.');
  const termId = 't-' + Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
  openTab(s.id, termId);
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
  // Manual reconnect when the terminal socket dropped (the terminal also
  // retries automatically a few times first).
  const reconnectBtn = tab.panelEl.querySelector('.reconnect-btn');
  const termState = S.termStates.get(tab.key);
  reconnectBtn.classList.toggle('hidden',
    termState !== 'disconnected' && termState !== 'error');
}

// Guards toggleSession against double-clicks while a stop/start is in flight.
const toggleBusy = new Set();

async function toggleSession(tab) {
  if (toggleBusy.has(tab.key)) return;
  toggleBusy.add(tab.key);
  try {
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
  } catch (e) {
    console.error('toggle session failed', e);
    toast('Operation failed: ' + (e && e.message ? e.message : e));
  } finally {
    toggleBusy.delete(tab.key);
    await loadSessions();
  }
}

async function deleteSession(tab) {
  if (!confirm(`Delete terminal "${tab.session.name}"? Its container and /workspace volume will be removed.`)) return;
  const r = await api('DELETE', `/api/sessions/${tab.session.id}`);
  if (!r.ok) return toast('Delete failed: ' + apiError(r));
  closeTab(tab.key);
  await loadSessions();
}

// (Re)attach a TermDockTerminal to the tab. Safe to call again after Start.
function attachTab(tab) {
  if (tab.tterm) tab.tterm.close();
  const wrap = tab.panelEl.querySelector('.terminal-wrap');
  wrap.innerHTML = '';
  const tterm = new TermDockTerminal({
    sessionId: tab.sessionId,
    termId: tab.termId,
    api,
    notify: (msg) => toast(msg),
    updateTitle: false, // dashboard keeps its own title
    onState: (state) => {
      S.termStates.set(tab.key, state);
      renderSessionList(); // refresh the sidebar activity marker
      renderToolbar(tab);  // refresh the reconnect button
    },
    onRole: (role) => {
      tab.panelEl.querySelector('.viewer-banner').classList.toggle('hidden', role !== 'viewer');
      refreshTabLabel(tab); // role arrives right after attach; term exists now
    },
    onTermClosed: () => {
      // Ephemeral extra terminal's shell exited: the tab is done.
      toast(`Terminal "${tab.label}" closed (shell exited)`);
      closeTab(tab.key);
    },
  });
  tab.tterm = tterm;
  tterm.open(wrap, tab.panelEl.querySelector('.td-badge-slot'));
}

/* ---------------- New session modal ---------------- */

function initNewSessionModal() {
  $('new-session-btn').onclick = async () => {
    await loadImageOptions(); // refresh the dropdown each time it opens
    $('ns-image-custom-wrap').classList.add('hidden');
    $('ns-image-custom').value = '';
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
    let image = $('ns-image').value;
    if (image === '__custom') image = $('ns-image-custom').value.trim();
    const body = {
      name: $('ns-name').value.trim(),
      org_id: $('ns-org').value,
      image: image || undefined,
      shell: $('ns-shell').value.trim() || undefined,
      display: $('ns-display').value || undefined,
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
let termPollTimer = null;
function startPolling() {
  if (pollTimer) return;
  pollTimer = setInterval(loadSessions, 5000);
  // Pick up terminals opened in popups: for every session with open tabs,
  // open (without stealing focus) any live terminal not already tabbed.
  termPollTimer = setInterval(syncTerminalTabs, 5000);
}

async function syncTerminalTabs() {
  const sessionIds = new Set([...S.tabs.values()].map((t) => t.sessionId));
  for (const sessionId of sessionIds) {
    let r;
    try {
      r = await api('GET', `/api/sessions/${sessionId}/terms`);
    } catch (e) { continue; }
    if (!r.ok) continue;
    const s = S.sessions.find((x) => x.id === sessionId);
    const isViewer = s && s.my_role === 'viewer';
    for (const term of r.data.terms || []) {
      if (term.id !== 'main' && isViewer) continue; // viewers only get main
      if (!S.tabs.has(tabKey(sessionId, term.id))) {
        openTab(sessionId, term.id, { activate: false });
      }
    }
  }
}

/* ---------------- Admin: image management ---------------- */

function initAdmin() {
  $('admin-btn').onclick = () => showAdmin(true);
  $('admin-back-btn').onclick = () => showAdmin(false);
  $('allowed-add-form').onsubmit = async (e) => {
    e.preventDefault();
    const image = $('allowed-add-input').value.trim();
    if (!image) return;
    const r = await api('POST', '/api/admin/images', { image, desktop: $('allowed-add-desktop').checked });
    if (!r.ok) return toast('Add failed: ' + apiError(r));
    $('allowed-add-input').value = '';
    $('allowed-add-desktop').checked = false;
    await loadAllowedImages();
    await loadImageOptions(); // keep the new-terminal dropdown in sync
  };
  $('custom-new-btn').onclick = () => {
    $('custom-id').value = '';
    $('custom-name').value = '';
    $('custom-desktop').checked = false;
    $('custom-desktop-cmd').value = '';
    $('custom-name').disabled = false;
    $('custom-dockerfile').value = 'FROM ubuntu:24.04\n';
    $('custom-editor').classList.remove('hidden');
  };
  $('custom-cancel-btn').onclick = () => $('custom-editor').classList.add('hidden');
  $('custom-editor').onsubmit = async (e) => {
    e.preventDefault();
    const id = $('custom-id').value;
    const body = { name: $('custom-name').value.trim(), dockerfile: $('custom-dockerfile').value, desktop: $('custom-desktop').checked, desktop_cmd: $('custom-desktop-cmd').value.trim() };
    const r = id
      ? await api('PUT', `/api/admin/custom-images/${id}`, body)
      : await api('POST', '/api/admin/custom-images', body);
    if (!r.ok) return toast('Save failed: ' + apiError(r));
    $('custom-editor').classList.add('hidden');
    await loadCustomImages();
    await loadImageOptions();
  };
}

function showAdmin(show) {
  document.querySelector('#app-view .layout').classList.toggle('hidden', show);
  $('admin-view').classList.toggle('hidden', !show);
  if (show) { loadAllowedImages(); loadCustomImages(); }
}

async function loadAllowedImages() {
  const r = await api('GET', '/api/admin/images');
  if (!r.ok) return;
  const list = $('allowed-list');
  list.innerHTML = '';
  for (const a of r.data.images || []) {
    const row = document.createElement('div');
    row.className = 'admin-row';
    const code = document.createElement('code');
    code.className = 'grow';
    code.textContent = a.image + (a.desktop ? '  🖥️ desktop' : '');
    const del = document.createElement('button');
    del.className = 'btn small danger';
    del.textContent = 'Remove';
    del.onclick = async () => {
      if (!confirm(`Remove "${a.image}" from the allowed list?`)) return;
      const dr = await api('DELETE', `/api/admin/images/${a.id}`);
      if (!dr.ok) return toast('Remove failed: ' + apiError(dr));
      await loadAllowedImages();
      await loadImageOptions();
    };
    row.appendChild(code); row.appendChild(del);
    list.appendChild(row);
  }
  if (!list.children.length) list.innerHTML = '<p class="muted">No images allowed yet.</p>';
}

let buildPollTimer = null;
function scheduleBuildPoll() {
  if (buildPollTimer) return;
  buildPollTimer = setInterval(() => {
    if ($('admin-view').classList.contains('hidden')) {
      clearInterval(buildPollTimer); buildPollTimer = null; return;
    }
    loadCustomImages(); // re-renders; stops itself when nothing is building
  }, 2000);
}

async function loadCustomImages() {
  const r = await api('GET', '/api/admin/custom-images');
  if (!r.ok) return;
  const list = $('custom-list');
  list.innerHTML = '';
  for (const c of r.data.images || []) {
    const row = document.createElement('div');
    row.className = 'admin-row';
    row.style.flexWrap = 'wrap';
    const code = document.createElement('code');
    code.className = 'grow';
    code.textContent = `termdock/${c.name}:latest` + (c.desktop ? '  🖥️ desktop' : '');
    const tag = document.createElement('span');
    tag.className = 'status-tag ' + c.status;
    tag.textContent = c.status;
    const edit = document.createElement('button');
    edit.className = 'btn small ghost';
    edit.textContent = 'Edit';
    edit.onclick = () => {
      $('custom-id').value = c.id;
      $('custom-name').value = c.name;
      $('custom-name').disabled = false;
      $('custom-dockerfile').value = c.dockerfile;
      $('custom-desktop').checked = !!c.desktop;
      $('custom-desktop-cmd').value = c.desktop_cmd || '';
      $('custom-editor').classList.remove('hidden');
    };
    const build = document.createElement('button');
    build.className = 'btn small ghost';
    build.textContent = 'Build now';
    build.onclick = async () => {
      const br = await api('POST', `/api/admin/custom-images/${c.id}/build`);
      if (!br.ok) return toast('Build failed: ' + apiError(br));
      toast('Build started — status updates here.');
      await loadCustomImages();
    };
    const del = document.createElement('button');
    del.className = 'btn small danger';
    del.textContent = 'Delete';
    del.onclick = async () => {
      if (!confirm(`Delete custom image "${c.name}"? Its Dockerfile record is removed; any cached built image stays.`)) return;
      const dr = await api('DELETE', `/api/admin/custom-images/${c.id}`);
      if (!dr.ok) return toast('Delete failed: ' + apiError(dr));
      await loadCustomImages();
      await loadImageOptions();
    };
    row.appendChild(code); row.appendChild(tag);
    row.appendChild(edit); row.appendChild(build); row.appendChild(del);
    // Build progress bar: green while building, red on error. Shows the
    // build name and percentage.
    if (c.status === 'building' || (c.status === 'error' && c.build_progress > 0)) {
      const bar = document.createElement('div');
      bar.className = 'build-bar' + (c.status === 'error' ? ' error' : '');
      const fill = document.createElement('div');
      fill.className = 'build-fill';
      fill.style.width = Math.min(100, Math.max(0, c.build_progress || 0)) + '%';
      const label = document.createElement('span');
      label.className = 'build-label';
      label.textContent = `termdock/${c.name}:latest · ${c.build_progress || 0}%`;
      bar.appendChild(fill); bar.appendChild(label);
      row.appendChild(bar);
    }
    if (c.status_msg) {
      const msg = document.createElement('div');
      msg.className = 'err-msg';
      msg.textContent = c.status_msg;
      row.appendChild(msg);
    }
    list.appendChild(row);
  }
  if (!list.children.length) list.innerHTML = '<p class="muted">No custom images yet.</p>';
  const anyBuilding = (r.data.images || []).some((c) => c.status === 'building');
  if (anyBuilding) scheduleBuildPoll();
  else if (buildPollTimer) { clearInterval(buildPollTimer); buildPollTimer = null; }
}

// Fill the new-terminal image dropdown. Admins also get a "Custom…" entry
// revealing a free-text field.
async function loadImageOptions() {
  const r = await api('GET', '/api/images');
  S.imageOptions = r.ok ? (r.data.images || []) : [];
  renderImageOptions();
}

// The image dropdown is filtered by the selected display type: desktop
// sessions only offer images flagged as providing an X stack, so you can't
// pick a terminal-only image for a desktop (it would fail at Xvfb).
function renderImageOptions() {
  const sel = $('ns-image');
  const prev = sel.value;
  const wantDesktop = $('ns-display').value === 'desktop';
  sel.innerHTML = '';
  let kept = false;
  for (const o of S.imageOptions || []) {
    if (wantDesktop && !o.desktop) continue;
    const opt = document.createElement('option');
    opt.value = o.image;
    opt.textContent = (o.custom ? `${o.image} (custom)` : o.image) + (o.desktop ? ' 🖥️' : '');
    sel.appendChild(opt);
    if (o.image === prev) kept = true;
  }
  if (S.user && S.user.is_admin) {
    const opt = document.createElement('option');
    opt.value = '__custom';
    opt.textContent = 'Custom…';
    sel.appendChild(opt);
    if (prev === '__custom') kept = true;
  }
  if (!sel.options.length) {
    const opt = document.createElement('option');
    opt.value = '';
    opt.textContent = wantDesktop ? '(no desktop images available — ask an admin)' : '(no images available)';
    sel.appendChild(opt);
  } else if (kept) {
    sel.value = prev;
  }
  $('ns-image-custom-wrap').classList.toggle('hidden', sel.value !== '__custom');
}

function initImageSelect() {
  $('ns-image').onchange = () => {
    $('ns-image-custom-wrap').classList.toggle('hidden', $('ns-image').value !== '__custom');
  };
  // Re-filter the image list when the display type changes.
  $('ns-display').onchange = () => renderImageOptions();
}

/* ---------------- Boot ---------------- */

document.addEventListener('DOMContentLoaded', () => {
  initNewSessionModal();
  initShareModal();
  initAdmin();
  initImageSelect();
  $('new-term-btn').onclick = () => newTerminalTab();
  window.addEventListener('resize', () => {
    for (const [id, t] of S.tabs) {
      if (t.panelEl.classList.contains('active') && t.tterm) t.tterm.refit();
    }
  });
  init();
});
