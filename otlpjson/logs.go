package otlpjson

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Severity numbers from the OTLP log data model.
const (
	SeverityInfo  = 9
	SeverityWarn  = 13
	SeverityError = 17
)

func severityText(n int) string {
	switch {
	case n >= SeverityError:
		return "ERROR"
	case n >= SeverityWarn:
		return "WARN"
	default:
		return "INFO"
	}
}

type logRecord struct {
	TimeUnixNano         string     `json:"timeUnixNano"`
	ObservedTimeUnixNano string     `json:"observedTimeUnixNano"`
	SeverityNumber       int        `json:"severityNumber"`
	SeverityText         string     `json:"severityText"`
	Body                 anyValue   `json:"body"`
	Attributes           []keyValue `json:"attributes,omitempty"`
}

type scopeLogs struct {
	Scope      instrumentationScope `json:"scope"`
	LogRecords []logRecord          `json:"logRecords"`
}

type resourceLogs struct {
	Resource  resource    `json:"resource"`
	ScopeLogs []scopeLogs `json:"scopeLogs"`
}

type logsData struct {
	ResourceLogs []resourceLogs `json:"resourceLogs"`
}

// LogBuffer holds device events until they can be exported as OTLP logs.
//
// The capacity is fixed when the buffer is created and never grows. A device
// that cannot reach its collector keeps producing events, and an unbounded
// queue would turn a network outage into an out-of-memory reset. When the
// buffer is full the oldest record is overwritten and counted in Dropped, so
// the loss is visible instead of silent.
type LogBuffer struct {
	records []logRecord
	head    int // index of the oldest record
	n       int // number of records held

	// Dropped counts records overwritten before they were exported.
	Dropped int

	scopeName    string
	scopeVersion string
	resourceAttr []keyValue

	data logsData
	buf  *bytes.Buffer
	enc  *json.Encoder
}

// NewLogBuffer creates a LogBuffer that holds at most capacity records.
func NewLogBuffer(capacity int, scopeName, scopeVersion string, resourceAttrs []Attr) *LogBuffer {
	buf := bytes.NewBuffer(make([]byte, 0, 2048))
	return &LogBuffer{
		records:      make([]logRecord, capacity),
		scopeName:    scopeName,
		scopeVersion: scopeVersion,
		resourceAttr: toKeyValues(resourceAttrs),
		buf:          buf,
		enc:          json.NewEncoder(buf),
	}
}

// Len reports the number of records waiting to be exported.
func (b *LogBuffer) Len() int { return b.n }

// Add records an event that happened at unixNano.
func (b *LogBuffer) Add(unixNano uint64, severity int, msg string, attrs ...Attr) {
	if len(b.records) == 0 {
		b.Dropped++
		return
	}
	var i int
	if b.n < len(b.records) {
		i = (b.head + b.n) % len(b.records)
		b.n++
	} else {
		// Full: overwrite the oldest.
		i = b.head
		b.head = (b.head + 1) % len(b.records)
		b.Dropped++
	}
	ts := strconv.FormatUint(unixNano, 10)
	b.records[i] = logRecord{
		TimeUnixNano:         ts,
		ObservedTimeUnixNano: ts,
		SeverityNumber:       severity,
		SeverityText:         severityText(severity),
		Body:                 anyValue{StringValue: msg},
		Attributes:           toKeyValues(attrs),
	}
}

// Encode returns the held records as an OTLP/JSON logs payload, oldest first.
// The records stay in the buffer until Clear is called, so a failed export
// can be retried with the same events.
func (b *LogBuffer) Encode() ([]byte, error) {
	out := make([]logRecord, b.n)
	for k := 0; k < b.n; k++ {
		out[k] = b.records[(b.head+k)%len(b.records)]
	}
	b.data = logsData{ResourceLogs: []resourceLogs{{
		Resource: resource{Attributes: b.resourceAttr},
		ScopeLogs: []scopeLogs{{
			Scope:      instrumentationScope{Name: b.scopeName, Version: b.scopeVersion},
			LogRecords: out,
		}},
	}}}
	b.buf.Reset()
	if err := b.enc.Encode(&b.data); err != nil {
		return nil, err
	}
	p := b.buf.Bytes()
	if n := len(p); n > 0 && p[n-1] == '\n' {
		p = p[:n-1]
	}
	return p, nil
}

// Clear removes every held record. Call it after a successful export.
func (b *LogBuffer) Clear() {
	for k := range b.records {
		b.records[k] = logRecord{}
	}
	b.head, b.n = 0, 0
}
