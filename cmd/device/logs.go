//go:build tinygo

package main

import (
	"time"

	"github.com/ymotongpoo/tinygo-otel-esp32/otlpjson"
)

// Device events are exported as OTLP logs. They are always JSON, whichever
// encoding the metrics use: OTLP/HTTP lets every request choose its own
// Content-Type, and a hand-written protobuf encoder for the logs data model
// would add code without changing what the collector receives.
//
// The buffer is fixed-size. While the collector is unreachable, events keep
// arriving; they are held and sent after the next successful metrics export.
// If the outage outlasts the buffer, the oldest events are overwritten and
// counted, so the loss shows up in the device.logs.dropped metric.
const logCapacity = 16

type eventLog struct {
	buf  *otlpjson.LogBuffer
	path string
}

func newEventLog(resourceAttrs []attr, path string) *eventLog {
	ra := make([]otlpjson.Attr, len(resourceAttrs))
	for i, a := range resourceAttrs {
		ra[i] = otlpjson.Attr{Key: a.Key, Value: a.Value}
	}
	return &eventLog{
		buf:  otlpjson.NewLogBuffer(logCapacity, "tinygo-otel-esp32", "0.1.0", ra),
		path: path,
	}
}

func (l *eventLog) add(severity int, msg string, attrs ...otlpjson.Attr) {
	l.buf.Add(uint64(time.Now().UnixNano()), severity, msg, attrs...)
	println("log:", msg)
}

// flush sends the held events. They stay in the buffer if sending fails.
func (l *eventLog) flush(exp exporter) {
	if l.buf.Len() == 0 {
		return
	}
	body, err := l.buf.Encode()
	if err != nil {
		println("log encode failed:", err.Error())
		return
	}
	if _, err := exp.ExportTo(l.path, body, "application/json"); err != nil {
		println("log export failed:", err.Error())
		return
	}
	l.buf.Clear()
}
