//go:build tinygo

package main

import (
	"crypto/rand"
	"time"

	"github.com/ymotongpoo/tinygo-otel-esp32/otlpjson"
)

// Each export cycle is one trace: a root span for the cycle and child spans
// for reading the sensors, encoding, and the HTTP request. On a server this
// would be the work of an SDK and an instrumentation library; here it is a
// handful of timestamps and IDs.
//
// Trace and span IDs come from crypto/rand, which TinyGo backs with the
// ESP32-S3 hardware RNG (machine.GetRNG). The RNG draws on RF noise, so IDs
// are generated only after WiFi is up.
//
// Like the logs, spans are always JSON and are held in a fixed-size buffer.
// A cycle's spans are sent with the next cycle, after its metrics export
// succeeds, because the export span cannot be finished until the export is.
const spanCapacity = 16

type tracer struct {
	buf  *otlpjson.SpanBuffer
	path string
}

func newTracer(resourceAttrs []attr, path string) *tracer {
	ra := make([]otlpjson.Attr, len(resourceAttrs))
	for i, a := range resourceAttrs {
		ra[i] = otlpjson.Attr{Key: a.Key, Value: a.Value}
	}
	return &tracer{
		buf:  otlpjson.NewSpanBuffer(spanCapacity, "tinygo-otel-esp32", "0.1.0", ra),
		path: path,
	}
}

func newTraceID() (id otlpjson.TraceID) {
	rand.Read(id[:])
	return id
}

func newSpanID() (id otlpjson.SpanID) {
	rand.Read(id[:])
	return id
}

func nowNano() uint64 { return uint64(time.Now().UnixNano()) }

// spanRec is an in-progress span.
type spanRec struct {
	s otlpjson.Span
}

func (t *tracer) start(trace otlpjson.TraceID, parent otlpjson.SpanID, name string, kind int) *spanRec {
	return &spanRec{s: otlpjson.Span{
		TraceID: trace, SpanID: newSpanID(), Parent: parent,
		Name: name, Kind: kind, StartNano: nowNano(),
	}}
}

func (t *tracer) end(r *spanRec, err error, attrs ...otlpjson.Attr) {
	r.s.EndNano = nowNano()
	r.s.Attrs = attrs
	if err != nil {
		r.s.Status = otlpjson.StatusError
		r.s.StatusMsg = err.Error()
	}
	t.buf.Add(r.s)
}

// flush sends the held spans. They stay in the buffer if sending fails.
func (t *tracer) flush(exp exporter) {
	if t.buf.Len() == 0 {
		return
	}
	body, err := t.buf.Encode()
	if err != nil {
		println("trace encode failed:", err.Error())
		return
	}
	if _, err := exp.ExportTo(t.path, body, "application/json"); err != nil {
		println("trace export failed:", err.Error())
		return
	}
	t.buf.Clear()
}
