// Package sessions serves the /api/sessions/* endpoints and owns the
// container lifecycle: provisioning, start/stop/delete, the idle reaper,
// and boot reconciliation.
package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"termdock/internal/access"
	"termdock/internal/auth"
	"termdock/internal/db"
	"termdock/internal/dockerx"
	"termdock/internal/hub"
	"termdock/internal/llm"
	"termdock/internal/model"
)

// Handler serves session endpoints.
type Handler struct {
	DB     *db.DB
	Docker *dockerx.Client // nil when no Docker daemon is reachable
	Hub    *hub.Hub
	// DefaultImage and DefaultIdleMin come from config; kept here so the
	// package stays decoupled from the config package.
	DefaultImage   string
	DefaultIdleMin int
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// accessCtx bundles everything an endpoint needs after authorization.
type accessCtx struct {
	session *model.Session
	orgRole string // "" when not an org member
	role    string // effective role: viewer|collaborator
}

// resolve loads the session and computes the caller's effective role.
// It writes the error response and returns ok=false when access is denied.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, sessionID string) (*accessCtx, bool) {
	u := auth.UserFromContext(r.Context())
	sess, err := h.DB.GetSession(sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return nil, false
	}
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return nil, false
	}
	orgRole, err := h.DB.GetOrgRole(sess.OrgID, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return nil, false
	}
	shareRole, err := h.DB.GetShareRole(sess.ID, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return nil, false
	}
	role, ok := access.EffectiveRole(shareRole, orgRole)
	if !ok {
		writeErr(w, http.StatusForbidden, "no access to this session")
		return nil, false
	}
	return &accessCtx{session: sess, orgRole: orgRole, role: role}, true
}

// sessionView is the JSON representation of a session.
func sessionView(s *model.Session, myRole string) map[string]any {
	return map[string]any{
		"id":             s.ID,
		"name":           s.Name,
		"org_id":         s.OrgID,
		"image":          s.Image,
		"shell":          s.Shell,
		"display":        s.Display,
		"cpu":            s.CPU,
		"memory_mb":      s.MemoryMB,
		"idle_minutes":   s.IdleMinutes,
		"status":         s.Status,
		"status_msg":     s.StatusMsg,
		"created_by":     s.CreatedBy,
		"created_at":     s.CreatedAt,
		"last_active_at": s.LastActiveAt,
		"my_role":        myRole,
	}
}

// Create handles POST /api/sessions. It writes the DB row immediately and
// provisions the container in the background; the frontend polls until
// status=running and then opens the terminal.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if h.Docker == nil {
		writeErr(w, http.StatusServiceUnavailable, "docker daemon is not reachable")
		return
	}
	var req struct {
		Name        string  `json:"name"`
		OrgID       string  `json:"org_id"`
		Image       string  `json:"image"`
		Shell       string  `json:"shell"`
		Display     string  `json:"display"` // terminal (default) | desktop
		CPU         float64 `json:"cpu"`
		MemoryMB    int     `json:"memory_mb"`
		IdleMinutes *int    `json:"idle_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	u := auth.UserFromContext(r.Context())
	if req.OrgID == "" {
		writeErr(w, http.StatusBadRequest, "org_id is required")
		return
	}
	orgRole, err := h.DB.GetOrgRole(req.OrgID, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if orgRole == "" {
		writeErr(w, http.StatusForbidden, "not a member of this org")
		return
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		image = h.DefaultImage
	}
	if len(image) > 256 || strings.ContainsAny(image, " \t\n") {
		writeErr(w, http.StatusBadRequest, "invalid image")
		return
	}
	// Non-admins may only run images from the admin-managed list (allowed
	// images and custom-image tags). Admins keep free choice.
	if !u.IsAdmin {
		ok, err := h.imagePermitted(image)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, "image is not in the allowed list")
			return
		}
	}
	shell := strings.TrimSpace(req.Shell)
	if shell == "" {
		shell = "/bin/bash"
	}
	if !strings.HasPrefix(shell, "/") || len(shell) > 128 {
		writeErr(w, http.StatusBadRequest, "shell must be an absolute path")
		return
	}
	cpu := req.CPU
	if cpu == 0 {
		cpu = 2
	}
	if cpu < 0.1 || cpu > 32 {
		writeErr(w, http.StatusBadRequest, "cpu must be between 0.1 and 32")
		return
	}
	mem := req.MemoryMB
	if mem == 0 {
		mem = 2048
	}
	if mem < 128 || mem > 131072 {
		writeErr(w, http.StatusBadRequest, "memory_mb must be between 128 and 131072")
		return
	}
	idle := h.DefaultIdleMin
	if req.IdleMinutes != nil {
		idle = *req.IdleMinutes
	}
	if idle < 0 || idle > 10080 {
		writeErr(w, http.StatusBadRequest, "idle_minutes must be between 0 and 10080")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "terminal"
	}
	if len(name) > 64 {
		writeErr(w, http.StatusBadRequest, "name must be at most 64 characters")
		return
	}
	display := strings.ToLower(strings.TrimSpace(req.Display))
	if display == "" {
		display = model.DisplayTerminal
	}
	if display != model.DisplayTerminal && display != model.DisplayDesktop {
		writeErr(w, http.StatusBadRequest, "display must be terminal or desktop")
		return
	}
	// Desktop sessions need an image with an X stack. Known images carry a
	// desktop flag; admin free-text images skip this check and fail
	// descriptively at provision time if Xvfb is absent.
	if display == model.DisplayDesktop {
		provides, err := h.DB.ImageProvidesDesktop(image)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		if !provides {
			writeErr(w, http.StatusBadRequest, "image \""+image+"\" is not marked as providing a desktop (X11); pick a desktop image or ask an admin")
			return
		}
	}

	id := db.NewID()
	now := db.Now()
	sess := &model.Session{
		ID:           id,
		Name:         name,
		OrgID:        req.OrgID,
		Image:        image,
		Shell:        shell,
		Display:      display,
		CPU:          cpu,
		MemoryMB:     mem,
		IdleMinutes:  idle,
		Volume:       dockerx.VolumeName(id),
		Status:       model.SessionCreating,
		CreatedBy:    u.ID,
		CreatedAt:    now,
		LastActiveAt: now,
	}
	if err := h.DB.CreateSession(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	go h.provision(id)
	writeJSON(w, http.StatusCreated, sessionView(sess, model.ShareRoleCollaborator))
}

// provision runs the creating -> pulling -> running state machine in the
// background. Any failure lands the session in status=error with a message.
// Every step is logged so a stuck provision can be pinpointed from the
// server log (e.g. a runtime whose exec API hangs).
func (h *Handler) provision(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	fail := func(msg string) {
		log.Printf("provision %s failed: %s", sessionID, msg)
		_ = h.DB.UpdateSessionStatus(sessionID, model.SessionError, msg)
	}
	set := func(status, msg string) {
		_ = h.DB.UpdateSessionStatus(sessionID, status, msg)
	}

	sess, err := h.DB.GetSession(sessionID)
	if err != nil || sess == nil {
		return // session was deleted before we started
	}

	log.Printf("provision %s: pulling image %s", sessionID, sess.Image)
	if name, isCustom := model.ParseCustomImageName(sess.Image); isCustom {
		// Custom image: build from the stored Dockerfile only when no
		// cached image exists.
		if h.Docker.ImageExists(ctx, sess.Image) {
			log.Printf("provision %s: custom image %s already cached", sessionID, sess.Image)
			set(model.SessionPulling, "using cached image "+sess.Image)
		} else {
			custom, err := h.DB.GetCustomImageByName(name)
			if err != nil || custom == nil {
				fail("custom image " + sess.Image + " has no stored Dockerfile")
				return
			}
			log.Printf("provision %s: building custom image %s", sessionID, sess.Image)
			set(model.SessionBuilding, "building image "+sess.Image)
			_ = h.DB.UpdateCustomImageStatus(custom.ID, model.CustomImageBuilding, "", db.Now())
			if err := h.Docker.BuildImage(ctx, sess.Image, custom.Dockerfile, nil); err != nil {
				_ = h.DB.UpdateCustomImageStatus(custom.ID, model.CustomImageError, err.Error(), db.Now())
				fail("image build failed: " + err.Error())
				return
			}
			_ = h.DB.UpdateCustomImageStatus(custom.ID, model.CustomImageReady, "", db.Now())
		}
	} else {
		set(model.SessionPulling, "pulling image "+sess.Image)
		if err := h.Docker.PullImage(ctx, sess.Image); err != nil {
			fail("image pull failed: " + err.Error())
			return
		}
	}

	log.Printf("provision %s: creating container", sessionID)
	set(model.SessionCreating, "creating container")
	containerID, err := h.Docker.CreateAndStart(ctx, dockerx.CreateOptions{
		SessionID: sess.ID,
		Name:      sess.Name,
		Image:     sess.Image,
		CPU:       sess.CPU,
		MemoryMB:  sess.MemoryMB,
	})
	if err != nil {
		fail("container create failed: " + err.Error())
		return
	}
	log.Printf("provision %s: container %s started", sessionID, containerID)

	// Shell detection is best-effort and bounded: some runtimes (Podman)
	// can hang on exec attach, and that must never wedge provisioning.
	// On timeout we fall back to the requested shell.
	log.Printf("provision %s: resolving shell", sessionID)
	rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
	shell := h.Docker.ResolveShell(rctx, containerID, sess.Shell)
	rcancel()
	if shell != sess.Shell {
		_ = h.DB.UpdateSessionShell(sessionID, shell)
	}
	// For desktop sessions, start the X stack (Xvfb + window manager + VNC)
	// so the display is up before the session is marked running.
	if sess.Display == model.DisplayDesktop {
		log.Printf("provision %s: starting X stack", sessionID)
		set(model.SessionCreating, "starting desktop")
		if err := h.startXStack(ctx, containerID, sess.Image); err != nil {
			fail("desktop start failed: " + err.Error())
			return
		}
		log.Printf("provision %s: X stack up", sessionID)
	}

	log.Printf("provision %s: shell=%s, marking running", sessionID, shell)
	if err := h.DB.UpdateSessionContainer(sessionID, containerID, model.SessionRunning); err != nil {
		fail("database error: " + err.Error())
		return
	}
	_ = h.DB.TouchSession(sessionID, db.Now())
}

// desktopCmdForImage returns the custom startup command for a custom image,
// or "" for the default (openbox).
func (h *Handler) desktopCmdForImage(image string) string {
	name, isCustom := model.ParseCustomImageName(image)
	if !isCustom {
		return ""
	}
	c, err := h.DB.GetCustomImageByName(name)
	if err != nil || c == nil {
		return ""
	}
	return strings.TrimSpace(c.DesktopCmd)
}

// X display layout for desktop sessions.
const (
	xDisplay    = ":99"
	xScreen     = "1920x1080x24"
	xVNCPort    = 5900
	xVNCPortStr = "5900"
)

// startXStack launches Xvfb, a window manager, and x11vnc inside a desktop
// session's container, then waits for the VNC port to accept connections.
// The image must provide the X stack binaries (see the seeded "desktop"
// custom image); otherwise this returns a descriptive error.
func (h *Handler) startXStack(ctx context.Context, containerID, image string) error {
	// Xvfb first: everything else needs the display socket.
	if err := h.Docker.ExecDetached(ctx, containerID, nil,
		"Xvfb", xDisplay, "-screen", "0", xScreen); err != nil {
		return fmt.Errorf("Xvfb: %w (image needs the xvfb package)", err)
	}
	// Window manager / desktop environment. Custom images may define their
	// own startup command (e.g. KDE); otherwise openbox.
	wmCmd := []string{"openbox"}
	desktopCmd := h.desktopCmdForImage(image)
	if desktopCmd != "" {
		wmCmd = strings.Fields(desktopCmd)
	}
	// Give Xvfb a moment to create the socket, then start the WM and VNC.
	time.Sleep(2 * time.Second)
	if err := h.Docker.ExecDetached(ctx, containerID,
		[]string{"DISPLAY=" + xDisplay}, wmCmd...); err != nil {
		return fmt.Errorf("%s: %w (image needs the desktop packages)", strings.Join(wmCmd, " "), err)
	}
	if err := h.Docker.ExecDetached(ctx, containerID, nil,
		"x11vnc", "-display", xDisplay, "-forever", "-shared", "-nopw",
		"-listen", "0.0.0.0", "-rfbport", xVNCPortStr); err != nil {
		return fmt.Errorf("x11vnc: %w (image needs the x11vnc package)", err)
	}
	// Wait for the VNC port to come up.
	ip, err := h.Docker.ContainerIP(ctx, containerID)
	if err != nil {
		return fmt.Errorf("container IP: %w", err)
	}
	addr := net.JoinHostPort(ip, xVNCPortStr)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var d net.Dialer
		c, derr := d.DialContext(dctx, "tcp", addr)
		cancel()
		if derr == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("VNC port %s not reachable after 30s", addr)
}

// List handles GET /api/sessions?org_id=. Without org_id it returns every
// session the caller can see (org sessions + direct shares).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	orgID := r.URL.Query().Get("org_id")
	if orgID != "" {
		role, err := h.DB.GetOrgRole(orgID, u.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		if role == "" {
			writeErr(w, http.StatusForbidden, "not a member of this org")
			return
		}
	}
	sessions, err := h.DB.ListVisibleSessions(u.ID, orgID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	views := make([]map[string]any, 0, len(sessions))
	for i := range sessions {
		s := &sessions[i]
		role := h.myRole(u.ID, s)
		views = append(views, sessionView(s, role))
	}
	writeJSON(w, http.StatusOK, views)
}

// myRole resolves the caller's effective role on a session (best effort;
// empty when something fails — callers treat it as no special role).
func (h *Handler) myRole(userID string, s *model.Session) string {
	orgRole, _ := h.DB.GetOrgRole(s.OrgID, userID)
	shareRole, _ := h.DB.GetShareRole(s.ID, userID)
	role, _ := access.EffectiveRole(shareRole, orgRole)
	return role
}

// Detail handles GET /api/sessions/{id}.
func (h *Handler) Detail(w http.ResponseWriter, r *http.Request, sessionID string) {
	ac, ok := h.resolve(w, r, sessionID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sessionView(ac.session, ac.role))
}

// Start handles POST /api/sessions/{id}/start. It recreates the container
// with the SAME volume, so /workspace survives.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request, sessionID string) {
	if h.Docker == nil {
		writeErr(w, http.StatusServiceUnavailable, "docker daemon is not reachable")
		return
	}
	u := auth.UserFromContext(r.Context())
	ac, ok := h.resolve(w, r, sessionID)
	if !ok {
		return
	}
	if !access.CanControl(ac.role, ac.session.CreatedBy == u.ID, ac.orgRole) {
		writeErr(w, http.StatusForbidden, "viewers cannot start sessions")
		return
	}
	switch ac.session.Status {
	case model.SessionRunning:
		writeErr(w, http.StatusConflict, "session is already running")
		return
	case model.SessionCreating, model.SessionPulling:
		writeErr(w, http.StatusConflict, "session is already starting")
		return
	}
	if err := h.DB.UpdateSessionStatus(sessionID, model.SessionCreating, "restart requested"); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	go h.provision(sessionID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": model.SessionCreating})
}

// Stop handles POST /api/sessions/{id}/stop. The container is stopped and
// removed but the volume is kept.
func (h *Handler) Stop(w http.ResponseWriter, r *http.Request, sessionID string) {
	u := auth.UserFromContext(r.Context())
	ac, ok := h.resolve(w, r, sessionID)
	if !ok {
		return
	}
	if !access.CanControl(ac.role, ac.session.CreatedBy == u.ID, ac.orgRole) {
		writeErr(w, http.StatusForbidden, "viewers cannot stop sessions")
		return
	}
	if ac.session.Status != model.SessionRunning {
		writeErr(w, http.StatusConflict, "session is not running")
		return
	}
	h.stopContainer(sessionID, ac.session.ContainerID, "session stopped")
	writeJSON(w, http.StatusOK, map[string]string{"status": model.SessionStopped})
}

// stopContainer kills the hub exec, removes the container, and marks the
// session stopped. It is idempotent.
func (h *Handler) stopContainer(sessionID, containerID, notice string) {
	h.Hub.CloseSession(sessionID, notice)
	if h.Docker != nil && containerID != "" {
		_ = h.Docker.StopAndRemove(context.Background(), containerID)
	}
	_ = h.DB.UpdateSessionContainer(sessionID, "", model.SessionStopped)
}

// Delete handles DELETE /api/sessions/{id}. Stops the container, removes the
// volume, and deletes the row. Creator or org owner/admin only.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request, sessionID string) {
	u := auth.UserFromContext(r.Context())
	ac, ok := h.resolve(w, r, sessionID)
	if !ok {
		return
	}
	if !access.CanDelete(ac.session.CreatedBy == u.ID, ac.orgRole) {
		writeErr(w, http.StatusForbidden, "only the creator or an org owner/admin can delete sessions")
		return
	}
	h.Hub.CloseSession(ac.session.ID, "session deleted")
	if h.Docker != nil {
		if ac.session.ContainerID != "" {
			_ = h.Docker.StopAndRemove(context.Background(), ac.session.ContainerID)
		}
		_ = h.Docker.RemoveVolume(context.Background(), ac.session.Volume)
	}
	if err := h.DB.DeleteSession(ac.session.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Attach handles GET /api/sessions/{id}/attach — the websocket endpoint that
// streams the interactive shell. Viewers get a read-only stream;
// collaborators get input.
func (h *Handler) Attach(w http.ResponseWriter, r *http.Request, sessionID string) {
	u := auth.UserFromContext(r.Context())
	ac, ok := h.resolve(w, r, sessionID)
	if !ok {
		return
	}
	// Re-read: the session may have changed state since resolve().
	sess, err := h.DB.GetSession(sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if sess == nil || sess.Status != model.SessionRunning || sess.ContainerID == "" {
		writeErr(w, http.StatusConflict, "session is not running")
		return
	}
	// Which terminal of the container to attach: the persistent "main"
	// terminal by default, or a client-generated ID for extra tabs. Viewers
	// only ever see the main terminal.
	termID := r.URL.Query().Get("term")
	if termID == "" {
		termID = hub.MainTermID
	}
	if !validTermID(termID) {
		writeErr(w, http.StatusBadRequest, "invalid term id")
		return
	}
	if ac.role == model.ShareRoleViewer && termID != hub.MainTermID {
		writeErr(w, http.StatusForbidden, "viewers can only watch the main terminal")
		return
	}
	ws, err := hub.Upgrade(w, r)
	if err != nil {
		return
	}
	_ = h.DB.TouchSession(sessionID, db.Now())
	// Blocks until the stream closes.
	h.Hub.Attach(r.Context(), sess.ID, sess.ContainerID, sess.Shell, termID, u.ID, ac.role, ws)
}

// ListTerms handles GET /api/sessions/{id}/terms — the session's live
// terminals (main plus any extra shells), ordered for tab display.
func (h *Handler) ListTerms(w http.ResponseWriter, r *http.Request, sessionID string) {
	if _, ok := h.resolve(w, r, sessionID); !ok {
		return
	}
	terms := h.Hub.ListTerms(sessionID)
	if terms == nil {
		terms = []hub.TermInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"terms": terms})
}

// validTermID allows the fixed "main" ID and client-generated extra-terminal
// IDs (alphanumeric, dash, underscore; bounded length).
func validTermID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// imagePermitted reports whether image may be run by a non-admin: it is on
// the allowed list or is a custom-image tag with a stored Dockerfile.
func (h *Handler) imagePermitted(image string) (bool, error) {
	if ok, err := h.DB.IsImageAllowed(image); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	if name, isCustom := model.ParseCustomImageName(image); isCustom {
		c, err := h.DB.GetCustomImageByName(name)
		if err != nil {
			return false, err
		}
		return c != nil, nil
	}
	return false, nil
}

// CreateShare handles POST /api/sessions/{id}/shares {username, role}.
// Creator or org owner/admin only. Re-sharing updates the existing grant.
func (h *Handler) CreateShare(w http.ResponseWriter, r *http.Request, sessionID string) {
	u := auth.UserFromContext(r.Context())
	ac, ok := h.resolve(w, r, sessionID)
	if !ok {
		return
	}
	if !access.CanShare(ac.session.CreatedBy == u.ID, ac.orgRole) {
		writeErr(w, http.StatusForbidden, "only the creator or an org owner/admin can share sessions")
		return
	}
	var req struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	role := strings.ToLower(strings.TrimSpace(req.Role))
	if !access.ValidShareRole(role) {
		writeErr(w, http.StatusBadRequest, "role must be viewer or collaborator")
		return
	}
	target, err := h.DB.GetUserByUsername(strings.TrimSpace(req.Username))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	if target.ID == u.ID {
		writeErr(w, http.StatusBadRequest, "cannot share with yourself")
		return
	}
	existingRole, err := h.DB.GetShareRole(sessionID, target.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if existingRole != "" {
		// Update the existing grant in place.
		shares, err := h.DB.ListShares(sessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		for _, sh := range shares {
			if sh.UserID == target.ID {
				_ = h.DB.DeleteShare(sh.ID)
				break
			}
		}
	}
	sh := &model.Share{
		ID:        db.NewID(),
		SessionID: sessionID,
		UserID:    target.ID,
		Role:      role,
		CreatedBy: u.ID,
		CreatedAt: db.Now(),
	}
	if err := h.DB.CreateShare(sh); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, sh)
}

// ListShares handles GET /api/sessions/{id}/shares.
func (h *Handler) ListShares(w http.ResponseWriter, r *http.Request, sessionID string) {
	if _, ok := h.resolve(w, r, sessionID); !ok {
		return
	}
	shares, err := h.DB.ListShares(sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if shares == nil {
		shares = []model.Share{}
	}
	writeJSON(w, http.StatusOK, shares)
}

// RevokeShare handles DELETE /api/sessions/{id}/shares/{shareID}. The revoked
// user's sockets are closed immediately.
func (h *Handler) RevokeShare(w http.ResponseWriter, r *http.Request, sessionID, shareID string) {
	u := auth.UserFromContext(r.Context())
	ac, ok := h.resolve(w, r, sessionID)
	if !ok {
		return
	}
	if !access.CanShare(ac.session.CreatedBy == u.ID, ac.orgRole) {
		writeErr(w, http.StatusForbidden, "only the creator or an org owner/admin can revoke shares")
		return
	}
	sh, err := h.DB.GetShare(shareID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if sh == nil || sh.SessionID != sessionID {
		writeErr(w, http.StatusNotFound, "share not found")
		return
	}
	if err := h.DB.DeleteShare(shareID); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	h.Hub.CloseUser(sessionID, sh.UserID, "your access to this session was revoked")
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// ReaperLoop periodically stops idle sessions and purges expired logins.
func (h *Handler) ReaperLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.reapOnce()
		}
	}
}

func (h *Handler) reapOnce() {
	_ = h.DB.DeleteExpiredAuthSessions(db.Now())
	sessions, err := h.DB.ListSessionsByStatus(model.SessionRunning)
	if err != nil {
		return
	}
	now := db.Now()
	for i := range sessions {
		s := &sessions[i]
		if s.IdleMinutes <= 0 {
			continue
		}
		idleFor := int64(s.IdleMinutes) * 60
		if h.Hub.NumClients(s.ID) > 0 {
			continue // someone is watching
		}
		if now-s.LastActiveAt < idleFor {
			continue // recent input or attach
		}
		if now-h.Hub.LastOutputAt(s.ID) < idleFor {
			continue // the shell is still producing output (e.g. a build)
		}
		// Idle on all three axes: stop the container, keep the volume.
		h.stopContainer(s.ID, s.ContainerID, "session stopped after idle timeout")
	}
}

// Reconcile runs at boot: for every session that was mid-flight when the
// process died, adopt its container if it is still running, otherwise mark it
// stopped so the user can start it again.
func (h *Handler) Reconcile(ctx context.Context) {
	if h.Docker == nil {
		// Without Docker we cannot verify anything; park everything as stopped.
		for _, st := range []string{model.SessionCreating, model.SessionPulling, model.SessionRunning} {
			sessions, err := h.DB.ListSessionsByStatus(st)
			if err != nil {
				continue
			}
			for i := range sessions {
				_ = h.DB.UpdateSessionContainer(sessions[i].ID, "", model.SessionStopped)
			}
		}
		return
	}
	sessions, err := h.DB.ListSessionsByStatus(
		model.SessionCreating, model.SessionPulling, model.SessionRunning)
	if err != nil {
		return
	}
	for i := range sessions {
		s := &sessions[i]
		id, running, err := h.Docker.FindSessionContainer(ctx, s.ID)
		if err != nil || !running {
			_ = h.DB.UpdateSessionContainer(s.ID, "", model.SessionStopped)
			continue
		}
		// Adopt the surviving container.
		_ = h.DB.UpdateSessionContainer(s.ID, id, model.SessionRunning)
	}
}

// LLMChat handles POST /api/sessions/{id}/llm-chat.
// Proxies a chat message to the configured LLM provider with session context.
func (h *Handler) LLMChat(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.resolve(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var body llm.ChatRequest
	body.SessionID = ac.session.ID
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Get the provider (includes the API key, server-side only).
	provider, err := h.DB.GetLLMProvider(body.ProviderID)
	if err != nil || provider == nil {
		writeErr(w, http.StatusNotFound, "LLM provider not found")
		return
	}
	if provider.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "LLM provider has no API key configured")
		return
	}
	systemPrompt := llm.SystemPrompt(ac.session.Display, body.TerminalContext, body.ScreenshotB64 != "")
	resp, err := llm.Chat(r.Context(), provider.ProviderType, provider.Endpoint,
		provider.APIKey, provider.Model, systemPrompt, body.Messages, body.ScreenshotB64)
	if err != nil {
		log.Printf("llm chat failed: %v", err)
		writeErr(w, http.StatusBadGateway, "LLM request failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, llm.ChatResponse{Response: resp})
}

// ListLLMProviders handles GET /api/llm-providers (names only, for the chat panel).
func (h *Handler) ListLLMProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.DB.ListLLMProviders()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	// Strip API keys; the browser only needs id, name, and model.
	type public struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	out := make([]public, 0, len(providers))
	for _, p := range providers {
		out = append(out, public{ID: p.ID, Name: p.Name, Model: p.Model})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}
