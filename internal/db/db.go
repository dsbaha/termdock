// Package db owns the SQLite connection and schema migrations.
package db

import (
	"strings"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps a *sql.DB with small helpers.
type DB struct {
	*sql.DB
}

// Open creates the data directory, opens the SQLite database, and migrates it.
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "termdock.db")
	sqldb, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1) // SQLite + WAL: single writer keeps things simple
	d := &DB{sqldb}
	if err := d.migrate(); err != nil {
		sqldb.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			token TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_sessions_user ON auth_sessions(user_id)`,
		`CREATE TABLE IF NOT EXISTS orgs (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS org_members (
			org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (org_id, user_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_org_members_user ON org_members(user_id)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			image TEXT NOT NULL,
			shell TEXT NOT NULL,
			display TEXT NOT NULL DEFAULT 'terminal',
			cpu REAL NOT NULL,
			memory_mb INTEGER NOT NULL,
			idle_minutes INTEGER NOT NULL,
			volume TEXT NOT NULL,
			container_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			status_msg TEXT NOT NULL DEFAULT '',
			created_by TEXT NOT NULL REFERENCES users(id),
			created_at INTEGER NOT NULL,
			last_active_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_org ON sessions(org_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status)`,
		`CREATE TABLE IF NOT EXISTS shares (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role TEXT NOT NULL,
			created_by TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE (session_id, user_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_session ON shares(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_shares_user ON shares(user_id)`,
		`CREATE TABLE IF NOT EXISTS allowed_images (
			id TEXT PRIMARY KEY,
			image TEXT NOT NULL UNIQUE,
			desktop INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_images (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			dockerfile TEXT NOT NULL,
			desktop INTEGER NOT NULL DEFAULT 0,
			desktop_cmd TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'idle',
			status_msg TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// Existing databases predate the display column.
	if _, err := d.Exec(`ALTER TABLE sessions ADD COLUMN display TEXT NOT NULL DEFAULT 'terminal'`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate: %w", err)
	}
	// Existing databases predate the desktop flag on images.
	for _, alter := range []string{
		`ALTER TABLE allowed_images ADD COLUMN desktop INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE custom_images ADD COLUMN desktop INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE custom_images ADD COLUMN desktop_cmd TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := d.Exec(alter); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// NewID returns a random 32-hex-char identifier.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewToken returns a random 64-hex-char auth token.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Now returns the current Unix time in seconds.
func Now() int64 { return time.Now().Unix() }
