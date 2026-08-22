package hass

import (
	"encoding/json"
	"fmt"
	"strings"
)

// topicBase is the key whose value replaces a leading or trailing "~" in every
// topic field of a discovery payload.
const topicBase = "~"

// discoveryTopic is a parsed `<prefix>/<component>/[<node>/]<object>/config`
// topic.
type discoveryTopic struct {
	Prefix    string
	Component string
	NodeID    string
	ObjectID  string
	// DeviceScoped is true for the 2024.10+ format where one message under
	// `<prefix>/device/<object>/config` describes an entire device.
	DeviceScoped bool
}

// parseDiscoveryTopic splits a discovery topic. It returns false for topics
// that are not discovery configs, which is most of what arrives under the
// discovery prefix once entities start publishing state.
func parseDiscoveryTopic(prefix, topic string) (discoveryTopic, bool) {
	if !strings.HasPrefix(topic, prefix+"/") {
		return discoveryTopic{}, false
	}
	rest := strings.TrimPrefix(topic, prefix+"/")
	parts := strings.Split(rest, "/")

	// <component>/<object_id>/config or <component>/<node_id>/<object_id>/config
	if len(parts) < 3 || len(parts) > 4 || parts[len(parts)-1] != "config" {
		return discoveryTopic{}, false
	}

	// An empty segment is rejected rather than accepted and normalised away.
	// topicOf() omits an empty node ID, so "<prefix>/x//y/config" would rebuild
	// as "<prefix>/x/y/config" — and since that string is the entity's identity
	// key, two different discovery topics would land on one entry, each
	// overwriting and deleting the other. Home Assistant does not allow empty
	// IDs either.
	for _, p := range parts[:len(parts)-1] {
		if p == "" {
			return discoveryTopic{}, false
		}
	}

	dt := discoveryTopic{Prefix: prefix, Component: parts[0]}
	if len(parts) == 4 {
		dt.NodeID = parts[1]
		dt.ObjectID = parts[2]
	} else {
		dt.ObjectID = parts[1]
	}
	dt.DeviceScoped = dt.Component == "device"
	return dt, true
}

// entityConfig is a discovery payload with abbreviations expanded and "~"
// resolved. The raw map is kept so the UI can show every key the device sent,
// including ones mqttview does not act on.
type entityConfig map[string]any

// parseConfig decodes a discovery payload and normalises it.
func parseConfig(payload []byte) (entityConfig, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("hass: discovery payload is not valid JSON: %w", err)
	}
	cfg := expandEntity(raw)
	expandTopicBase(cfg)
	return cfg, nil
}

// expandEntity rewrites abbreviated keys and recurses into the nested device,
// origin and availability blocks.
func expandEntity(raw map[string]any) entityConfig {
	out := make(entityConfig, len(raw))
	for k, v := range raw {
		key := k
		if full, ok := entityAbbreviations[k]; ok {
			key = full
		} else if full, ok := componentAbbreviations[k]; ok {
			key = full
		}

		switch key {
		case "device":
			if m, ok := v.(map[string]any); ok {
				v = expandWith(m, deviceAbbreviations)
			}
		case "origin":
			if m, ok := v.(map[string]any); ok {
				v = expandWith(m, originAbbreviations)
			}
		case "availability":
			if list, ok := v.([]any); ok {
				expanded := make([]any, 0, len(list))
				for _, item := range list {
					if m, ok := item.(map[string]any); ok {
						expanded = append(expanded, map[string]any(expandEntity(m)))
					} else {
						expanded = append(expanded, item)
					}
				}
				v = expanded
			}
		case "components":
			// Device-based discovery: each value is itself an entity config.
			if m, ok := v.(map[string]any); ok {
				comps := make(map[string]any, len(m))
				for id, sub := range m {
					if subMap, ok := sub.(map[string]any); ok {
						comps[id] = map[string]any(expandEntity(subMap))
					} else {
						comps[id] = sub
					}
				}
				v = comps
			}
		}
		out[key] = v
	}
	return out
}

func expandWith(raw map[string]any, table map[string]string) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		if full, ok := table[k]; ok {
			k = full
		}
		out[k] = v
	}
	return out
}

// expandTopicBase resolves the "~" shorthand. Home Assistant only applies it
// to keys ending in "topic", and only at the very start or end of the value.
func expandTopicBase(cfg entityConfig) {
	base, ok := cfg[topicBase].(string)
	if !ok || base == "" {
		return
	}
	delete(cfg, topicBase)

	expand := func(m map[string]any) {
		for k, v := range m {
			s, ok := v.(string)
			if !ok || s == "" || !strings.HasSuffix(k, "topic") {
				continue
			}
			if strings.HasPrefix(s, topicBase) {
				s = base + s[1:]
			}
			if strings.HasSuffix(s, topicBase) {
				s = s[:len(s)-1] + base
			}
			m[k] = s
		}
	}

	expand(cfg)
	if list, ok := cfg["availability"].([]any); ok {
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				expand(m)
			}
		}
	}
	// Device-based payloads share one "~" across all their components.
	if comps, ok := cfg["components"].(map[string]any); ok {
		for _, sub := range comps {
			if m, ok := sub.(map[string]any); ok {
				expand(m)
				if _, has := m[topicBase]; !has {
					m[topicBase] = base
					expandTopicBase(entityConfig(m))
				}
			}
		}
	}
}

// Typed accessors. Discovery payloads are loosely typed in practice — numbers
// arrive as strings, booleans as "true" — so each accessor is forgiving.

func (c entityConfig) str(key string) string {
	switch v := c[key].(type) {
	case string:
		return v
	case float64:
		return trimFloat(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func (c entityConfig) strDefault(key, def string) string {
	if s := c.str(key); s != "" {
		return s
	}
	return def
}

func (c entityConfig) boolean(key string, def bool) bool {
	switch v := c[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(v) {
		case "true", "yes", "1", "on":
			return true
		case "false", "no", "0", "off":
			return false
		}
	case float64:
		return v != 0
	}
	return def
}

func (c entityConfig) number(key string) (float64, bool) {
	switch v := c[key].(type) {
	case float64:
		return v, true
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func (c entityConfig) strings(key string) []string {
	switch v := c[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{v}
	}
	return nil
}

func (c entityConfig) submap(key string) map[string]any {
	m, _ := c[key].(map[string]any)
	return m
}

// availabilityTopics returns every topic that reports whether the entity is
// online: the single availability_topic plus any in the availability list.
func (c entityConfig) availabilityTopics() []availabilitySpec {
	var out []availabilitySpec

	if t := c.str("availability_topic"); t != "" {
		out = append(out, availabilitySpec{
			Topic:    t,
			Template: c.str("availability_template"),
			Online:   c.strDefault("payload_available", "online"),
			Offline:  c.strDefault("payload_not_available", "offline"),
		})
	}
	if list, ok := c["availability"].([]any); ok {
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			sub := entityConfig(m)
			t := sub.str("topic")
			if t == "" {
				continue
			}
			out = append(out, availabilitySpec{
				Topic:    t,
				Template: sub.str("value_template"),
				Online:   sub.strDefault("payload_available", "online"),
				Offline:  sub.strDefault("payload_not_available", "offline"),
			})
		}
	}
	return out
}

// availabilitySpec is one online/offline source for an entity.
type availabilitySpec struct {
	Topic    string `json:"topic"`
	Template string `json:"template,omitempty"`
	Online   string `json:"online"`
	Offline  string `json:"offline"`
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%g", f)
	return s
}
