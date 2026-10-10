package hub

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second
)

// upgrader is the v1 transport upgrader. permessage-deflate is intentionally
// NOT enabled: compressing the terminal stream adds flush latency on every
// frame, and PTY traffic is latency-sensitive.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	// Auth is enforced via the session cookie before upgrading; the page is
	// same-origin by deployment.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Upgrade performs the websocket handshake for the v1 transport and returns
// the connection as a Stream. The /attach HTTP handler calls this and hands
// the Stream to Hub.Attach.
func Upgrade(w http.ResponseWriter, r *http.Request) (Stream, error) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	s := &wsStream{conn: conn, pingStop: make(chan struct{})}
	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	go s.pingLoop()
	return s, nil
}

// wsStream adapts *websocket.Conn to the Stream interface.
type wsStream struct {
	conn     *websocket.Conn
	wmu      sync.Mutex // gorilla allows one concurrent writer; pings share it
	pingStop chan struct{}
	once     sync.Once
}

func (s *wsStream) Read() (bool, []byte, error) {
	mt, data, err := s.conn.ReadMessage()
	if err != nil {
		return false, nil, err
	}
	return mt == websocket.BinaryMessage, data, nil
}

func (s *wsStream) WriteBinary(p []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return s.conn.WriteMessage(websocket.BinaryMessage, p)
}

func (s *wsStream) WriteText(p []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return s.conn.WriteMessage(websocket.TextMessage, p)
}

func (s *wsStream) Close() error {
	s.once.Do(func() { close(s.pingStop) })
	return s.conn.Close()
}

// pingLoop keeps the connection alive and lets the hub notice dead peers via
// the read deadline. It stops when the stream is closed.
func (s *wsStream) pingLoop() {
	t := time.NewTicker(pingPeriod)
	defer t.Stop()
	for {
		select {
		case <-s.pingStop:
			return
		case <-t.C:
			s.wmu.Lock()
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
			err := s.conn.WriteMessage(websocket.PingMessage, nil)
			s.wmu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
