// Package model defines the core domain types shared across packages.
package model

import "strings"

// Share roles.
const (
	ShareRoleViewer       = "viewer"
	ShareRoleCollaborator = "collaborator"
)

// Org roles.
const (
	OrgRoleOwner  = "owner"
	OrgRoleAdmin  = "admin"
	OrgRoleMember = "member"
)

// Session statuses.
const (
	SessionCreating = "creating"
	SessionPulling  = "pulling"
	SessionBuilding = "building"
	SessionRunning  = "running"
	SessionStopped  = "stopped"
	SessionError    = "error"
)

// Display types.
const (
	DisplayTerminal = "terminal"
	DisplayDesktop  = "desktop" // X11 via Xvfb + VNC
)

// CustomImage statuses.
const (
	CustomImageIdle     = "idle"
	CustomImageBuilding = "building"
	CustomImageReady    = "ready"
	CustomImageError    = "error"
)

// CustomImagePrefix is the repository prefix for admin-built images:
// a custom image named "zot" becomes "termdock/zot:latest".
const CustomImagePrefix = "termdock/"

// CustomImageTag returns the image reference for a custom image name.
func CustomImageTag(name string) string {
	return CustomImagePrefix + name + ":latest"
}

// ParseCustomImageName returns the custom image name if ref is a
// termdock/<name>:latest reference.
func ParseCustomImageName(ref string) (string, bool) {
	if !strings.HasPrefix(ref, CustomImagePrefix) || !strings.HasSuffix(ref, ":latest") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(ref, CustomImagePrefix), ":latest")
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// User is a TermDock account.
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    int64  `json:"created_at"`
}

// Org is a tenant grouping users and sessions.
type Org struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

// Session is a managed terminal container.
type Session struct {
	ID           string
	Name         string
	OrgID        string
	Image        string
	Shell        string
	Display      string // terminal | desktop
	CPU          float64
	MemoryMB     int
	IdleMinutes  int
	Volume       string
	ContainerID  string
	Status       string
	StatusMsg    string
	CreatedBy    string
	CreatedAt    int64
	LastActiveAt int64
}

// Share grants a user access to a session outside (or overriding) org membership.
type Share struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
	Username  string `json:"username,omitempty"` // denormalized for listing
	Role      string `json:"role"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
}

// AuthSession is a login session (cookie token).
type AuthSession struct {
	Token     string
	UserID    string
	ExpiresAt int64
}

// AllowedImage is an admin-approved image users may run.
type AllowedImage struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	CreatedAt int64  `json:"created_at"`
}

// CustomImage is an admin-defined Dockerfile. It is built on demand as
// termdock/<name>:latest when a session requests it and no cached image
// exists.
type CustomImage struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Dockerfile string `json:"dockerfile"`
	Status     string `json:"status"`
	StatusMsg  string `json:"status_msg"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}
