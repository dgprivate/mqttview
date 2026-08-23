package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// ConnectionRecord is a persisted broker connection plus its audit fields.
type ConnectionRecord struct {
	Spec      mqttc.ConnectionSpec `json:"spec"`
	CreatedBy string               `json:"createdBy,omitempty"`
	CreatedAt time.Time            `json:"createdAt"`
	UpdatedAt time.Time            `json:"updatedAt"`
}

const connectionColumns = `id, name, url, version, client_id, username, password_enc,
    keep_alive, clean_start, session_expiry, connect_timeout, tls_json, will_json,
    subscriptions_json, auto_connect, history_size, created_by, created_at, updated_at`

// SaveConnection inserts or replaces a connection definition. The broker
// password and the TLS settings are encrypted before they are written.
func (s *Store) SaveConnection(rec ConnectionRecord) error {
	spec := rec.Spec
	if err := spec.Normalize(); err != nil {
		return err
	}
	if spec.ID == "" {
		return errors.New("store: connection id is required")
	}

	passwordEnc, err := s.box.Seal(spec.Password)
	if err != nil {
		return fmt.Errorf("store: encrypt broker password: %w", err)
	}
	// The whole TLS block, not just the key inside it: a client private key is
	// a credential exactly like the password beside it, and four files in this
	// repository said it was encrypted while it was going in as plain JSON.
	// Sealing the block rather than the field keeps it that way whatever gets
	// added to TLSSpec later.
	tlsPlain, err := json.Marshal(spec.TLS)
	if err != nil {
		return fmt.Errorf("store: encode tls settings: %w", err)
	}
	tlsJSON, err := s.box.Seal(string(tlsPlain))
	if err != nil {
		return fmt.Errorf("store: encrypt tls settings: %w", err)
	}
	subsJSON, err := json.Marshal(spec.Subscriptions)
	if err != nil {
		return fmt.Errorf("store: encode subscriptions: %w", err)
	}
	var willJSON any
	if spec.Will != nil {
		raw, err := json.Marshal(spec.Will)
		if err != nil {
			return fmt.Errorf("store: encode will: %w", err)
		}
		willJSON = string(raw)
	}

	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}

	_, err = s.db.Exec(
		`INSERT INTO connections (`+connectionColumns+`)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET
            name = excluded.name,
            url = excluded.url,
            version = excluded.version,
            client_id = excluded.client_id,
            username = excluded.username,
            password_enc = excluded.password_enc,
            keep_alive = excluded.keep_alive,
            clean_start = excluded.clean_start,
            session_expiry = excluded.session_expiry,
            connect_timeout = excluded.connect_timeout,
            tls_json = excluded.tls_json,
            will_json = excluded.will_json,
            subscriptions_json = excluded.subscriptions_json,
            auto_connect = excluded.auto_connect,
            history_size = excluded.history_size,
            updated_at = excluded.updated_at`,
		spec.ID, spec.Name, spec.URL, int(spec.Version), spec.ClientID, spec.Username, passwordEnc,
		spec.KeepAlive, boolToInt(spec.CleanStart), spec.SessionExpiry, spec.ConnectTimeout,
		tlsJSON, willJSON, string(subsJSON), boolToInt(spec.AutoConnect), spec.HistorySize,
		nullIfEmpty(rec.CreatedBy), rec.CreatedAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: save connection: %w", err)
	}
	return nil
}

// GetConnection loads one connection, decrypting its password.
func (s *Store) GetConnection(id string) (ConnectionRecord, error) {
	return s.scanConnection(s.db.QueryRow(`SELECT `+connectionColumns+` FROM connections WHERE id = ?`, id))
}

// ListConnections loads every connection ordered by name.
func (s *Store) ListConnections() ([]ConnectionRecord, error) {
	rows, err := s.db.Query(`SELECT ` + connectionColumns + ` FROM connections ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list connections: %w", err)
	}
	defer rows.Close()

	var out []ConnectionRecord
	for rows.Next() {
		rec, err := s.scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteConnection removes a connection definition.
func (s *Store) DeleteConnection(id string) error {
	res, err := s.db.Exec(`DELETE FROM connections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete connection: %w", err)
	}
	return affected(res)
}

func (s *Store) scanConnection(row rowScanner) (ConnectionRecord, error) {
	var (
		rec        ConnectionRecord
		spec       mqttc.ConnectionSpec
		version    int
		cleanStart int
		autoConn   int
		passwordEn string
		tlsJSON    string
		willJSON   sql.NullString
		subsJSON   string
		createdBy  sql.NullString
		createdAt  string
		updatedAt  string
	)
	err := row.Scan(&spec.ID, &spec.Name, &spec.URL, &version, &spec.ClientID, &spec.Username,
		&passwordEn, &spec.KeepAlive, &cleanStart, &spec.SessionExpiry, &spec.ConnectTimeout,
		&tlsJSON, &willJSON, &subsJSON, &autoConn, &spec.HistorySize,
		&createdBy, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionRecord{}, ErrNotFound
	}
	if err != nil {
		return ConnectionRecord{}, fmt.Errorf("store: scan connection: %w", err)
	}

	spec.Version = mqttc.Version(version)
	spec.CleanStart = cleanStart != 0
	spec.AutoConnect = autoConn != 0

	if spec.Password, err = s.box.Open(passwordEn); err != nil {
		// A key rotation should not make every connection unreadable; report
		// the connection with an empty password so the user can re-enter it.
		spec.Password = ""
	}
	if err := decodeTLS(tlsJSON, s, &spec); err != nil {
		return ConnectionRecord{}, fmt.Errorf("store: decode tls settings for %s: %w", spec.ID, err)
	}
	if err := json.Unmarshal([]byte(subsJSON), &spec.Subscriptions); err != nil {
		return ConnectionRecord{}, fmt.Errorf("store: decode subscriptions for %s: %w", spec.ID, err)
	}
	if willJSON.Valid && willJSON.String != "" {
		var w mqttc.Will
		if err := json.Unmarshal([]byte(willJSON.String), &w); err != nil {
			return ConnectionRecord{}, fmt.Errorf("store: decode will for %s: %w", spec.ID, err)
		}
		spec.Will = &w
	}

	rec.Spec = spec
	rec.CreatedBy = createdBy.String
	rec.CreatedAt = parseTime(createdAt)
	rec.UpdatedAt = parseTime(updatedAt)
	return rec, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// decodeTLS reads the TLS column, which holds sealed JSON.
//
// A row written before the key was encrypted holds plain JSON instead, and is
// read as such rather than failing: an upgrade must not make every existing
// connection unreadable. It is re-sealed the next time the connection is
// saved. The fallback is safe in the sense that matters — anyone who can put
// plaintext in that column can already read the key that would decrypt it.
func decodeTLS(column string, s *Store, spec *mqttc.ConnectionSpec) error {
	if column == "" {
		return nil
	}
	if plain, err := s.box.Open(column); err == nil {
		return json.Unmarshal([]byte(plain), &spec.TLS)
	}
	return json.Unmarshal([]byte(column), &spec.TLS)
}
