package otlpjson

import (
	"encoding/json"
	"testing"

	mpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	testStart = uint64(1_700_000_000_000_000_000)
	testNow   = uint64(1_700_000_010_000_000_000)
)

func newTestEncoder() (*Encoder, *Instrument, *Instrument) {
	enc := NewEncoder("test-scope", "1.2.3", []Attr{
		{Key: "service.name", Value: "unit"},
		{Key: "device.id", Value: "dev-1"},
	})
	g := enc.Register(&Instrument{
		Name: "g", Unit: "dBm", Desc: "a gauge", Kind: KindGauge,
	})
	c := enc.Register(&Instrument{
		Name: "c", Unit: "{attempt}", Desc: "a counter", Kind: KindCounter,
		Attrs: []Attr{{Key: "outcome", Value: "success"}},
	})
	return enc, g, c
}

// decode parses our JSON with protojson, the reference implementation of the
// OTLP JSON mapping. This is the test that matters: a collector validates the
// payload against the same schema, so anything protojson rejects would be a
// 400 on the wire.
func decode(t *testing.T, b []byte) *mpb.MetricsData {
	t.Helper()
	var md mpb.MetricsData
	// DiscardUnknown stays false: an unknown or misspelled field must fail
	// here rather than be silently dropped by the collector.
	if err := protojson.Unmarshal(b, &md); err != nil {
		t.Fatalf("protojson rejected our payload: %v\npayload: %s", err, b)
	}
	return &md
}

func TestEncodeMatchesOTLPJSONMapping(t *testing.T) {
	enc, g, c := newTestEncoder()
	g.Set(-57.5)
	c.Add(3)

	b, err := enc.Encode(testStart, testNow)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	md := decode(t, b)

	rm := md.ResourceMetrics[0]
	if got := len(rm.Resource.Attributes); got != 2 {
		t.Fatalf("resource attributes = %d, want 2", got)
	}
	if v := rm.Resource.Attributes[0].Value.GetStringValue(); v != "unit" {
		t.Errorf("resource attr 0 = %q, want unit", v)
	}

	sm := rm.ScopeMetrics[0]
	if sm.Scope.Name != "test-scope" || sm.Scope.Version != "1.2.3" {
		t.Errorf("scope = %q/%q", sm.Scope.Name, sm.Scope.Version)
	}
	if got := len(sm.Metrics); got != 2 {
		t.Fatalf("metrics = %d, want 2", got)
	}

	gm := sm.Metrics[0]
	if gm.Name != "g" || gm.Unit != "dBm" || gm.Description != "a gauge" {
		t.Errorf("gauge descriptor = %q/%q/%q", gm.Name, gm.Unit, gm.Description)
	}
	gp := gm.GetGauge().DataPoints[0]
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

	s := sm.Metrics[1].GetSum()
	if s == nil {
		t.Fatalf("metric 1 is not a Sum")
	}
	if !s.IsMonotonic {
		t.Error("IsMonotonic = false, want true")
	}
	if s.AggregationTemporality != mpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Errorf("temporality = %v, want cumulative", s.AggregationTemporality)
	}
	sp := s.DataPoints[0]
	if sp.GetAsDouble() != 3 {
		t.Errorf("counter value = %v, want 3", sp.GetAsDouble())
	}
	// A cumulative sum without a start time cannot be distinguished from a
	// counter reset by the backend.
	if sp.StartTimeUnixNano != testStart {
		t.Errorf("counter StartTimeUnixNano = %d, want %d", sp.StartTimeUnixNano, testStart)
	}
	if got := len(sp.Attributes); got != 1 {
		t.Fatalf("counter attributes = %d, want 1", got)
	}
}

// Timestamps must be JSON strings. A uint64 nanosecond value does not survive
// a float64, so emitting it as a number silently corrupts the timestamp.
func TestTimestampsAreStrings(t *testing.T) {
	enc, g, _ := newTestEncoder()
	g.Set(1)

	b, err := enc.Encode(testStart, testNow)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	rm := raw["resourceMetrics"].([]any)[0].(map[string]any)
	sm := rm["scopeMetrics"].([]any)[0].(map[string]any)
	dp := sm["metrics"].([]any)[0].(map[string]any)["gauge"].(map[string]any)["dataPoints"].([]any)[0].(map[string]any)

	ts, ok := dp["timeUnixNano"]
	if !ok {
		t.Fatal("timeUnixNano missing")
	}
	if _, isString := ts.(string); !isString {
		t.Errorf("timeUnixNano is %T, want string (a JSON number loses precision)", ts)
	}
	if ts.(string) != "1700000010000000000" {
		t.Errorf("timeUnixNano = %v, want 1700000010000000000", ts)
	}
}

func TestEncodeTransmitsZeroValues(t *testing.T) {
	enc, g, _ := newTestEncoder()
	g.Set(0)

	b, err := enc.Encode(testStart, testNow)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	md := decode(t, b)
	dp := md.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0]
	if _, ok := dp.Value.(*mpb.NumberDataPoint_AsDouble); !ok {
		t.Fatalf("zero value was dropped; Value = %#v", dp.Value)
	}
}

func TestEncodeReusesMessageTree(t *testing.T) {
	enc, g, _ := newTestEncoder()

	g.Set(1)
	if _, err := enc.Encode(testStart, testNow); err != nil {
		t.Fatalf("first Encode: %v", err)
	}
	g.Set(2)
	b, err := enc.Encode(testStart, testNow+1)
	if err != nil {
		t.Fatalf("second Encode: %v", err)
	}

	md := decode(t, b)
	dp := md.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0]
	if dp.GetAsDouble() != 2 {
		t.Errorf("value after second Encode = %v, want 2", dp.GetAsDouble())
	}
	if dp.TimeUnixNano != testNow+1 {
		t.Errorf("timestamp was not updated: %d", dp.TimeUnixNano)
	}
}

// The encoder reuses one buffer for the life of the program. json.Marshal
// would allocate a new one per call, which showed up on the device as 4,816
// bytes of heap churn per export.
func TestEncodeReusesBuffer(t *testing.T) {
	enc, g, _ := newTestEncoder()

	g.Set(1)
	first, err := enc.Encode(testStart, testNow)
	if err != nil {
		t.Fatalf("first Encode: %v", err)
	}
	firstCap := cap(first)

	for i := 0; i < 10; i++ {
		g.Set(float64(i))
		if _, err := enc.Encode(testStart, testNow); err != nil {
			t.Fatalf("Encode %d: %v", i, err)
		}
	}

	last, err := enc.Encode(testStart, testNow)
	if err != nil {
		t.Fatalf("last Encode: %v", err)
	}
	if cap(last) != firstCap {
		t.Errorf("buffer was reallocated: cap %d then %d", firstCap, cap(last))
	}
}

// Encode must not leave the trailing newline that json.Encoder appends, so
// that the reported length matches what the collector parses.
func TestEncodeHasNoTrailingNewline(t *testing.T) {
	enc, g, _ := newTestEncoder()
	g.Set(1)

	b, err := enc.Encode(testStart, testNow)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty payload")
	}
	if b[len(b)-1] == '\n' {
		t.Error("payload ends with a newline")
	}
	if b[len(b)-1] != '}' {
		t.Errorf("payload ends with %q, want '}'", b[len(b)-1])
	}
}

// An empty attribute value must survive as a present, empty stringValue.
// In JSON the field is not omitempty, so this should already hold; the test
// pins it because the protobuf path had exactly this bug.
func TestEncodeKeepsEmptyAttributeValues(t *testing.T) {
	enc := NewEncoder("scope", "1", []Attr{{Key: "device.label", Value: ""}})
	in := enc.Register(&Instrument{Name: "m", Kind: KindGauge})
	in.Set(1)

	b, err := enc.Encode(testStart, testNow)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	md := decode(t, b)
	attrs := md.ResourceMetrics[0].Resource.Attributes
	if len(attrs) != 1 {
		t.Fatalf("attributes = %d, want 1", len(attrs))
	}
	if attrs[0].Value == nil {
		t.Fatal("AnyValue is nil; the empty string was dropped")
	}
	if got := attrs[0].Value.GetStringValue(); got != "" {
		t.Errorf("value = %q, want empty", got)
	}
}
