// Package model defines the core domain types shared across packages.
package model

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
	SessionRunning  = "running"
	SessionStopped  = "stopped"
	SessionError    = "error"
)

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
