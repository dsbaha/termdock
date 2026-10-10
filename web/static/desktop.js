// DesktopViewer: a noVNC-based X11 desktop viewer for TermDock desktop
// sessions. Connects to /api/sessions/{id}/vnc and renders the VNC stream
// to a canvas. Viewers are watch-only (enforced server-side in the proxy;
// viewOnly is a client-side convenience).
import RFB from '/vendor/novnc/core/rfb.js';

export class DesktopViewer {
  constructor(container, sessionId, role) {
    this.container = container;
    this.sessionId = sessionId;
    this.role = role;
    this.rfb = null;
    this.onStatus = null; // optional (statusText) => void

    container.classList.add('desktop-view');
    container.innerHTML =
      '<div class="desktop-screen"></div>' +
      '<div class="desktop-overlay"><span class="desktop-status">Connecting…</span>' +
      '<button class="desktop-reconnect" style="display:none">Reconnect</button></div>';

    this.screen = container.querySelector('.desktop-screen');
    this.overlay = container.querySelector('.desktop-overlay');
    this.statusEl = container.querySelector('.desktop-status');
    this.reconnectBtn = container.querySelector('.desktop-reconnect');
    this.reconnectBtn.addEventListener('click', () => this.connect());
  }

  setStatus(text, showReconnect) {
    this.statusEl.textContent = text;
    this.reconnectBtn.style.display = showReconnect ? '' : 'none';
    if (this.onStatus) this.onStatus(text);
  }

  connect() {
    this.disconnect();
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = proto + '//' + location.host + '/api/sessions/' +
      encodeURIComponent(this.sessionId) + '/vnc';
    this.setStatus('Connecting…', false);
    this.overlay.style.display = '';
    try {
      this.rfb = new RFB(this.screen, url, { shared: true });
    } catch (e) {
      this.setStatus('Failed to start VNC client: ' + e.message, true);
      return;
    }
    this.rfb.viewOnly = (this.role === 'viewer');
    this.rfb.scaleViewport = true;
    this.rfb.resizeSession = false;
    this.rfb.addEventListener('connect', () => {
      this.overlay.style.display = 'none';
      this.setStatus('Connected', false);
    });
    this.rfb.addEventListener('disconnect', (e) => {
      const clean = e && e.detail && e.detail.clean;
      this.overlay.style.display = '';
      this.setStatus(clean ? 'Disconnected' : 'Connection lost', true);
    });
    this.rfb.addEventListener('desktopname', (e) => {
      if (e.detail && e.detail.name) this.setStatus(e.detail.name, false);
    });
  }

  disconnect() {
    if (this.rfb) {
      try { this.rfb.disconnect(); } catch (e) { /* ignore */ }
      this.rfb = null;
    }
    this.screen.innerHTML = '';
  }

  destroy() {
    this.disconnect();
    this.container.innerHTML = '';
  }
}
