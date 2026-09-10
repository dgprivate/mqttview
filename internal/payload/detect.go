package payload

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// Kind is what a payload appears to be.
type Kind string

const (
	KindEmpty     Kind = "empty"
	KindJSON      Kind = "json"
	KindText      Kind = "text"
	KindImage     Kind = "image"
	KindSparkplug Kind = "sparkplug"
	KindBinary    Kind = "binary"
)

// Detected describes a payload well enough for the UI to choose how to show it.
type Detected struct {
	Kind Kind `json:"kind"`
	// MediaType is set only for images, and only for the handful below.
	MediaType string `json:"mediaType,omitempty"`
	Size      int    `json:"size"`
}

// imageSignature is one magic-number test.
type imageSignature struct {
	mediaType string
	prefix    []byte
	// at12 is the second marker some containers carry after a length field.
	offset12 []byte
}

// The image formats mqttview will render, and nothing else.
//
// This list is an allow-list rather than a starting point, and SVG is
// deliberately absent. An SVG is a document that can carry script, and these
// bytes came off a broker that anybody on the network may be publishing to;
// serving one back under this origin with a media type that makes a browser
// execute it would turn any writable topic into stored cross-site scripting.
// The same goes for HTML, which is why nothing here ever answers text/html.
var imageSignatures = []imageSignature{
	{mediaType: "image/png", prefix: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}},
	{mediaType: "image/jpeg", prefix: []byte{0xff, 0xd8, 0xff}},
	{mediaType: "image/gif", prefix: []byte("GIF87a")},
	{mediaType: "image/gif", prefix: []byte("GIF89a")},
	{mediaType: "image/bmp", prefix: []byte("BM")},
	// RIFF....WEBP: the four bytes between are the file length.
	{mediaType: "image/webp", prefix: []byte("RIFF"), offset12: []byte("WEBP")},
}

// ImageMediaType returns the media type when the payload is one of the image
// formats mqttview will render, and false otherwise.
func ImageMediaType(raw []byte) (string, bool) {
	for _, sig := range imageSignatures {
		if !bytes.HasPrefix(raw, sig.prefix) {
			continue
		}
		if sig.offset12 != nil {
			if len(raw) < 12+len(sig.offset12) || !bytes.Equal(raw[8:12], sig.offset12) {
				continue
			}
		}
		return sig.mediaType, true
	}
	return "", false
}

// Detect classifies a payload. The topic is consulted because one encoding —
// Sparkplug B — has no magic number of its own and is identified by the
// namespace it is published under.
func Detect(topic string, raw []byte) Detected {
	d := Detected{Size: len(raw)}

	switch {
	case len(raw) == 0:
		d.Kind = KindEmpty
	case LooksLikeSparkplug(topic):
		d.Kind = KindSparkplug
	default:
		if mediaType, ok := ImageMediaType(raw); ok {
			d.Kind, d.MediaType = KindImage, mediaType
			return d
		}
		switch {
		case isJSON(raw):
			d.Kind = KindJSON
		case isText(raw):
			d.Kind = KindText
		default:
			d.Kind = KindBinary
		}
	}
	return d
}

// isJSON reports whether the payload parses as a JSON document.
//
// A bare number is valid JSON and is deliberately not counted as one: almost
// every sensor on a broker publishes "21.5", and calling that a JSON document
// would put a pretty-printer around a single number on half the topics in the
// tree.
func isJSON(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return false
	}
	return json.Valid(trimmed)
}

// isText reports whether a payload is valid UTF-8 without control characters
// that would make a mess of a text view.
func isText(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for _, r := range string(raw) {
		// Tab, newline and carriage return are ordinary in a text payload.
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// SafeMediaType is what an HTTP handler may echo a payload back with.
//
// Anything not on the image allow-list becomes application/octet-stream, so a
// payload can be downloaded and looked at but never interpreted as a document
// by the browser that fetched it.
func SafeMediaType(raw []byte) string {
	if mediaType, ok := ImageMediaType(raw); ok {
		return mediaType
	}
	return "application/octet-stream"
}
