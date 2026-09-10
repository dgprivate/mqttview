package payload

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sparkplug B is a protobuf payload under a fixed topic namespace, and without
// decoding it a Sparkplug broker's topic tree is a wall of binary.
//
// The field numbers and the data-type indices below are read from
// sparkplug_b.proto in Eclipse Tahu, the specification's own schema, and not
// from a plausible-looking example. Two of them are easy to get backwards from
// memory — bytes_value is 16 and dataset_value is 17 — and a decoder that has
// them the wrong way round does not fail, it reports confident nonsense.
//
// Only the parts that carry a value are decoded. Templates, datasets and
// property sets are recognised and named rather than expanded: a person
// looking at a payload wants to know what a metric reads, and a half-expanded
// template that quietly drops the half nobody implemented is worse than an
// honest "a dataset, not shown".

// SparkplugTopicPrefix is the namespace the specification reserves.
const SparkplugTopicPrefix = "spBv1.0/"

// Payload field numbers.
const (
	fPayloadTimestamp = 1
	fPayloadMetrics   = 2
	fPayloadSeq       = 3
	fPayloadUUID      = 4
	fPayloadBody      = 5
)

// Metric field numbers.
const (
	fMetricName         = 1
	fMetricAlias        = 2
	fMetricTimestamp    = 3
	fMetricDatatype     = 4
	fMetricIsHistorical = 5
	fMetricIsTransient  = 6
	fMetricIsNull       = 7
	fMetricInt          = 10
	fMetricLong         = 11
	fMetricFloat        = 12
	fMetricDouble       = 13
	fMetricBool         = 14
	fMetricString       = 15
	fMetricBytes        = 16
	fMetricDataSet      = 17
	fMetricTemplate     = 18
)

// DataType indices, from the specification's enum.
const (
	dtUnknown  = 0
	dtInt8     = 1
	dtInt16    = 2
	dtInt32    = 3
	dtInt64    = 4
	dtUInt8    = 5
	dtUInt16   = 6
	dtUInt32   = 7
	dtUInt64   = 8
	dtFloat    = 9
	dtDouble   = 10
	dtBoolean  = 11
	dtString   = 12
	dtDateTime = 13
	dtText     = 14
	dtUUID     = 15
	dtDataSet  = 16
	dtBytes    = 17
	dtFile     = 18
	dtTemplate = 19
)

var dataTypeNames = map[uint32]string{
	dtUnknown: "Unknown", dtInt8: "Int8", dtInt16: "Int16", dtInt32: "Int32", dtInt64: "Int64",
	dtUInt8: "UInt8", dtUInt16: "UInt16", dtUInt32: "UInt32", dtUInt64: "UInt64",
	dtFloat: "Float", dtDouble: "Double", dtBoolean: "Boolean", dtString: "String",
	dtDateTime: "DateTime", dtText: "Text", dtUUID: "UUID", dtDataSet: "DataSet",
	dtBytes: "Bytes", dtFile: "File", dtTemplate: "Template",
	20: "PropertySet", 21: "PropertySetList",
	22: "Int8Array", 23: "Int16Array", 24: "Int32Array", 25: "Int64Array",
	26: "UInt8Array", 27: "UInt16Array", 28: "UInt32Array", 29: "UInt64Array",
	30: "FloatArray", 31: "DoubleArray", 32: "BooleanArray", 33: "StringArray",
	34: "DateTimeArray",
}

// SparkplugMetric is one reading out of a payload.
type SparkplugMetric struct {
	Name     string `json:"name,omitempty"`
	Alias    uint64 `json:"alias,omitempty"`
	HasAlias bool   `json:"hasAlias,omitempty"`
	DataType string `json:"dataType,omitempty"`
	// Value is the decoded reading, rendered for display. It is absent when
	// the metric is explicitly null or carries something not expanded here.
	Value string `json:"value,omitempty"`
	// Note explains an absent value rather than leaving a blank cell.
	Note         string     `json:"note,omitempty"`
	IsNull       bool       `json:"isNull,omitempty"`
	IsHistorical bool       `json:"isHistorical,omitempty"`
	IsTransient  bool       `json:"isTransient,omitempty"`
	Timestamp    *time.Time `json:"timestamp,omitempty"`
}

// SparkplugPayload is a decoded Sparkplug B message.
type SparkplugPayload struct {
	Timestamp *time.Time        `json:"timestamp,omitempty"`
	Seq       *uint64           `json:"seq,omitempty"`
	UUID      string            `json:"uuid,omitempty"`
	Metrics   []SparkplugMetric `json:"metrics"`
	// BodyBytes counts a body the specification allows a publisher to use
	// instead of metrics. Its contents are not ours to interpret.
	BodyBytes int `json:"bodyBytes,omitempty"`
}

// SparkplugTopic is the parsed form of a topic in the reserved namespace.
type SparkplugTopic struct {
	Group       string `json:"group"`
	MessageType string `json:"messageType"`
	EdgeNode    string `json:"edgeNode"`
	Device      string `json:"device,omitempty"`
}

// ParseSparkplugTopic reads the namespace's fixed shape:
// spBv1.0/<group>/<message type>/<edge node>[/<device>].
func ParseSparkplugTopic(topic string) (SparkplugTopic, bool) {
	rest, ok := strings.CutPrefix(topic, SparkplugTopicPrefix)
	if !ok {
		return SparkplugTopic{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || len(parts) > 4 {
		return SparkplugTopic{}, false
	}
	t := SparkplugTopic{Group: parts[0], MessageType: parts[1], EdgeNode: parts[2]}
	if len(parts) == 4 {
		t.Device = parts[3]
	}
	if t.Group == "" || t.MessageType == "" || t.EdgeNode == "" {
		return SparkplugTopic{}, false
	}
	return t, true
}

// LooksLikeSparkplug reports whether a topic is in the reserved namespace.
// The topic decides, not the payload: protobuf has no magic number, and
// guessing from the bytes would decode arbitrary binary as metrics.
func LooksLikeSparkplug(topic string) bool {
	_, ok := ParseSparkplugTopic(topic)
	return ok
}

// DecodeSparkplug decodes a Sparkplug B payload.
func DecodeSparkplug(raw []byte) (SparkplugPayload, error) {
	var out SparkplugPayload
	r := &reader{buf: raw}

	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return out, err
		}
		switch {
		case field == fPayloadTimestamp && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return out, err
			}
			t := millis(v)
			out.Timestamp = &t
		case field == fPayloadMetrics && wire == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return out, err
			}
			m, err := decodeMetric(b)
			if err != nil {
				return out, err
			}
			out.Metrics = append(out.Metrics, m)
		case field == fPayloadSeq && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return out, err
			}
			out.Seq = &v
		case field == fPayloadUUID && wire == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return out, err
			}
			out.UUID = string(b)
		case field == fPayloadBody && wire == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return out, err
			}
			out.BodyBytes = len(b)
		default:
			// Unknown fields are skipped rather than refused: the schema
			// reserves 6 and up for third-party extensions, and a payload
			// carrying one is valid.
			if err := r.skip(wire); err != nil {
				return out, err
			}
		}
	}

	if out.Metrics == nil {
		out.Metrics = []SparkplugMetric{}
	}
	return out, nil
}

// metricValue holds whichever arm of the oneof was present, before the
// datatype decides how to read it.
type metricValue struct {
	kind   int
	u32    uint32
	u64    uint64
	f32    uint32
	f64    uint64
	str    string
	blob   []byte
	nested string
}

func decodeMetric(raw []byte) (SparkplugMetric, error) {
	var (
		m        SparkplugMetric
		datatype uint32
		val      metricValue
		hasVal   bool
	)
	r := &reader{buf: raw}

	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return m, err
		}
		switch {
		case field == fMetricName && wire == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return m, err
			}
			m.Name = string(b)
		case field == fMetricAlias && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			m.Alias, m.HasAlias = v, true
		case field == fMetricTimestamp && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			t := millis(v)
			m.Timestamp = &t
		case field == fMetricDatatype && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			datatype = uint32(v)
		case field == fMetricIsHistorical && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			m.IsHistorical = v != 0
		case field == fMetricIsTransient && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			m.IsTransient = v != 0
		case field == fMetricIsNull && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			m.IsNull = v != 0
		case field == fMetricInt && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricInt, u32: uint32(v)}, true
		case field == fMetricLong && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricLong, u64: v}, true
		case field == fMetricFloat && wire == wireI32:
			v, err := r.fixed32()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricFloat, f32: v}, true
		case field == fMetricDouble && wire == wireI64:
			v, err := r.fixed64()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricDouble, f64: v}, true
		case field == fMetricBool && wire == wireVarint:
			v, err := r.varint()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricBool, u64: v}, true
		case field == fMetricString && wire == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricString, str: string(b)}, true
		case field == fMetricBytes && wire == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricBytes, blob: b}, true
		case field == fMetricDataSet && wire == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricDataSet, nested: byteCount(len(b))}, true
		case field == fMetricTemplate && wire == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return m, err
			}
			val, hasVal = metricValue{kind: fMetricTemplate, nested: byteCount(len(b))}, true
		default:
			if err := r.skip(wire); err != nil {
				return m, err
			}
		}
	}

	m.DataType = dataTypeName(datatype)
	switch {
	case m.IsNull:
		// The specification has an explicit null rather than a sentinel value,
		// and saying "null" is different from saying nothing arrived.
		m.Note = "null"
	case !hasVal:
		m.Note = "no value in this message"
	default:
		m.Value, m.Note = renderValue(datatype, val)
	}
	return m, nil
}

// renderValue turns the wire value into something readable, using the datatype
// to decide how to read it.
//
// The signed types are the reason this is not just "print whichever field was
// set": Int8, Int16 and Int32 all travel in int_value, which is a uint32, and
// Int64 travels in long_value, which is a uint64. A temperature of minus five
// on the wire is 4294967291, and a decoder that prints the field as it found
// it reports that number with complete confidence.
func renderValue(datatype uint32, v metricValue) (value, note string) {
	switch datatype {
	case dtInt8:
		return strconv.FormatInt(int64(int8(v.u32)), 10), ""
	case dtInt16:
		return strconv.FormatInt(int64(int16(v.u32)), 10), ""
	case dtInt32:
		return strconv.FormatInt(int64(int32(v.u32)), 10), ""
	case dtInt64:
		return strconv.FormatInt(int64(v.u64), 10), ""
	case dtUInt8, dtUInt16, dtUInt32:
		return strconv.FormatUint(uint64(v.u32), 10), ""
	case dtUInt64:
		return strconv.FormatUint(v.u64, 10), ""
	case dtFloat:
		return strconv.FormatFloat(float32From(v.f32), 'g', -1, 32), ""
	case dtDouble:
		return strconv.FormatFloat(float64From(v.f64), 'g', -1, 64), ""
	case dtBoolean:
		return strconv.FormatBool(v.u64 != 0), ""
	case dtString, dtText, dtUUID:
		return v.str, ""
	case dtDateTime:
		return millis(v.u64).UTC().Format(time.RFC3339Nano), ""
	case dtBytes, dtFile:
		return "", byteCount(len(v.blob)) + ", not shown"
	case dtDataSet:
		return "", "a dataset (" + v.nested + "), not expanded"
	case dtTemplate:
		return "", "a template (" + v.nested + "), not expanded"
	}

	// An unknown datatype index. Rather than guess, say which arm of the
	// oneof arrived and print it as that — which is at least true.
	switch v.kind {
	case fMetricInt:
		return strconv.FormatUint(uint64(v.u32), 10), unknownTypeNote(datatype)
	case fMetricLong:
		return strconv.FormatUint(v.u64, 10), unknownTypeNote(datatype)
	case fMetricFloat:
		return strconv.FormatFloat(float32From(v.f32), 'g', -1, 32), unknownTypeNote(datatype)
	case fMetricDouble:
		return strconv.FormatFloat(float64From(v.f64), 'g', -1, 64), unknownTypeNote(datatype)
	case fMetricBool:
		return strconv.FormatBool(v.u64 != 0), unknownTypeNote(datatype)
	case fMetricString:
		return v.str, unknownTypeNote(datatype)
	case fMetricBytes:
		return "", byteCount(len(v.blob)) + ", " + unknownTypeNote(datatype)
	}
	return "", unknownTypeNote(datatype)
}

func unknownTypeNote(datatype uint32) string {
	return fmt.Sprintf("datatype %d is not one this build knows", datatype)
}

func dataTypeName(datatype uint32) string {
	if name, ok := dataTypeNames[datatype]; ok {
		return name
	}
	return "datatype " + strconv.FormatUint(uint64(datatype), 10)
}

// millis converts the specification's milliseconds-since-epoch.
func millis(v uint64) time.Time {
	// Guarded because the value is attacker-controlled: an absurd number of
	// milliseconds would otherwise overflow into a date in the distant past.
	const maxMillis = 1 << 53
	if v > maxMillis {
		return time.Unix(0, 0).UTC()
	}
	return time.UnixMilli(int64(v)).UTC()
}

func byteCount(n int) string {
	if n == 1 {
		return "1 byte"
	}
	return strconv.Itoa(n) + " bytes"
}

// ErrNotSparkplug is returned when a payload under the namespace will not
// decode. It is kept distinct so the caller can fall back to showing the raw
// bytes rather than reporting a failure.
var ErrNotSparkplug = errors.New("payload: not a Sparkplug B payload")
