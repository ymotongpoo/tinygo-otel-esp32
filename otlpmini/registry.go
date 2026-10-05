// Package otlpmini is a minimal OpenTelemetry metrics pipeline for TinyGo.
//
// Two layers of the official Go stack do not work on a microcontroller, for
// two unrelated reasons:
//
//   - go.opentelemetry.io/otel/sdk/metric does not cross-compile. sdk/resource
//     imports golang.org/x/sys/unix for host detection, and the otlpmetrichttp
//     exporter references tls.X509KeyPair, which TinyGo's crypto/tls does not
//     implement.
//   - google.golang.org/protobuf compiles but panics at run time on the first
//     Marshal: protobuf-go's makeStructInfo calls reflect.Type.MethodByName
//     unconditionally, and TinyGo's implementation of that method is a panic.
//
// So this package encodes the OTLP wire format by hand, using the field
// numbers from opentelemetry-proto v1.11.0. The protocol is reachable; the
// reference implementation of it is not.
package otlpmini

import "github.com/ymotongpoo/tinygo-otel-esp32/wire"

// Kind selects the OTLP data type used to encode an instrument.
type Kind uint8

const (
	// KindGauge encodes a value that can go up and down, such as free memory.
	// It carries no temporality.
	KindGauge Kind = iota
	// KindCounter encodes a monotonically increasing cumulative total, such as
	// a count of export attempts.
	KindCounter
)

// Attr is a string-valued attribute. Only strings are supported because every
// attribute this firmware attaches is a label, and the other AnyValue variants
// would be dead code.
type Attr struct {
	Key   string
	Value string
}

// Instrument is one metric stream. Callers hold a pointer and write to it
// directly, so recording a measurement performs no allocation.
type Instrument struct {
	Name  string
	Unit  string
	Desc  string
	Kind  Kind
	Attrs []Attr

	value float64
}

// Set replaces the current value. Use it for gauges.
func (i *Instrument) Set(v float64) { i.value = v }

// Add increases the current value. Use it for counters.
func (i *Instrument) Add(v float64) { i.value += v }

// Value reports the current value.
func (i *Instrument) Value() float64 { return i.value }

// Registry owns the instruments of one device and encodes OTLP payloads.
type Registry struct {
	scopeName    string
	scopeVersion string
	resource     []Attr

	instruments []*Instrument
	buf         *wire.Buffer
}

// NewRegistry creates a Registry for a single device.
//
// The resource attributes are the ones a microcontroller can know about
// itself. There is no automatic detection because there is no operating system
// to ask; dropping sdk/resource makes that the caller's job.
func NewRegistry(scopeName, scopeVersion string, resourceAttrs []Attr) *Registry {
	return &Registry{
		scopeName:    scopeName,
		scopeVersion: scopeVersion,
		resource:     resourceAttrs,
		buf:          wire.NewBuffer(2048),
	}
}

// Register adds an instrument and returns it.
func (r *Registry) Register(in *Instrument) *Instrument {
	r.instruments = append(r.instruments, in)
	return in
}

// Encode writes a MetricsData message into the Registry's reusable buffer and
// returns it. The result is valid until the next call to Encode.
//
// startUnixNano is the time the firmware started measuring, in Unix
// nanoseconds. OTLP requires it on cumulative sums so that a backend can tell
// a counter reset from a gap in data. A device that has not finished NTP
// synchronisation cannot supply a meaningful value, which is why the caller
// must get the wall clock before the first export.
func (r *Registry) Encode(startUnixNano, nowUnixNano uint64) ([]byte, error) {
	w := r.buf
	w.Reset()

	w.Nested(wire.FieldMetricsDataResourceMetrics, func() {
		w.Nested(wire.FieldResourceMetricsResource, func() {
			for i := range r.resource {
				r.encodeAttr(&r.resource[i], wire.FieldResourceAttributes)
			}
		})
		w.Nested(wire.FieldResourceMetricsScopeMetrics, func() {
			w.Nested(wire.FieldScopeMetricsScope, func() {
				w.String(wire.FieldScopeName, r.scopeName)
				w.String(wire.FieldScopeVersion, r.scopeVersion)
			})
			for _, in := range r.instruments {
				w.Nested(wire.FieldScopeMetricsMetrics, func() {
					r.encodeMetric(in, startUnixNano, nowUnixNano)
				})
			}
		})
	})

	return w.Bytes(), nil
}

// ContentType reports the Content-Type for a binary protobuf body.
func (r *Registry) ContentType() string { return "application/x-protobuf" }

func (r *Registry) encodeMetric(in *Instrument, startNano, nowNano uint64) {
	w := r.buf
	w.String(wire.FieldMetricName, in.Name)
	w.String(wire.FieldMetricDescription, in.Desc)
	w.String(wire.FieldMetricUnit, in.Unit)

	if in.Kind == KindCounter {
		w.Nested(wire.FieldMetricSum, func() {
			w.Nested(wire.FieldSumDataPoints, func() {
				r.encodeDataPoint(in, startNano, nowNano)
			})
			w.Varint(wire.FieldSumAggregationTemporality, wire.AggregationTemporalityCumulative)
			w.Bool(wire.FieldSumIsMonotonic, true)
		})
		return
	}

	w.Nested(wire.FieldMetricGauge, func() {
		w.Nested(wire.FieldGaugeDataPoints, func() {
			// A gauge has no interval, so OTLP wants no start time on it.
			r.encodeDataPoint(in, 0, nowNano)
		})
	})
}

func (r *Registry) encodeDataPoint(in *Instrument, startNano, nowNano uint64) {
	w := r.buf
	w.Fixed64(wire.FieldNDPStartTimeUnixNano, startNano)
	w.Fixed64(wire.FieldNDPTimeUnixNano, nowNano)
	w.Double(wire.FieldNDPAsDouble, in.value)
	for i := range in.Attrs {
		r.encodeAttr(&in.Attrs[i], wire.FieldNDPAttributes)
	}
}

func (r *Registry) encodeAttr(a *Attr, field int) {
	w := r.buf
	w.Nested(field, func() {
		w.String(wire.FieldKeyValueKey, a.Key)
		w.Nested(wire.FieldKeyValueValue, func() {
			// AnyValue is a oneof: an empty string still has to be written,
			// or the value reads as unset instead of empty.
			w.StringAlways(wire.FieldAnyValueStringValue, a.Value)
		})
	})
}
