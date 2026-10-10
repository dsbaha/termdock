// Package hub multiplexes one interactive `docker exec` PTY per running
// session to any number of browser websockets.
//
// Design note: instead of running gotty/ttyd inside every container, the
// backend holds a single `docker exec -it <shell>` PTY per session and
// broadcasts its output to all attached sockets. Input is accepted only from
// sockets whose holder has the collaborator role; viewers get a read-only
// stream. This means images need no modification, auth stays central, and
// sharing/revoke is just socket management.
//
// Lifecycle: closing a browser tab or popup window only detaches its
// websocket — the exec keeps running even with zero viewers. The last 64KB of
// PTY output is kept in a per-session ring buffer and replayed to the next
// attacher, so re-attaching never lands on a blank screen. Output activity is
// timestamped so the idle reaper can tell a busy session from an idle one.
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
	maxExecRetries = 5
	readBufSize    = 32 * 1024
	ringBufSize    = 64 * 1024 // scrollback replayed to a client that (re)attaches
)

// Hub tracks the live exec hub of every session with attached browsers.
type Hub struct {
	mu      sync.Mutex
	hubs    map[string]*sessionHub
	open    OpenFunc
	onInput func(sessionID string) // called when collaborator input arrives
}

// New creates a Hub. open creates exec PTYs; onInput marks a session active.
func New(open OpenFunc, onInput func(sessionID string)) *Hub {
	return &Hub{
		hubs:    make(map[string]*sessionHub),
		open:    open,
		onInput: onInput,
	}
}

// Attach adds an already-authorized client stream to the session's broadcast
// group. The caller must have authorized userID/role already (e.g. via the
// /attach handler upgrading the websocket into a Stream). It returns when the
// stream closes.
func (h *Hub) Attach(ctx context.Context, sessionID, containerID, shell, userID, role string, stream Stream) {
	sh := h.getOrCreate(sessionID, containerID, shell)
	c := &client{
		stream: stream,
		userID: userID,
		role:   role,
		send:   make(chan wsOut, 64),
		done:   make(chan struct{}),
	}
	sh.add(c)
	go c.writePump()
	c.readPump(ctx, sh) // blocks until the socket closes
	sh.remove(c)
}

func (h *Hub) getOrCreate(sessionID, containerID, shell string) *sessionHub {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.hubs[sessionID]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		sh = &sessionHub{
			hub:         h,
			sessionID:   sessionID,
			containerID: containerID,
			shell:       shell,
			ctx:         ctx,
			cancel:      cancel,
			clients:     make(map[*client]struct{}),
			cols:        80,
			rows:        24,
		}
		h.hubs[sessionID] = sh
		go sh.run()
	}
	return sh
}

func (h *Hub) get(sessionID string) *sessionHub {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hubs[sessionID]
}

// CloseSession tears down the exec and closes every attached socket.
func (h *Hub) CloseSession(sessionID, notice string) {
	h.mu.Lock()
	sh, ok := h.hubs[sessionID]
	if ok {
		delete(h.hubs, sessionID)
	}
	h.mu.Unlock()
	if ok {
		sh.shutdown(notice)
	}
}

// CloseUser closes all sockets of userID on the session (share revocation).
func (h *Hub) CloseUser(sessionID, userID, notice string) {
	if sh := h.get(sessionID); sh != nil {
		sh.closeUser(userID, notice)
	}
}

// NumClients returns the number of attached sockets for a session.
func (h *Hub) NumClients(sessionID string) int {
	if sh := h.get(sessionID); sh != nil {
		return sh.count()
	}
	return 0
}

// LastOutputAt returns the Unix time of the most recent PTY output for the
// session, or 0 if the hub has never seen output for it. The idle reaper uses
// this so a session that is still producing output is never considered idle.
func (h *Hub) LastOutputAt(sessionID string) int64 {
	if sh := h.get(sessionID); sh != nil {
		return sh.lastOutputAt()
	}
	return 0
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
func (c *client) readPump(ctx context.Context, sh *sessionHub) {
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
			if pty := sh.currentPTY(); pty != nil {
				_, _ = pty.Write(data)
			}
			if sh.hub.onInput != nil {
				sh.hub.onInput(sh.sessionID)
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
			sh.resize(ctx, msg.Cols, msg.Rows)
		}
	}
}

// sessionHub owns the exec PTY lifecycle and client set for one session.
type sessionHub struct {
	hub         *Hub
	sessionID   string
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

func (s *sessionHub) add(c *client) {
	s.mu.Lock()
	s.clients[c] = struct{}{}
	replay := make([]byte, len(s.ring))
	copy(replay, s.ring)
	s.mu.Unlock()
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

func (s *sessionHub) remove(c *client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.close()
}

func (s *sessionHub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

func (s *sessionHub) lastOutputAt() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOutput
}

// appendRing records output bytes in the replay buffer, keeping the newest
// ringBufSize bytes. Caller must hold s.mu.
func (s *sessionHub) appendRing(b []byte) {
	s.ring = append(s.ring, b...)
	if len(s.ring) > ringBufSize {
		s.ring = s.ring[len(s.ring)-ringBufSize:]
	}
	s.lastOutput = time.Now().Unix()
}

func (s *sessionHub) currentPTY() PTYConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pty
}

func (s *sessionHub) setPTY(p PTYConn) {
	s.mu.Lock()
	s.pty = p
	s.mu.Unlock()
}

// resize applies new dimensions to the live exec (collaborators only) and
// remembers them so the next exec starts at the right size.
func (s *sessionHub) resize(ctx context.Context, cols, rows uint) {
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	pty := s.pty
	s.mu.Unlock()
	if pty != nil {
		_ = pty.Resize(ctx, cols, rows)
	}
}

func (s *sessionHub) noticeAll(msg string) {
	out := noticeMsg(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		select {
		case c.send <- out:
		default:
			// Slow consumer: drop it rather than blocking the broadcast.
			go s.remove(c)
		}
	}
}

func (s *sessionHub) closeUser(userID, notice string) {
	s.mu.Lock()
	var targets []*client
	for c := range s.clients {
		if c.userID == userID {
			targets = append(targets, c)
		}
	}
	s.mu.Unlock()
	out := noticeMsg(notice)
	for _, c := range targets {
		select {
		case c.send <- out:
		default:
		}
		// Small delay so the notice frame flushes before the socket closes.
		time.Sleep(50 * time.Millisecond)
		s.remove(c)
	}
}

// shutdown stops the exec loop and closes every client socket.
func (s *sessionHub) shutdown(notice string) {
	s.noticeAll(notice)
	s.cancel()
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.pty = nil
	s.mu.Unlock()
	// Give notices a beat to flush, then close sockets.
	time.Sleep(100 * time.Millisecond)
	for _, c := range clients {
		s.remove(c)
	}
}

// run keeps exactly one exec PTY alive, restarting it if the shell exits.
func (s *sessionHub) run() {
	failures := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		s.mu.Lock()
		cols, rows := s.cols, s.rows
		s.mu.Unlock()

		pty, err := s.hub.open(s.ctx, s.containerID, s.shell, cols, rows)
		if err != nil {
			failures++
			s.noticeAll(fmt.Sprintf("could not start shell (attempt %d/%d): %v", failures, maxExecRetries, err))
			if failures >= maxExecRetries {
				s.noticeAll("terminal process failed to start; close and reopen the tab to retry")
				s.shutdown("terminal unavailable")
				s.hub.mu.Lock()
				delete(s.hub.hubs, s.sessionID)
				s.hub.mu.Unlock()
				return
			}
			if !sleepCtx(s.ctx, time.Second) {
				return
			}
			continue
		}
		failures = 0
		s.setPTY(pty)
		gotOutput := s.pump(pty)
		s.setPTY(nil)
		_ = pty.Close()

		select {
		case <-s.ctx.Done():
			return
		default:
		}
		if gotOutput {
			s.noticeAll("shell exited — restarting…")
		}
		if !sleepCtx(s.ctx, time.Second) {
			return
		}
	}
}

// pump broadcasts PTY output to all clients until EOF. It reports whether any
// output was seen (used to reset the restart-failure counter semantics).
func (s *sessionHub) pump(pty PTYConn) bool {
	buf := make([]byte, readBufSize)
	gotOutput := false
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			gotOutput = true
			frame := make([]byte, n)
			copy(frame, buf[:n])
			out := wsOut{binary: true, data: frame}
			s.mu.Lock()
			s.appendRing(frame) // recorded even with zero viewers
			for c := range s.clients {
				select {
				case c.send <- out:
				default:
					go s.remove(c) // slow consumer
				}
			}
			s.mu.Unlock()
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
