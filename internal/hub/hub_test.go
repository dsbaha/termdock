package hub

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakePTY is a controllable stand-in for a docker exec PTY.
type fakePTY struct {
	r   *io.PipeReader
	w   *io.PipeWriter
	mu  sync.Mutex
	in  bytes.Buffer
	res []string
}

func newFakePTY() *fakePTY {
	r, w := io.Pipe()
	return &fakePTY{r: r, w: w}
}

func (f *fakePTY) Read(b []byte) (int, error) { return f.r.Read(b) }
func (f *fakePTY) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.in.Write(b)
}
func (f *fakePTY) Close() error {
	f.r.Close()
	f.w.Close()
	return nil
}
func (f *fakePTY) Resize(ctx context.Context, cols, rows uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.res = append(f.res, "resize")
	return nil
}
func (f *fakePTY) Running(ctx context.Context) bool { return true }

func (f *fakePTY) input() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.in.String()
}

// testServer spins up an httptest server whose handler upgrades the websocket
// via the real hub.Upgrade adapter and attaches the resulting Stream to the
// hub. Query params: role, user, term.
func testServer(t *testing.T, h *Hub) (*httptest.Server, func(user, role, term string) *websocket.Conn) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream, err := Upgrade(w, r)
		if err != nil {
			return
		}
		h.Attach(r.Context(), "sess1", "ctr1", "/bin/bash",
			r.URL.Query().Get("term"),
			r.URL.Query().Get("user"), r.URL.Query().Get("role"), stream)
	}))
	dial := func(user, role, term string) *websocket.Conn {
		ws, _, err := websocket.DefaultDialer.Dial(
			"ws"+strings.TrimPrefix(srv.URL, "http")+"/?user="+user+"&role="+role+"&term="+term, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return ws
	}
	return srv, dial
}

// readFrame skips role/notice text frames and returns the first binary frame.
func readBinary(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt == websocket.BinaryMessage {
			return data
		}
	}
}

func TestBroadcastToAllClients(t *testing.T) {
	fpty := newFakePTY()
	h := New(func(ctx context.Context, _, _ string, _, _ uint) (PTYConn, error) {
		return fpty, nil
	}, nil)
	srv, dial := testServer(t, h)
	defer srv.Close()

	collab := dial("u1", "collaborator", "")
	defer collab.Close()
	viewer := dial("u2", "viewer", "")
	defer viewer.Close()

	time.Sleep(200 * time.Millisecond) // let both attach
	if n := h.NumClients("sess1"); n != 2 {
		t.Fatalf("NumClients = %d, want 2", n)
	}

	if _, err := fpty.w.Write([]byte("hello-pty")); err != nil {
		t.Fatal(err)
	}
	if got := readBinary(t, collab); string(got) != "hello-pty" {
		t.Fatalf("collaborator got %q", got)
	}
	if got := readBinary(t, viewer); string(got) != "hello-pty" {
		t.Fatalf("viewer got %q", got)
	}
}

func TestInputOnlyFromCollaborators(t *testing.T) {
	fpty := newFakePTY()
	h := New(func(ctx context.Context, _, _ string, _, _ uint) (PTYConn, error) {
		return fpty, nil
	}, nil)
	srv, dial := testServer(t, h)
	defer srv.Close()

	collab := dial("u1", "collaborator", "")
	defer collab.Close()
	viewer := dial("u2", "viewer", "")
	defer viewer.Close()
	time.Sleep(200 * time.Millisecond)

	if err := viewer.WriteMessage(websocket.BinaryMessage, []byte("VIEWER-DATA")); err != nil {
		t.Fatal(err)
	}
	if err := collab.WriteMessage(websocket.BinaryMessage, []byte("COLLAB-DATA")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && fpty.input() != "COLLAB-DATA" {
		time.Sleep(20 * time.Millisecond)
	}
	if got := fpty.input(); got != "COLLAB-DATA" {
		t.Fatalf("pty received %q, want only collaborator input", got)
	}
}

func TestResizeOnlyFromCollaborators(t *testing.T) {
	fpty := newFakePTY()
	h := New(func(ctx context.Context, _, _ string, _, _ uint) (PTYConn, error) {
		return fpty, nil
	}, nil)
	srv, dial := testServer(t, h)
	defer srv.Close()

	collab := dial("u1", "collaborator", "")
	defer collab.Close()
	viewer := dial("u2", "viewer", "")
	defer viewer.Close()
	time.Sleep(200 * time.Millisecond)

	_ = viewer.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":100,"rows":40}`))
	time.Sleep(300 * time.Millisecond)
	fpty.mu.Lock()
	n := len(fpty.res)
	fpty.mu.Unlock()
	if n != 0 {
		t.Fatalf("viewer resize applied %d times, want 0", n)
	}
	_ = collab.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":100,"rows":40}`))
	time.Sleep(300 * time.Millisecond)
	fpty.mu.Lock()
	n = len(fpty.res)
	fpty.mu.Unlock()
	if n != 1 {
		t.Fatalf("collaborator resize applied %d times, want 1", n)
	}
}

func TestCloseUserRevokesOnlyThatUser(t *testing.T) {
	fpty := newFakePTY()
	h := New(func(ctx context.Context, _, _ string, _, _ uint) (PTYConn, error) {
		return fpty, nil
	}, nil)
	srv, dial := testServer(t, h)
	defer srv.Close()

	collab := dial("u1", "collaborator", "")
	defer collab.Close()
	viewer := dial("u2", "viewer", "")
	defer viewer.Close()
	time.Sleep(200 * time.Millisecond)

	h.CloseUser("sess1", "u2", "share revoked")

	// Viewer socket should die.
	viewerClosed := make(chan struct{})
	go func() {
		defer close(viewerClosed)
		_ = viewer.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := viewer.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case <-viewerClosed:
	case <-time.After(6 * time.Second):
		t.Fatal("viewer socket was not closed after revoke")
	}

	// Collaborator must still be attached.
	if n := h.NumClients("sess1"); n != 1 {
		t.Fatalf("NumClients = %d, want 1 (collaborator survives)", n)
	}
	if _, err := fpty.w.Write([]byte("still-alive")); err != nil {
		t.Fatal(err)
	}
	if got := readBinary(t, collab); string(got) != "still-alive" {
		t.Fatalf("collaborator got %q after revoke", got)
	}
}

func TestOnInputCallback(t *testing.T) {
	fpty := newFakePTY()
	var mu sync.Mutex
	var hits []string
	h := New(func(ctx context.Context, _, _ string, _, _ uint) (PTYConn, error) {
		return fpty, nil
	}, func(sessionID string) {
		mu.Lock()
		hits = append(hits, sessionID)
		mu.Unlock()
	})
	srv, dial := testServer(t, h)
	defer srv.Close()

	collab := dial("u1", "collaborator", "")
	defer collab.Close()
	time.Sleep(200 * time.Millisecond)

	_ = collab.WriteMessage(websocket.BinaryMessage, []byte("x"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(hits)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 || hits[0] != "sess1" {
		t.Fatalf("onInput hits = %v, want [sess1]", hits)
	}
}

func TestRingBufferReplayedToLateAttacher(t *testing.T) {
	fpty := newFakePTY()
	h := New(func(ctx context.Context, _, _ string, _, _ uint) (PTYConn, error) {
		return fpty, nil
	}, nil)
	srv, dial := testServer(t, h)
	defer srv.Close()

	// First client attaches and receives live output.
	first := dial("u1", "collaborator", "")
	time.Sleep(200 * time.Millisecond)
	if _, err := fpty.w.Write([]byte("scrollback-line-1\nscrollback-line-2\n")); err != nil {
		t.Fatal(err)
	}
	if got := readBinary(t, first); !strings.Contains(string(got), "scrollback-line-2") {
		t.Fatalf("first client got %q", got)
	}
	// Detach; the exec (and its ring buffer) stays alive with zero viewers.
	first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.NumClients("sess1") != 0 {
		time.Sleep(20 * time.Millisecond)
	}

	// Late attacher must get the replayed scrollback, not a blank screen.
	ws := dial("u2", "collaborator", "")
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var sawReplay bool
	for i := 0; i < 4 && !sawReplay; i++ {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt == websocket.BinaryMessage && strings.Contains(string(data), "scrollback-line-2") {
			sawReplay = true
		}
	}
	if !sawReplay {
		t.Fatal("late attacher did not receive replayed scrollback")
	}
	if got := h.LastOutputAt("sess1"); got <= 0 {
		t.Fatalf("LastOutputAt = %d, want > 0", got)
	}
}

func TestLastOutputAtZeroForUnknownSession(t *testing.T) {
	h := New(nil, nil)
	if got := h.LastOutputAt("nope"); got != 0 {
		t.Fatalf("LastOutputAt = %d, want 0", got)
	}
	if n := h.NumClients("nope"); n != 0 {
		t.Fatalf("NumClients = %d, want 0", n)
	}
}

func TestTerminalsAreIndependentPTYs(t *testing.T) {
	ptys := make(map[string]*fakePTY)
	var mu sync.Mutex
	// The open func doesn't know the termID; return a fresh PTY per call and
	// record them in order. Main is opened first, then the extra term.
	h := New(func(ctx context.Context, _, _ string, _, _ uint) (PTYConn, error) {
		mu.Lock()
		defer mu.Unlock()
		f := newFakePTY()
		ptys[string(rune('a'+len(ptys)))] = f
		return f, nil
	}, nil)
	srv, dial := testServer(t, h)
	defer srv.Close()

	mainWS := dial("u1", "collaborator", "main")
	defer mainWS.Close()
	extraWS := dial("u1", "collaborator", "t-xyz")
	defer extraWS.Close()
	time.Sleep(300 * time.Millisecond)

	if n := h.NumClients("sess1"); n != 2 {
		t.Fatalf("NumClients = %d, want 2 across both terminals", n)
	}
	mu.Lock()
	if len(ptys) != 2 {
		mu.Unlock()
		t.Fatalf("got %d PTYs, want 2 (one per terminal)", len(ptys))
	}
	var pa, pb *fakePTY
	for _, f := range ptys {
		if pa == nil {
			pa = f
		} else {
			pb = f
		}
	}
	mu.Unlock()

	// Input on the main terminal must not reach the extra terminal's PTY.
	if err := mainWS.WriteMessage(websocket.BinaryMessage, []byte("MAIN-DATA")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && pa.input()+pb.input() == "" {
		time.Sleep(20 * time.Millisecond)
	}
	if got := pa.input() + pb.input(); got != "MAIN-DATA" {
		t.Fatalf("pty input = %q, want exactly MAIN-DATA on one pty", got)
	}
}

func TestEphemeralTermShutsDownWhenShellExits(t *testing.T) {
	h := New(func(ctx context.Context, _, _ string, _, _ uint) (PTYConn, error) {
		f := newFakePTY()
		f.w.Close() // EOF immediately: shell "exited" with no output
		return f, nil
	}, nil)
	srv, dial := testServer(t, h)
	defer srv.Close()

	ws := dial("u1", "collaborator", "t-eph")
	defer ws.Close()
	// Expect a term-closed control frame telling the tab to close.
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	sawClosed := false
	for !sawClosed {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt == websocket.TextMessage && strings.Contains(string(data), "term-closed") {
			sawClosed = true
		}
	}
	// The terminal should be gone from the hub afterwards.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.NumClients("sess1") != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := h.NumClients("sess1"); n != 0 {
		t.Fatalf("NumClients = %d after ephemeral term exited, want 0", n)
	}
}
