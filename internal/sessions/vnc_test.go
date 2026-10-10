package sessions

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestFrameClientMsg(t *testing.T) {
	// PointerEvent: 6 bytes, forwarded.
	msg := []byte{5, 1, 0x12, 0x34, 0x56, 0x78}
	typ, full, rest, ok := frameClientMsg(msg)
	if !ok || typ != 5 || !bytes.Equal(full, msg) || len(rest) != 0 {
		t.Fatalf("pointer event: got typ=%d ok=%v full=%v rest=%v", typ, ok, full, rest)
	}
	// KeyEvent: 8 bytes.
	typ, full, _, ok = frameClientMsg([]byte{4, 1, 0, 0, 0, 0, 0, 0x61})
	if !ok || typ != 4 || len(full) != 8 {
		t.Fatalf("key event: got typ=%d ok=%v len=%d", typ, ok, len(full))
	}
	// SetEncodings: 4 + 4*count.
	enc := []byte{2, 0, 0, 2, 0, 0, 0, 1, 0, 0, 0, 5}
	typ, full, rest, ok = frameClientMsg(enc)
	if !ok || typ != 2 || len(full) != 12 || len(rest) != 0 {
		t.Fatalf("setEncodings: got typ=%d ok=%v len=%d", typ, ok, len(full))
	}
	// ClientCutText with length prefix.
	cut := make([]byte, 8+5)
	cut[0] = 6
	binary.BigEndian.PutUint32(cut[4:8], 5)
	copy(cut[8:], "hello")
	typ, full, _, ok = frameClientMsg(cut)
	if !ok || typ != 6 || len(full) != 13 {
		t.Fatalf("cuttext: got typ=%d ok=%v len=%d", typ, ok, len(full))
	}
	// Split message: not enough bytes yet.
	typ, _, rest, ok = frameClientMsg([]byte{5, 1, 0x12})
	if ok {
		t.Fatalf("partial message should not be complete")
	}
	if len(rest) != 3 {
		t.Fatalf("partial message should be returned intact")
	}
	// Two messages bundled: first is extracted, remainder returned.
	bundled := append(append([]byte{}, msg...), msg...)
	typ, full, rest, ok = frameClientMsg(bundled)
	if !ok || typ != 5 || len(full) != 6 || len(rest) != 6 {
		t.Fatalf("bundled: got typ=%d ok=%v full=%d rest=%d", typ, ok, len(full), len(rest))
	}
	// Empty buffer.
	if _, _, _, ok := frameClientMsg(nil); ok {
		t.Fatalf("empty buffer should not yield a message")
	}
}
