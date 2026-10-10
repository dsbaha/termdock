package hub

// Stream is the transport-agnostic byte stream between the hub and one
// attached client.
//
// WebSocket is the v1 transport (see transport_websocket.go). A future
// WebTransport-over-QUIC transport would implement this same interface, and
// the hub would not change: it only ever speaks Stream.
//
// Framing contract (mirrors the websocket mapping):
//   - Read reports each inbound frame: binary=true carries raw PTY input
//     bytes (data plane); binary=false carries a JSON control message
//     (currently only {"type":"resize","cols":n,"rows":n}).
//   - WriteBinary sends raw PTY output bytes to the client.
//   - WriteText sends a JSON notice/role frame to the client.
//
// Implementations must be safe for one concurrent reader and one concurrent
// writer (the hub runs its read and write pumps on separate goroutines),
// and Close must be idempotent.
type Stream interface {
	Read() (binary bool, p []byte, err error)
	WriteBinary([]byte) error
	WriteText([]byte) error
	Close() error
}
