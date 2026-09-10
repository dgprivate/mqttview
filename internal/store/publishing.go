package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultPublishHistory is how many publishes are kept per connection. It is a
// convenience, not a record: a script publishing in a loop must not be able to
// grow the database without limit, and the oldest entries are the ones nobody
// is going to look for again.
const DefaultPublishHistory = 500

// PublishRecord is one message that was sent, kept so it can be found and sent
// again.
type PublishRecord struct {
	ID           int64     `json:"id"`
	ConnectionID string    `json:"connectionId"`
	UserID       string    `json:"userId,omitempty"`
	Username     string    `json:"username,omitempty"`
	Topic        string    `json:"topic"`
	Payload      []byte    `json:"-"`
	QoS          byte      `json:"qos"`
	Retain       bool      `json:"retain"`
	PublishedAt  time.Time `json:"publishedAt"`
}

// RecordPublish appends to a connection's publish history and prunes it back
// to keep entries.
func (s *Store) RecordPublish(rec PublishRecord, keep int) error {
	if keep <= 0 {
		keep = DefaultPublishHistory
	}
	if rec.PublishedAt.IsZero() {
		rec.PublishedAt = time.Now()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: record publish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT INTO publish_history (connection_id, user_id, username, topic, payload, qos, retain, published_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ConnectionID, nullIfEmpty(rec.UserID), rec.Username, rec.Topic, rec.Payload,
		int(rec.QoS), boolToInt(rec.Retain), rec.PublishedAt.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("store: record publish: %w", err)
	}

	// Pruning inside the same transaction as the insert, so the history can
	// never be observed over its bound.
	if _, err := tx.Exec(
		`DELETE FROM publish_history
          WHERE connection_id = ?
            AND id NOT IN (
                SELECT id FROM publish_history WHERE connection_id = ? ORDER BY id DESC LIMIT ?
            )`,
		rec.ConnectionID, rec.ConnectionID, keep,
	); err != nil {
		return fmt.Errorf("store: prune publish history: %w", err)
	}

	return tx.Commit()
}

// PublishHistory returns a connection's recent publishes, newest first. A
// non-empty query filters on the topic and the payload, which is how somebody
// finds the message they sent last week without remembering the topic.
func (s *Store) PublishHistory(connectionID, query string, limit int) ([]PublishRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	sqlText := `SELECT id, connection_id, user_id, username, topic, payload, qos, retain, published_at
                FROM publish_history WHERE connection_id = ?`
	args := []any{connectionID}
	if q := strings.TrimSpace(query); q != "" {
		// CAST so a binary payload does not fail the comparison; a match
		// inside binary is unlikely but the query must not error on one.
		sqlText += ` AND (topic LIKE ? ESCAPE '\' OR CAST(payload AS TEXT) LIKE ? ESCAPE '\')`
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like)
	}
	sqlText += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: publish history: %w", err)
	}
	defer rows.Close()

	var out []PublishRecord
	for rows.Next() {
		rec, err := scanPublish(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// GetPublish returns one entry, which is what republishing reads.
func (s *Store) GetPublish(connectionID string, id int64) (PublishRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, connection_id, user_id, username, topic, payload, qos, retain, published_at
         FROM publish_history WHERE id = ? AND connection_id = ?`, id, connectionID)
	return scanPublish(row)
}

// ClearPublishHistory forgets everything a connection has sent.
func (s *Store) ClearPublishHistory(connectionID string) error {
	if _, err := s.db.Exec(`DELETE FROM publish_history WHERE connection_id = ?`, connectionID); err != nil {
		return fmt.Errorf("store: clear publish history: %w", err)
	}
	return nil
}

func scanPublish(row rowScanner) (PublishRecord, error) {
	var (
		rec         PublishRecord
		userID      sql.NullString
		qos         int
		retain      int
		publishedAt string
	)
	err := row.Scan(&rec.ID, &rec.ConnectionID, &userID, &rec.Username, &rec.Topic,
		&rec.Payload, &qos, &retain, &publishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishRecord{}, ErrNotFound
	}
	if err != nil {
		return PublishRecord{}, fmt.Errorf("store: scan publish: %w", err)
	}
	rec.UserID = userID.String
	rec.QoS = byte(qos)
	rec.Retain = retain != 0
	rec.PublishedAt, _ = time.Parse(time.RFC3339Nano, publishedAt)
	return rec, nil
}

// SavedMessage is a message somebody chose to keep, as opposed to one that
// merely happened. ConnectionID is empty when the message belongs to no
// particular broker, which is what makes one collection usable against
// staging and then against production.
type SavedMessage struct {
	ID           string    `json:"id"`
	ConnectionID string    `json:"connectionId,omitempty"`
	Folder       string    `json:"folder"`
	Name         string    `json:"name"`
	Topic        string    `json:"topic"`
	Payload      []byte    `json:"-"`
	QoS          byte      `json:"qos"`
	Retain       bool      `json:"retain"`
	SortOrder    int       `json:"sortOrder"`
	CreatedBy    string    `json:"createdBy,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// SaveMessage inserts or replaces a saved message.
func (s *Store) SaveMessage(msg SavedMessage) error {
	if strings.TrimSpace(msg.ID) == "" {
		return errors.New("store: a saved message needs an id")
	}
	if strings.TrimSpace(msg.Name) == "" {
		return errors.New("store: a saved message needs a name")
	}
	if strings.TrimSpace(msg.Topic) == "" {
		return errors.New("store: a saved message needs a topic")
	}

	now := time.Now()
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = now
	}
	_, err := s.db.Exec(
		`INSERT INTO saved_messages
            (id, connection_id, folder, name, topic, payload, qos, retain, sort_order, created_by, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET
            connection_id = excluded.connection_id,
            folder = excluded.folder,
            name = excluded.name,
            topic = excluded.topic,
            payload = excluded.payload,
            qos = excluded.qos,
            retain = excluded.retain,
            sort_order = excluded.sort_order,
            updated_at = excluded.updated_at`,
		msg.ID, nullIfEmpty(msg.ConnectionID), msg.Folder, msg.Name, msg.Topic, msg.Payload,
		int(msg.QoS), boolToInt(msg.Retain), msg.SortOrder, nullIfEmpty(msg.CreatedBy),
		msg.CreatedAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: save message: %w", err)
	}
	return nil
}

// SavedMessages returns the collection for a connection: the ones scoped to it
// and the ones scoped to no connection at all, which apply everywhere.
func (s *Store) SavedMessages(connectionID string) ([]SavedMessage, error) {
	rows, err := s.db.Query(
		`SELECT id, connection_id, folder, name, topic, payload, qos, retain, sort_order,
                created_by, created_at, updated_at
         FROM saved_messages
         WHERE connection_id IS NULL OR connection_id = ?
         ORDER BY folder, sort_order, name`, nullIfEmpty(connectionID))
	if err != nil {
		return nil, fmt.Errorf("store: saved messages: %w", err)
	}
	defer rows.Close()

	var out []SavedMessage
	for rows.Next() {
		msg, err := scanSaved(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// GetSavedMessage returns one saved message.
func (s *Store) GetSavedMessage(id string) (SavedMessage, error) {
	row := s.db.QueryRow(
		`SELECT id, connection_id, folder, name, topic, payload, qos, retain, sort_order,
                created_by, created_at, updated_at
         FROM saved_messages WHERE id = ?`, id)
	return scanSaved(row)
}

// DeleteSavedMessage removes one.
func (s *Store) DeleteSavedMessage(id string) error {
	res, err := s.db.Exec(`DELETE FROM saved_messages WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete saved message: %w", err)
	}
	return affected(res)
}

func scanSaved(row rowScanner) (SavedMessage, error) {
	var (
		msg       SavedMessage
		connID    sql.NullString
		createdBy sql.NullString
		qos       int
		retain    int
		createdAt string
		updatedAt string
	)
	err := row.Scan(&msg.ID, &connID, &msg.Folder, &msg.Name, &msg.Topic, &msg.Payload,
		&qos, &retain, &msg.SortOrder, &createdBy, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SavedMessage{}, ErrNotFound
	}
	if err != nil {
		return SavedMessage{}, fmt.Errorf("store: scan saved message: %w", err)
	}
	msg.ConnectionID = connID.String
	msg.CreatedBy = createdBy.String
	msg.QoS = byte(qos)
	msg.Retain = retain != 0
	msg.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	msg.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return msg, nil
}

// escapeLike neutralises the wildcards in a user's search text, so searching
// for "50%" finds "50%" rather than everything beginning with "50".
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
