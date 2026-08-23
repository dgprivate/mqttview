package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// Role controls what a user may do. Roles are ordered: admin > operator >
// viewer.
type Role string

const (
	// RoleAdmin can manage users, connections and plugins.
	RoleAdmin Role = "admin"
	// RoleOperator can publish and change subscriptions, but not manage users.
	RoleOperator Role = "operator"
	// RoleViewer has read-only access.
	RoleViewer Role = "viewer"
)

// ValidRole reports whether r is a role mqttview knows.
func ValidRole(r Role) bool {
	return r == RoleAdmin || r == RoleOperator || r == RoleViewer
}

// AtLeast reports whether r is at least as privileged as want.
func (r Role) AtLeast(want Role) bool {
	rank := map[Role]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}
	return rank[r] >= rank[want]
}

// ProviderLocal marks an account that signs in with a password held here,
// rather than through an identity provider.
const ProviderLocal = "local"

// User is an mqttview account. Accounts are created either locally (with a
// password) or on first SSO login.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	// PasswordHash is empty for SSO-only accounts. Never serialised.
	PasswordHash string `json:"-"`
	Role         Role   `json:"role"`
	// Provider is ProviderLocal or the SSO provider ID.
	Provider string `json:"provider"`
	// ProviderSubject is the stable subject claim from the SSO provider.
	ProviderSubject string     `json:"-"`
	Disabled        bool       `json:"disabled"`
	CreatedAt       time.Time  `json:"createdAt"`
	LastLoginAt     *time.Time `json:"lastLoginAt,omitempty"`
	// TOTPSecretEnc is the encrypted TOTP seed. Never serialised: the plaintext
	// seed is shown exactly once, during enrolment, and never again.
	TOTPSecretEnc string `json:"-"`
	// TOTPConfirmedAt is set when the user proved they could generate a code.
	// A secret that was issued but never confirmed does not gate sign-in, so a
	// half-finished enrolment cannot lock somebody out.
	TOTPConfirmedAt *time.Time `json:"totpConfirmedAt,omitempty"`
}

// TwoFactorEnabled reports whether this account must present a second factor.
func (u User) TwoFactorEnabled() bool {
	return u.TOTPSecretEnc != "" && u.TOTPConfirmedAt != nil
}

// NormalizeEmail lowercases and trims an address so lookups are consistent.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidEmail reports whether an address is one an account can be created with.
//
// net/mail rather than a regular expression of our own: it accepts
// "admin@localhost", which is what a first run bootstraps with and what a
// stricter rule would reject, and it refuses the strings that would break SSO
// linking, which matches accounts by address.
func ValidEmail(email string) bool {
	addr, err := mail.ParseAddress(NormalizeEmail(email))
	return err == nil && addr.Address == NormalizeEmail(email)
}

const userColumns = `id, email, name, password_hash, role, provider, provider_subject, disabled, created_at, last_login_at, totp_secret_enc, totp_confirmed_at`

// CreateUser inserts a user. The caller supplies the ID so it can be reused
// for auditing before the write succeeds.
func (s *Store) CreateUser(u User) (User, error) {
	u.Email = NormalizeEmail(u.Email)
	if u.Email == "" {
		return User{}, errors.New("store: email is required")
	}
	if !ValidRole(u.Role) {
		return User{}, fmt.Errorf("store: invalid role %q", u.Role)
	}
	if u.Provider == "" {
		u.Provider = ProviderLocal
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}

	_, err := s.db.Exec(
		`INSERT INTO users (`+userColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.Name, u.PasswordHash, string(u.Role), u.Provider,
		u.ProviderSubject, boolToInt(u.Disabled), u.CreatedAt.Format(time.RFC3339Nano), nil,
		u.TOTPSecretEnc, nil)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("store: create user: %w", err)
	}
	return u, nil
}

// GetUser looks a user up by ID.
func (s *Store) GetUser(id string) (User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// GetUserByEmail looks a user up by email address.
func (s *Store) GetUserByEmail(email string) (User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE email = ?`, NormalizeEmail(email)))
}

// GetUserByProviderSubject looks a user up by SSO identity.
func (s *Store) GetUserByProviderSubject(provider, subject string) (User, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT `+userColumns+` FROM users WHERE provider = ? AND provider_subject = ?`,
		provider, subject))
}

// ListUsers returns all users ordered by email.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY email`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := s.scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers reports how many accounts exist; used to decide whether the
// first-run bootstrap should offer to create an admin.
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins reports how many enabled admins exist, so the last one cannot
// be deleted or demoted by accident.
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ? AND disabled = 0`, string(RoleAdmin)).Scan(&n)
	return n, err
}

// UpdateUser writes the mutable profile fields.
func (s *Store) UpdateUser(u User) error {
	if !ValidRole(u.Role) {
		return fmt.Errorf("store: invalid role %q", u.Role)
	}
	res, err := s.db.Exec(
		`UPDATE users SET email = ?, name = ?, role = ?, disabled = ? WHERE id = ?`,
		NormalizeEmail(u.Email), u.Name, string(u.Role), boolToInt(u.Disabled), u.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: update user: %w", err)
	}
	return affected(res)
}

// SetPasswordHash sets or clears a user's local password.
func (s *Store) SetPasswordHash(id, hash string) error {
	res, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	if err != nil {
		return fmt.Errorf("store: set password: %w", err)
	}
	return affected(res)
}

// LinkProvider attaches an SSO identity to an existing account, which is how a
// local user upgrades to SSO login.
func (s *Store) LinkProvider(id, provider, subject string) error {
	res, err := s.db.Exec(
		`UPDATE users SET provider = ?, provider_subject = ? WHERE id = ?`,
		provider, subject, id)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: link provider: %w", err)
	}
	return affected(res)
}

// TouchLogin records a successful login.
func (s *Store) TouchLogin(id string) error {
	// Reports a missing row like every other update here does. The caller only
	// logs it, but a silent no-op would be the odd one out in this file.
	res, err := s.db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, nowString(), id)
	if err != nil {
		return fmt.Errorf("store: record login time: %w", err)
	}
	return affected(res)
}

// DeleteUser removes an account and, by cascade, its sessions.
func (s *Store) DeleteUser(id string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete user: %w", err)
	}
	return affected(res)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanUser(row rowScanner) (User, error) {
	var (
		u          User
		role       string
		disabled   int
		createdAt  string
		lastLogin  sql.NullString
		providerID string
		totpAt     sql.NullString
	)
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &role, &providerID,
		&u.ProviderSubject, &disabled, &createdAt, &lastLogin, &u.TOTPSecretEnc, &totpAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: scan user: %w", err)
	}
	u.Role = Role(role)
	u.Provider = providerID
	u.Disabled = disabled != 0
	u.CreatedAt = parseTime(createdAt)
	if lastLogin.Valid {
		t := parseTime(lastLogin.String)
		u.LastLoginAt = &t
	}
	if totpAt.Valid {
		t := parseTime(totpAt.String)
		u.TOTPConfirmedAt = &t
	}
	return u, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	// modernc's driver reports constraint failures in the message; matching on
	// it avoids depending on the driver's internal error types.
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT")
}
