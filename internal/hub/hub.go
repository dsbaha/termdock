// Package hub multiplexes interactive `docker exec` PTYs to browser
// websockets.
//
// Design note: instead of running gotty/ttyd inside every container, the
// backend holds `docker exec -it <shell>` PTYs per session and broadcasts
// their output to attached sockets. Input is accepted only from sockets
// whose holder has the collaborator role; viewers get a read-only stream.
// This means images need no modification, auth stays central, and
// sharing/revoke is just socket management.
//
// Terminal model: a session (container) has one or more terminals. The
// "main" terminal is persistent — its shell survives with zero viewers and
// restarts if it exits, so closing a tab only detaches. Extra terminals
// (one per additional tab) are ephemeral: when their shell exits or their
// last viewer leaves, the exec is torn down. The last 64KB of PTY output
// per terminal is replayed to the next attacher. Output activity is
// timestamped so the idle reaper can tell a busy session from an idle one.
//
// Lifecycle: closing a browser tab or popup window only detaches its
// websocket — the exec keeps running even with zero viewers (main terminal).
//
// Transport: the hub never touches network framing directly — every attached
// client is a Stream (see stream.go). WebSocket is the v1 transport
// (transport_websocket.go); a WebTransport implementation could be swapped in
// later without touching this file.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"sync"
	"time"
)

// PTYConn is an attached interactive exec session. *dockerx.PTY satisfies it;
// tests use a fake.
type PTYConn interface {
	io.Reader
	io.Writer
	Close() error
	Resize(ctx context.Context, cols, rows uint) error
	Running(ctx context.Context) bool
}

// OpenFunc creates a new interactive exec PTY in the given container.
type OpenFunc func(ctx context.Context, containerID, shell string, cols, rows uint) (PTYConn, error)

const (
	// MainTermID is the persistent terminal every session starts with. Extra
	// terminals get client-generated IDs.
	MainTermID     = "main"
	maxExecRetries = 5
	readBufSize    = 32 * 1024
	ringBufSize    = 64 * 1024 // scrollback replayed to a client that (re)attaches
)

// Hub tracks the live exec hubs of every session with attached browsers.
type Hub struct {
	mu       sync.Mutex
	sessions map[string]*sessionHub // key: sessionID
	open     OpenFunc
	onInput  func(sessionID string) // called when collaborator input arrives
}

// New creates a Hub. open creates exec PTYs; onInput marks a session active.
func New(open OpenFunc, onInput func(sessionID string)) *Hub {
	return &Hub{
		sessions: make(map[string]*sessionHub),
		open:     open,
		onInput:  onInput,
	}
}

// Attach adds an already-authorized client stream to a terminal's broadcast
// group. The caller must have authorized userID/role already (e.g. via the
// /attach handler upgrading the websocket into a Stream). An empty termID
// means the main terminal. It returns when the stream closes.
func (h *Hub) Attach(ctx context.Context, sessionID, containerID, shell, termID, userID, role string, stream Stream) {
	if termID == "" {
		termID = MainTermID
	}
	sh := h.getOrCreateSession(sessionID, containerID, shell)
	th := sh.getOrCreateTerm(termID)
	c := &client{
		stream: stream,
		userID: userID,
		role:   role,
		send:   make(chan wsOut, 64),
		done:   make(chan struct{}),
	}
	th.add(c)
	go c.writePump()
	c.readPump(ctx, th) // blocks until the socket closes
	th.remove(c)
}

func (h *Hub) getOrCreateSession(sessionID, containerID, shell string) *sessionHub {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.sessions[sessionID]
	if !ok {
		sh = &sessionHub{
			hub:       h,
			sessionID: sessionID,
			terms:     make(map[string]*termHub),
			nextSeq:   1, // main is seq 1; extras take 2, 3, …
		}
		h.sessions[sessionID] = sh
	}
	// Refresh the container/shell: after a stop+resume the container ID
	// changes, and subsequently created terminals must exec into the new one.
	sh.mu.Lock()
	sh.containerID, sh.shell = containerID, shell
	sh.mu.Unlock()
	return sh
}

func (h *Hub) get(sessionID string) *sessionHub {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[sessionID]
}

// CloseSession tears down every terminal of the session and closes all
// attached sockets.
func (h *Hub) CloseSession(sessionID, notice string) {
	h.mu.Lock()
	sh, ok := h.sessions[sessionID]
	if ok {
		delete(h.sessions, sessionID)
	}
	h.mu.Unlock()
	if ok {
		sh.shutdownAll(notice)
	}
}

// CloseUser closes all sockets of userID on the session (share revocation).
func (h *Hub) CloseUser(sessionID, userID, notice string) {
	h.mu.Lock()
	sh := h.sessions[sessionID]
	h.mu.Unlock()
	if sh == nil {
		return
	}
	for _, th := range sh.termList() {
		th.closeUser(userID, notice)
	}
}

// NumClients returns the number of attached sockets across all terminals of
// the session.
func (h *Hub) NumClients(sessionID string) int {
	sh := h.get(sessionID)
	if sh == nil {
		return 0
	}
	n := 0
	for _, th := range sh.termList() {
		n += th.count()
	}
	return n
}

// LastOutputAt returns the Unix time of the most recent PTY output across all
// terminals of the session, or 0 if the hub has never seen output for it.
// The idle reaper uses this so a session that is still producing output is
// never considered idle.
func (h *Hub) LastOutputAt(sessionID string) int64 {
	sh := h.get(sessionID)
	if sh == nil {
		return 0
	}
	var latest int64
	for _, th := range sh.termList() {
		if t := th.lastOutputAt(); t > latest {
			latest = t
		}
	}
	return latest
}

// TermInfo describes one live terminal of a session.
type TermInfo struct {
	ID      string `json:"id"`
	Seq     int    `json:"seq"`     // 1 for the main terminal, 2,3,… for extras
	Clients int    `json:"clients"` // attached sockets
}

// ListTerms returns the session's live terminals ordered by seq. It returns
// nil when the session has no hub entry.
func (h *Hub) ListTerms(sessionID string) []TermInfo {
	sh := h.get(sessionID)
	if sh == nil {
		return nil
	}
	terms := sh.termList()
	out := make([]TermInfo, 0, len(terms))
	for _, th := range terms {
		out = append(out, TermInfo{ID: th.termID, Seq: th.seq, Clients: th.count()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// wsOut is one outbound frame: binary=true for PTY bytes, false for JSON
// control/notice frames.
type wsOut struct {
	binary bool
	data   []byte
}

func noticeMsg(msg string) wsOut {
	b, _ := json.Marshal(map[string]string{"type": "notice", "msg": msg})
	return wsOut{data: b}
}

func roleMsg(role string) wsOut {
	b, _ := json.Marshal(map[string]string{"type": "role", "role": role})
	return wsOut{data: b}
}

// termClosedMsg tells the browser that an ephemeral terminal's shell exited
// and the tab should close (no reconnect).
func termClosedMsg() wsOut {
	b, _ := json.Marshal(map[string]string{"type": "term-closed"})
	return wsOut{data: b}
}

// client is one attached stream.
type client struct {
	stream Stream
	userID string
	role   string
	send   chan wsOut
	done   chan struct{}
	once   sync.Once
}

func (c *client) close() {
	c.once.Do(func() { close(c.done); _ = c.stream.Close() })
}

// writePump forwards queued frames to the client. Transport-level keepalive
// (websocket pings) lives in the Stream implementation, not here.
func (c *client) writePump() {
	for {
		select {
		case <-c.done:
			return
		case out := <-c.send:
			var err error
			if out.binary {
				err = c.stream.WriteBinary(out.data)
			} else {
				err = c.stream.WriteText(out.data)
			}
			if err != nil {
				c.close()
				return
			}
		}
	}
}

// readPump handles inbound client frames until the stream closes.
func (c *client) readPump(ctx context.Context, th *termHub) {
	defer c.close()
	for {
		binary, data, err := c.stream.Read()
		if err != nil {
			return
		}
		if binary {
			// PTY input bytes, forwarded immediately (collaborators only).
			if c.role != "collaborator" || len(data) == 0 {
				continue
			}
			if pty := th.currentPTY(); pty != nil {
				_, _ = pty.Write(data)
			}
			if th.session.hub.onInput != nil {
				th.session.hub.onInput(th.session.sessionID)
			}
			continue
		}
		var msg struct {
			Type string `json:"type"`
			Cols uint   `json:"cols"`
			Rows uint   `json:"rows"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Type == "resize" {
			if c.role != "collaborator" || msg.Cols == 0 || msg.Rows == 0 {
				continue
			}
			th.resize(ctx, msg.Cols, msg.Rows)
		}
	}
}

// sessionHub groups the terminals of one session (container).
type sessionHub struct {
	hub       *Hub
	sessionID string

	mu          sync.Mutex
	containerID string
	shell       string
	terms       map[string]*termHub // key: termID
	nextSeq     int                 // next seq for an extra terminal (main is always 1)
}

func (s *sessionHub) getOrCreateTerm(termID string) *termHub {
	s.mu.Lock()
	defer s.mu.Unlock()
	if th, ok := s.terms[termID]; ok {
		return th
	}
	ctx, cancel := context.WithCancel(context.Background())
	// The main terminal is always seq 1; extras take 2, 3, … in creation
	// order, so tab labels stay stable no matter which attaches first.
	seq := 1
	if termID != MainTermID {
		s.nextSeq++
		seq = s.nextSeq
	}
	th := &termHub{
		session:     s,
		termID:      termID,
		seq:         seq,
		persistent:  termID == MainTermID,
		containerID: s.containerID,
		shell:       s.shell,
		ctx:         ctx,
		cancel:      cancel,
		clients:     make(map[*client]struct{}),
		cols:        80,
		rows:        24,
	}
	s.terms[termID] = th
	go th.run()
	return th
}

// termList returns a snapshot of the session's terminals.
func (s *sessionHub) termList() []*termHub {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*termHub, 0, len(s.terms))
	for _, th := range s.terms {
		out = append(out, th)
	}
	return out
}

// shutdownAll tears down every terminal of the session.
func (s *sessionHub) shutdownAll(notice string) {
	s.mu.Lock()
	terms := make([]*termHub, 0, len(s.terms))
	for _, th := range s.terms {
		terms = append(terms, th)
	}
	s.terms = make(map[string]*termHub)
	s.mu.Unlock()
	for _, th := range terms {
		th.shutdown(notice)
	}
}

// termHub owns the exec PTY lifecycle and client set for one terminal.
type termHub struct {
	session     *sessionHub
	termID      string
	seq         int  // display order: 1 = main, 2,3,… = extras
	persistent  bool // main terminal: survives zero viewers, shell restarts
	containerID string
	shell       string
	ctx         context.Context
	cancel      context.CancelFunc

	mu      sync.Mutex
	clients map[*client]struct{}
	pty     PTYConn
	cols    uint
	rows    uint
	// ring holds the last ringBufSize bytes of PTY output so a client that
	// (re)attaches doesn't land on a blank screen. lastOutput is the Unix
	// time output was last seen (for the idle reaper).
	ring       []byte
	lastOutput int64
}

func (t *termHub) add(c *client) {
	t.mu.Lock()
	t.clients[c] = struct{}{}
	replay := make([]byte, len(t.ring))
	copy(replay, t.ring)
	t.mu.Unlock()
	// Tell the browser its role right away so the UI can show "view only",
	// then replay recent scrollback before any live frames.
	select {
	case c.send <- roleMsg(c.role):
	default:
	}
	if len(replay) > 0 {
		select {
		case c.send <- wsOut{binary: true, data: replay}:
		default:
		}
	}
}

func (t *termHub) remove(c *client) {
	t.mu.Lock()
	delete(t.clients, c)
	empty := len(t.clients) == 0
	t.mu.Unlock()
	c.close()
	if empty && !t.persistent {
		// Ephemeral terminal with no viewers left: tear down the exec so
		// abandoned extra tabs don't leak shells.
		t.shutdownTerm("")
	}
}

func (t *termHub) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.clients)
}

func (t *termHub) lastOutputAt() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastOutput
}

// appendRing records output bytes in the replay buffer, keeping the newest
// ringBufSize bytes. Caller must hold t.mu.
func (t *termHub) appendRing(b []byte) {
	t.ring = append(t.ring, b...)
	if len(t.ring) > ringBufSize {
		t.ring = t.ring[len(t.ring)-ringBufSize:]
	}
	t.lastOutput = time.Now().Unix()
}

func (t *termHub) currentPTY() PTYConn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pty
}

func (t *termHub) setPTY(p PTYConn) {
	t.mu.Lock()
	t.pty = p
	t.mu.Unlock()
}

// resize applies new dimensions to the live exec (collaborators only) and
// remembers them so the next exec starts at the right size.
func (t *termHub) resize(ctx context.Context, cols, rows uint) {
	t.mu.Lock()
	t.cols, t.rows = cols, rows
	pty := t.pty
	t.mu.Unlock()
	if pty != nil {
		_ = pty.Resize(ctx, cols, rows)
	}
}

func (t *termHub) noticeAll(msg string) {
	out := noticeMsg(msg)
	t.mu.Lock()
	defer t.mu.Unlock()
	for c := range t.clients {
		select {
		case c.send <- out:
		default:
			// Slow consumer: drop it rather than blocking the broadcast.
			go t.remove(c)
		}
	}
}

func (t *termHub) closeUser(userID, notice string) {
	t.mu.Lock()
	var targets []*client
	for c := range t.clients {
		if c.userID == userID {
			targets = append(targets, c)
		}
	}
	t.mu.Unlock()
	out := noticeMsg(notice)
	for _, c := range targets {
		select {
		case c.send <- out:
		default:
		}
		// Small delay so the notice frame flushes before the socket closes.
		time.Sleep(50 * time.Millisecond)
		t.remove(c)
	}
}

// shutdown stops the exec loop and closes every client socket.
func (t *termHub) shutdown(notice string) {
	if notice != "" {
		t.noticeAll(notice)
	}
	t.cancel()
	t.mu.Lock()
	clients := make([]*client, 0, len(t.clients))
	for c := range t.clients {
		clients = append(clients, c)
	}
	t.pty = nil
	t.mu.Unlock()
	// Give notices a beat to flush, then close sockets.
	time.Sleep(100 * time.Millisecond)
	for _, c := range clients {
		t.remove(c)
	}
}

// shutdownTerm removes the terminal from its session and shuts it down.
// Used for ephemeral terminals (shell exited, or last viewer left).
func (t *termHub) shutdownTerm(notice string) {
	t.session.mu.Lock()
	if cur, ok := t.session.terms[t.termID]; ok && cur == t {
		delete(t.session.terms, t.termID)
	}
	t.session.mu.Unlock()
	t.shutdown(notice)
}

// run keeps exactly one exec PTY alive for a persistent terminal, restarting
// it if the shell exits. For an ephemeral terminal the shell exiting ends
// the terminal: clients are told to close the tab.
func (t *termHub) run() {
	failures := 0
	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}
		t.mu.Lock()
		cols, rows := t.cols, t.rows
		t.mu.Unlock()

		pty, err := t.session.hub.open(t.ctx, t.containerID, t.shell, cols, rows)
		if err != nil {
			failures++
			log.Printf("hub: session %s term %s exec open failed (attempt %d): %v",
				t.session.sessionID, t.termID, failures, err)
			t.noticeAll(fmt.Sprintf("could not start shell (attempt %d): %v", failures, err))
			// A freshly (re)started container can take a moment before it
			// accepts exec; back off between attempts (2s,4s,… capped at
			// 10s) instead of failing fast.
			if failures >= maxExecRetries {
				t.noticeAll("terminal process failed to start; use the Reconnect button to retry")
				t.shutdownTerm("terminal unavailable")
				return
			}
			backoff := time.Duration(failures) * 2 * time.Second
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			if !sleepCtx(t.ctx, backoff) {
				return
			}
			continue
		}
		failures = 0
		t.setPTY(pty)
		gotOutput := t.pump(pty)
		t.setPTY(nil)
		_ = pty.Close()

		select {
		case <-t.ctx.Done():
			return
		default:
		}
		if t.persistent {
			if gotOutput {
				t.noticeAll("shell exited — restarting…")
			}
			if !sleepCtx(t.ctx, time.Second) {
				return
			}
			continue
		}
		// Ephemeral terminal: shell exited, terminal is done.
		t.mu.Lock()
		for c := range t.clients {
			select {
			case c.send <- termClosedMsg():
			default:
			}
		}
		t.mu.Unlock()
		t.shutdownTerm("")
		return
	}
}

// pump broadcasts PTY output to all clients until EOF. It reports whether any
// output was seen (used to reset the restart-failure counter semantics).
func (t *termHub) pump(pty PTYConn) bool {
	buf := make([]byte, readBufSize)
	gotOutput := false
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			gotOutput = true
			frame := make([]byte, n)
			copy(frame, buf[:n])
			out := wsOut{binary: true, data: frame}
			t.mu.Lock()
			t.appendRing(frame) // recorded even with zero viewers
			for c := range t.clients {
				select {
				case c.send <- out:
				default:
					go t.remove(c) // slow consumer
				}
			}
			t.mu.Unlock()
		}
		if err != nil {
			return gotOutput
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
