// Package admin serves the image-management endpoints. Admins maintain the
// dropdown of runnable images and define custom Dockerfiles that TermDock
// builds on demand:
//
//	GET    /api/images                          images the caller may run
//	GET    /api/admin/images                    allowed-image list
//	POST   /api/admin/images            {image} add to the allowed list
//	DELETE /api/admin/images/{id}               remove from the allowed list
//	GET    /api/admin/custom-images             custom Dockerfiles
//	POST   /api/admin/custom-images     {name, dockerfile}
//	PUT    /api/admin/custom-images/{id} {name, dockerfile}
//	DELETE /api/admin/custom-images/{id}
//	POST   /api/admin/custom-images/{id}/build  build now, in the background
//
// Custom images are tagged termdock/<name>:latest. Provisioning builds one
// only when no cached image with that tag exists.
package admin

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"termdock/internal/auth"
	"termdock/internal/db"
	"termdock/internal/dockerx"
	"termdock/internal/model"
)

// Handler serves image-management endpoints.
type Handler struct {
	DB     *db.DB
	Docker *dockerx.Client // nil when no Docker daemon is reachable

	mu       sync.Mutex
	building map[string]bool // custom-image IDs with a build in flight
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// requireAdmin writes 403 unless the caller is an admin.
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u := auth.UserFromContext(r.Context())
	if u == nil || !u.IsAdmin {
		writeErr(w, http.StatusForbidden, "admin required")
		return false
	}
	return true
}

// imageOption is one entry in the new-terminal image dropdown.
type imageOption struct {
	Image   string `json:"image"`
	Custom  bool   `json:"custom"`
	Name    string `json:"name,omitempty"` // custom image name
	Desktop bool   `json:"desktop"`        // provides an X stack
}

// AvailableImages lists what the caller may run: the allowed list plus the
// custom-image tags. Admins additionally get free choice (handled by the
// session create endpoint, not here).
func (h *Handler) AvailableImages(w http.ResponseWriter, r *http.Request) {
	allowed, err := h.DB.ListAllowedImages()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	custom, err := h.DB.ListCustomImages()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	var opts []imageOption
	for _, a := range allowed {
		opts = append(opts, imageOption{Image: a.Image, Desktop: a.Desktop})
	}
	for _, c := range custom {
		opts = append(opts, imageOption{Image: model.CustomImageTag(c.Name), Custom: true, Name: c.Name, Desktop: c.Desktop})
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": opts})
}

// ListAllowed handles GET /api/admin/images.
func (h *Handler) ListAllowed(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	allowed, err := h.DB.ListAllowedImages()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": allowed})
}

// AddAllowed handles POST /api/admin/images {image}.
func (h *Handler) AddAllowed(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var body struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Image = strings.TrimSpace(body.Image)
	if body.Image == "" || len(body.Image) > 256 {
		writeErr(w, http.StatusBadRequest, "image must be 1-256 characters")
		return
	}
	a := &model.AllowedImage{ID: db.NewID(), Image: body.Image, CreatedAt: db.Now()}
	if err := h.DB.AddAllowedImage(a); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// DeleteAllowed handles DELETE /api/admin/images/{id}.
func (h *Handler) DeleteAllowed(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if err := h.DB.DeleteAllowedImage(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var customNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func validCustomName(name string) bool { return customNameRe.MatchString(name) }

// ListCustom handles GET /api/admin/custom-images.
func (h *Handler) ListCustom(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	custom, err := h.DB.ListCustomImages()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": custom})
}

// CreateCustom handles POST /api/admin/custom-images {name, dockerfile}.
func (h *Handler) CreateCustom(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var body struct {
		Name       string `json:"name"`
		Dockerfile string `json:"dockerfile"`
		Desktop    bool   `json:"desktop"`
		DesktopCmd string `json:"desktop_cmd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Name = strings.ToLower(strings.TrimSpace(body.Name))
	if !validCustomName(body.Name) {
		writeErr(w, http.StatusBadRequest, "name must be lowercase alphanumeric, dash or underscore, up to 64 chars")
		return
	}
	if strings.TrimSpace(body.Dockerfile) == "" {
		writeErr(w, http.StatusBadRequest, "dockerfile is required")
		return
	}
	now := db.Now()
	c := &model.CustomImage{
		ID:         db.NewID(),
		Name:       body.Name,
		Dockerfile: body.Dockerfile,
		Desktop:    body.Desktop,
		DesktopCmd: strings.TrimSpace(body.DesktopCmd),
		Status:     model.CustomImageIdle,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := h.DB.CreateCustomImage(c); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// UpdateCustom handles PUT /api/admin/custom-images/{id} {name, dockerfile}.
func (h *Handler) UpdateCustom(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	c, err := h.DB.GetCustomImage(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if c == nil {
		writeErr(w, http.StatusNotFound, "custom image not found")
		return
	}
	var body struct {
		Name       string `json:"name"`
		Dockerfile string `json:"dockerfile"`
		Desktop    bool   `json:"desktop"`
		DesktopCmd string `json:"desktop_cmd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Name = strings.ToLower(strings.TrimSpace(body.Name))
	if !validCustomName(body.Name) {
		writeErr(w, http.StatusBadRequest, "name must be lowercase alphanumeric, dash or underscore, up to 64 chars")
		return
	}
	if strings.TrimSpace(body.Dockerfile) == "" {
		writeErr(w, http.StatusBadRequest, "dockerfile is required")
		return
	}
	c.Name = body.Name
	c.Dockerfile = body.Dockerfile
	c.Desktop = body.Desktop
	c.DesktopCmd = strings.TrimSpace(body.DesktopCmd)
	c.UpdatedAt = db.Now()
	if err := h.DB.UpdateCustomImage(c); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// DeleteCustom handles DELETE /api/admin/custom-images/{id}. This removes
// the Dockerfile record only; any built image stays cached in the daemon.
func (h *Handler) DeleteCustom(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if err := h.DB.DeleteCustomImage(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// BuildCustom handles POST /api/admin/custom-images/{id}/build. The build
// runs in the background; poll the custom-image list for status.
func (h *Handler) BuildCustom(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if h.Docker == nil {
		writeErr(w, http.StatusServiceUnavailable, "no container runtime connected")
		return
	}
	c, err := h.DB.GetCustomImage(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if c == nil {
		writeErr(w, http.StatusNotFound, "custom image not found")
		return
	}
	h.mu.Lock()
	if h.building == nil {
		h.building = make(map[string]bool)
	}
	if h.building[c.ID] {
		h.mu.Unlock()
		writeErr(w, http.StatusConflict, "build already in progress")
		return
	}
	h.building[c.ID] = true
	h.mu.Unlock()

	go h.runBuild(c)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "building"})
}

func (h *Handler) runBuild(c *model.CustomImage) {
	defer func() {
		h.mu.Lock()
		delete(h.building, c.ID)
		h.mu.Unlock()
	}()
	tag := model.CustomImageTag(c.Name)
	_ = h.DB.UpdateCustomImageStatus(c.ID, model.CustomImageBuilding, "", time.Now().Unix())
	_ = h.DB.UpdateBuildProgress(c.ID, 0, time.Now().Unix())
	// Builds can take a long time; don't inherit the HTTP request's context.
	err := h.Docker.BuildImage(context.Background(), tag, c.Dockerfile,
		func(step, total int, text string) {
			pct := 0
			if total > 0 {
				pct = step * 100 / total
			}
			_ = h.DB.UpdateBuildProgress(c.ID, pct, time.Now().Unix())
		})
	now := time.Now().Unix()
	if err != nil {
		log.Printf("admin: build of custom image %q failed: %v", tag, err)
		_ = h.DB.UpdateCustomImageStatus(c.ID, model.CustomImageError, err.Error(), now)
		return
	}
	log.Printf("admin: built custom image %q", tag)
	_ = h.DB.UpdateCustomImageStatus(c.ID, model.CustomImageReady, "", now)
}
