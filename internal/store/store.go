// Package store is mqttview's persistence layer: users, login sessions,
// broker connection definitions and plugin state, all in a single SQLite file.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so cross-compiling stays trivial

	"github.com/mqttview/mqttview/internal/secrets"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned on a uniqueness violation, e.g. a duplicate email.
var ErrConflict = errors.New("store: already exists")

// Store wraps the database handle plus the box used to encrypt broker
// credentials before they touch disk.
type Store struct {
	db  *sql.DB
	box *secrets.Box
}

// Open creates or opens the database at path, applying migrations.
func Open(path string, box *secrets.Box) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}

	// WAL keeps the UI responsive while a plugin writes state; the busy
	// timeout absorbs the brief contention that remains.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// SQLite writers serialise anyway; a small pool avoids lock churn.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	s := &Store{db: db, box: box}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the underlying handle for plugins that need raw SQL.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// migrations are applied in order and recorded in schema_migrations. Never
// edit an applied migration; append a new one instead.
var migrations = []struct {
	name string
	stmt string
}{
	{
		name: "0001_init",
		stmt: `
CREATE TABLE users (
    id               TEXT PRIMARY KEY,
    email            TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL DEFAULT '',
    password_hash    TEXT NOT NULL DEFAULT '',
    role             TEXT NOT NULL DEFAULT 'viewer',
    provider         TEXT NOT NULL DEFAULT 'local',
    provider_subject TEXT NOT NULL DEFAULT '',
    disabled         INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    last_login_at    TEXT
);

CREATE UNIQUE INDEX users_provider_subject_idx
    ON users(provider, provider_subject)
    WHERE provider_subject <> '';

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    user_agent TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX sessions_user_idx ON sessions(user_id);
CREATE INDEX sessions_expiry_idx ON sessions(expires_at);

CREATE TABLE connections (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    url                TEXT NOT NULL,
    version            INTEGER NOT NULL,
    client_id          TEXT NOT NULL,
    username           TEXT NOT NULL DEFAULT '',
    password_enc       TEXT NOT NULL DEFAULT '',
    keep_alive         INTEGER NOT NULL DEFAULT 60,
    clean_start        INTEGER NOT NULL DEFAULT 1,
    session_expiry     INTEGER NOT NULL DEFAULT 0,
    connect_timeout    INTEGER NOT NULL DEFAULT 10,
    tls_json           TEXT NOT NULL DEFAULT '{}',
    will_json          TEXT,
    subscriptions_json TEXT NOT NULL DEFAULT '[]',
    auto_connect       INTEGER NOT NULL DEFAULT 0,
    history_size       INTEGER NOT NULL DEFAULT 0,
    created_by         TEXT,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);

CREATE TABLE plugin_settings (
    plugin_id     TEXT PRIMARY KEY,
    enabled       INTEGER NOT NULL DEFAULT 0,
    settings_json TEXT NOT NULL DEFAULT '{}',
    updated_at    TEXT NOT NULL
);

CREATE TABLE plugin_state (
    plugin_id  TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (plugin_id, key)
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`,
	},
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
        name       TEXT PRIMARY KEY,
        applied_at TEXT NOT NULL
    )`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var seen int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, m.name).Scan(&seen)
		if err != nil {
			return fmt.Errorf("store: check migration %s: %w", m.name, err)
		}
		if seen > 0 {
			continue
		}

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(m.stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			m.name, nowString()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %s: %w", m.name, err)
		}
	}
	return nil
}

// Setting reads a key from the generic settings table.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting writes a key to the generic settings table.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
         ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
