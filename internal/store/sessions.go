package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is a browser login session. The row ID is a hash of the cookie
// value, so a database leak does not hand out usable session tokens.
type Session struct {
	ID        string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
	UserAgent string
	IP        string
}

// CreateSession stores a session keyed by the already-hashed token.
func (s *Store) CreateSession(sess Session) error {
	if sess.ID == "" || sess.UserID == "" {
		return errors.New("store: session id and user id are required")
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, user_id, created_at, expires_at, user_agent, ip)
         VALUES (?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID,
		sess.CreatedAt.UTC().Format(time.RFC3339Nano),
		sess.ExpiresAt.UTC().Format(time.RFC3339Nano),
		sess.UserAgent, sess.IP)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// SessionUser resolves a hashed session token to its user, rejecting expired
// sessions and disabled accounts in the same query path.
func (s *Store) SessionUser(hashedID string) (Session, User, error) {
	row := s.db.QueryRow(
		`SELECT s.id, s.user_id, s.created_at, s.expires_at, s.user_agent, s.ip,
                u.id, u.email, u.name, u.password_hash, u.role, u.provider,
                u.provider_subject, u.disabled, u.created_at, u.last_login_at
           FROM sessions s
           JOIN users u ON u.id = s.user_id
          WHERE s.id = ?`, hashedID)

	var (
		sess      Session
		createdAt string
		expiresAt string

		u          User
		role       string
		disabled   int
		uCreated   string
		lastLogin  sql.NullString
		providerID string
	)
	err := row.Scan(&sess.ID, &sess.UserID, &createdAt, &expiresAt, &sess.UserAgent, &sess.IP,
		&u.ID, &u.Email, &u.Name, &u.PasswordHash, &role, &providerID,
		&u.ProviderSubject, &disabled, &uCreated, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, User{}, ErrNotFound
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("store: load session: %w", err)
	}

	sess.CreatedAt = parseTime(createdAt)
	sess.ExpiresAt = parseTime(expiresAt)
	if time.Now().After(sess.ExpiresAt) {
		// Clean up lazily; a background sweep also runs.
		_ = s.DeleteSession(hashedID)
		return Session{}, User{}, ErrNotFound
	}

	u.Role = Role(role)
	u.Provider = providerID
	u.Disabled = disabled != 0
	u.CreatedAt = parseTime(uCreated)
	if lastLogin.Valid {
		t := parseTime(lastLogin.String)
		u.LastLoginAt = &t
	}
	if u.Disabled {
		return Session{}, User{}, ErrNotFound
	}
	return sess, u, nil
}

// DeleteSession removes one session (logout).
func (s *Store) DeleteSession(hashedID string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, hashedID)
	return err
}

// DeleteUserSessions logs a user out everywhere, e.g. after a password change.
func (s *Store) DeleteUserSessions(userID string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeExpiredSessions deletes sessions past their expiry and reports how many
// were removed.
func (s *Store) PurgeExpiredSessions() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, nowString())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
