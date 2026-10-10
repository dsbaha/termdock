/* TermDock shared terminal client.
 *
 * Used by BOTH the dashboard (app.js) and the standalone popup page (t.html)
 * — do not duplicate this logic elsewhere.
 *
 * Owns: xterm.js setup, the /attach websocket, OSC 133 parsing, the
 * starting/waiting/running state machine, the status badge pill, and
 * (optionally) document.title updates.
 *
 * Shell integration: when the session's shell is bash, the backend injects
 * PROMPT_COMMAND=__td_done into the docker exec, so bash emits
 * "\x1b]133;D;<exitcode>\x07" after every command (same protocol VS Code
 * uses). This module strips those sequences before writing to xterm and uses
 * them for precise waiting/running states. Shells without the integration
 * (non-bash, or a ~/.bashrc that overrides PROMPT_COMMAND) fall back to an
 * output-activity heuristic: running if output arrived in the last 3s.
 *
 * Latency design (hard requirements):
 *  - PTY bytes travel as binary websocket frames in both directions — input is
 *    sent as raw bytes, never wrapped in JSON/base64.
 *  - permessage-deflate is never enabled on the terminal socket.
 *  - term.write() is called immediately on every message (no rAF batching);
 *    keystrokes are sent immediately; the OSC-133 parser is streaming O(n).
 *  - The backend hub forwards PTY bytes to viewers immediately with no
 *    batching; each viewer has a bounded non-blocking queue and a slow viewer
 *    is dropped rather than blocking the broadcast.
 *  - Rendering uses the WebGL addon when available, falling back to canvas.
 */

/** Terminal states. */
export const TD_STATE = {
  STARTING: 'starting',         // session not running yet
  WAITING: 'waiting',           // shell at prompt, waiting for input
  RUNNING: 'running',           // a command is executing
  STOPPED: 'stopped',           // container stopped (from session API)
  ERROR: 'error',               // session failed (from session API)
  DISCONNECTED: 'disconnected', // websocket closed
};

const HEURISTIC_IDLE_MS = 3000;   // heuristic: no output for 3s => waiting
const EXIT_FLASH_MS = 2500;       // how long a nonzero exit code is shown
const EXIT_OK_FLASH_MS = 1200;    // how long the zero-exit check is shown

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function fmtElapsed(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}

export class TermDockTerminal {
  /**
   * @param {object} opts
   * @param {string} opts.sessionId
   * @param {(method, path, body) => Promise<{ok,status,data}>} opts.api
   * @param {(msg: string) => void} [opts.notify] sink for server notices
   * @param {(state: string) => void} [opts.onState] state-change callback
   * @param {(role: string) => void} [opts.onRole] role callback (viewer|cowlaborator)
   * @param {boolean} [opts.updateTitle] update document.title with state
   * @param {string} [opts.titleName] base name for document.title
   */
  constructor(opts) {
    this._sessionId = opts.sessionId;
    this._api = opts.api;
    this._notify = opts.notify || (() => {});
    this._onState = opts.onState || (() => {});
    this._onRole = opts.onRole || (() => {});
    this._updateTitle = !!opts.updateTitle;
    this._titleName = opts.titleName || 'terminal';

    this._term = null;
    this._fit = null;
    this._ws = null;
    this._badgeEl = null;
    this._closed = false;
    this._state = TD_STATE.STARTING;
    this._role = null;
    this._sawOsc133 = false; // precise mode once a D sequence arrives
    this._lastOutput = 0;
    this._runningSince = 0;
    this._lastExit = null;
    this._flashUntil = 0;
    this._flashTimer = null;
    this._tickTimer = null;
    this._decoder = new TextDecoder('utf-8');
    this._encoder = new TextEncoder(); // keystrokes -> binary ws frames
    this._pending = ''; // held-back possible partial OSC sequence
    this._ro = null;    // ResizeObserver on the terminal container
    this._resizeTimer = null;
  }

  get state() { return this._state; }
  get role() { return this._role; }

  /** Build the terminal, wait for the session to run, attach the socket. */
  async open(termContainer, badgeContainer) {
    this._badgeEl = badgeContainer || null;
    this._term = new Terminal({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: '"JetBrains Mono", Menlo, Consolas, monospace',
      allowTransparency: false,
      scrollback: 5000,
      theme: {
        // Polished modern dark. 256-color and truecolor stay enabled.
        background: '#0d1117',
        foreground: '#e6edf3',
        cursor: '#58a6ff',
        selectionBackground: '#264f78',
        black: '#484f58', red: '#ff7b72', green: '#3fb950', yellow: '#d29922',
        blue: '#58a6ff', magenta: '#bc8cff', cyan: '#39c5cf', white: '#b1bac4',
        brightBlack: '#6e7681', brightRed: '#ffa198', brightGreen: '#56d364',
        brightYellow: '#e3b341', brightBlue: '#79c0ff', brightMagenta: '#d2a8ff',
        brightCyan: '#76e3ea', brightWhite: '#ffffff',
      },
    });
    this._fit = new FitAddon.FitAddon();
    this._term.loadAddon(this._fit);
    this._term.open(termContainer);
    // WebGL renderer for heavy-output performance; falls back to canvas if
    // WebGL is unavailable or the context is lost.
    try {
      if (typeof WebglAddon !== 'undefined') {
        const webgl = new WebglAddon.WebglAddon();
        webgl.onContextLoss(() => { try { webgl.dispose(); } catch (e) { /* canvas takes over */ } });
        this._term.loadAddon(webgl);
      }
    } catch (e) { /* canvas renderer fallback */ }
    this._fit.fit();
    // Refit + propagate the PTY size whenever the container changes size.
    // Debounced (~120ms) so a drag-resize doesn't hammer the docker API.
    this._ro = new ResizeObserver(() => {
      clearTimeout(this._resizeTimer);
      this._resizeTimer = setTimeout(() => this.refit(), 120);
    });
    this._ro.observe(termContainer);

    this._setState(TD_STATE.STARTING);
    this._startTicker();
    this._term.onData((data) => this._onInput(data));

    // Wait until the session is running (the "pushed to the page" moment).
    // While waiting, show the backend's live progress (status_msg, e.g.
    // "pulling image ubuntu:24.04") on one updating line so the terminal
    // never looks dead during a slow first pull.
    let lastMsg = null;
    const showProgress = (msg) => {
      if (msg === lastMsg) return;
      lastMsg = msg;
      this._term.write(`\r\x1b[K\x1b[33m⏳ ${msg}…\x1b[0m`);
    };
    for (;;) {
      if (this._closed) return;
      const r = await this._api('GET', `/api/sessions/${this._sessionId}`);
      if (!r.ok) {
        this._term.writeln(`\r\n\x1b[31mFailed to load session: ${(r.data && r.data.error) || 'request failed'}\x1b[0m`);
        this._setState(TD_STATE.ERROR);
        return;
      }
      const st = r.data.status;
      if (st === 'running') break;
      if (st === 'error') {
        this._term.write('\r\x1b[K');
        this._term.writeln(`\x1b[31mSession failed to start: ${r.data.status_msg || 'unknown error'}\x1b[0m`);
        this._setState(TD_STATE.ERROR);
        return;
      }
      if (st === 'stopped') {
        this._term.write('\r\x1b[K');
        this._term.writeln('\x1b[33mSession is stopped. Start it to attach a terminal.\x1b[0m');
        this._setState(TD_STATE.STOPPED);
        return;
      }
      showProgress(r.data.status_msg || st);
      await sleep(2000);
    }
    if (this._closed) return;
    this._term.write('\r\x1b[K'); // clear the progress line
    this._connect();
  }

  /** Detach the websocket and dispose the terminal. Never touches the session. */
  close() {
    this._closed = true;
    clearInterval(this._tickTimer);
    clearTimeout(this._flashTimer);
    clearTimeout(this._resizeTimer);
    if (this._ro) {
      this._ro.disconnect();
      this._ro = null;
    }
    if (this._ws) {
      this._ws.onclose = null;
      try { this._ws.close(); } catch (e) { /* ignore */ }
      this._ws = null;
    }
    if (this._term) {
      this._term.dispose();
      this._term = null;
    }
  }

  refit() {
    if (this._fit) {
      this._fit.fit();
      this._sendResize();
    }
  }

  /* ---------------- websocket ---------------- */

  _connect() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${location.host}/api/sessions/${this._sessionId}/attach`);
    ws.binaryType = 'arraybuffer';
    this._ws = ws;

    ws.onopen = () => {
      this._setState(TD_STATE.WAITING); // shell is (presumably) at a prompt
      this.refit();
    };

    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        let msg;
        try { msg = JSON.parse(ev.data); } catch (e) { return; }
        if (msg.type === 'role') {
          this._role = msg.role;
          this._onRole(msg.role);
          this._renderBadge();
        } else if (msg.type === 'notice') {
          if (this._term) this._term.writeln(`\r\n\x1b[33m[termdock] ${msg.msg}\x1b[0m`);
          this._notify(msg.msg);
        }
        return;
      }
      this._feed(ev.data);
    };

    ws.onclose = () => {
      this._ws = null;
      if (!this._closed && this._term) {
        this._setState(TD_STATE.DISCONNECTED);
        this._term.writeln('\r\n\x1b[31m[disconnected from terminal]\x1b[0m');
      }
    };
    ws.onerror = () => { try { ws.close(); } catch (e) { /* ignore */ } };
  }

  _onInput(data) {
    if (this._role !== 'collaborator') return; // viewers are read-only
    // Precise mode: Enter while waiting => a command started running.
    if (this._sawOsc133 && this._state === TD_STATE.WAITING && data.includes('\r')) {
      this._runningSince = Date.now();
      this._setState(TD_STATE.RUNNING);
    }
    // Keystrokes go out immediately as a binary frame (data plane).
    if (this._ws && this._ws.readyState === WebSocket.OPEN) {
      this._ws.send(this._encoder.encode(data));
    }
  }

  _sendResize() {
    if (!this._ws || this._ws.readyState !== WebSocket.OPEN) return;
    if (this._role !== 'collaborator') return;
    const dims = this._fit.proposeDimensions();
    if (dims && dims.cols > 0 && dims.rows > 0) {
      this._ws.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }));
    }
  }

  /* ---------------- OSC 133 parsing ---------------- */

  // Feed raw PTY bytes: strip OSC 133 D sequences (recording exit codes) and
  // write everything else to xterm. Sequences split across frames are
  // reassembled via this._pending.
  _feed(buf) {
    const text = this._pending + this._decoder.decode(buf, { stream: true });
    let out = '';
    let pos = 0;
    const re = /\x1b\]133;D;(\d+)(\x07|\x1b\\)/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      out += text.slice(pos, m.index);
      this._onCommandDone(parseInt(m[1], 10));
      pos = m.index + m[0].length;
    }
    let rest = text.slice(pos);
    // Hold back a plausible partial "\x1b]133;D;<digits>" prefix so a
    // sequence split across websocket frames is still recognized.
    const escIdx = rest.lastIndexOf('\x1b');
    if (escIdx !== -1) {
      const tail = rest.slice(escIdx);
      if (tail.length <= 24 && /^\x1b(\]133;D;\d*)?$/.test(tail)) {
        out += rest.slice(0, escIdx);
        this._pending = tail;
      } else {
        out += rest;
        this._pending = '';
      }
    } else {
      out += rest;
      this._pending = '';
    }
    if (out && this._term) this._term.write(out);
    this._noteOutput();
  }

  _noteOutput() {
    this._lastOutput = Date.now();
    // Heuristic mode only: any output => running. Precise mode is driven by
    // Enter/D sequences instead (background output shouldn't flip the badge).
    if (!this._sawOsc133 && this._state === TD_STATE.WAITING) {
      this._runningSince = this._lastOutput;
      this._setState(TD_STATE.RUNNING);
    }
  }

  _onCommandDone(exitCode) {
    this._sawOsc133 = true; // precise mode from here on
    this._lastExit = exitCode;
    clearTimeout(this._flashTimer);
    // Briefly flash the exit code, then settle back to the waiting pill.
    this._flashUntil = Date.now() + (exitCode === 0 ? EXIT_OK_FLASH_MS : EXIT_FLASH_MS);
    this._flashTimer = setTimeout(() => {
      this._flashUntil = 0;
      this._renderBadge();
      this._renderTitle();
    }, exitCode === 0 ? EXIT_OK_FLASH_MS : EXIT_FLASH_MS);
    this._setState(TD_STATE.WAITING);
  }

  /* ---------------- state machine / badge ---------------- */

  _setState(s) {
    if (this._state === s) {
      this._renderBadge();
      this._renderTitle();
      return;
    }
    this._state = s;
    if (s === TD_STATE.RUNNING && !this._runningSince) this._runningSince = Date.now();
    if (s !== TD_STATE.RUNNING) this._runningSince = 0;
    this._renderBadge();
    this._renderTitle();
    this._onState(s);
  }

  _startTicker() {
    clearInterval(this._tickTimer);
    this._tickTimer = setInterval(() => {
      if (this._closed) return;
      // Heuristic fallback: quiet for 3s => the shell is waiting for input.
      if (!this._sawOsc133 && this._state === TD_STATE.RUNNING &&
          Date.now() - this._lastOutput > HEURISTIC_IDLE_MS) {
        this._setState(TD_STATE.WAITING);
        return;
      }
      if (this._state === TD_STATE.RUNNING || Date.now() < this._flashUntil) {
        this._renderBadge();
        this._renderTitle();
      }
    }, 1000);
  }

  _renderBadge() {
    if (!this._badgeEl) return;
    const flashing = Date.now() < this._flashUntil;
    let cls = 'td-badge', label;
    if (flashing && this._lastExit !== null) {
      if (this._lastExit === 0) { cls += ' td-exit-ok'; label = '✓'; }
      else { cls += ' td-exit-bad'; label = `✗ exit ${this._lastExit}`; }
    } else {
      switch (this._state) {
        case TD_STATE.STARTING: label = '○ starting…'; break;
        case TD_STATE.WAITING:
          label = this._role === 'viewer' ? '○ view only' : '○ waiting for input';
          break;
        case TD_STATE.RUNNING:
          cls += ' td-running';
          label = `● running ${fmtElapsed(Date.now() - this._runningSince)}`;
          break;
        case TD_STATE.STOPPED: label = '■ stopped'; break;
        case TD_STATE.ERROR: label = '■ error'; cls += ' td-exit-bad'; break;
        case TD_STATE.DISCONNECTED: label = '○ disconnected'; break;
        default: label = '○';
      }
    }
    this._badgeEl.innerHTML = '';
    const span = document.createElement('span');
    span.className = cls;
    span.textContent = label;
    this._badgeEl.appendChild(span);
  }

  _renderTitle() {
    if (!this._updateTitle) return;
    const name = this._titleName;
    const flashing = Date.now() < this._flashUntil;
    let prefix;
    if (flashing && this._lastExit !== null && this._lastExit !== 0) {
      prefix = `(✗ ${this._lastExit})`;
    } else {
      switch (this._state) {
        case TD_STATE.RUNNING: prefix = `(● ${fmtElapsed(Date.now() - this._runningSince)})`; break;
        case TD_STATE.WAITING: prefix = '(○)'; break;
        case TD_STATE.STARTING: prefix = '(…)'; break;
        case TD_STATE.STOPPED:
        case TD_STATE.ERROR: prefix = '(■)'; break;
        default: prefix = '(○)';
      }
    }
    document.title = `${prefix} ${name}`;
  }
}
