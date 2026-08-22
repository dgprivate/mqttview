package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Two-factor state lives on the user row, and recovery codes in a table of
// their own. Both are written through this file so the "confirmed" flag and the
// secret can never drift apart.

// SetTOTPSecret stores a newly issued, not yet confirmed secret. Any previous
// secret and every recovery code are discarded: re-enrolling must invalidate
// whatever the old authenticator held.
func (s *Store) SetTOTPSecret(userID, secretEnc string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin TOTP enrolment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE users SET totp_secret_enc = ?, totp_confirmed_at = NULL WHERE id = ?`,
		secretEnc, userID); err != nil {
		return fmt.Errorf("store: set TOTP secret: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: clear recovery codes: %w", err)
	}
	return tx.Commit()
}

// ConfirmTOTP marks the secret as proven, which is what makes sign-in ask for
// it. It refuses when no secret is pending, so a confirmation cannot enable
// two-factor on an account that has nothing to verify against.
func (s *Store) ConfirmTOTP(userID string, at time.Time) error {
	res, err := s.db.Exec(
		`UPDATE users SET totp_confirmed_at = ?
         WHERE id = ? AND totp_secret_enc <> '' AND totp_confirmed_at IS NULL`,
		at.UTC().Format(time.RFC3339Nano), userID)
	if err != nil {
		return fmt.Errorf("store: confirm TOTP: %w", err)
	}
	return affected(res)
}

// DisableTOTP removes the secret and every recovery code.
func (s *Store) DisableTOTP(userID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin TOTP removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE users SET totp_secret_enc = '', totp_confirmed_at = NULL WHERE id = ?`,
		userID); err != nil {
		return fmt.Errorf("store: disable TOTP: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: clear recovery codes: %w", err)
	}
	return tx.Commit()
}

// ReplaceRecoveryCodes stores a fresh set of hashes, discarding the old ones.
func (s *Store) ReplaceRecoveryCodes(userID string, hashes []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin recovery codes: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: clear recovery codes: %w", err)
	}
	for _, h := range hashes {
		if _, err := tx.Exec(
			`INSERT INTO recovery_codes (id, user_id, hash) VALUES (?, ?, ?)`,
			uuid.NewString(), userID, h); err != nil {
			return fmt.Errorf("store: insert recovery code: %w", err)
		}
	}
	return tx.Commit()
}

// RecoveryCodeHashes returns the unused hashes for a user.
func (s *Store) RecoveryCodeHashes(userID string) (map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT id, hash FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list recovery codes: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, fmt.Errorf("store: scan recovery code: %w", err)
		}
		out[id] = hash
	}
	return out, rows.Err()
}

// UseRecoveryCode marks one code as spent. It updates only a row that is still
// unused, so two requests racing on the same code cannot both succeed: the
// second changes no rows and is told so.
func (s *Store) UseRecoveryCode(id string, at time.Time) error {
	res, err := s.db.Exec(
		`UPDATE recovery_codes SET used_at = ? WHERE id = ? AND used_at IS NULL`,
		at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("store: use recovery code: %w", err)
	}
	return affected(res)
}

// CountUnusedRecoveryCodes reports how many are left, so the UI can say when
// they are running out.
func (s *Store) CountUnusedRecoveryCodes(userID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`,
		userID).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: count recovery codes: %w", err)
	}
	return n, nil
}
