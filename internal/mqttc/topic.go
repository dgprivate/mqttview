package mqttc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxTopicBytes = 65535

// ValidateTopic checks a concrete topic name (used for publishing). Wildcards
// are not allowed here.
func ValidateTopic(topic string) error {
	if topic == "" {
		return errors.New("topic must not be empty")
	}
	if len(topic) > maxTopicBytes {
		return fmt.Errorf("topic must be at most %d bytes", maxTopicBytes)
	}
	if !utf8.ValidString(topic) {
		return errors.New("topic must be valid UTF-8")
	}
	if strings.ContainsAny(topic, "+#") {
		return errors.New("topic must not contain wildcards (+ or #)")
	}
	if strings.ContainsRune(topic, 0) {
		return errors.New("topic must not contain a null character")
	}
	return nil
}

// ValidateFilter checks a subscription filter, where wildcards are allowed but
// only in the positions MQTT permits.
func ValidateFilter(filter string) error {
	if filter == "" {
		return errors.New("topic filter must not be empty")
	}
	if len(filter) > maxTopicBytes {
		return fmt.Errorf("topic filter must be at most %d bytes", maxTopicBytes)
	}
	if !utf8.ValidString(filter) {
		return errors.New("topic filter must be valid UTF-8")
	}
	if strings.ContainsRune(filter, 0) {
		return errors.New("topic filter must not contain a null character")
	}
	levels := strings.Split(filter, "/")
	for i, level := range levels {
		switch {
		case level == "#":
			if i != len(levels)-1 {
				return errors.New("the # wildcard must be the last level of a filter")
			}
		case level == "+":
			// Fine anywhere.
		case strings.ContainsAny(level, "+#"):
			return errors.New("wildcards must occupy an entire level, e.g. a/+/b not a/x+/b")
		}
	}
	return nil
}

// MatchFilter reports whether a concrete topic matches an MQTT topic filter.
func MatchFilter(filter, topic string) bool {
	if filter == topic {
		return true
	}
	fl := strings.Split(filter, "/")
	tl := strings.Split(topic, "/")

	// A leading wildcard never matches the $SYS-style reserved namespace.
	if len(tl) > 0 && strings.HasPrefix(tl[0], "$") && len(fl) > 0 && (fl[0] == "#" || fl[0] == "+") {
		return false
	}

	for i, f := range fl {
		if f == "#" {
			// '#' matches the parent level too, but only if it is the final
			// level of the filter, which ValidateFilter guarantees.
			return true
		}
		if i >= len(tl) {
			return false
		}
		if f != "+" && f != tl[i] {
			return false
		}
	}
	return len(fl) == len(tl)
}

func shortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is fatal in practice; a fixed suffix at least
		// keeps the client ID well-formed so the error surfaces as a broker
		// rejection rather than a panic in a UI handler.
		return "00000000"
	}
	return hex.EncodeToString(b)
}
