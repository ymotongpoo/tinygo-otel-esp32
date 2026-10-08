// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

package otlpmini

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	mpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

const (
	testStart = uint64(1_700_000_000_000_000_000)
	testNow   = uint64(1_700_000_010_000_000_000)
)

func newTestRegistry() (*Registry, *Instrument, *Instrument) {
	reg := NewRegistry("test-scope", "1.2.3", []Attr{
		{Key: "service.name", Value: "unit"},
		{Key: "device.id", Value: "dev-1"},
	})
	g := reg.Register(&Instrument{
		Name: "g", Unit: "dBm", Desc: "a gauge", Kind: KindGauge,
	})
	c := reg.Register(&Instrument{
		Name: "c", Unit: "{attempt}", Desc: "a counter", Kind: KindCounter,
		Attrs: []Attr{{Key: "outcome", Value: "success"}},
	})
	return reg, g, c
}

// decode parses the hand-written bytes with the reference protobuf runtime.
// This is the test that matters: the device cannot run protobuf-go, so the
// only way to know the hand encoder is correct is to check its output against
// the official implementation on a host.
// encode runs Encode and fails the test on error, so each case reads as one
// expression.
func encode(t *testing.T, r *Registry, start, now uint64) []byte {
	t.Helper()
	b, err := r.Encode(start, now)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return b
}

func decode(t *testing.T, b []byte) *mpb.MetricsData {
	t.Helper()
	var md mpb.MetricsData
	if err := proto.Unmarshal(b, &md); err != nil {
		t.Fatalf("reference protobuf runtime rejected our bytes: %v", err)
	}
	return &md
}

func TestEncodeMatchesReferenceImplementation(t *testing.T) {
	reg, g, c := newTestRegistry()
	g.Set(-57.5)
	c.Add(3)

	md := decode(t, encode(t, reg, testStart, testNow))

	if got := len(md.ResourceMetrics); got != 1 {
		t.Fatalf("ResourceMetrics = %d, want 1", got)
	}
	rm := md.ResourceMetrics[0]

	if got := len(rm.Resource.Attributes); got != 2 {
		t.Fatalf("resource attributes = %d, want 2", got)
	}
	if k := rm.Resource.Attributes[0].Key; k != "service.name" {
		t.Errorf("resource attr 0 key = %q, want service.name", k)
	}
	if v := rm.Resource.Attributes[0].Value.GetStringValue(); v != "unit" {
		t.Errorf("resource attr 0 value = %q, want unit", v)
	}

	sm := rm.ScopeMetrics[0]
	if sm.Scope.Name != "test-scope" || sm.Scope.Version != "1.2.3" {
		t.Errorf("scope = %q/%q, want test-scope/1.2.3", sm.Scope.Name, sm.Scope.Version)
	}
	if got := len(sm.Metrics); got != 2 {
		t.Fatalf("metrics = %d, want 2", got)
	}

	gm := sm.Metrics[0]
	if gm.Name != "g" || gm.Unit != "dBm" || gm.Description != "a gauge" {
		t.Errorf("gauge descriptor = %q/%q/%q", gm.Name, gm.Unit, gm.Description)
	}
	gauge := gm.GetGauge()
	if gauge == nil {
		t.Fatalf("metric 0 is not a Gauge: %T", gm.Data)
	}
	gp := gauge.DataPoints[0]
	if gp.GetAsDouble() != -57.5 {
		t.Errorf("gauge value = %v, want -57.5", gp.GetAsDouble())
	}
	if gp.TimeUnixNano != testNow {
		t.Errorf("gauge TimeUnixNano = %d, want %d", gp.TimeUnixNano, testNow)
	}
	// A gauge has no interval, so OTLP does not want a start time on it.
	if gp.StartTimeUnixNano != 0 {
		t.Errorf("gauge StartTimeUnixNano = %d, want 0", gp.StartTimeUnixNano)
	}

	cm := sm.Metrics[1]
	sum := cm.GetSum()
	if sum == nil {
		t.Fatalf("metric 1 is not a Sum: %T", cm.Data)
	}
	if !sum.IsMonotonic {
		t.Error("counter IsMonotonic = false, want true")
	}
	if sum.AggregationTemporality != mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Errorf("counter temporality = %v, want cumulative", sum.AggregationTemporality)
	}
	sp := sum.DataPoints[0]
	if sp.GetAsDouble() != 3 {
		t.Errorf("counter value = %v, want 3", sp.GetAsDouble())
	}
	// A cumulative sum without a start time cannot be distinguished from a
	// counter reset by the backend.
	if sp.StartTimeUnixNano != testStart {
		t.Errorf("counter StartTimeUnixNano = %d, want %d", sp.StartTimeUnixNano, testStart)
	}
	if got := len(sp.Attributes); got != 1 {
		t.Fatalf("counter data point attributes = %d, want 1", got)
	}
	if k, v := sp.Attributes[0].Key, sp.Attributes[0].Value.GetStringValue(); k != "outcome" || v != "success" {
		t.Errorf("counter attr = %q=%q, want outcome=success", k, v)
	}
}

// A zero measurement must still be transmitted. Skipping it would make a
// backend read the previous value as current, and for a counter it would hide
// the fact that the device is alive but has sent nothing.
func TestEncodeTransmitsZeroValues(t *testing.T) {
	reg, g, _ := newTestRegistry()
	g.Set(0)

	md := decode(t, encode(t, reg, testStart, testNow))
	dp := md.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0]

	if _, ok := dp.Value.(*mpb.NumberDataPoint_AsDouble); !ok {
		t.Fatalf("zero value was not encoded; Value = %#v", dp.Value)
	}
	if dp.GetAsDouble() != 0 {
		t.Errorf("value = %v, want 0", dp.GetAsDouble())
	}
}

// The nested-length patching in wire.Buffer moves bytes when a message needs
// fewer length bytes than reserved. A payload large enough to need a two-byte
// length exercises that path.
func TestEncodeHandlesMultiByteLengths(t *testing.T) {
	long := ""
	for len(long) < 300 {
		long += "abcdefghij"
	}
	reg := NewRegistry("scope", "1", []Attr{{Key: "long", Value: long}})
	in := reg.Register(&Instrument{Name: "m", Kind: KindGauge})
	in.Set(1)

	md := decode(t, encode(t, reg, testStart, testNow))
	got := md.ResourceMetrics[0].Resource.Attributes[0].Value.GetStringValue()
	if got != long {
		t.Errorf("long attribute round-tripped to %d bytes, want %d", len(got), len(long))
	}
}

func TestEncodeReusesBuffer(t *testing.T) {
	reg, g, _ := newTestRegistry()

	g.Set(1)
	first := encode(t, reg, testStart, testNow)
	firstLen := len(first)
	g.Set(2)
	second := encode(t, reg, testStart, testNow)

	if len(second) != firstLen {
		t.Errorf("payload length changed between encodes: %d then %d", firstLen, len(second))
	}
	md := decode(t, second)
	if v := md.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0].GetAsDouble(); v != 2 {
		t.Errorf("value after second Encode = %v, want 2", v)
	}
}

func TestExportSendsProtobufToPath(t *testing.T) {
	var gotCT, gotPath, gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		io.ReadFull(r.Body, buf)
		gotBody = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg, g, _ := newTestRegistry()
	g.Set(12)
	exp := NewExporter(srv.URL + "/v1/metrics")

	n, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if n != len(gotBody) {
		t.Errorf("reported %d bytes, server received %d", n, len(gotBody))
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/metrics" {
		t.Errorf("path = %s, want /v1/metrics", gotPath)
	}
	// A wrong Content-Type is the most common cause of a 400 during a demo.
	if gotCT != "application/x-protobuf" {
		t.Errorf("Content-Type = %q, want application/x-protobuf", gotCT)
	}
	decode(t, gotBody)
}

func TestExportClassifiesFailures(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		retryable bool
		wantErr   bool
	}{
		{"ok", http.StatusOK, false, false},
		{"bad request is permanent", http.StatusBadRequest, false, true},
		{"too many requests is retryable", http.StatusTooManyRequests, true, true},
		{"unavailable is retryable", http.StatusServiceUnavailable, true, true},
		// OTLP lists 429, 502, 503 and 504 as retryable. A bare 500 is not on
		// that list, so retrying it forever is wrong.
		{"internal error is permanent", http.StatusInternalServerError, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			reg, _, _ := newTestRegistry()
			exp := NewExporter(srv.URL + "/v1/metrics")
			_, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType())

			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			// Retrying a 400 resends bytes the collector already rejected, so
			// the distinction has to hold.
			if got := IsRetryable(err); got != tc.retryable {
				t.Errorf("retryable = %v, want %v (err: %v)", got, tc.retryable, err)
			}
		})
	}
}

func TestExportUnreachableIsRetryable(t *testing.T) {
	reg, _, _ := newTestRegistry()
	// Port 1 on the loopback interface refuses connections.
	exp := NewExporter("http://127.0.0.1:1/v1/metrics")

	if _, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType()); err == nil {
		t.Fatal("Export to an unreachable endpoint succeeded")
	} else if !IsRetryable(err) {
		t.Errorf("a lost connection must be retryable, got %v", err)
	}
}

// An attribute with an empty value must still carry a present stringValue.
//
// proto3 skips zero values, but AnyValue is a oneof: dropping the empty string
// leaves the value unset, so the collector sees an attribute with no type
// rather than an empty one. An empty string is a legitimate attribute value
// (an unset device label, for instance).
func TestEncodeKeepsEmptyAttributeValues(t *testing.T) {
	reg := NewRegistry("scope", "1", []Attr{
		{Key: "device.label", Value: ""},
	})
	in := reg.Register(&Instrument{Name: "m", Kind: KindGauge})
	in.Set(1)

	md := decode(t, encode(t, reg, testStart, testNow))
	attrs := md.ResourceMetrics[0].Resource.Attributes
	if len(attrs) != 1 {
		t.Fatalf("attributes = %d, want 1", len(attrs))
	}
	if attrs[0].Key != "device.label" {
		t.Errorf("key = %q, want device.label", attrs[0].Key)
	}
	if attrs[0].Value == nil {
		t.Fatal("AnyValue is nil; the empty string was dropped entirely")
	}
	if _, ok := attrs[0].Value.Value.(*cpb.AnyValue_StringValue); !ok {
		t.Errorf("AnyValue variant = %T, want AnyValue_StringValue", attrs[0].Value.Value)
	}
	if got := attrs[0].Value.GetStringValue(); got != "" {
		t.Errorf("value = %q, want empty", got)
	}
}
