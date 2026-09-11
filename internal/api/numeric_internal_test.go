package api

import (
	"math"
	"testing"
)

// These are the shapes a broker actually publishes. Each one that is not
// handled is a chart with a hole in it or a field the chooser never offers.
func TestNumbersAreReadOutOfThePayloadsDevicesSend(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		field   string
		want    float64
		ok      bool
	}{
		{"a bare reading", "21.5", "", 21.5, true},
		{"a bare integer", "42", "", 42, true},
		{"a negative reading", "-3.25", "", -3.25, true},
		{"whitespace around it", "  17  ", "", 17, true},
		{"a relay saying ON", "ON", "", 1, true},
		{"a relay saying off", "off", "", 0, true},
		{"a cover saying OPEN", "OPEN", "", 1, true},
		{"a cover saying closed", "closed", "", 0, true},
		{"a top-level field", `{"temp":19.5}`, "temp", 19.5, true},
		{"a nested field", `{"env":{"in":{"temp":20}}}`, "env.in.temp", 20, true},
		{"a boolean field", `{"on":true}`, "on", 1, true},
		{"a false boolean", `{"on":false}`, "on", 0, true},
		{"a number carried as a string", `{"temp":"21.5"}`, "temp", 21.5, true},
		{"ON carried in a field", `{"state":"ON"}`, "state", 1, true},

		{"a word that is not a state", "unavailable", "", 0, false},
		{"a path into something that is not an object", `{"temp":21}`, "temp.deeper", 0, false},
		{"a field that is not there", `{"temp":21}`, "humidity", 0, false},
		{"a string field", `{"name":"kitchen"}`, "name", 0, false},
		{"an array, which has no stable names", `[1,2,3]`, "0", 0, false},
		{"a payload that is not JSON at all", "not json", "temp", 0, false},
		{"nothing", "", "", 0, false},
	} {
		got, ok := numericAt([]byte(c.payload), c.field)
		if ok != c.ok {
			t.Errorf("%s: read = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%s: value = %v, want %v", c.name, got, c.want)
		}
	}
}

// NaN and the infinities are not JSON numbers. Encoding one would fail the
// whole response rather than the one sample, so they are refused here.
func TestValuesThatAreNotJSONNumbersAreRefused(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, ok := finite(v); ok {
			t.Errorf("%v was accepted as a chartable value", v)
		}
	}
	if got, ok := finite(21.5); !ok || got != 21.5 {
		t.Errorf("an ordinary number was refused")
	}
}

func TestTheFieldChooserWalksObjectsButNotArrays(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		want    []string
		absent  []string
	}{
		{
			name:    "a flat object",
			payload: `{"temp":21.5,"hum":40,"name":"kitchen"}`,
			want:    []string{"temp", "hum"},
			absent:  []string{"name"},
		},
		{
			name:    "nested objects",
			payload: `{"a":{"b":{"c":1}}}`,
			want:    []string{"a.b.c"},
		},
		{
			// An index is not a stable name: readings.3.value means something
			// different on the next message.
			name:    "an array of numbers",
			payload: `{"readings":[1,2,3]}`,
			absent:  []string{"readings.0", "readings"},
		},
		{
			name:    "a bare number is the payload itself",
			payload: `21.5`,
			want:    []string{""},
		},
		{
			name:    "booleans chart as 0 and 1",
			payload: `{"on":true}`,
			want:    []string{"on"},
		},
	} {
		found := map[string]bool{}
		collectNumericPaths([]byte(c.payload), found)

		for _, want := range c.want {
			if !found[want] {
				t.Errorf("%s: %q was not offered; got %v", c.name, want, keys(found))
			}
		}
		for _, absent := range c.absent {
			if found[absent] {
				t.Errorf("%s: %q was offered and should not have been", c.name, absent)
			}
		}
	}
}

// A payload nested past any sensible depth must not be walked forever.
func TestTheFieldChooserStopsDescending(t *testing.T) {
	payload := `{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":{"j":1}}}}}}}}}}`
	found := map[string]bool{}
	collectNumericPaths([]byte(payload), found)

	for path := range found {
		if len(path) > 40 {
			t.Errorf("descended to %q, past any depth worth offering", path)
		}
	}
}

func TestByteCountReadsAsEnglish(t *testing.T) {
	for _, c := range []struct {
		in   int
		want string
	}{
		{0, "0 bytes"},
		{1, "1 byte"},
		{512, "512 bytes"},
		{2048, "2 KiB"},
	} {
		if got := byteCount(c.in); got != c.want {
			t.Errorf("byteCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAnExportFilenameIsSomethingAFilesystemAccepts(t *testing.T) {
	for _, c := range []struct{ topic, wantPrefix string }{
		{"home/kitchen/temperature", "home-kitchen-temperature-"},
		{"a b/c", "a-b-c-"},
		{"///", "topic-"},
		{"", "topic-"},
	} {
		got := exportFilename(c.topic, "csv")
		if len(got) < len(c.wantPrefix) || got[:len(c.wantPrefix)] != c.wantPrefix {
			t.Errorf("exportFilename(%q) = %q, want it to start %q", c.topic, got, c.wantPrefix)
		}
		for _, r := range got {
			if r == '/' || r == '\\' || r == 0 {
				t.Errorf("exportFilename(%q) = %q, which a filesystem will not take", c.topic, got)
			}
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
