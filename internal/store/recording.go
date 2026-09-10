package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultRecordKeep is how many recorded messages a connection keeps when it
// does not say otherwise. It bounds the one table whose size follows broker
// traffic rather than configuration.
const DefaultRecordKeep = 200_000

// RecordedMessage is one message written to disk.
type RecordedMessage struct {
	ID           int64     `json:"id"`
	ConnectionID string    `json:"connectionId"`
	Topic        string    `json:"topic"`
	Payload      []byte    `json:"-"`
	QoS          byte      `json:"qos"`
	Retain       bool      `json:"retain"`
	ReceivedAt   time.Time `json:"receivedAt"`
}

// AppendRecorded writes a batch in one transaction.
//
// A batch rather than a row: a broker at forty-five messages a second is
// forty-five transactions a second otherwise, each with its own fsync, which
// is how a recording feature becomes the reason the whole thing is slow.
func (s *Store) AppendRecorded(batch []RecordedMessage) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: record messages: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`INSERT INTO recorded_messages (connection_id, topic, payload, qos, retain, received_at)
         VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: record messages: %w", err)
	}
	defer stmt.Close()

	for _, m := range batch {
		if _, err := stmt.Exec(m.ConnectionID, m.Topic, m.Payload, int(m.QoS),
			boolToInt(m.Retain), m.ReceivedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("store: record messages: %w", err)
		}
	}
	return tx.Commit()
}

// PruneRecorded drops all but the newest keep rows for a connection and
// reports how many went.
//
// Called on a timer rather than after every insert: the delete is a scan and
// doing it per batch would spend more time pruning than recording.
func (s *Store) PruneRecorded(connectionID string, keep int) (int64, error) {
	if keep <= 0 {
		keep = DefaultRecordKeep
	}
	res, err := s.db.Exec(
		`DELETE FROM recorded_messages
          WHERE connection_id = ?
            AND id NOT IN (
                SELECT id FROM recorded_messages WHERE connection_id = ? ORDER BY id DESC LIMIT ?
            )`, connectionID, connectionID, keep)
	if err != nil {
		return 0, fmt.Errorf("store: prune recordings: %w", err)
	}
	// The delete itself has already succeeded. A driver that cannot report the
	// count is still worth surfacing rather than reporting nought rows
	// removed, which reads as "retention is not working".
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune recordings: counting removed rows: %w", err)
	}
	return n, nil
}

// RecordedQuery is what a caller is asking for.
type RecordedQuery struct {
	ConnectionID string
	// Topic is an exact topic. Empty means every topic.
	Topic string
	// Since and Until bound the window; zero values mean unbounded.
	Since time.Time
	Until time.Time
	Limit int
}

// Recorded reads back what was recorded, newest first.
func (s *Store) Recorded(q RecordedQuery) ([]RecordedMessage, error) {
	if q.Limit <= 0 {
		q.Limit = 500
	}

	sqlText := `SELECT id, connection_id, topic, payload, qos, retain, received_at
                FROM recorded_messages WHERE connection_id = ?`
	args := []any{q.ConnectionID}
	if t := strings.TrimSpace(q.Topic); t != "" {
		sqlText += ` AND topic = ?`
		args = append(args, t)
	}
	if !q.Since.IsZero() {
		sqlText += ` AND received_at >= ?`
		args = append(args, q.Since.Format(time.RFC3339Nano))
	}
	if !q.Until.IsZero() {
		sqlText += ` AND received_at <= ?`
		args = append(args, q.Until.Format(time.RFC3339Nano))
	}
	sqlText += ` ORDER BY id DESC LIMIT ?`
	args = append(args, q.Limit)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read recordings: %w", err)
	}
	defer rows.Close()

	var out []RecordedMessage
	for rows.Next() {
		var (
			m          RecordedMessage
			qos        int
			retain     int
			receivedAt string
		)
		if err := rows.Scan(&m.ID, &m.ConnectionID, &m.Topic, &m.Payload, &qos, &retain, &receivedAt); err != nil {
			return nil, fmt.Errorf("store: scan recording: %w", err)
		}
		m.QoS = byte(qos)
		m.Retain = retain != 0
		m.ReceivedAt, _ = time.Parse(time.RFC3339Nano, receivedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordingStats is what the UI shows about a connection's recording, so that
// a setting which makes the database grow is not invisible.
type RecordingStats struct {
	Rows   int64      `json:"rows"`
	Oldest *time.Time `json:"oldest,omitempty"`
	Newest *time.Time `json:"newest,omitempty"`
	Bytes  int64      `json:"payloadBytes"`
}

// RecordingStats counts what a connection has recorded.
func (s *Store) RecordingStats(connectionID string) (RecordingStats, error) {
	var (
		stats  RecordingStats
		oldest sql.NullString
		newest sql.NullString
		bytes  sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT COUNT(*), MIN(received_at), MAX(received_at), SUM(LENGTH(payload))
         FROM recorded_messages WHERE connection_id = ?`, connectionID,
	).Scan(&stats.Rows, &oldest, &newest, &bytes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RecordingStats{}, fmt.Errorf("store: recording stats: %w", err)
	}
	if oldest.Valid {
		if t, err := time.Parse(time.RFC3339Nano, oldest.String); err == nil {
			stats.Oldest = &t
		}
	}
	if newest.Valid {
		if t, err := time.Parse(time.RFC3339Nano, newest.String); err == nil {
			stats.Newest = &t
		}
	}
	stats.Bytes = bytes.Int64
	return stats, nil
}

// DeleteRecorded forgets everything a connection recorded.
func (s *Store) DeleteRecorded(connectionID string) error {
	if _, err := s.db.Exec(`DELETE FROM recorded_messages WHERE connection_id = ?`, connectionID); err != nil {
		return fmt.Errorf("store: delete recordings: %w", err)
	}
	return nil
}
