// Package otlpjson encodes OTLP metrics payloads as JSON using encoding/json.
//
// OTLP/HTTP specifies two encodings, binary protobuf and JSON, and a collector
// accepts either. The JSON encoding is therefore not a workaround: it is the
// protocol, and reaching it needs nothing but the standard library.
//
// This matters on TinyGo because the two obvious ways to speak OTLP both fail:
// go.opentelemetry.io/otel/sdk/metric does not cross-compile, and
// google.golang.org/protobuf compiles but panics at run time on the first
// Marshal. encoding/json has neither problem, so this is the shortest correct
// path from a microcontroller to an OpenTelemetry pipeline.
//
// The field names follow the OTLP JSON mapping, which is protobuf's canonical
// JSON: lowerCamelCase field names, and 64-bit integers as strings because
// JSON numbers cannot hold them exactly.
package otlpjson

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// aggregationTemporalityCumulative is AGGREGATION_TEMPORALITY_CUMULATIVE.
const aggregationTemporalityCumulative = 2

type anyValue struct {
	StringValue string `json:"stringValue"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type numberDataPoint struct {
	// Timestamps are strings: they are uint64 nanoseconds, and a JSON number
	// is a float64, which cannot represent them exactly.
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string     `json:"timeUnixNano"`
	AsDouble          float64    `json:"asDouble"`
	Attributes        []keyValue `json:"attributes,omitempty"`
}

type gauge struct {
	DataPoints []numberDataPoint `json:"dataPoints"`
}

type sum struct {
	DataPoints             []numberDataPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

type metric struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Gauge       *gauge `json:"gauge,omitempty"`
	Sum         *sum   `json:"sum,omitempty"`
}

type instrumentationScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type scopeMetrics struct {
	Scope   instrumentationScope `json:"scope"`
	Metrics []metric             `json:"metrics"`
}

type resource struct {
	Attributes []keyValue `json:"attributes,omitempty"`
}

type resourceMetrics struct {
	Resource     resource       `json:"resource"`
	ScopeMetrics []scopeMetrics `json:"scopeMetrics"`
}

type metricsData struct {
	ResourceMetrics []resourceMetrics `json:"resourceMetrics"`
}

// Attr is a string-valued attribute.
type Attr struct {
	Key   string
	Value string
}

// Kind selects the OTLP data type used to encode an instrument.
type Kind uint8

const (
	// KindGauge encodes a value that can go up and down.
	KindGauge Kind = iota
	// KindCounter encodes a monotonically increasing cumulative total.
	KindCounter
)

// Instrument is one metric stream.
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

// Encoder builds OTLP/JSON payloads for one device.
//
// The message tree is built once and reused: Encode overwrites the timestamps
// and values in place, so the steady-state path allocates only what
// json.Marshal needs for its output.
type Encoder struct {
	instruments []*Instrument
	data        metricsData
	built       bool

	scopeName    string
	scopeVersion string
	resourceAttr []Attr

	// json.Marshal ends with append([]byte(nil), ...), so it allocates a new
	// buffer on every call. json.Encoder writes into an io.Writer instead,
	// which lets one buffer be reused for the life of the program. Measured
	// on M5Stack CoreS3: this removes 4,816 bytes of heap churn per export.
	buf *bytes.Buffer
	enc *json.Encoder
}

// NewEncoder creates an Encoder for a single device.
//
// The resource attributes are the ones a microcontroller can know about
// itself. There is no automatic detection because there is no operating system
// to ask.
func NewEncoder(scopeName, scopeVersion string, resourceAttrs []Attr) *Encoder {
	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	enc := json.NewEncoder(buf)
	return &Encoder{
		scopeName:    scopeName,
		scopeVersion: scopeVersion,
		resourceAttr: resourceAttrs,
		buf:          buf,
		enc:          enc,
	}
}

// Register adds an instrument and returns it. Register must not be called
// after the first Encode.
func (e *Encoder) Register(in *Instrument) *Instrument {
	e.instruments = append(e.instruments, in)
	e.built = false
	return in
}

func toKeyValues(attrs []Attr) []keyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]keyValue, len(attrs))
	for i, a := range attrs {
		out[i] = keyValue{Key: a.Key, Value: anyValue{StringValue: a.Value}}
	}
	return out
}

func (e *Encoder) build() {
	metrics := make([]metric, len(e.instruments))
	for i, in := range e.instruments {
		m := metric{Name: in.Name, Description: in.Desc, Unit: in.Unit}
		point := numberDataPoint{Attributes: toKeyValues(in.Attrs)}
		if in.Kind == KindCounter {
			m.Sum = &sum{
				DataPoints:             []numberDataPoint{point},
				AggregationTemporality: aggregationTemporalityCumulative,
				IsMonotonic:            true,
			}
		} else {
			m.Gauge = &gauge{DataPoints: []numberDataPoint{point}}
		}
		metrics[i] = m
	}
	e.data = metricsData{ResourceMetrics: []resourceMetrics{{
		Resource: resource{Attributes: toKeyValues(e.resourceAttr)},
		ScopeMetrics: []scopeMetrics{{
			Scope:   instrumentationScope{Name: e.scopeName, Version: e.scopeVersion},
			Metrics: metrics,
		}},
	}}}
	e.built = true
}

// Encode returns the OTLP/JSON payload for the current instrument values.
//
// startUnixNano is the time the firmware started measuring. OTLP requires it
// on cumulative sums so that a backend can tell a counter reset from a gap in
// data, which is why the caller must obtain the wall clock before the first
// export.
func (e *Encoder) Encode(startUnixNano, nowUnixNano uint64) ([]byte, error) {
	if !e.built {
		e.build()
	}
	now := strconv.FormatUint(nowUnixNano, 10)
	start := strconv.FormatUint(startUnixNano, 10)

	ms := e.data.ResourceMetrics[0].ScopeMetrics[0].Metrics
	for i, in := range e.instruments {
		var dp *numberDataPoint
		if in.Kind == KindCounter {
			dp = &ms[i].Sum.DataPoints[0]
			dp.StartTimeUnixNano = start
		} else {
			// A gauge has no interval, so OTLP wants no start time on it.
			dp = &ms[i].Gauge.DataPoints[0]
		}
		dp.TimeUnixNano = now
		dp.AsDouble = in.value
	}
	e.buf.Reset()
	if err := e.enc.Encode(&e.data); err != nil {
		return nil, err
	}
	// json.Encoder.Encode appends a newline. It is harmless in a request body
	// but is not part of the payload, so it is trimmed to keep the reported
	// byte count equal to what the collector parses.
	b := e.buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b, nil
}

// ContentType reports the Content-Type for an OTLP/JSON body.
func (e *Encoder) ContentType() string { return "application/json" }
