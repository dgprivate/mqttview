package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]string{"a": "b"})

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got["a"] != "b" {
		t.Errorf("body = %v", got)
	}
}

func TestWriteJSONWithNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusNoContent, nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected an empty body, got %q", rec.Body.String())
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusForbidden, "not allowed")

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden || body.Error != "not allowed" {
		t.Errorf("got %d %+v", rec.Code, body)
	}
}

func TestWriteErrorf(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrorf(rec, http.StatusBadRequest, "address %d is out of range", 99)

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "address 99 is out of range" {
		t.Errorf("error = %q", body.Error)
	}
}

func decodeInto(t *testing.T, body string, v any) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return DecodeJSON(httptest.NewRecorder(), req, v)
}

func TestDecodeJSON(t *testing.T) {
	var out struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	if err := decodeInto(t, `{"name":"x","n":3}`, &out); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
	if out.Name != "x" || out.N != 3 {
		t.Errorf("decoded %+v", out)
	}
}

// The messages matter: a client author reads them, so each one names what is
// actually wrong rather than repeating "bad request".
func TestDecodeJSONReportsWhatIsWrong(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"truncated", `{"name":`, "ends in the middle of a value"},
		{"malformed", `{"name" "x"}`, "malformed JSON"},
		{"wrong type", `{"n":"not a number"}`, `field "n" has the wrong type`},
		{"empty", ``, "request body is empty"},
		{"two objects", `{"name":"a"}{"name":"b"}`, "single JSON object"},
		// DisallowUnknownFields is what turns a client-side typo into an error
		// instead of a silently ignored field.
		{"unknown field", `{"nmae":"typo"}`, "unknown field"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out struct {
				Name string `json:"name"`
				N    int    `json:"n"`
			}
			err := decodeInto(t, tt.body, &out)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestDecodeJSONBoundsTheBody(t *testing.T) {
	// A body past the limit must be refused rather than buffered.
	huge := `{"name":"` + strings.Repeat("x", (4<<20)+1024) + `"}`

	var out struct {
		Name string `json:"name"`
	}
	if err := decodeInto(t, huge, &out); err == nil {
		t.Fatal("an oversized body was accepted")
	}
}
