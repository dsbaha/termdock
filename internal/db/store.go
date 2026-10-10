package db

import (
	"database/sql"
	"errors"

	"termdock/internal/model"
)

// ---------- users ----------

func (d *DB) CreateUser(u *model.User) error {
	_, err := d.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (?,?,?,?,?)`,
		u.ID, u.Username, u.PasswordHash, boolToInt(u.IsAdmin), u.CreatedAt)
	return err
}

func (d *DB) GetUserByUsername(username string) (*model.User, error) {
	return d.getUser(`WHERE username = ?`, username)
}

func (d *DB) GetUserByID(id string) (*model.User, error) {
	return d.getUser(`WHERE id = ?`, id)
}

func (d *DB) CountUsers() (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (d *DB) getUser(where string, arg any) (*model.User, error) {
	var u model.User
	var admin int
	err := d.QueryRow(`SELECT id, username, password_hash, is_admin, created_at FROM users `+where,
		arg).Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = admin != 0
	return &u, nil
}

// ---------- auth sessions ----------

func (d *DB) CreateAuthSession(token, userID string, expiresAt int64) error {
	_, err := d.Exec(`INSERT INTO auth_sessions (token, user_id, expires_at) VALUES (?,?,?)`,
		token, userID, expiresAt)
	return err
}

func (d *DB) GetAuthSession(token string) (*model.AuthSession, error) {
	var s model.AuthSession
	err := d.QueryRow(`SELECT token, user_id, expires_at FROM auth_sessions WHERE token = ?`, token).
		Scan(&s.Token, &s.UserID, &s.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (d *DB) DeleteAuthSession(token string) error {
	_, err := d.Exec(`DELETE FROM auth_sessions WHERE token = ?`, token)
	return err
}

func (d *DB) DeleteExpiredAuthSessions(now int64) error {
	_, err := d.Exec(`DELETE FROM auth_sessions WHERE expires_at < ?`, now)
	return err
}

// ---------- orgs ----------

func (d *DB) CreateOrg(o *model.Org) error {
	_, err := d.Exec(`INSERT INTO orgs (id, name, created_at) VALUES (?,?,?)`, o.ID, o.Name, o.CreatedAt)
	return err
}

func (d *DB) GetOrg(id string) (*model.Org, error) {
	var o model.Org
	err := d.QueryRow(`SELECT id, name, created_at FROM orgs WHERE id = ?`, id).
		Scan(&o.ID, &o.Name, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (d *DB) ListOrgsForUser(userID string) ([]model.Org, error) {
	rows, err := d.Query(`SELECT o.id, o.name, o.created_at FROM orgs o
		JOIN org_members m ON m.org_id = o.id WHERE m.user_id = ? ORDER BY o.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Org
	for rows.Next() {
		var o model.Org
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (d *DB) AddOrgMember(orgID, userID, role string, now int64) error {
	_, err := d.Exec(`INSERT INTO org_members (org_id, user_id, role, created_at) VALUES (?,?,?,?)`,
		orgID, userID, role, now)
	return err
}

func (d *DB) GetOrgRole(orgID, userID string) (string, error) {
	var role string
	err := d.QueryRow(`SELECT role FROM org_members WHERE org_id = ? AND user_id = ?`, orgID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func (d *DB) RemoveOrgMember(orgID, userID string) error {
	_, err := d.Exec(`DELETE FROM org_members WHERE org_id = ? AND user_id = ?`, orgID, userID)
	return err
}

func (d *DB) CountOrgOwners(orgID string) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM org_members WHERE org_id = ? AND role = 'owner'`, orgID).Scan(&n)
	return n, err
}

// ListOrgMembers returns members with their usernames.
func (d *DB) ListOrgMembers(orgID string) ([]OrgMemberView, error) {
	rows, err := d.Query(`SELECT m.user_id, u.username, m.role, m.created_at
		FROM org_members m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = ? ORDER BY u.username`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgMemberView
	for rows.Next() {
		var v OrgMemberView
		if err := rows.Scan(&v.UserID, &v.Username, &v.Role, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// OrgMemberView is a membership row with the username joined in.
type OrgMemberView struct {
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
}

// ---------- sessions ----------

func (d *DB) CreateSession(s *model.Session) error {
	_, err := d.Exec(`INSERT INTO sessions
		(id, name, org_id, image, shell, display, cpu, memory_mb, idle_minutes, volume, container_id,
		 status, status_msg, created_by, created_at, last_active_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Name, s.OrgID, s.Image, s.Shell, s.Display, s.CPU, s.MemoryMB, s.IdleMinutes,
		s.Volume, s.ContainerID, s.Status, s.StatusMsg, s.CreatedBy, s.CreatedAt, s.LastActiveAt)
	return err
}

func (d *DB) GetSession(id string) (*model.Session, error) {
	var s model.Session
	err := d.QueryRow(`SELECT id, name, org_id, image, shell, display, cpu, memory_mb, idle_minutes,
		volume, container_id, status, status_msg, created_by, created_at, last_active_at
		FROM sessions WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.OrgID, &s.Image, &s.Shell, &s.Display, &s.CPU, &s.MemoryMB, &s.IdleMinutes,
			&s.Volume, &s.ContainerID, &s.Status, &s.StatusMsg, &s.CreatedBy, &s.CreatedAt, &s.LastActiveAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListVisibleSessions returns sessions in orgID (if the caller names one they
// belong to) plus any sessions shared directly with the user.
func (d *DB) ListVisibleSessions(userID, orgID string) ([]model.Session, error) {
	const cols = `SELECT DISTINCT s.id, s.name, s.org_id, s.image, s.shell, s.display, s.cpu,
		s.memory_mb, s.idle_minutes, s.volume, s.container_id, s.status, s.status_msg,
		s.created_by, s.created_at, s.last_active_at
		FROM sessions s
		LEFT JOIN org_members m ON m.org_id = s.org_id AND m.user_id = ?
		LEFT JOIN shares sh ON sh.session_id = s.id AND sh.user_id = ?`
	var rows *sql.Rows
	var err error
	if orgID == "" {
		// No filter: every session in the user's orgs plus direct shares.
		// (A bare `s.org_id = ?` with an empty orgID would match nothing.)
		rows, err = d.Query(cols+`
			WHERE m.user_id IS NOT NULL OR sh.user_id IS NOT NULL
			ORDER BY s.created_at DESC`, userID, userID)
	} else {
		rows, err = d.Query(cols+`
			WHERE (s.org_id = ? AND m.user_id IS NOT NULL) OR sh.user_id IS NOT NULL
			ORDER BY s.created_at DESC`, userID, userID, orgID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		var s model.Session
		if err := rows.Scan(&s.ID, &s.Name, &s.OrgID, &s.Image, &s.Shell, &s.Display, &s.CPU, &s.MemoryMB,
			&s.IdleMinutes, &s.Volume, &s.ContainerID, &s.Status, &s.StatusMsg,
			&s.CreatedBy, &s.CreatedAt, &s.LastActiveAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListSessionsByStatus returns all sessions currently in one of the statuses.
func (d *DB) ListSessionsByStatus(statuses ...string) ([]model.Session, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	q := `SELECT id, name, org_id, image, shell, display, cpu, memory_mb, idle_minutes,
		volume, container_id, status, status_msg, created_by, created_at, last_active_at
		FROM sessions WHERE status IN (`
	args := make([]any, 0, len(statuses))
	for i, st := range statuses {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, st)
	}
	q += ")"
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		var s model.Session
		if err := rows.Scan(&s.ID, &s.Name, &s.OrgID, &s.Image, &s.Shell, &s.Display, &s.CPU, &s.MemoryMB,
			&s.IdleMinutes, &s.Volume, &s.ContainerID, &s.Status, &s.StatusMsg,
			&s.CreatedBy, &s.CreatedAt, &s.LastActiveAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) UpdateSessionStatus(id, status, msg string) error {
	_, err := d.Exec(`UPDATE sessions SET status = ?, status_msg = ? WHERE id = ?`, status, msg, id)
	return err
}

func (d *DB) UpdateSessionContainer(id, containerID, status string) error {
	_, err := d.Exec(`UPDATE sessions SET container_id = ?, status = ? WHERE id = ?`, containerID, status, id)
	return err
}

func (d *DB) UpdateSessionShell(id, shell string) error {
	_, err := d.Exec(`UPDATE sessions SET shell = ? WHERE id = ?`, shell, id)
	return err
}

func (d *DB) TouchSession(id string, now int64) error {
	_, err := d.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, now, id)
	return err
}

func (d *DB) DeleteSession(id string) error {
	_, err := d.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ---------- shares ----------

func (d *DB) CreateShare(sh *model.Share) error {
	_, err := d.Exec(`INSERT INTO shares (id, session_id, user_id, role, created_by, created_at)
		VALUES (?,?,?,?,?,?)`, sh.ID, sh.SessionID, sh.UserID, sh.Role, sh.CreatedBy, sh.CreatedAt)
	return err
}

func (d *DB) GetShare(id string) (*model.Share, error) {
	var sh model.Share
	err := d.QueryRow(`SELECT id, session_id, user_id, role, created_by, created_at
		FROM shares WHERE id = ?`, id).
		Scan(&sh.ID, &sh.SessionID, &sh.UserID, &sh.Role, &sh.CreatedBy, &sh.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sh, nil
}

// GetShareRole returns the share role for userID on sessionID, or "" if none.
func (d *DB) GetShareRole(sessionID, userID string) (string, error) {
	var role string
	err := d.QueryRow(`SELECT role FROM shares WHERE session_id = ? AND user_id = ?`, sessionID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func (d *DB) ListShares(sessionID string) ([]model.Share, error) {
	rows, err := d.Query(`SELECT sh.id, sh.session_id, sh.user_id, u.username, sh.role, sh.created_by, sh.created_at
		FROM shares sh JOIN users u ON u.id = sh.user_id
		WHERE sh.session_id = ? ORDER BY u.username`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Share
	for rows.Next() {
		var sh model.Share
		if err := rows.Scan(&sh.ID, &sh.SessionID, &sh.UserID, &sh.Username, &sh.Role, &sh.CreatedBy, &sh.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (d *DB) DeleteShare(id string) error {
	_, err := d.Exec(`DELETE FROM shares WHERE id = ?`, id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- images (admin-managed) ----------

func (d *DB) ListAllowedImages() ([]model.AllowedImage, error) {
	rows, err := d.Query(`SELECT id, image, desktop, created_at FROM allowed_images ORDER BY image`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AllowedImage
	for rows.Next() {
		var a model.AllowedImage
		if err := rows.Scan(&a.ID, &a.Image, &a.Desktop, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) AddAllowedImage(a *model.AllowedImage) error {
	_, err := d.Exec(`INSERT INTO allowed_images (id, image, desktop, created_at) VALUES (?, ?, ?, ?)`,
		a.ID, a.Image, a.Desktop, a.CreatedAt)
	return err
}

func (d *DB) DeleteAllowedImage(id string) error {
	_, err := d.Exec(`DELETE FROM allowed_images WHERE id = ?`, id)
	return err
}

// EnsureDefaultAllowedImage seeds the allowed list with image when empty.
func (d *DB) EnsureDefaultAllowedImage(image string, now int64) error {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM allowed_images`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return d.AddAllowedImage(&model.AllowedImage{ID: NewID(), Image: image, CreatedAt: now})
}

func (d *DB) IsImageAllowed(image string) (bool, error) {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM allowed_images WHERE image = ?`, image).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ImageProvidesDesktop reports whether a known image (allowed list or custom)
// is flagged as providing an X stack. Unknown images (admin free-text) return
// true — provisioning will fail descriptively if the X stack is absent.
func (d *DB) ImageProvidesDesktop(image string) (bool, error) {
	var desktop bool
	err := d.QueryRow(`SELECT desktop FROM allowed_images WHERE image = ?`, image).Scan(&desktop)
	if err == nil {
		return desktop, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	if name, isCustom := model.ParseCustomImageName(image); isCustom {
		c, cerr := d.GetCustomImageByName(name)
		if cerr != nil {
			return false, cerr
		}
		if c != nil {
			return c.Desktop, nil
		}
	}
	return true, nil
}

func (d *DB) ListCustomImages() ([]model.CustomImage, error) {
	rows, err := d.Query(`SELECT id, name, dockerfile, desktop, desktop_cmd, status, status_msg, build_progress, created_at, updated_at
		FROM custom_images ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CustomImage
	for rows.Next() {
		var c model.CustomImage
		if err := rows.Scan(&c.ID, &c.Name, &c.Dockerfile, &c.Desktop, &c.DesktopCmd, &c.Status, &c.StatusMsg, &c.BuildProgress, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) GetCustomImage(id string) (*model.CustomImage, error) {
	return d.getCustomImage(`id = ?`, id)
}

func (d *DB) GetCustomImageByName(name string) (*model.CustomImage, error) {
	return d.getCustomImage(`name = ?`, name)
}

func (d *DB) getCustomImage(where, arg string) (*model.CustomImage, error) {
	var c model.CustomImage
	err := d.QueryRow(`SELECT id, name, dockerfile, desktop, desktop_cmd, status, status_msg, build_progress, created_at, updated_at
		FROM custom_images WHERE `+where,
		arg).Scan(&c.ID, &c.Name, &c.Dockerfile, &c.Desktop, &c.DesktopCmd, &c.Status, &c.StatusMsg, &c.BuildProgress, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------- LLM providers ----------

func (d *DB) CreateLLMProvider(p *model.LLMProvider) error {
	_, err := d.Exec(`INSERT INTO llm_providers
		(id, name, provider_type, endpoint, api_key, model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.ProviderType, p.Endpoint, p.APIKey, p.Model, p.CreatedAt, p.UpdatedAt)
	return err
}

func (d *DB) GetLLMProvider(id string) (*model.LLMProvider, error) {
	var p model.LLMProvider
	err := d.QueryRow(`SELECT id, name, provider_type, endpoint, api_key, model, created_at, updated_at
		FROM llm_providers WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &p.ProviderType, &p.Endpoint, &p.APIKey, &p.Model, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) ListLLMProviders() ([]*model.LLMProvider, error) {
	rows, err := d.Query(`SELECT id, name, provider_type, endpoint, api_key, model, created_at, updated_at
		FROM llm_providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.LLMProvider
	for rows.Next() {
		var p model.LLMProvider
		if err := rows.Scan(&p.ID, &p.Name, &p.ProviderType, &p.Endpoint, &p.APIKey, &p.Model, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (d *DB) UpdateLLMProvider(p *model.LLMProvider) error {
	_, err := d.Exec(`UPDATE llm_providers SET name = ?, provider_type = ?, endpoint = ?,
		api_key = ?, model = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.ProviderType, p.Endpoint, p.APIKey, p.Model, p.UpdatedAt, p.ID)
	return err
}

func (d *DB) DeleteLLMProvider(id string) error {
	_, err := d.Exec(`DELETE FROM llm_providers WHERE id = ?`, id)
	return err
}

func (d *DB) CreateCustomImage(c *model.CustomImage) error {
	_, err := d.Exec(`INSERT INTO custom_images
		(id, name, dockerfile, desktop, desktop_cmd, status, status_msg, build_progress, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.Dockerfile, c.Desktop, c.DesktopCmd, c.Status, c.StatusMsg, c.BuildProgress, c.CreatedAt, c.UpdatedAt)
	return err
}

// DesktopDockerfile is the seeded custom image for desktop (X11/VNC)
// sessions. It provides Xvfb, openbox, and x11vnc; TermDock starts the X
// stack at provision time.
const DesktopDockerfile = `FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    xvfb openbox x11vnc \
 && apt-get clean && rm -rf /var/lib/apt/lists/*
`

// EnsureDesktopCustomImage seeds the "desktop" custom image when missing.
func (d *DB) EnsureDesktopCustomImage(now int64) error {
	existing, err := d.GetCustomImageByName("desktop")
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	return d.CreateCustomImage(&model.CustomImage{
		ID:         NewID(),
		Name:       "desktop",
		Dockerfile: DesktopDockerfile,
		Desktop:    true,
		Status:     model.CustomImageIdle,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}

// KDEDesktopDockerfile is the seeded "kde" custom image: full KDE Plasma
// desktop with Konqueror, for desktop sessions that want a complete DE.
const KDEDesktopDockerfile = `FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \\
    xvfb x11vnc dbus-x11 \\
    kde-plasma-desktop konqueror \\
 && apt-get clean && rm -rf /var/lib/apt/lists/*
`

// KDEDesktopCmd starts a KDE Plasma X11 session (needs a D-Bus session bus).
const KDEDesktopCmd = "dbus-run-session -- startplasma-x11"

// EnsureKDECustomImage seeds the "kde" custom image when missing.
func (d *DB) EnsureKDECustomImage(now int64) error {
	existing, err := d.GetCustomImageByName("kde")
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	return d.CreateCustomImage(&model.CustomImage{
		ID:         NewID(),
		Name:       "kde",
		Dockerfile: KDEDesktopDockerfile,
		Desktop:    true,
		DesktopCmd: KDEDesktopCmd,
		Status:     model.CustomImageIdle,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}

func (d *DB) UpdateCustomImage(c *model.CustomImage) error {
	_, err := d.Exec(`UPDATE custom_images SET name = ?, dockerfile = ?, desktop = ?, desktop_cmd = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.Dockerfile, c.Desktop, c.DesktopCmd, c.UpdatedAt, c.ID)
	return err
}

func (d *DB) UpdateCustomImageStatus(id, status, msg string, now int64) error {
	_, err := d.Exec(`UPDATE custom_images SET status = ?, status_msg = ?, updated_at = ? WHERE id = ?`,
		status, msg, now, id)
	return err
}

// UpdateBuildProgress sets the 0-100 build progress for a custom image.
func (d *DB) UpdateBuildProgress(id string, progress int, now int64) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	_, err := d.Exec(`UPDATE custom_images SET build_progress = ?, updated_at = ? WHERE id = ?`,
		progress, now, id)
	return err
}

func (d *DB) DeleteCustomImage(id string) error {
	_, err := d.Exec(`DELETE FROM custom_images WHERE id = ?`, id)
	return err
}
