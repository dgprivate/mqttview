package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PluginSettings is a plugin's persisted enablement and configuration. The
// settings blob is opaque to the store; each plugin decodes its own shape.
type PluginSettings struct {
	PluginID  string         `json:"pluginId"`
	Enabled   bool           `json:"enabled"`
	Settings  map[string]any `json:"settings"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

// GetPluginSettings loads one plugin's settings.
func (s *Store) GetPluginSettings(pluginID string) (PluginSettings, error) {
	var (
		ps        PluginSettings
		enabled   int
		raw       string
		updatedAt string
	)
	err := s.db.QueryRow(
		`SELECT plugin_id, enabled, settings_json, updated_at FROM plugin_settings WHERE plugin_id = ?`,
		pluginID).Scan(&ps.PluginID, &enabled, &raw, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PluginSettings{}, ErrNotFound
	}
	if err != nil {
		return PluginSettings{}, fmt.Errorf("store: get plugin settings: %w", err)
	}

	ps.Enabled = enabled != 0
	ps.UpdatedAt = parseTime(updatedAt)
	if err := json.Unmarshal([]byte(raw), &ps.Settings); err != nil {
		return PluginSettings{}, fmt.Errorf("store: decode settings for plugin %s: %w", pluginID, err)
	}
	return ps, nil
}

// ListPluginSettings loads settings for every plugin that has any.
func (s *Store) ListPluginSettings() ([]PluginSettings, error) {
	rows, err := s.db.Query(`SELECT plugin_id, enabled, settings_json, updated_at FROM plugin_settings`)
	if err != nil {
		return nil, fmt.Errorf("store: list plugin settings: %w", err)
	}
	defer rows.Close()

	var out []PluginSettings
	for rows.Next() {
		var (
			ps        PluginSettings
			enabled   int
			raw       string
			updatedAt string
		)
		if err := rows.Scan(&ps.PluginID, &enabled, &raw, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan plugin settings: %w", err)
		}
		ps.Enabled = enabled != 0
		ps.UpdatedAt = parseTime(updatedAt)
		if err := json.Unmarshal([]byte(raw), &ps.Settings); err != nil {
			return nil, fmt.Errorf("store: decode settings for plugin %s: %w", ps.PluginID, err)
		}
		out = append(out, ps)
	}
	return out, rows.Err()
}

// SavePluginSettings upserts a plugin's settings.
func (s *Store) SavePluginSettings(ps PluginSettings) error {
	if ps.Settings == nil {
		ps.Settings = map[string]any{}
	}
	raw, err := json.Marshal(ps.Settings)
	if err != nil {
		return fmt.Errorf("store: encode plugin settings: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO plugin_settings (plugin_id, enabled, settings_json, updated_at)
         VALUES (?, ?, ?, ?)
         ON CONFLICT(plugin_id) DO UPDATE SET
            enabled = excluded.enabled,
            settings_json = excluded.settings_json,
            updated_at = excluded.updated_at`,
		ps.PluginID, boolToInt(ps.Enabled), string(raw), nowString())
	if err != nil {
		return fmt.Errorf("store: save plugin settings: %w", err)
	}
	return nil
}

// PluginGet reads a value from a plugin's namespaced key-value store.
func (s *Store) PluginGet(pluginID, key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM plugin_state WHERE plugin_id = ? AND key = ?`,
		pluginID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// PluginSet writes a value to a plugin's namespaced key-value store.
func (s *Store) PluginSet(pluginID, key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO plugin_state (plugin_id, key, value, updated_at)
         VALUES (?, ?, ?, ?)
         ON CONFLICT(plugin_id, key) DO UPDATE SET
            value = excluded.value,
            updated_at = excluded.updated_at`,
		pluginID, key, value, nowString())
	return err
}

// PluginDelete removes a key.
func (s *Store) PluginDelete(pluginID, key string) error {
	_, err := s.db.Exec(`DELETE FROM plugin_state WHERE plugin_id = ? AND key = ?`, pluginID, key)
	return err
}

// PluginList returns every key/value a plugin has stored, keyed by key.
func (s *Store) PluginList(pluginID string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM plugin_state WHERE plugin_id = ?`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("store: list plugin state: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: scan plugin state: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
