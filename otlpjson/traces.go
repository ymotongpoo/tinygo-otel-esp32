// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

package otlpjson

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// Span kinds and status codes from the OTLP trace data model.
const (
	SpanKindInternal = 1
	SpanKindClient   = 3

	StatusUnset = 0
	StatusOK    = 1
	StatusError = 2
)

type spanStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type span struct {
	// OTLP/JSON writes trace and span IDs as hex strings, not base64: this is
	// one of the places where the OTLP JSON mapping departs from protobuf's
	// canonical JSON.
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []keyValue `json:"attributes,omitempty"`
	Status            spanStatus `json:"status"`
}

type scopeSpans struct {
	Scope instrumentationScope `json:"scope"`
	Spans []span               `json:"spans"`
}

type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}

type tracesData struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

// TraceID and SpanID are the raw 16- and 8-byte identifiers.
type (
	TraceID [16]byte
	SpanID  [8]byte
)

// Span is one finished span, ready to be buffered.
type Span struct {
	TraceID   TraceID
	SpanID    SpanID
	Parent    SpanID // zero for a root span
	Name      string
	Kind      int
	StartNano uint64
	EndNano   uint64
	Status    int
	StatusMsg string
	Attrs     []Attr
}

// SpanBuffer holds finished spans until they can be exported as OTLP traces.
//
// Like LogBuffer, the capacity is fixed: while the collector is unreachable the
// oldest spans are overwritten and counted in Dropped.
type SpanBuffer struct {
	spans []span
	head  int
	n     int

	// Dropped counts spans overwritten before they were exported.
	Dropped int

	scopeName    string
	scopeVersion string
	resourceAttr []keyValue

	data tracesData
	out  []span
	buf  *bytes.Buffer
	enc  *json.Encoder
}

// NewSpanBuffer creates a SpanBuffer that holds at most capacity spans.
func NewSpanBuffer(capacity int, scopeName, scopeVersion string, resourceAttrs []Attr) *SpanBuffer {
	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	return &SpanBuffer{
		spans:        make([]span, capacity),
		out:          make([]span, 0, capacity),
		scopeName:    scopeName,
		scopeVersion: scopeVersion,
		resourceAttr: toKeyValues(resourceAttrs),
		buf:          buf,
		enc:          json.NewEncoder(buf),
	}
}

// Len reports the number of spans waiting to be exported.
func (b *SpanBuffer) Len() int { return b.n }

// Add buffers a finished span.
func (b *SpanBuffer) Add(s Span) {
	if len(b.spans) == 0 {
		b.Dropped++
		return
	}
	var i int
	if b.n < len(b.spans) {
		i = (b.head + b.n) % len(b.spans)
		b.n++
	} else {
		i = b.head
		b.head = (b.head + 1) % len(b.spans)
		b.Dropped++
	}
	var parent string
	if s.Parent != (SpanID{}) {
		parent = hex.EncodeToString(s.Parent[:])
	}
	b.spans[i] = span{
		TraceID:           hex.EncodeToString(s.TraceID[:]),
		SpanID:            hex.EncodeToString(s.SpanID[:]),
		ParentSpanID:      parent,
		Name:              s.Name,
		Kind:              s.Kind,
		StartTimeUnixNano: strconv.FormatUint(s.StartNano, 10),
		EndTimeUnixNano:   strconv.FormatUint(s.EndNano, 10),
		Attributes:        toKeyValues(s.Attrs),
		Status:            spanStatus{Code: s.Status, Message: s.StatusMsg},
	}
}

// Encode returns the held spans as an OTLP/JSON traces payload. The spans stay
// in the buffer until Clear is called.
func (b *SpanBuffer) Encode() ([]byte, error) {
	b.out = b.out[:0]
	for k := 0; k < b.n; k++ {
		b.out = append(b.out, b.spans[(b.head+k)%len(b.spans)])
	}
	b.data = tracesData{ResourceSpans: []resourceSpans{{
		Resource: resource{Attributes: b.resourceAttr},
		ScopeSpans: []scopeSpans{{
			Scope: instrumentationScope{Name: b.scopeName, Version: b.scopeVersion},
			Spans: b.out,
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

// Clear removes every held span. Call it after a successful export.
func (b *SpanBuffer) Clear() {
	for k := range b.spans {
		b.spans[k] = span{}
	}
	b.head, b.n = 0, 0
}
