// Package httpx holds the small HTTP helpers shared by the API layer, the
// auth middleware and plugins, so every endpoint reports errors identically.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrorBody is the JSON shape of every error response.
type ErrorBody struct {
	Error string `json:"error"`
	// Details carries field-level validation messages when relevant.
	Details map[string]string `json:"details,omitempty"`
}

// WriteJSON writes v as JSON with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so there is nothing to do but note
		// it; the client will see a truncated body.
		_ = err
	}
}

// WriteError writes a JSON error body.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorBody{Error: msg})
}

// WriteErrorf is WriteError with formatting.
func WriteErrorf(w http.ResponseWriter, status int, format string, args ...any) {
	WriteError(w, status, fmt.Sprintf(format, args...))
}

// maxBodyBytes bounds request bodies so a malformed or hostile client cannot
// force the server to buffer arbitrary amounts. TLS material pushes the limit
// higher than a typical JSON API needs.
const maxBodyBytes = 4 << 20 // 4 MiB

// DecodeJSON reads a JSON request body into v, rejecting unknown fields so
// that typos in a client payload surface as errors rather than silent no-ops.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntaxErr):
			return fmt.Errorf("malformed JSON at byte %d", syntaxErr.Offset)
		case errors.As(err, &typeErr):
			return fmt.Errorf("field %q has the wrong type", typeErr.Field)
		case errors.Is(err, io.ErrUnexpectedEOF):
			// A truncated body: the encoder gives "unexpected EOF", which tells
			// a client author nothing about what to look for.
			return errors.New("malformed JSON: the body ends in the middle of a value")
		case errors.Is(err, io.EOF):
			return errors.New("request body is empty")
		default:
			return err
		}
	}
	// A second value in the body usually means the client sent NDJSON or
	// concatenated objects by mistake.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}
