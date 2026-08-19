package hass

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Home Assistant templates are Jinja2, which mqttview deliberately does not
// implement. What it does implement is the handful of shapes that cover the
// overwhelming majority of real discovery payloads:
//
//	{{ value }}
//	{{ value_json }}
//	{{ value_json.temperature }}
//	{{ value_json['sensor']['temp'] }}
//	{{ value_json.readings[0].value }}
//	{{ value_json.temp | round(1) }}
//
// Anything else is reported as unsupported and the raw payload is shown
// instead, so the UI never displays a value mqttview only guessed at.

// templateResult is the outcome of rendering a value_template.
type templateResult struct {
	// Value is the extracted value, or the raw payload string when no
	// template applied.
	Value any `json:"value"`
	// Supported is false when the template used syntax mqttview cannot
	// evaluate; Value then holds the raw payload.
	Supported bool `json:"supported"`
}

var (
	// templateExpr matches a template consisting of exactly one {{ ... }}
	// expression with nothing else around it.
	templateExpr = regexp.MustCompile(`^\s*\{\{\s*(.+?)\s*\}\}\s*$`)
	// pathSegment matches .name, ['name'], ["name"] or [0].
	pathSegment = regexp.MustCompile(`^(?:\.([A-Za-z_][A-Za-z0-9_]*)|\[\s*'([^']*)'\s*\]|\[\s*"([^"]*)"\s*\]|\[\s*(\d+)\s*\])`)
	// filterCall matches a trailing Jinja filter such as `round(1)`.
	filterCall = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(\s*([^)]*)\s*\))?$`)
)

// renderTemplate evaluates a value_template against a payload.
func renderTemplate(tpl string, payload []byte) templateResult {
	raw := string(payload)
	if strings.TrimSpace(tpl) == "" {
		return templateResult{Value: raw, Supported: true}
	}

	m := templateExpr.FindStringSubmatch(tpl)
	if m == nil {
		return templateResult{Value: raw, Supported: false}
	}

	expr := m[1]
	parts := splitFilters(expr)
	base := strings.TrimSpace(parts[0])
	filters := parts[1:]

	value, ok := evalBase(base, payload, raw)
	if !ok {
		return templateResult{Value: raw, Supported: false}
	}

	for _, f := range filters {
		var applied bool
		value, applied = applyFilter(value, strings.TrimSpace(f))
		if !applied {
			return templateResult{Value: raw, Supported: false}
		}
	}
	return templateResult{Value: value, Supported: true}
}

// evalBase resolves the part of the expression before any filters.
func evalBase(expr string, payload []byte, raw string) (any, bool) {
	switch {
	case expr == "value":
		return raw, true

	case expr == "value_json" || strings.HasPrefix(expr, "value_json.") || strings.HasPrefix(expr, "value_json["):
		var doc any
		if err := json.Unmarshal(payload, &doc); err != nil {
			// A template that reads value_json against a non-JSON payload is
			// a device bug, not something to paper over.
			return nil, false
		}
		return walkPath(doc, strings.TrimPrefix(expr, "value_json"))

	default:
		return nil, false
	}
}

// walkPath follows a chain of .key, ['key'] and [index] accessors.
func walkPath(doc any, path string) (any, bool) {
	cur := doc
	for path != "" {
		m := pathSegment.FindStringSubmatch(path)
		if m == nil {
			return nil, false
		}
		path = path[len(m[0]):]

		switch {
		case m[1] != "" || m[2] != "" || m[3] != "":
			key := m[1] + m[2] + m[3]
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, true // valid template, missing key: report as null
			}
			v, ok := obj[key]
			if !ok {
				return nil, true
			}
			cur = v

		default:
			idx, err := strconv.Atoi(m[4])
			if err != nil {
				return nil, false
			}
			arr, ok := cur.([]any)
			if !ok || idx < 0 || idx >= len(arr) {
				return nil, true
			}
			cur = arr[idx]
		}
	}
	return cur, true
}

// splitFilters splits on '|' while leaving pipes inside quotes alone.
func splitFilters(expr string) []string {
	var out []string
	var buf strings.Builder
	var quote rune

	for _, r := range expr {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			buf.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			buf.WriteRune(r)
		case r == '|':
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	out = append(out, buf.String())
	return out
}

// applyFilter implements the numeric filters that appear in real payloads.
// The bool reports whether the filter was understood.
func applyFilter(value any, filter string) (any, bool) {
	m := filterCall.FindStringSubmatch(filter)
	if m == nil {
		return value, false
	}
	name, arg := m[1], strings.TrimSpace(m[2])

	switch name {
	case "round":
		f, ok := toFloat(value)
		if !ok {
			return value, false
		}
		digits := 0
		if arg != "" {
			n, err := strconv.Atoi(arg)
			if err != nil {
				return value, false
			}
			digits = n
		}
		scale := math.Pow(10, float64(digits))
		return math.Round(f*scale) / scale, true

	case "float":
		f, ok := toFloat(value)
		if !ok {
			return value, false
		}
		return f, true

	case "int":
		f, ok := toFloat(value)
		if !ok {
			return value, false
		}
		return int64(f), true

	case "string":
		return toString(value), true

	case "abs":
		f, ok := toFloat(value)
		if !ok {
			return value, false
		}
		return math.Abs(f), true

	case "upper":
		return strings.ToUpper(toString(value)), true

	case "lower":
		return strings.ToLower(toString(value)), true

	case "trim":
		return strings.TrimSpace(toString(value)), true

	default:
		return value, false
	}
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}
