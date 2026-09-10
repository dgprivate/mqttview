// Package payload turns a raw MQTT payload into something a person can read:
// what kind of thing it is, and for the encodings mqttview understands, what
// it says.
package payload

import (
	"encoding/binary"
	"errors"
	"math"
)

// Protobuf wire types, from the encoding specification.
const (
	wireVarint = 0
	wireI64    = 1
	wireBytes  = 2
	wireSGroup = 3 // deprecated groups; skipped, not parsed
	wireEGroup = 4
	wireI32    = 5
)

// errTruncated is what every read returns when the buffer runs out. Sparkplug
// payloads arrive from the network and a truncated one is a normal thing to
// receive, not an exceptional one.
var errTruncated = errors.New("payload: truncated")

// reader walks a protobuf message. It is deliberately small: mqttview decodes
// two message shapes and needs no descriptors, no reflection and no
// dependency to do it.
type reader struct {
	buf []byte
	pos int
}

func (r *reader) done() bool { return r.pos >= len(r.buf) }

// tag reads a field number and wire type.
func (r *reader) tag() (field int, wire int, err error) {
	v, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	field = int(v >> 3)
	wire = int(v & 7)
	if field <= 0 {
		return 0, 0, errors.New("payload: field number zero")
	}
	return field, wire, nil
}

func (r *reader) varint() (uint64, error) {
	var v uint64
	var shift uint
	for {
		if r.pos >= len(r.buf) {
			return 0, errTruncated
		}
		b := r.buf[r.pos]
		r.pos++
		// The tenth byte can only carry one bit; more than that is not a
		// 64-bit varint and continuing would silently wrap.
		if shift >= 64 {
			return 0, errors.New("payload: varint overflows 64 bits")
		}
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, nil
		}
		shift += 7
	}
}

func (r *reader) fixed32() (uint32, error) {
	if r.pos+4 > len(r.buf) {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *reader) fixed64() (uint64, error) {
	if r.pos+8 > len(r.buf) {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *reader) bytes() ([]byte, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	// The length is attacker-controlled: comparing it against the remaining
	// buffer before slicing is what stops a crafted length from panicking.
	if n > uint64(len(r.buf)-r.pos) {
		return nil, errTruncated
	}
	out := r.buf[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return out, nil
}

// skip advances past a field whose value is not wanted.
func (r *reader) skip(wire int) error {
	switch wire {
	case wireVarint:
		_, err := r.varint()
		return err
	case wireI64:
		_, err := r.fixed64()
		return err
	case wireBytes:
		_, err := r.bytes()
		return err
	case wireI32:
		_, err := r.fixed32()
		return err
	case wireSGroup, wireEGroup:
		// Groups were removed from the language long before Sparkplug was
		// written. Refusing beats guessing at a nesting depth.
		return errors.New("payload: deprecated group encoding")
	default:
		return errors.New("payload: unknown wire type")
	}
}

func float32From(bits uint32) float64 { return float64(math.Float32frombits(bits)) }
func float64From(bits uint64) float64 { return math.Float64frombits(bits) }
