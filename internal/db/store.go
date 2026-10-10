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
		(id, name, org_id, image, shell, cpu, memory_mb, idle_minutes, volume, container_id,
		 status, status_msg, created_by, created_at, last_active_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Name, s.OrgID, s.Image, s.Shell, s.CPU, s.MemoryMB, s.IdleMinutes,
		s.Volume, s.ContainerID, s.Status, s.StatusMsg, s.CreatedBy, s.CreatedAt, s.LastActiveAt)
	return err
}

func (d *DB) GetSession(id string) (*model.Session, error) {
	var s model.Session
	err := d.QueryRow(`SELECT id, name, org_id, image, shell, cpu, memory_mb, idle_minutes,
		volume, container_id, status, status_msg, created_by, created_at, last_active_at
		FROM sessions WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.OrgID, &s.Image, &s.Shell, &s.CPU, &s.MemoryMB, &s.IdleMinutes,
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
	rows, err := d.Query(`SELECT DISTINCT s.id, s.name, s.org_id, s.image, s.shell, s.cpu,
		s.memory_mb, s.idle_minutes, s.volume, s.container_id, s.status, s.status_msg,
		s.created_by, s.created_at, s.last_active_at
		FROM sessions s
		LEFT JOIN org_members m ON m.org_id = s.org_id AND m.user_id = ?
		LEFT JOIN shares sh ON sh.session_id = s.id AND sh.user_id = ?
		WHERE (s.org_id = ? AND m.user_id IS NOT NULL) OR sh.user_id IS NOT NULL
		ORDER BY s.created_at DESC`, userID, userID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		var s model.Session
		if err := rows.Scan(&s.ID, &s.Name, &s.OrgID, &s.Image, &s.Shell, &s.CPU, &s.MemoryMB,
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
	q := `SELECT id, name, org_id, image, shell, cpu, memory_mb, idle_minutes,
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
		if err := rows.Scan(&s.ID, &s.Name, &s.OrgID, &s.Image, &s.Shell, &s.CPU, &s.MemoryMB,
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
