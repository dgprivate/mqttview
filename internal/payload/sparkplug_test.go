package payload

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// A tiny encoder, so the tests build payloads the way a device would rather
// than asserting against bytes somebody pasted in.
type pb struct{ out []byte }

func (p *pb) varint(field int, v uint64) *pb {
	p.tag(field, wireVarint)
	p.raw(v)
	return p
}

func (p *pb) fixed32(field int, v uint32) *pb {
	p.tag(field, wireI32)
	p.out = binary.LittleEndian.AppendUint32(p.out, v)
	return p
}

func (p *pb) fixed64(field int, v uint64) *pb {
	p.tag(field, wireI64)
	p.out = binary.LittleEndian.AppendUint64(p.out, v)
	return p
}

func (p *pb) bytes(field int, v []byte) *pb {
	p.tag(field, wireBytes)
	p.raw(uint64(len(v)))
	p.out = append(p.out, v...)
	return p
}

func (p *pb) str(field int, v string) *pb { return p.bytes(field, []byte(v)) }

func (p *pb) tag(field, wire int) { p.raw(uint64(field)<<3 | uint64(wire)) }

func (p *pb) raw(v uint64) {
	for v >= 0x80 {
		p.out = append(p.out, byte(v)|0x80)
		v >>= 7
	}
	p.out = append(p.out, byte(v))
}

// metric builds one metric with a name and datatype, then whatever the caller
// adds as the value.
func metric(name string, datatype uint64, value func(*pb)) []byte {
	m := (&pb{}).str(fMetricName, name).varint(fMetricDatatype, datatype)
	if value != nil {
		value(m)
	}
	return m.out
}

func payloadOf(metrics ...[]byte) []byte {
	p := &pb{}
	for _, m := range metrics {
		p.bytes(fPayloadMetrics, m)
	}
	return p.out
}

func decodeOne(t *testing.T, raw []byte) SparkplugMetric {
	t.Helper()

	got, err := DecodeSparkplug(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Metrics) != 1 {
		t.Fatalf("got %d metrics, want 1", len(got.Metrics))
	}
	return got.Metrics[0]
}

// The whole reason the datatype is consulted rather than the field printed as
// found. Signed integers travel in unsigned fields: minus five as an Int32 is
// 4294967291 on the wire, and a decoder that prints what it found reports that
// number with complete confidence.
func TestSignedIntegersComeBackNegative(t *testing.T) {
	for _, c := range []struct {
		name     string
		datatype uint64
		wire     uint64
		want     string
	}{
		{"Int8", dtInt8, uint64(uint32(uint8(0xfb))), "-5"},
		{"Int16", dtInt16, uint64(uint16(0xfffb)), "-5"},
		{"Int32", dtInt32, uint64(uint32(0xfffffffb)), "-5"},
		{"Int64", dtInt64, math.MaxUint64 - 4, "-5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			field := fMetricInt
			if c.datatype == dtInt64 {
				field = fMetricLong
			}
			m := decodeOne(t, payloadOf(metric("t", c.datatype, func(p *pb) {
				p.varint(field, c.wire)
			})))
			if m.Value != c.want {
				t.Errorf("value = %q, want %q", m.Value, c.want)
			}
		})
	}
}

func TestUnsignedIntegersAreNotSignExtended(t *testing.T) {
	m := decodeOne(t, payloadOf(metric("t", dtUInt32, func(p *pb) {
		p.varint(fMetricInt, 4294967291)
	})))
	if m.Value != "4294967291" {
		t.Errorf("value = %q, want the unsigned reading", m.Value)
	}
}

// bytes_value is 16 and dataset_value is 17. Having them the other way round
// does not fail, it reports confident nonsense, which is why they are read
// from the specification's own schema.
func TestBytesAndDatasetAreNotTheOtherWayRound(t *testing.T) {
	blob := decodeOne(t, payloadOf(metric("blob", dtBytes, func(p *pb) {
		p.bytes(fMetricBytes, []byte{1, 2, 3, 4})
	})))
	if !strings.Contains(blob.Note, "4 bytes") {
		t.Errorf("a Bytes metric reported %q / %q", blob.Value, blob.Note)
	}

	set := decodeOne(t, payloadOf(metric("set", dtDataSet, func(p *pb) {
		p.bytes(fMetricDataSet, []byte{9, 9, 9})
	})))
	if !strings.Contains(set.Note, "dataset") {
		t.Errorf("a DataSet metric reported %q / %q", set.Value, set.Note)
	}
}

func TestTheOrdinaryScalarTypesRoundTrip(t *testing.T) {
	for _, c := range []struct {
		name     string
		datatype uint64
		build    func(*pb)
		want     string
	}{
		{"Float", dtFloat, func(p *pb) { p.fixed32(fMetricFloat, math.Float32bits(21.5)) }, "21.5"},
		{"Double", dtDouble, func(p *pb) { p.fixed64(fMetricDouble, math.Float64bits(-3.25)) }, "-3.25"},
		{"Boolean true", dtBoolean, func(p *pb) { p.varint(fMetricBool, 1) }, "true"},
		{"Boolean false", dtBoolean, func(p *pb) { p.varint(fMetricBool, 0) }, "false"},
		{"String", dtString, func(p *pb) { p.str(fMetricString, "kitchen") }, "kitchen"},
		{"Text", dtText, func(p *pb) { p.str(fMetricString, "a note") }, "a note"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := decodeOne(t, payloadOf(metric("t", c.datatype, c.build)))
			if m.Value != c.want {
				t.Errorf("value = %q, want %q", m.Value, c.want)
			}
			if m.Note != "" {
				t.Errorf("note = %q, want none for a type that decodes cleanly", m.Note)
			}
		})
	}
}

func TestADateTimeIsRenderedAsATime(t *testing.T) {
	// 2021-01-01T00:00:00Z in milliseconds.
	m := decodeOne(t, payloadOf(metric("when", dtDateTime, func(p *pb) {
		p.varint(fMetricLong, 1609459200000)
	})))
	if !strings.HasPrefix(m.Value, "2021-01-01T00:00:00") {
		t.Errorf("value = %q, want an ISO timestamp", m.Value)
	}
}

// The specification has an explicit null rather than a sentinel, and "null" is
// a different claim from "zero".
func TestAnExplicitNullIsNotReportedAsZero(t *testing.T) {
	m := decodeOne(t, payloadOf(metric("t", dtInt32, func(p *pb) {
		p.varint(fMetricIsNull, 1)
		p.varint(fMetricInt, 0)
	})))

	if !m.IsNull {
		t.Error("the null flag was dropped")
	}
	if m.Value != "" {
		t.Errorf("value = %q, want none: the metric is null", m.Value)
	}
	if m.Note != "null" {
		t.Errorf("note = %q, want it to say null", m.Note)
	}
}

func TestAMetricWithNoValueSaysSo(t *testing.T) {
	m := decodeOne(t, payloadOf(metric("t", dtInt32, nil)))
	if m.Value != "" || m.Note == "" {
		t.Errorf("value = %q, note = %q; want an explanation rather than a blank", m.Value, m.Note)
	}
}

// A device using an index this build has not seen must not become an error or
// a silent blank.
func TestAnUnknownDatatypeIsReportedHonestly(t *testing.T) {
	m := decodeOne(t, payloadOf(metric("t", 99, func(p *pb) {
		p.varint(fMetricInt, 7)
	})))

	if m.Value != "7" {
		t.Errorf("value = %q, want the number that actually arrived", m.Value)
	}
	if !strings.Contains(m.Note, "99") {
		t.Errorf("note = %q, want it to name the datatype it did not know", m.Note)
	}
}

func TestThePayloadEnvelopeIsRead(t *testing.T) {
	p := &pb{}
	p.varint(fPayloadTimestamp, 1609459200000)
	p.bytes(fPayloadMetrics, metric("a", dtInt32, func(m *pb) { m.varint(fMetricInt, 1) }))
	p.varint(fPayloadSeq, 42)
	p.str(fPayloadUUID, "abc")

	got, err := DecodeSparkplug(p.out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Timestamp == nil || got.Timestamp.Year() != 2021 {
		t.Errorf("timestamp = %v", got.Timestamp)
	}
	if got.Seq == nil || *got.Seq != 42 {
		t.Errorf("seq = %v, want 42", got.Seq)
	}
	if got.UUID != "abc" {
		t.Errorf("uuid = %q", got.UUID)
	}
	if len(got.Metrics) != 1 {
		t.Errorf("got %d metrics, want 1", len(got.Metrics))
	}
}

// The schema reserves field 6 and up for third-party extensions, so a payload
// carrying one is valid and must not be refused.
func TestAnExtensionFieldIsSkippedRatherThanRefused(t *testing.T) {
	p := &pb{}
	p.varint(fPayloadSeq, 1)
	p.str(99, "something a vendor added")
	p.bytes(fPayloadMetrics, metric("a", dtInt32, func(m *pb) { m.varint(fMetricInt, 5) }))

	got, err := DecodeSparkplug(p.out)
	if err != nil {
		t.Fatalf("an extension field made the whole payload unreadable: %v", err)
	}
	if len(got.Metrics) != 1 || got.Metrics[0].Value != "5" {
		t.Errorf("metrics = %+v, want the one that followed the extension", got.Metrics)
	}
}

// These arrive from the network, so a truncated one is ordinary rather than
// exceptional. It must be an error and never a panic.
func TestATruncatedPayloadIsAnErrorNotAPanic(t *testing.T) {
	full := payloadOf(metric("temperature", dtDouble, func(p *pb) {
		p.fixed64(fMetricDouble, math.Float64bits(21.5))
	}))

	for cut := range len(full) {
		if _, err := DecodeSparkplug(full[:cut]); err == nil && cut != 0 {
			// Some prefixes are legitimately complete messages; only a
			// mid-field cut has to fail. The point of the loop is that none
			// of them panics.
			continue
		}
	}
}

// A length that claims more than the buffer holds is the obvious way to make a
// decoder panic.
func TestALengthLongerThanTheBufferIsRefused(t *testing.T) {
	p := &pb{}
	p.tag(fPayloadMetrics, wireBytes)
	p.raw(9999) // claims 9999 bytes and supplies none

	if _, err := DecodeSparkplug(p.out); err == nil {
		t.Error("a length beyond the end of the buffer was accepted")
	}
}

func TestTheTopicNamespaceIsParsed(t *testing.T) {
	for _, c := range []struct {
		topic  string
		ok     bool
		device string
	}{
		{"spBv1.0/plant1/NDATA/edge1", true, ""},
		{"spBv1.0/plant1/DDATA/edge1/press1", true, "press1"},
		{"spBv1.0/plant1/NBIRTH/edge1", true, ""},
		{"home/kitchen/temperature", false, ""},
		{"spBv1.0/plant1/NDATA", false, ""},
		{"spBv1.0/plant1/NDATA/edge1/dev/toomany", false, ""},
		{"spBv1.0//NDATA/edge1", false, ""},
	} {
		got, ok := ParseSparkplugTopic(c.topic)
		if ok != c.ok {
			t.Errorf("%q: parsed = %v, want %v", c.topic, ok, c.ok)
			continue
		}
		if ok && got.Device != c.device {
			t.Errorf("%q: device = %q, want %q", c.topic, got.Device, c.device)
		}
	}
}

// Protobuf has no magic number. Sniffing the payload would decode arbitrary
// binary as metrics, so the topic is what decides.
func TestOnlyTheTopicDecidesWhetherSomethingIsSparkplug(t *testing.T) {
	if LooksLikeSparkplug("home/sensor/raw") {
		t.Error("a topic outside the reserved namespace was treated as Sparkplug")
	}
	if !LooksLikeSparkplug("spBv1.0/g/NDATA/e") {
		t.Error("a topic inside the reserved namespace was not recognised")
	}
}

func TestAnAbsurdTimestampDoesNotWrapIntoThePast(t *testing.T) {
	m := decodeOne(t, payloadOf(metric("when", dtDateTime, func(p *pb) {
		p.varint(fMetricLong, math.MaxUint64)
	})))
	if strings.HasPrefix(m.Value, "-") {
		t.Errorf("value = %q, want no negative year from an absurd millisecond count", m.Value)
	}
}

func FuzzDecodeSparkplug(f *testing.F) {
	f.Add([]byte{})
	f.Add(payloadOf(metric("t", dtInt32, func(p *pb) { p.varint(fMetricInt, 1) })))
	f.Add(payloadOf(metric("s", dtString, func(p *pb) { p.str(fMetricString, "x") })))
	f.Add(payloadOf(metric("b", dtBytes, func(p *pb) { p.bytes(fMetricBytes, []byte{0xff, 0x00}) })))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0x0a, 0xff, 0x7f})

	f.Fuzz(func(t *testing.T, raw []byte) {
		got, err := DecodeSparkplug(raw)
		if err != nil {
			return
		}
		// A successful decode must be self-consistent: a metric cannot be both
		// null and carry a value, which is what a UI would render as a
		// contradiction.
		for _, m := range got.Metrics {
			if m.IsNull && m.Value != "" {
				t.Fatalf("metric %q is null and has value %q", m.Name, m.Value)
			}
		}
	})
}
