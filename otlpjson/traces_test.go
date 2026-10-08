// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

package otlpjson

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	tpb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// decodeTraces checks the structure with protojson.
//
// protojson is not a valid reference for the ID fields. The OTLP JSON
// encoding writes traceId, spanId and parentSpanId as hex, a documented
// exception to protobuf's canonical JSON, while protojson follows the
// canonical mapping and reads them as base64. It accepts the hex strings
// (they are valid base64 alphabet) but decodes them to the wrong bytes. So the
// IDs are rewritten to base64 here before decoding, and the hex form on the
// wire is pinned separately by TestTraceIDsAreHex. The collector itself parses
// hex; that is checked end to end against a running collector, not here.
func decodeTraces(t *testing.T, b []byte) *tpb.TracesData {
	t.Helper()
	b = hexIDsToBase64(t, b)
	var td tpb.TracesData
	if err := protojson.Unmarshal(b, &td); err != nil {
		t.Fatalf("protojson rejected our traces payload: %v\npayload: %s", err, b)
	}
	return &td
}

var (
	tid  = TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	root = SpanID{0, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
	kid  = SpanID{1, 2, 3, 4, 5, 6, 7, 8}
)

func TestTracesMatchOTLPJSONMapping(t *testing.T) {
	b := NewSpanBuffer(4, "scope", "1.0", []Attr{{Key: "service.name", Value: "unit"}})
	b.Add(Span{TraceID: tid, SpanID: root, Name: "cycle", Kind: SpanKindInternal,
		StartNano: testStart, EndNano: testNow, Status: StatusOK})
	b.Add(Span{TraceID: tid, SpanID: kid, Parent: root, Name: "export", Kind: SpanKindClient,
		StartNano: testStart + 1, EndNano: testNow - 1, Status: StatusError, StatusMsg: "boom",
		Attrs: []Attr{{Key: "server.address", Value: "192.0.2.1"}}})

	p, err := b.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	spans := decodeTraces(t, p).ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("spans = %d", len(spans))
	}
	if !bytes.Equal(spans[0].TraceId, tid[:]) || !bytes.Equal(spans[0].SpanId, root[:]) {
		t.Errorf("root IDs = %x %x", spans[0].TraceId, spans[0].SpanId)
	}
	if len(spans[0].ParentSpanId) != 0 {
		t.Errorf("root has parent %x", spans[0].ParentSpanId)
	}
	if !bytes.Equal(spans[1].ParentSpanId, root[:]) {
		t.Errorf("child parent = %x, want %x", spans[1].ParentSpanId, root[:])
	}
	if spans[1].Kind != tpb.Span_SPAN_KIND_CLIENT {
		t.Errorf("kind = %v", spans[1].Kind)
	}
	if spans[1].Status.GetCode() != tpb.Status_STATUS_CODE_ERROR || spans[1].Status.GetMessage() != "boom" {
		t.Errorf("status = %v", spans[1].Status)
	}
	if spans[0].StartTimeUnixNano != testStart || spans[0].EndTimeUnixNano != testNow {
		t.Errorf("times = %d %d", spans[0].StartTimeUnixNano, spans[0].EndTimeUnixNano)
	}
}

// OTLP/JSON encodes IDs as hex, unlike protobuf's canonical JSON (base64).
// The collector parses hex; base64 here would be read as garbage hex or
// rejected, so the raw form on the wire is pinned directly.
func TestTraceIDsAreHex(t *testing.T) {
	b := NewSpanBuffer(1, "s", "", nil)
	b.Add(Span{TraceID: tid, SpanID: root, Name: "x", Kind: SpanKindInternal, StartNano: 1, EndNano: 2})
	p, _ := b.Encode()
	if !strings.Contains(string(p), `"traceId":"4bf92f3577b34da6a3ce929d0e0e4736"`) {
		t.Errorf("trace ID not hex:\n%s", p)
	}
	if !strings.Contains(string(p), `"spanId":"00f067aa0ba902b7"`) {
		t.Errorf("span ID not hex:\n%s", p)
	}
}

func TestSpanBufferBounded(t *testing.T) {
	b := NewSpanBuffer(2, "s", "", nil)
	for i := 0; i < 5; i++ {
		b.Add(Span{TraceID: tid, SpanID: SpanID{byte(i + 1)}, Name: "s", StartNano: 1, EndNano: 2})
	}
	if b.Len() != 2 || b.Dropped != 3 {
		t.Errorf("Len=%d Dropped=%d, want 2 and 3", b.Len(), b.Dropped)
	}
	p, _ := b.Encode()
	spans := decodeTraces(t, p).ResourceSpans[0].ScopeSpans[0].Spans
	if spans[0].SpanId[0] != 4 || spans[1].SpanId[0] != 5 {
		t.Errorf("kept %x %x, want the newest two", spans[0].SpanId, spans[1].SpanId)
	}
	b.Clear()
	if b.Len() != 0 {
		t.Error("Clear left spans")
	}
}

var idField = regexp.MustCompile(`"(traceId|spanId|parentSpanId)":"([0-9a-f]*)"`)

func hexIDsToBase64(t *testing.T, b []byte) []byte {
	t.Helper()
	return idField.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := idField.FindSubmatch(m)
		raw, err := hex.DecodeString(string(sub[2]))
		if err != nil {
			t.Fatalf("%s is not hex: %q", sub[1], sub[2])
		}
		return []byte(`"` + string(sub[1]) + `":"` + base64.StdEncoding.EncodeToString(raw) + `"`)
	})
}
