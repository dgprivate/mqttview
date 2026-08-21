package plc

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Mapping is a human description a person attached to a point, as opposed to
// the one the PLC publishes about itself.
//
// The PLC describes only the forty points somebody bothered to configure in
// Node-RED; the other three hundred are addresses. Naming one here is how a
// point found by pressing its button stops being "DI-31-5" and becomes the
// kitchen light switch, without touching the PLC to do it.
type Mapping struct {
	// Name is the PLC's own name for the point, e.g. "DI-31-5". It is the key,
	// because addresses can be renumbered but the name travels with the point.
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Location string `json:"location,omitempty"`
	// Type mirrors the PLC's sensor type field: motion, door, button and so on.
	Type string `json:"type,omitempty"`
	// Notes is free text — what a signal should do, for the person who will
	// write the PLC logic later.
	Notes string `json:"notes,omitempty"`
}

// empty reports whether a mapping carries nothing worth storing.
func (m Mapping) empty() bool {
	return m.Label == "" && m.Location == "" && m.Type == "" && m.Notes == ""
}

// mappingKeyPrefix namespaces mappings inside the plugin's KV store.
const mappingKeyPrefix = "map:"

// mappingKey builds the KV key for one point. The connection is part of the
// key because the same PLC name may exist on two brokers.
func mappingKey(connID, name string) string {
	return mappingKeyPrefix + connID + "|" + name
}

// parseMappingKey splits a KV key back into its parts.
func parseMappingKey(key string) (connID, name string, ok bool) {
	if !strings.HasPrefix(key, mappingKeyPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, mappingKeyPrefix)
	// The PLC name cannot contain "|", so splitting on the first one is safe.
	i := strings.Index(rest, "|")
	if i < 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// SetMapping stores or clears a human description for a point. An empty
// mapping removes it, so clearing every field in the UI is a delete.
func (r *Registry) SetMapping(connID, name string, m Mapping) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	m.Name = name

	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.state(connID)
	if m.empty() {
		delete(c.mappings, name)
		return
	}
	c.mappings[name] = m
}

// Mappings returns every stored description for a connection, or for all of
// them when connID is empty.
func (r *Registry) Mappings(connID string) []Mapping {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := []Mapping{}
	for id, c := range r.conns {
		if connID != "" && id != connID {
			continue
		}
		for _, m := range c.mappings {
			out = append(out, m)
		}
	}
	return out
}

// LoadMappings restores descriptions saved in a previous run.
func (r *Registry) LoadMappings(stored map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var firstErr error
	for key, raw := range stored {
		connID, name, ok := parseMappingKey(key)
		if !ok {
			continue
		}
		var m Mapping
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("plc: stored mapping %q is unreadable: %w", key, err)
			}
			continue
		}
		m.Name = name
		r.state(connID).mappings[name] = m
	}
	return firstErr
}

// describePoint fills in a point's human fields, preferring what a person
// entered over what the PLC published about itself. A person who renames a
// point has looked at it more recently than whoever configured Node-RED.
func describePoint(c *connState, name string) (label, location, sensorType string, alarmZone, known bool) {
	if m, ok := c.meta[name]; ok {
		label, location, sensorType, alarmZone, known = m.Name, m.Location, m.Type, m.AlarmZone, true
	}
	if m, ok := c.mappings[name]; ok {
		known = true
		if m.Label != "" {
			label = m.Label
		}
		if m.Location != "" {
			location = m.Location
		}
		if m.Type != "" {
			sensorType = m.Type
		}
	}
	return label, location, sensorType, alarmZone, known
}
