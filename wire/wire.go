// Package wire encodes OTLP metrics payloads without reflection.
//
// It exists because google.golang.org/protobuf cannot run on these targets.
// The generated code links and the host tests pass, but on the device the
// first Marshal panics:
//
//	panic: unimplemented: (reflect.Type).MethodByName()
//
// The call is unconditional in protobuf-go's makeStructInfo
// (internal/impl/message.go), which looks for the XXX_OneofFuncs and
// XXX_OneofWrappers legacy methods on every message type it initialises.
// TinyGo's reflect declares MethodByName but its body is a panic
// (src/reflect/type.go), so the failure is not about oneof fields or about
// memory: any use of the protobuf runtime reaches it.
//
// The wire format itself is trivial to produce by hand, so this package writes
// the field tags directly. Field numbers come from opentelemetry-proto v1.11.0
// and are named in the constants below.
package wire

import "math"

// Protobuf wire types. Only these three appear in an OTLP metrics payload.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
)

// Field numbers from opentelemetry-proto v1.11.0.
const (
	// MetricsData
	FieldMetricsDataResourceMetrics = 1

	// ResourceMetrics
	FieldResourceMetricsResource     = 1
	FieldResourceMetricsScopeMetrics = 2

	// Resource
	FieldResourceAttributes = 1

	// ScopeMetrics
	FieldScopeMetricsScope   = 1
	FieldScopeMetricsMetrics = 2

	// InstrumentationScope
	FieldScopeName    = 1
	FieldScopeVersion = 2

	// KeyValue
	FieldKeyValueKey   = 1
	FieldKeyValueValue = 2

	// AnyValue
	FieldAnyValueStringValue = 1

	// Metric
	FieldMetricName        = 1
	FieldMetricDescription = 2
	FieldMetricUnit        = 3
	FieldMetricGauge       = 5
	FieldMetricSum         = 7

	// Gauge
	FieldGaugeDataPoints = 1

	// Sum
	FieldSumDataPoints             = 1
	FieldSumAggregationTemporality = 2
	FieldSumIsMonotonic            = 3

	// NumberDataPoint
	FieldNDPAttributes        = 7
	FieldNDPStartTimeUnixNano = 2
	FieldNDPTimeUnixNano      = 3
	FieldNDPAsDouble          = 4
)

// AggregationTemporalityCumulative is AGGREGATION_TEMPORALITY_CUMULATIVE.
const AggregationTemporalityCumulative = 2

// Buffer builds a protobuf message in a reusable byte slice.
//
// Nested messages need a length prefix, and their length is not known until
// the contents are written. Rather than compute sizes in a separate pass, the
// buffer reserves a fixed-width placeholder, writes the contents, then patches
// the length and moves the bytes if the final varint is shorter than the
// placeholder. Payloads here are small, so the move costs less than a second
// traversal of the message tree.
type Buffer struct {
	b []byte
}

// NewBuffer creates a Buffer with the given capacity.
func NewBuffer(capacity int) *Buffer {
	return &Buffer{b: make([]byte, 0, capacity)}
}

// Reset clears the buffer but keeps its storage.
func (w *Buffer) Reset() { w.b = w.b[:0] }

// Bytes returns the encoded message. It is valid until the next Reset.
func (w *Buffer) Bytes() []byte { return w.b }

// Len reports the current encoded length.
func (w *Buffer) Len() int { return len(w.b) }

func (w *Buffer) tag(field, wireType int) {
	w.varint(uint64(field)<<3 | uint64(wireType))
}

func (w *Buffer) varint(v uint64) {
	for v >= 0x80 {
		w.b = append(w.b, byte(v)|0x80)
		v >>= 7
	}
	w.b = append(w.b, byte(v))
}

func (w *Buffer) fixed64(v uint64) {
	w.b = append(w.b,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}

// String writes a length-delimited string field. Empty strings are skipped
// because proto3 does not transmit zero values.
func (w *Buffer) String(field int, s string) {
	if s == "" {
		return
	}
	w.tag(field, wireBytes)
	w.varint(uint64(len(s)))
	w.b = append(w.b, s...)
}

// StringAlways writes a length-delimited string field even when it is empty.
//
// Use it for a oneof member: presence is what selects the variant, so an empty
// string must still be transmitted or the field reads as unset rather than
// empty.
func (w *Buffer) StringAlways(field int, s string) {
	w.tag(field, wireBytes)
	w.varint(uint64(len(s)))
	w.b = append(w.b, s...)
}

// Fixed64 writes a fixed64 field. A zero is skipped.
func (w *Buffer) Fixed64(field int, v uint64) {
	if v == 0 {
		return
	}
	w.tag(field, wireFixed64)
	w.fixed64(v)
}

// Double writes a double field. Unlike the other writers this always emits,
// because a measured value of zero is meaningful and OTLP carries it inside a
// oneof, where presence is what selects the variant.
func (w *Buffer) Double(field int, v float64) {
	w.tag(field, wireFixed64)
	w.fixed64(float64bits(v))
}

// Varint writes a varint field. A zero is skipped.
func (w *Buffer) Varint(field int, v uint64) {
	if v == 0 {
		return
	}
	w.tag(field, wireVarint)
	w.varint(v)
}

// Bool writes a bool field. A false is skipped.
func (w *Buffer) Bool(field int, v bool) {
	if !v {
		return
	}
	w.tag(field, wireVarint)
	w.b = append(w.b, 1)
}

// maxLenPlaceholder is the width reserved for a nested message length.
//
// Five bytes covers every length a uint32 can express, so the patch below
// never has to grow the buffer. Three bytes would cover 2,097,151 and silently
// corrupt the last byte of anything larger: the copy that closes the gap would
// have a destination one byte shorter than its source, and the re-slice would
// extend past len into spare capacity.
const maxLenPlaceholder = 5

// Nested writes a nested message field. The callback appends the contents.
func (w *Buffer) Nested(field int, fn func()) {
	w.tag(field, wireBytes)
	start := len(w.b)
	w.b = append(w.b, 0, 0, 0, 0, 0)
	fn()
	size := len(w.b) - start - maxLenPlaceholder

	n := varintLen(uint64(size))
	if n < maxLenPlaceholder {
		// The length needs fewer bytes than reserved, so close the gap. The
		// destination is longer than the source here, which is why this
		// direction is safe.
		copy(w.b[start+n:], w.b[start+maxLenPlaceholder:])
		w.b = w.b[:len(w.b)-(maxLenPlaceholder-n)]
	}
	putVarint(w.b[start:start+n], uint64(size))
}

func varintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func putVarint(dst []byte, v uint64) {
	i := 0
	for v >= 0x80 {
		dst[i] = byte(v) | 0x80
		v >>= 7
		i++
	}
	dst[i] = byte(v)
}

// float64bits is math.Float64bits. The standard library version is used
// directly; it compiles to a bitcast with no allocation.
func float64bits(f float64) uint64 { return math.Float64bits(f) }
