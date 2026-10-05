package otlpmini

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A 400 means the collector rejected these bytes. Resending them produces the
// same rejection, so the request must be sent exactly once.
func TestExportDoesNotResendPermanentFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"bad request", http.StatusBadRequest},
		{"not found", http.StatusNotFound},
		{"payload too large", http.StatusRequestEntityTooLarge},
		// 500 is not on the OTLP retryable list, so it must not be resent
		// either.
		{"internal server error", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.Add(1)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			reg, _, _ := newTestRegistry()
			exp := NewExporter(srv.URL + "/v1/metrics")
			defer exp.Close()

			// Warm the connection so the retry path is live for the next call.
			exp.Export(encode(t, reg, testStart, testNow), reg.ContentType())
			got.Store(0)

			if _, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType()); err == nil {
				t.Fatalf("HTTP %d reported success", tc.status)
			}
			if n := got.Load(); n != 1 {
				t.Errorf("payload was sent %d times, want 1", n)
			}
		})
	}
}

// OTLP names 429, 502, 503 and 504 as retryable. Other 5xx are not on that
// list.
func TestExportClassifiesStatusesPerOTLPSpec(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusInternalServerError, false},
		{http.StatusNotImplemented, false},
		{http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			reg, _, _ := newTestRegistry()
			exp := NewExporter(srv.URL + "/v1/metrics")
			defer exp.Close()

			_, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType())
			if err == nil {
				t.Fatalf("HTTP %d reported success", tc.status)
			}
			if got := IsRetryable(err); got != tc.retryable {
				t.Errorf("HTTP %d retryable = %v, want %v", tc.status, got, tc.retryable)
			}
		})
	}
}

// An HTTP 200 whose body reports rejected points is data loss, not success.
func TestExportReportsPartialSuccess(t *testing.T) {
	body := `{"partialSuccess":{"rejectedDataPoints":"8","errorMessage":"stale points"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	reg, _, _ := newTestRegistry()
	exp := NewExporter(srv.URL + "/v1/metrics")
	defer exp.Close()

	_, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType())
	if err == nil {
		t.Fatal("a response rejecting 8 data points was reported as success")
	}
	if !errors.Is(err, ErrPartialSuccess) {
		t.Errorf("err = %v, want it to wrap ErrPartialSuccess", err)
	}
	// Retrying cannot fix a rejection of these same points.
	if IsRetryable(err) {
		t.Error("partial success was marked retryable")
	}
	var p *PartialSuccess
	if !errors.As(err, &p) {
		t.Fatalf("err does not carry *PartialSuccess: %v", err)
	}
	if p.RejectedDataPoints != 8 {
		t.Errorf("RejectedDataPoints = %d, want 8", p.RejectedDataPoints)
	}
	if p.ErrorMessage != "stale points" {
		t.Errorf("ErrorMessage = %q, want %q", p.ErrorMessage, "stale points")
	}
}

// A healthy collector returns an empty body or an empty partialSuccess. Those
// must stay successes, or every export would look like a failure.
func TestExportTreatsEmptyPartialSuccessAsSuccess(t *testing.T) {
	for _, body := range []string{"", "{}", `{"partialSuccess":{}}`,
		`{"partialSuccess":{"rejectedDataPoints":"0"}}`} {
		t.Run(fmt.Sprintf("%q", body), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(body))
			}))
			defer srv.Close()

			reg, _, _ := newTestRegistry()
			exp := NewExporter(srv.URL + "/v1/metrics")
			defer exp.Close()

			if _, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType()); err != nil {
				t.Errorf("body %q reported failure: %v", body, err)
			}
		})
	}
}

// The response encoding mirrors the request encoding, so the protobuf form
// must be understood too.
func TestParsePartialSuccessProtobuf(t *testing.T) {
	// ExportMetricsServiceResponse{partial_success:{rejected_data_points:5,
	// error_message:"nope"}} encoded by hand.
	inner := []byte{
		0x08, 0x05, // field 1 varint = 5
		0x12, 0x04, 'n', 'o', 'p', 'e', // field 2 string = "nope"
	}
	msg := append([]byte{0x0a, byte(len(inner))}, inner...)

	p := parsePartialSuccess(msg)
	if p == nil {
		t.Fatal("protobuf partialSuccess was not detected")
	}
	if p.RejectedDataPoints != 5 {
		t.Errorf("RejectedDataPoints = %d, want 5", p.RejectedDataPoints)
	}
	if p.ErrorMessage != "nope" {
		t.Errorf("ErrorMessage = %q, want nope", p.ErrorMessage)
	}

	// An all-zero partial_success is success.
	if got := parsePartialSuccess([]byte{0x0a, 0x00}); got != nil {
		t.Errorf("empty protobuf partialSuccess returned %v, want nil", got)
	}
}

// rejectedDataPoints is an int64; OTLP JSON allows a string or a number.
func TestParsePartialSuccessJSONAcceptsBothNumberForms(t *testing.T) {
	for _, body := range []string{
		`{"partialSuccess":{"rejectedDataPoints":"7"}}`,
		`{"partialSuccess":{"rejectedDataPoints":7}}`,
	} {
		p := parsePartialSuccess([]byte(body))
		if p == nil {
			t.Fatalf("%s was not detected", body)
		}
		if p.RejectedDataPoints != 7 {
			t.Errorf("%s gave %d, want 7", body, p.RejectedDataPoints)
		}
	}
}
