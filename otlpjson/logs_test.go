// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

package otlpjson

import (
	"strings"
	"testing"

	lpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func decodeLogs(t *testing.T, b []byte) *lpb.LogsData {
	t.Helper()
	var ld lpb.LogsData
	if err := protojson.Unmarshal(b, &ld); err != nil {
		t.Fatalf("protojson rejected our logs payload: %v\npayload: %s", err, b)
	}
	return &ld
}

func TestLogsMatchOTLPJSONMapping(t *testing.T) {
	b := NewLogBuffer(4, "scope", "1.0", []Attr{{Key: "service.name", Value: "unit"}})
	b.Add(testStart, SeverityInfo, "wifi connected", Attr{Key: "net.host.ip", Value: "192.0.2.5"})
	b.Add(testNow, SeverityWarn, "export failed")

	p, err := b.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	ld := decodeLogs(t, p)

	rl := ld.ResourceLogs[0]
	if got := rl.Resource.Attributes[0].GetValue().GetStringValue(); got != "unit" {
		t.Errorf("service.name = %q", got)
	}
	recs := rl.ScopeLogs[0].LogRecords
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	if recs[0].GetBody().GetStringValue() != "wifi connected" {
		t.Errorf("first body = %q", recs[0].GetBody().GetStringValue())
	}
	if recs[0].TimeUnixNano != testStart || recs[1].TimeUnixNano != testNow {
		t.Errorf("timestamps = %d, %d", recs[0].TimeUnixNano, recs[1].TimeUnixNano)
	}
	if recs[1].SeverityNumber != lpb.SeverityNumber_SEVERITY_NUMBER_WARN || recs[1].SeverityText != "WARN" {
		t.Errorf("severity = %v %q", recs[1].SeverityNumber, recs[1].SeverityText)
	}
	if recs[0].Attributes[0].Key != "net.host.ip" {
		t.Errorf("attribute key = %q", recs[0].Attributes[0].Key)
	}
}

// A device that cannot reach its collector keeps producing events. The
// buffer must stay bounded, keep the newest events, and count what it lost.
func TestLogBufferOverwritesOldestAndCountsDrops(t *testing.T) {
	b := NewLogBuffer(3, "s", "", nil)
	for i, m := range []string{"a", "b", "c", "d", "e"} {
		b.Add(uint64(i+1), SeverityInfo, m)
	}
	if b.Len() != 3 {
		t.Fatalf("Len = %d, want 3", b.Len())
	}
	if b.Dropped != 2 {
		t.Errorf("Dropped = %d, want 2", b.Dropped)
	}
	p, _ := b.Encode()
	recs := decodeLogs(t, p).ResourceLogs[0].ScopeLogs[0].LogRecords
	var got []string
	for _, r := range recs {
		got = append(got, r.GetBody().GetStringValue())
	}
	if strings.Join(got, "") != "cde" {
		t.Errorf("held records = %v, want [c d e] oldest first", got)
	}
}

// A failed export must be retryable with the same events, so Encode does not
// consume the buffer; only Clear does.
func TestLogBufferEncodeDoesNotConsume(t *testing.T) {
	b := NewLogBuffer(2, "s", "", nil)
	b.Add(1, SeverityError, "x")
	first, _ := b.Encode()
	firstCopy := string(first)
	second, _ := b.Encode()
	if firstCopy != string(second) {
		t.Error("second Encode differs; Encode consumed the buffer")
	}
	b.Clear()
	if b.Len() != 0 {
		t.Errorf("Len after Clear = %d", b.Len())
	}
	b.Add(2, SeverityInfo, "y")
	p, _ := b.Encode()
	recs := decodeLogs(t, p).ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(recs) != 1 || recs[0].GetBody().GetStringValue() != "y" {
		t.Errorf("after Clear+Add got %d records", len(recs))
	}
}

// Timestamps are uint64 nanoseconds and must be JSON strings, as for metrics.
func TestLogTimestampsAreStrings(t *testing.T) {
	b := NewLogBuffer(1, "s", "", nil)
	b.Add(testNow, SeverityInfo, "m")
	p, _ := b.Encode()
	want := `"timeUnixNano":"` + "1700000010000000000" + `"`
	if !strings.Contains(string(p), want) {
		t.Errorf("payload lacks %s:\n%s", want, p)
	}
}
