package payload

import (
	"bytes"
	"strings"
	"testing"
)

func png() []byte  { return append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 1, 2, 3) }
func jpeg() []byte { return append([]byte{0xff, 0xd8, 0xff, 0xe0}, 4, 5, 6) }
func webp() []byte {
	out := []byte("RIFF")
	out = append(out, 0x20, 0, 0, 0)
	return append(out, []byte("WEBPVP8 ")...)
}

func TestTheImageFormatsAreRecognisedByTheirMagicNumbers(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"PNG", png(), "image/png"},
		{"JPEG", jpeg(), "image/jpeg"},
		{"GIF87a", []byte("GIF87a...."), "image/gif"},
		{"GIF89a", []byte("GIF89a...."), "image/gif"},
		{"BMP", []byte("BM......"), "image/bmp"},
		{"WebP", webp(), "image/webp"},
	} {
		got, ok := ImageMediaType(c.raw)
		if !ok || got != c.want {
			t.Errorf("%s: media type = %q (%v), want %q", c.name, got, ok, c.want)
		}
	}
}

// RIFF is a container for several things; only the ones marked WEBP are images
// mqttview will render.
func TestARiffContainerThatIsNotWebPIsNotAnImage(t *testing.T) {
	raw := append([]byte("RIFF"), 0x20, 0, 0, 0)
	raw = append(raw, []byte("WAVEfmt ")...)

	if _, ok := ImageMediaType(raw); ok {
		t.Error("a WAV was offered as an image")
	}
}

// These bytes came off a broker anybody on the network may be publishing to.
// Serving one back under this origin with a type the browser executes turns
// any writable topic into stored cross-site scripting.
func TestNothingExecutableIsEverOfferedAsAMediaType(t *testing.T) {
	for _, c := range []struct{ name, raw string }{
		{"SVG", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		{"SVG with a leading declaration", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`},
		{"HTML", `<html><body><script>alert(1)</script></body></html>`},
		{"an HTML fragment", `<img src=x onerror=alert(1)>`},
	} {
		if got := SafeMediaType([]byte(c.raw)); got != "application/octet-stream" {
			t.Errorf("%s would be served as %q", c.name, got)
		}
		if _, ok := ImageMediaType([]byte(c.raw)); ok {
			t.Errorf("%s was classified as a renderable image", c.name)
		}
		if d := Detect("a/b", []byte(c.raw)); d.Kind == KindImage {
			t.Errorf("%s was detected as an image", c.name)
		}
	}
}

func TestAnImageIsServedAsItselfAndEverythingElseAsBytes(t *testing.T) {
	if got := SafeMediaType(png()); got != "image/png" {
		t.Errorf("a PNG would be served as %q", got)
	}
	if got := SafeMediaType([]byte{0x00, 0x01, 0x02}); got != "application/octet-stream" {
		t.Errorf("arbitrary bytes would be served as %q", got)
	}
}

func TestPayloadsAreClassified(t *testing.T) {
	for _, c := range []struct {
		name  string
		topic string
		raw   []byte
		want  Kind
	}{
		{"nothing at all", "a/b", nil, KindEmpty},
		{"an object", "a/b", []byte(`{"t":21.5}`), KindJSON},
		{"an array", "a/b", []byte(`[1,2,3]`), KindJSON},
		{"a reading", "a/b", []byte("21.5"), KindText},
		{"a word", "a/b", []byte("ON"), KindText},
		{"a PNG", "a/b", png(), KindImage},
		{"arbitrary bytes", "a/b", []byte{0x00, 0xff, 0xfe}, KindBinary},
		{"Sparkplug, by its topic", "spBv1.0/g/NDATA/e", []byte{0x08, 0x01}, KindSparkplug},
	} {
		if got := Detect(c.topic, c.raw); got.Kind != c.want {
			t.Errorf("%s: kind = %q, want %q", c.name, got.Kind, c.want)
		}
	}
}

// Almost every sensor on a broker publishes a bare number. Calling that a JSON
// document puts a pretty-printer around a single value on half the tree.
func TestABareNumberIsTextRatherThanAJSONDocument(t *testing.T) {
	for _, raw := range []string{"21.5", "0", "-3", "true", `"a string"`} {
		if got := Detect("a/b", []byte(raw)); got.Kind == KindJSON {
			t.Errorf("%q was classified as a JSON document", raw)
		}
	}
}

func TestBrokenJSONIsNotOfferedAsJSON(t *testing.T) {
	if got := Detect("a/b", []byte(`{"t":`)); got.Kind == KindJSON {
		t.Error("a truncated object was classified as JSON, so a viewer would try to pretty-print it")
	}
}

func TestTextWithControlCharactersIsTreatedAsBinary(t *testing.T) {
	if got := Detect("a/b", []byte("hello\x00world")); got.Kind != KindBinary {
		t.Errorf("kind = %q, want binary: a NUL byte is not text", got.Kind)
	}
	if got := Detect("a/b", []byte("line one\nline two\ttabbed\r\n")); got.Kind != KindText {
		t.Errorf("kind = %q, want text: tabs and newlines are ordinary", got.Kind)
	}
}

func TestSizeIsAlwaysReported(t *testing.T) {
	raw := bytes.Repeat([]byte("x"), 300)
	if got := Detect("a/b", raw); got.Size != 300 {
		t.Errorf("size = %d, want 300", got.Size)
	}
}

func TestUnicodeTextIsText(t *testing.T) {
	if got := Detect("a/b", []byte("dvorišče 21,5 °C")); got.Kind != KindText {
		t.Errorf("kind = %q, want text", got.Kind)
	}
	if got := Detect("a/b", []byte{0xff, 0xfe, 0xfd}); got.Kind != KindBinary {
		t.Errorf("kind = %q, want binary: those bytes are not valid UTF-8", got.Kind)
	}
}

func FuzzDetect(f *testing.F) {
	f.Add("a/b", []byte(`{"a":1}`))
	f.Add("spBv1.0/g/NDATA/e", []byte{0x08, 0x01})
	f.Add("a/b", png())
	f.Add("a/b", []byte("RIFF"))
	f.Add("a/b", []byte(""))

	f.Fuzz(func(t *testing.T, topic string, raw []byte) {
		d := Detect(topic, raw)
		if d.Size != len(raw) {
			t.Fatalf("size = %d for %d bytes", d.Size, len(raw))
		}
		// The invariant that matters: whatever is decided, the media type
		// handed to a browser is never something it will execute.
		media := SafeMediaType(raw)
		if strings.Contains(media, "html") || strings.Contains(media, "svg") ||
			strings.Contains(media, "script") || strings.Contains(media, "xml") {
			t.Fatalf("payload would be served as %q", media)
		}
		if d.Kind == KindImage && d.MediaType == "" {
			t.Fatal("an image with no media type")
		}
	})
}
