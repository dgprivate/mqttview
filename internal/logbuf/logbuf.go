// Package logbuf keeps the most recent log records in memory so the UI can
// show them.
//
// It does not write files and does not rotate them. mqttview logs to standard
// error, and whatever runs it — systemd, Docker, the Supervisor — already
// collects, rotates and ships that. A second log file with its own rotation
// policy would be a second opinion about something the platform already
// decides, and the two would disagree the first time somebody looked.
//
// This is for the other problem: reading the last few hundred lines without
// shell access to the machine, which is the common case for an add-on running
// on somebody's Home Assistant.
package logbuf

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultSize is how many records are kept.
const DefaultSize = 1000

// Record is one log line, flattened for JSON.
type Record struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	// Attrs are the structured fields, rendered as strings. They are strings
	// rather than any: these go straight to a browser, and a log attribute is
	// a display value by the time it gets here.
	Attrs map[string]string `json:"attrs,omitempty"`
	// Seq lets the UI poll for what it has not seen without comparing
	// timestamps, which are not unique at this resolution.
	Seq uint64 `json:"seq"`
}

// Buffer is a slog.Handler that keeps records and passes them on.
type Buffer struct {
	next slog.Handler

	mu     sync.RWMutex
	buf    []Record
	pos    int
	filled bool
	seq    uint64
}

// New wraps a handler. Every record still reaches next, so the buffer never
// becomes the only place a line exists.
func New(next slog.Handler, size int) *Buffer {
	if size <= 0 {
		size = DefaultSize
	}
	return &Buffer{next: next, buf: make([]Record, size)}
}

// Enabled defers to the wrapped handler, so the configured log level means
// exactly what it did before this buffer existed.
//
// Capturing below that level was tried and removed. It would put debug records
// in the view on an install running at info, but only by making every
// suppressed Debug call build a record and take a lock — on a path that runs
// once per HTTP request and once per MQTT message. It also gives "level" two
// meanings, so somebody who set error and then read a view full of debug lines
// has been overruled without being told. Wanting debug in the view is a reason
// to run at debug.
func (b *Buffer) Enabled(ctx context.Context, level slog.Level) bool {
	return b.next.Enabled(ctx, level)
}

// Handle records the entry and passes it on.
func (b *Buffer) Handle(ctx context.Context, r slog.Record) error {
	b.record(r)
	return b.next.Handle(ctx, r)
}

// WithAttrs returns a handler that adds attrs, sharing the same ring.
func (b *Buffer) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &child{buffer: b, handler: b.next.WithAttrs(attrs), attrs: attrs}
}

// WithGroup returns a handler in a group, sharing the same ring.
func (b *Buffer) WithGroup(name string) slog.Handler {
	return &child{buffer: b, handler: b.next.WithGroup(name), group: name}
}

// child is a handler derived by WithAttrs or WithGroup. It writes into the
// same ring as its parent — otherwise a logger that any package derived would
// keep its records somewhere the log view cannot see, which is most of them.
type child struct {
	buffer  *Buffer
	handler slog.Handler
	attrs   []slog.Attr
	group   string
}

func (c *child) Enabled(ctx context.Context, level slog.Level) bool {
	return c.handler.Enabled(ctx, level)
}

func (c *child) Handle(ctx context.Context, r slog.Record) error {
	// The derived attributes are added to the copy that is recorded, so a line
	// in the view carries the same fields as the one on the console.
	recorded := r.Clone()
	if len(c.attrs) > 0 {
		recorded.AddAttrs(c.attrs...)
	}
	c.buffer.record(recorded)
	return c.handler.Handle(ctx, r)
}

func (c *child) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &child{buffer: c.buffer, handler: c.handler.WithAttrs(attrs), attrs: append(append([]slog.Attr(nil), c.attrs...), attrs...), group: c.group}
}

func (c *child) WithGroup(name string) slog.Handler {
	return &child{buffer: c.buffer, handler: c.handler.WithGroup(name), attrs: c.attrs, group: name}
}

// record appends without touching the wrapped handler.
func (b *Buffer) record(r slog.Record) {
	rec := Record{Time: r.Time, Level: r.Level.String(), Message: r.Message}
	if r.NumAttrs() > 0 {
		rec.Attrs = make(map[string]string, r.NumAttrs())
		r.Attrs(func(a slog.Attr) bool {
			rec.Attrs[a.Key] = a.Value.String()
			return true
		})
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	rec.Seq = b.seq
	b.buf[b.pos] = rec
	b.pos = (b.pos + 1) % len(b.buf)
	if b.pos == 0 {
		b.filled = true
	}
}

// Records returns the kept records, oldest first.
//
// minLevel filters by severity, and since returns only what a caller has not
// already seen, so the view can poll without re-rendering the whole buffer.
func (b *Buffer) Records(minLevel slog.Level, since uint64, limit int) []Record {
	b.mu.RLock()
	defer b.mu.RUnlock()

	all := b.chronological()
	out := make([]Record, 0, len(all))
	for _, r := range all {
		if r.Seq <= since {
			continue
		}
		if levelValue(r.Level) < minLevel {
			continue
		}
		out = append(out, r)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Seq is the newest sequence number, so a caller can anchor a poll.
func (b *Buffer) Seq() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.seq
}

func (b *Buffer) chronological() []Record {
	if !b.filled {
		return b.buf[:b.pos]
	}
	out := make([]Record, 0, len(b.buf))
	out = append(out, b.buf[b.pos:]...)
	out = append(out, b.buf[:b.pos]...)
	return out
}

// levelValue turns a rendered level back into a comparable one.
func levelValue(name string) slog.Level {
	switch name {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	}
	// An unrecognised level is shown rather than filtered out: a custom level
	// hidden by a filter nobody set is worse than one that appears too often.
	return slog.LevelError
}

// ParseLevel reads a level name for the API's filter.
func ParseLevel(name string) (slog.Level, bool) {
	switch name {
	case "", "debug", "DEBUG":
		return slog.LevelDebug, true
	case "info", "INFO":
		return slog.LevelInfo, true
	case "warn", "WARN", "warning":
		return slog.LevelWarn, true
	case "error", "ERROR":
		return slog.LevelError, true
	}
	return 0, false
}
