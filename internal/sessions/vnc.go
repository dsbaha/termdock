// Package sessions: VNC proxy for desktop sessions.
//
// A desktop session runs Xvfb + openbox + x11vnc in its container. This file
// proxies a browser websocket straight to the container's VNC port, so the
// browser's VNC client (noVNC core) talks RFB end to end. The sharing model
// is enforced in the proxy: viewers receive framebuffer updates but their
// input messages (key, pointer, clipboard) are dropped; collaborators pass
// through untouched.
package sessions

import (
	"encoding/binary"
	"log"
	"net"
	"net/http"
	"time"

	"termdock/internal/auth"
	"termdock/internal/db"
	"termdock/internal/hub"
	"termdock/internal/model"
)

// VNCProxy handles GET /api/sessions/{id}/vnc — the websocket endpoint that
// carries a desktop session's VNC stream. Only for display=desktop sessions.
func (h *Handler) VNCProxy(w http.ResponseWriter, r *http.Request, sessionID string) {
	u := auth.UserFromContext(r.Context())
	ac, ok := h.resolve(w, r, sessionID)
	if !ok {
		return
	}
	sess, err := h.DB.GetSession(sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if sess == nil || sess.Display != model.DisplayDesktop {
		writeErr(w, http.StatusNotFound, "not a desktop session")
		return
	}
	if sess.Status != model.SessionRunning || sess.ContainerID == "" {
		writeErr(w, http.StatusConflict, "session is not running")
		return
	}
	ctx := r.Context()
	ip, err := h.Docker.ContainerIP(ctx, sess.ContainerID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "cannot reach container: "+err.Error())
		return
	}
	vnc, err := net.DialTimeout("tcp", net.JoinHostPort(ip, xVNCPortStr), 10*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "VNC not reachable: "+err.Error())
		return
	}
	ws, err := hub.Upgrade(w, r)
	if err != nil {
		vnc.Close()
		return
	}
	_ = h.DB.TouchSession(sessionID, db.Now())
	log.Printf("vnc proxy %s: %s attached (role=%s)", sessionID, u.ID, ac.role)

	// Container -> browser: plain byte copy as binary frames.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ws.Close()
		buf := make([]byte, 64*1024)
		for {
			n, rerr := vnc.Read(buf)
			if rerr != nil {
				return
			}
			if werr := ws.WriteBinary(buf[:n]); werr != nil {
				return
			}
		}
	}()

	// Browser -> container: viewers get their input filtered out.
	if ac.role == model.ShareRoleViewer {
		proxyFiltered(ws, vnc)
	} else {
		for {
			_, p, rerr := ws.Read()
			if rerr != nil {
				break
			}
			if _, werr := vnc.Write(p); werr != nil {
				break
			}
		}
	}
	vnc.Close()
	<-done
}

// RFB client-to-server message types we filter on.
const (
	rfbSetPixelFormat = 0
	rfbSetEncodings   = 2
	rfbFBUpdateReq    = 3
	rfbKeyEvent       = 4
	rfbPointerEvent   = 5
	rfbClientCutText  = 6
)

// proxyFiltered forwards websocket messages to the VNC server, dropping input
// messages (key, pointer, clipboard) so viewers are watch-only. Messages are
// framed because a websocket frame can split or bundle RFB messages.
func proxyFiltered(ws hub.Stream, vnc net.Conn) {
	var pending []byte
	for {
		_, p, err := ws.Read()
		if err != nil {
			return
		}
		pending = append(pending, p...)
		for {
			t, msg, rest, ok := frameClientMsg(pending)
			if !ok {
				break
			}
			pending = rest
			if t == rfbKeyEvent || t == rfbPointerEvent || t == rfbClientCutText {
				continue // viewer input: drop
			}
			if _, err := vnc.Write(msg); err != nil {
				return
			}
		}
	}
}

// frameClientMsg extracts one complete RFB client message from the head of
// buf. It returns the message type, the full message bytes, the remainder,
// and whether a complete message was available.
func frameClientMsg(buf []byte) (byte, []byte, []byte, bool) {
	if len(buf) < 1 {
		return 0, nil, buf, false
	}
	need := -1
	switch buf[0] {
	case rfbSetPixelFormat:
		need = 20
	case rfbSetEncodings:
		if len(buf) < 4 {
			return 0, nil, buf, false
		}
		need = 4 + 4*int(binary.BigEndian.Uint16(buf[2:4]))
	case rfbFBUpdateReq:
		need = 10
	case rfbKeyEvent:
		need = 8
	case rfbPointerEvent:
		need = 6
	case rfbClientCutText:
		if len(buf) < 8 {
			return 0, nil, buf, false
		}
		need = 8 + int(binary.BigEndian.Uint32(buf[4:8]))
	default:
		// Unknown client message: not valid RFB; drop the connection's
		// input side by treating it as complete-but-unforwardable.
		return buf[0], nil, buf[1:], true
	}
	if len(buf) < need {
		return 0, nil, buf, false
	}
	return buf[0], buf[:need], buf[need:], true
}
