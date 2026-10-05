//go:build !socket

package otlpmini

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// ErrRetryable reports a transport-level failure that is worth retrying, such
// as a lost connection or a 503 from the collector. A 400 means the payload is
// wrong and resending the same bytes cannot help, so it is not retryable.
var ErrRetryable = errors.New("otlpmini: retryable export failure")

// ErrPartialSuccess reports that the collector accepted the request but
// rejected some data points. See PartialSuccess for the detail.
var ErrPartialSuccess = errors.New("otlpmini: collector rejected some data points")

// Exporter sends OTLP metric payloads to a collector over HTTP.
//
// The endpoint must be plain HTTP. TinyGo's crypto/tls cannot complete a
// handshake on these targets, so the device sends in the clear to a collector
// on the same network, and the collector owns TLS on the way out.
//
// This build exists mainly for comparison: importing net/http costs about
// 310 KB of flash on esp32s3. Build with -tags socket for the hand-written
// client.
type Exporter struct {
	endpoint string
	client   *http.Client
}

// NewExporter creates an Exporter. The endpoint is the full OTLP metrics URL,
// for example http://192.168.1.10:4318/v1/metrics.
func NewExporter(endpoint string) *Exporter {
	return &Exporter{endpoint: endpoint, client: &http.Client{}}
}

// Close releases idle connections.
func (e *Exporter) Close() error {
	e.client.CloseIdleConnections()
	return nil
}

// Export sends an encoded payload and returns the number of bytes sent.
//
// A wrapped ErrRetryable means the caller should keep the measurement and try
// again. A wrapped ErrPartialSuccess means the collector stored some of the
// payload and rejected the rest, which resending cannot fix.
func (e *Exporter) Export(body []byte, contentType string) (int, error) {
	return e.post(e.endpoint, body, contentType)
}

// ExportTo sends a payload to another path on the same collector, such as
// /v1/logs next to /v1/metrics.
func (e *Exporter) ExportTo(path string, body []byte, contentType string) (int, error) {
	host, _ := splitURL(e.endpoint)
	return e.post(host+path, body, contentType)
}

// SiblingPath returns the path for another signal on the same collector:
// /v1/metrics becomes /v1/logs.
func (e *Exporter) SiblingPath(signal string) string {
	_, path := splitURL(e.endpoint)
	return siblingPath(path, signal)
}

// splitURL splits http://host:port/path into "http://host:port" and "/path".
func splitURL(u string) (origin, path string) {
	const scheme = "http://"
	rest := u
	prefix := ""
	if len(rest) >= len(scheme) && rest[:len(scheme)] == scheme {
		prefix, rest = scheme, rest[len(scheme):]
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return prefix + rest[:i], rest[i:]
		}
	}
	return prefix + rest, "/"
}

func (e *Exporter) post(url string, body []byte, contentType string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := e.client.Do(req)
	if err != nil {
		return 0, errors.Join(ErrRetryable, err)
	}
	defer resp.Body.Close()

	// The body must be read even on success: OTLP reports rejected data points
	// in the body of an HTTP 200, and draining is also what allows the
	// connection to be reused.
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil && resp.StatusCode == http.StatusOK {
		return 0, errors.Join(ErrRetryable, readErr)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		if p := parsePartialSuccess(respBody); p != nil {
			return len(body), errors.Join(ErrPartialSuccess, p)
		}
		return len(body), nil
	// OTLP names 429, 502, 503 and 504 as retryable. Other 5xx are not on that
	// list, so they are reported as permanent rather than retried forever.
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusGatewayTimeout:
		return 0, errors.Join(ErrRetryable, errStatus(resp.StatusCode))
	default:
		return 0, errStatus(resp.StatusCode)
	}
}

// IsRetryable reports whether an error from Export is worth retrying.
func IsRetryable(err error) bool { return errors.Is(err, ErrRetryable) }

type errStatus int

func (e errStatus) Error() string {
	return "otlpmini: collector returned HTTP " + strconv.Itoa(int(e))
}
