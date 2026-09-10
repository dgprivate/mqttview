package logbuf

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func newTestBuffer(size int, consoleLevel slog.Level) (*Buffer, *bytes.Buffer) {
	console := &bytes.Buffer{}
	handler := slog.NewTextHandler(console, &slog.HandlerOptions{Level: consoleLevel})
	return New(handler, size), console
}

func TestRecordsComeBackOldestFirst(t *testing.T) {
	b, _ := newTestBuffer(10, slog.LevelDebug)
	log := slog.New(b)

	log.Info("first")
	log.Info("second")
	log.Info("third")

	got := b.Records(slog.LevelDebug, 0, 0)
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i].Message != want {
			t.Errorf("record %d is %q, want %q", i, got[i].Message, want)
		}
	}
}

func TestTheRingOverwritesTheOldestRecord(t *testing.T) {
	b, _ := newTestBuffer(3, slog.LevelDebug)
	log := slog.New(b)

	for _, m := range []string{"a", "b", "c", "d", "e"} {
		log.Info(m)
	}

	got := b.Records(slog.LevelDebug, 0, 0)
	if len(got) != 3 {
		t.Fatalf("got %d records, want the ring size of 3", len(got))
	}
	if got[0].Message != "c" || got[2].Message != "e" {
		t.Errorf("kept %q..%q, want c..e", got[0].Message, got[2].Message)
	}
}

// The buffer never becomes the only place a line exists.
func TestEveryRecordStillReachesTheConsole(t *testing.T) {
	b, console := newTestBuffer(10, slog.LevelInfo)
	log := slog.New(b)

	log.Info("on the console")

	if !strings.Contains(console.String(), "on the console") {
		t.Errorf("the console got %q", console.String())
	}
}

// The configured level keeps one meaning. A record suppressed for the console
// is suppressed for the view, so somebody running at warn is not later shown a
// view full of the debug lines they chose not to collect — and, more to the
// point, does not pay to build them on every request.
func TestTheConfiguredLevelGovernsTheViewAsWellAsTheConsole(t *testing.T) {
	b, console := newTestBuffer(10, slog.LevelWarn)
	log := slog.New(b)

	log.Debug("a quiet detail")
	log.Warn("something louder")

	kept := b.Records(slog.LevelDebug, 0, 0)
	if len(kept) != 1 || kept[0].Message != "something louder" {
		t.Fatalf("kept %+v, want only the record the level admits", kept)
	}
	if strings.Contains(console.String(), "a quiet detail") {
		t.Error("a debug line reached a console set to warn")
	}
	if !strings.Contains(console.String(), "something louder") {
		t.Error("a warning did not reach the console")
	}
}

func TestTheLevelFilterExcludesQuieterRecords(t *testing.T) {
	b, _ := newTestBuffer(10, slog.LevelDebug)
	log := slog.New(b)

	log.Debug("debug")
	log.Info("info")
	log.Warn("warn")
	log.Error("error")

	if got := b.Records(slog.LevelWarn, 0, 0); len(got) != 2 {
		t.Errorf("filtering at warn gave %d records, want 2", len(got))
	}
	if got := b.Records(slog.LevelError, 0, 0); len(got) != 1 {
		t.Errorf("filtering at error gave %d records, want 1", len(got))
	}
}

// A view that polls must be able to ask for what it has not seen.
func TestSinceReturnsOnlyWhatTheCallerHasNotSeen(t *testing.T) {
	b, _ := newTestBuffer(10, slog.LevelDebug)
	log := slog.New(b)

	log.Info("before")
	seen := b.Seq()
	log.Info("after")

	got := b.Records(slog.LevelDebug, seen, 0)
	if len(got) != 1 || got[0].Message != "after" {
		t.Errorf("got %+v, want only the record written after the anchor", got)
	}
}

// Most loggers in the program are derived with With(...). If those records
// went somewhere else, the view would be empty of nearly everything.
func TestRecordsFromADerivedLoggerLandInTheSameBuffer(t *testing.T) {
	b, _ := newTestBuffer(10, slog.LevelDebug)
	log := slog.New(b).With("component", "mqtt")

	log.Info("connected", "broker", "house")

	got := b.Records(slog.LevelDebug, 0, 0)
	if len(got) != 1 {
		t.Fatalf("got %d records, want the one from the derived logger", len(got))
	}
	if got[0].Attrs["component"] != "mqtt" {
		t.Errorf("attrs = %v, want the derived attribute carried through", got[0].Attrs)
	}
	if got[0].Attrs["broker"] != "house" {
		t.Errorf("attrs = %v, want the call's own attribute", got[0].Attrs)
	}
}

func TestAGroupedLoggerAlsoLandsInTheSameBuffer(t *testing.T) {
	b, _ := newTestBuffer(10, slog.LevelDebug)
	log := slog.New(b).WithGroup("mqtt")

	log.Info("connected")

	if got := b.Records(slog.LevelDebug, 0, 0); len(got) != 1 {
		t.Fatalf("got %d records from a grouped logger, want 1", len(got))
	}
}

func TestALimitTakesTheNewestRecords(t *testing.T) {
	b, _ := newTestBuffer(10, slog.LevelDebug)
	log := slog.New(b)

	for _, m := range []string{"a", "b", "c", "d"} {
		log.Info(m)
	}

	got := b.Records(slog.LevelDebug, 0, 2)
	if len(got) != 2 || got[0].Message != "c" || got[1].Message != "d" {
		t.Errorf("got %+v, want the two newest", got)
	}
}

func TestLevelNamesAreParsed(t *testing.T) {
	for _, c := range []struct {
		in   string
		want slog.Level
		ok   bool
	}{
		{"", slog.LevelDebug, true},
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"ERROR", slog.LevelError, true},
		{"verbose", 0, false},
	} {
		got, ok := ParseLevel(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestConcurrentLoggersAndReadersDoNotRace(t *testing.T) {
	b, _ := newTestBuffer(50, slog.LevelDebug)
	log := slog.New(b)

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			derived := log.With("worker", w)
			for range 100 {
				derived.Info("working")
			}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				b.Records(slog.LevelDebug, 0, 10)
				b.Seq()
			}
		}()
	}
	wg.Wait()
}

func TestZeroSizeTakesTheDefault(t *testing.T) {
	b := New(slog.NewTextHandler(&bytes.Buffer{}, nil), 0)
	if len(b.buf) != DefaultSize {
		t.Errorf("ring holds %d, want the default %d", len(b.buf), DefaultSize)
	}
}

// A caller that checks Enabled before doing expensive work must get the same
// answer it would have got without the buffer in the way.
func TestEnabledMatchesTheWrappedHandler(t *testing.T) {
	b, _ := newTestBuffer(10, slog.LevelError)

	if b.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug is enabled on a handler configured for error")
	}
	if !b.Enabled(context.Background(), slog.LevelError) {
		t.Error("error is disabled on a handler configured for error")
	}
}
