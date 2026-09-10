package store

import (
	"database/sql"
	"fmt"
	"time"
)

// DefaultAuditRetention bounds the log. It is a record of what was done, not
// an archive: a year of clearing retained messages is not something anybody
// reads, and an unbounded table in the same file as the connections is a
// database that grows without anybody choosing it.
const DefaultAuditRetention = 5000

// AuditEntry is one action that changed something outside mqttview.
type AuditEntry struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	UserID   string    `json:"userId,omitempty"`
	Username string    `json:"username"`
	Action   string    `json:"action"`
	Target   string    `json:"target,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// Audit records an action and prunes the log back to keep entries.
func (s *Store) Audit(entry AuditEntry, keep int) error {
	if keep <= 0 {
		keep = DefaultAuditRetention
	}
	if entry.At.IsZero() {
		entry.At = time.Now()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT INTO audit_log (at, user_id, username, action, target, detail)
         VALUES (?, ?, ?, ?, ?, ?)`,
		entry.At.Format(time.RFC3339Nano), nullIfEmpty(entry.UserID), entry.Username,
		entry.Action, entry.Target, entry.Detail,
	); err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}

	if _, err := tx.Exec(
		`DELETE FROM audit_log WHERE id NOT IN (SELECT id FROM audit_log ORDER BY id DESC LIMIT ?)`,
		keep,
	); err != nil {
		return fmt.Errorf("store: prune audit log: %w", err)
	}

	return tx.Commit()
}

// AuditLog returns recent entries, newest first.
func (s *Store) AuditLog(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, at, user_id, username, action, target, detail
         FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: audit log: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var (
			e      AuditEntry
			userID sql.NullString
			at     string
		)
		if err := rows.Scan(&e.ID, &at, &userID, &e.Username, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, fmt.Errorf("store: scan audit entry: %w", err)
		}
		e.UserID = userID.String
		e.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, e)
	}
	return out, rows.Err()
}
