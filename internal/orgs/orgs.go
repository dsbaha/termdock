// Package orgs serves the /api/orgs/* endpoints (multi-tenant management).
package orgs

import (
	"encoding/json"
	"net/http"
	"strings"

	"termdock/internal/access"
	"termdock/internal/auth"
	"termdock/internal/db"
	"termdock/internal/model"
)

// Handler serves org endpoints.
type Handler struct {
	DB *db.DB
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// requireMember loads the org and the caller's membership role.
// It returns (org, role, ok). role is "" when the caller is not a member.
func (h *Handler) requireMember(w http.ResponseWriter, r *http.Request, orgID string) (*model.Org, string, bool) {
	org, err := h.DB.GetOrg(orgID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return nil, "", false
	}
	if org == nil {
		writeErr(w, http.StatusNotFound, "org not found")
		return nil, "", false
	}
	u := auth.UserFromContext(r.Context())
	role, err := h.DB.GetOrgRole(orgID, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return nil, "", false
	}
	if role == "" {
		writeErr(w, http.StatusForbidden, "not a member of this org")
		return nil, "", false
	}
	return org, role, true
}

// Create handles POST /api/orgs. The creator becomes the org owner.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 64 {
		writeErr(w, http.StatusBadRequest, "name must be 1-64 characters")
		return
	}
	u := auth.UserFromContext(r.Context())
	now := db.Now()
	org := &model.Org{ID: db.NewID(), Name: req.Name, CreatedAt: now}
	if err := h.DB.CreateOrg(org); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, http.StatusConflict, "org name already taken")
			return
		}
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if err := h.DB.AddOrgMember(org.ID, u.ID, model.OrgRoleOwner, now); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

// List handles GET /api/orgs — orgs the caller belongs to.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	orgs, err := h.DB.ListOrgsForUser(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if orgs == nil {
		orgs = []model.Org{}
	}
	writeJSON(w, http.StatusOK, orgs)
}

// ListMembers handles GET /api/orgs/{id}/members.
func (h *Handler) ListMembers(w http.ResponseWriter, r *http.Request, orgID string) {
	if _, _, ok := h.requireMember(w, r, orgID); !ok {
		return
	}
	members, err := h.DB.ListOrgMembers(orgID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if members == nil {
		members = []db.OrgMemberView{}
	}
	writeJSON(w, http.StatusOK, members)
}

// AddMember handles POST /api/orgs/{id}/members {username, role}.
// Owner/admin only.
func (h *Handler) AddMember(w http.ResponseWriter, r *http.Request, orgID string) {
	_, actorRole, ok := h.requireMember(w, r, orgID)
	if !ok {
		return
	}
	if !access.CanManageMembers(actorRole) {
		writeErr(w, http.StatusForbidden, "only org owners/admins can add members")
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
	req.Role = strings.ToLower(strings.TrimSpace(req.Role))
	if !access.ValidOrgRole(req.Role) {
		writeErr(w, http.StatusBadRequest, "role must be owner, admin, or member")
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
	existingRole, err := h.DB.GetOrgRole(orgID, target.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if existingRole != "" {
		writeErr(w, http.StatusConflict, "user is already a member")
		return
	}
	if err := h.DB.AddOrgMember(orgID, target.ID, req.Role, db.Now()); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"user_id": target.ID, "role": req.Role})
}

// RemoveMember handles DELETE /api/orgs/{id}/members/{userID}.
// Owner/admin only; the last owner can never be removed.
func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request, orgID, targetUserID string) {
	_, actorRole, ok := h.requireMember(w, r, orgID)
	if !ok {
		return
	}
	targetRole, err := h.DB.GetOrgRole(orgID, targetUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if targetRole == "" {
		writeErr(w, http.StatusNotFound, "member not found")
		return
	}
	owners, err := h.DB.CountOrgOwners(orgID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if !access.CanRemoveMember(actorRole, targetRole, owners) {
		if targetRole == model.OrgRoleOwner && owners <= 1 {
			writeErr(w, http.StatusForbidden, "cannot remove the last owner")
			return
		}
		writeErr(w, http.StatusForbidden, "only org owners/admins can remove members")
		return
	}
	if err := h.DB.RemoveOrgMember(orgID, targetUserID); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}
