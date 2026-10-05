//go:build socket

package otlpmini

import (
	"errors"
	"net"
	"strconv"
	"time"
)

// ErrRetryable reports a transport-level failure that is worth retrying, such
// as a lost connection or a 503 from the collector. A 400 means the payload is
// wrong and resending the same bytes cannot help, so it is not retryable.
var ErrRetryable = errors.New("otlpmini: retryable export failure")

// ErrPartialSuccess reports that the collector accepted the request but
// rejected some data points. See PartialSuccess for the detail.
var ErrPartialSuccess = errors.New("otlpmini: collector rejected some data points")

// Exporter sends OTLP metric payloads over a TCP socket, writing the HTTP
// request and parsing the response by hand.
//
// This build avoids net/http. Importing it costs about 310 KB of flash on
// esp32s3, largely for crypto/tls, crypto/x509, encoding/asn1 and math/big,
// none of which a plain-HTTP client uses. TinyGo's crypto/tls cannot complete
// a handshake on this target anyway.
//
// Build with -tags socket.
type Exporter struct {
	host string // host:port
	path string

	// conn is kept open across exports. Each dial allocates fresh TCP buffers
	// in the lneto stack (TxBuf 4096 + RxBuf 1024 + queues = 8,272 bytes
	// measured on M5Stack CoreS3), so dialing per export made the heap
	// sawtooth and forced a GC every ~21 exports.
	conn net.Conn

	req []byte
	// resp accumulates the response headers. HTTP has no header size limit, so
	// a cap is required; exceeding it is reported rather than misparsed.
	resp    []byte
	discard []byte
}

const (
	socketTimeout = 10 * time.Second
	// maxHeaderBytes caps the status line plus headers. Collector responses are
	// a few hundred bytes; 2 KB leaves room without risking the heap.
	maxHeaderBytes = 2048
)

// NewExporter creates an Exporter. The endpoint must be a plain http:// URL,
// for example http://192.168.1.10:4318/v1/metrics.
func NewExporter(endpoint string) *Exporter {
	host, path := splitEndpoint(endpoint)
	return &Exporter{
		host:    host,
		path:    path,
		req:     make([]byte, 0, 4096),
		resp:    make([]byte, 0, maxHeaderBytes),
		discard: make([]byte, 512),
	}
}

// Export sends an encoded payload and returns the number of body bytes sent.
//
// Retries happen only when the transport failed, which for a kept-open
// connection is indistinguishable from a real fault until a write or read
// fails. A response the collector actually produced is never retried here: a
// 400 would be rejected identically, and a 503 is the caller's decision via
// IsRetryable.
func (e *Exporter) Export(body []byte, contentType string) (int, error) {
	return e.ExportTo(e.path, body, contentType)
}

// ExportTo sends a payload to another path on the same collector, such as
// /v1/logs next to /v1/metrics. It shares the kept-open connection with
// Export, so a second signal costs no extra dial and no extra TCP buffers.
func (e *Exporter) ExportTo(path string, body []byte, contentType string) (int, error) {
	reused := e.conn != nil
	n, err := e.export(path, body, contentType)
	if err == nil || !reused {
		return n, err
	}
	// Only a transport failure justifies resending. errStatus and
	// PartialSuccess mean the collector replied, so the request did arrive.
	var status errStatus
	var partial *PartialSuccess
	if errors.As(err, &status) || errors.As(err, &partial) {
		return n, err
	}
	e.closeConn()
	return e.export(path, body, contentType)
}

// Close releases the kept-open connection.
func (e *Exporter) Close() error {
	if e.conn == nil {
		return nil
	}
	err := e.conn.Close()
	e.conn = nil
	return err
}

func (e *Exporter) closeConn() {
	if e.conn != nil {
		e.conn.Close()
		e.conn = nil
	}
}

// SiblingPath returns the path for another signal on the same collector:
// /v1/metrics becomes /v1/logs.
func (e *Exporter) SiblingPath(signal string) string { return siblingPath(e.path, signal) }

func (e *Exporter) export(path string, body []byte, contentType string) (int, error) {
	r := e.req[:0]
	r = append(r, "POST "...)
	r = append(r, path...)
	r = append(r, " HTTP/1.1\r\nHost: "...)
	r = append(r, e.host...)
	r = append(r, "\r\nContent-Type: "...)
	r = append(r, contentType...)
	r = append(r, "\r\nContent-Length: "...)
	r = strconv.AppendInt(r, int64(len(body)), 10)
	r = append(r, "\r\n\r\n"...)
	r = append(r, body...)
	e.req = r

	if e.conn == nil {
		conn, err := net.DialTimeout("tcp", e.host, socketTimeout)
		if err != nil {
			return 0, errors.Join(ErrRetryable, err)
		}
		e.conn = conn
	}
	e.conn.SetDeadline(time.Now().Add(socketTimeout))

	if _, err := e.conn.Write(r); err != nil {
		return 0, errors.Join(ErrRetryable, err)
	}

	resp, err := e.readResponse()
	if err != nil {
		return 0, err
	}
	if resp.closeRequested {
		e.closeConn()
	}

	switch {
	case resp.status == 200:
		// HTTP 200 does not mean every point was stored. OTLP reports rejected
		// points in the body, and treating that as success hides data loss.
		if p := parsePartialSuccess(resp.body); p != nil {
			return len(body), errors.Join(ErrPartialSuccess, p)
		}
		return len(body), nil
	// OTLP names 429, 502, 503 and 504 as retryable. Other 5xx are not on that
	// list, so they are reported as permanent rather than retried forever.
	case resp.status == 429, resp.status == 502, resp.status == 503, resp.status == 504:
		return 0, errors.Join(ErrRetryable, errStatus(resp.status))
	default:
		return 0, errStatus(resp.status)
	}
}

type httpResponse struct {
	status         int
	body           []byte
	closeRequested bool
}

// readResponse reads the status line, headers and body to completion.
//
// Reading once and parsing whatever arrived is wrong twice over: TCP may split
// a valid response anywhere, and any body left in the socket is read as the
// next response's status line when the connection is reused.
func (e *Exporter) readResponse() (httpResponse, error) {
	var out httpResponse

	e.resp = e.resp[:0]
	headerEnd := -1
	for headerEnd < 0 {
		if len(e.resp) == cap(e.resp) {
			// Refusing is safer than parsing a truncated header block.
			e.closeConn()
			return out, errors.New("otlpmini: response header exceeds " +
				strconv.Itoa(maxHeaderBytes) + " bytes")
		}
		n, err := e.conn.Read(e.resp[len(e.resp):cap(e.resp)])
		if n > 0 {
			e.resp = e.resp[:len(e.resp)+n]
			headerEnd = indexCRLFCRLF(e.resp)
		}
		if headerEnd >= 0 {
			break
		}
		if err != nil {
			e.closeConn()
			return out, errors.Join(ErrRetryable, err)
		}
		if n == 0 {
			e.closeConn()
			return out, errors.Join(ErrRetryable,
				errors.New("otlpmini: connection closed before the headers ended"))
		}
	}

	head := e.resp[:headerEnd]
	status, err := parseStatus(head)
	if err != nil {
		e.closeConn()
		return out, errors.Join(ErrRetryable, err)
	}
	out.status = status
	out.closeRequested = headerHasToken(head, "connection", "close")

	contentLength, chunked, err := parseBodyFraming(head)
	if err != nil {
		e.closeConn()
		return out, err
	}

	bodyStart := headerEnd + 4
	have := e.resp[bodyStart:]

	switch {
	case chunked:
		// Chunked framing is not implemented. Closing the connection is the
		// only safe response, because the body length is unknown and leaving
		// it would corrupt the next export.
		e.closeConn()
		out.closeRequested = true
		out.body = nil
	case contentLength < 0:
		// No Content-Length and not chunked: the body ends when the peer
		// closes, so this connection cannot be reused.
		e.closeConn()
		out.closeRequested = true
		out.body = have
	default:
		body, err := e.readBody(have, contentLength)
		if err != nil {
			return out, err
		}
		out.body = body
	}
	return out, nil
}

// readBody returns exactly want bytes of body, reading more if the headers and
// body arrived in separate segments.
func (e *Exporter) readBody(have []byte, want int) ([]byte, error) {
	if want <= len(have) {
		return have[:want], nil
	}
	// The response buffer holds the headers; grow a copy for the body so the
	// header slice stays valid.
	body := make([]byte, 0, want)
	body = append(body, have...)
	for len(body) < want {
		n, err := e.conn.Read(body[len(body):want])
		if n > 0 {
			body = body[:len(body)+n]
		}
		if len(body) >= want {
			break
		}
		if err != nil {
			e.closeConn()
			return nil, errors.Join(ErrRetryable, err)
		}
		if n == 0 {
			e.closeConn()
			return nil, errors.Join(ErrRetryable,
				errors.New("otlpmini: connection closed mid-body"))
		}
	}
	return body, nil
}

// IsRetryable reports whether an error from Export is worth retrying.
func IsRetryable(err error) bool { return errors.Is(err, ErrRetryable) }

// splitEndpoint splits a plain http:// URL into host:port and path. net/url is
// avoided so this build does not pull in more of the net stack than it needs.
func splitEndpoint(endpoint string) (host, path string) {
	s := endpoint
	if after, ok := cutPrefix(s, "http://"); ok {
		s = after
	}
	if i := indexByte(s, '/'); i >= 0 {
		host, path = s[:i], s[i:]
	} else {
		host, path = s, "/"
	}
	if indexByte(host, ':') < 0 {
		host += ":4318"
	}
	return host, path
}

// parseStatus reads the code out of a status line such as "HTTP/1.1 200 OK".
func parseStatus(b []byte) (int, error) {
	i := indexByteSlice(b, ' ')
	if i < 0 || len(b) < i+4 {
		return 0, errors.New("otlpmini: malformed status line")
	}
	code, err := strconv.Atoi(string(b[i+1 : i+4]))
	if err != nil {
		return 0, errors.New("otlpmini: malformed status code")
	}
	return code, nil
}

// parseBodyFraming reports how the body is delimited. A negative length with
// chunked false means "until the peer closes".
func parseBodyFraming(head []byte) (length int, chunked bool, err error) {
	if v, ok := headerValue(head, "transfer-encoding"); ok {
		if containsFold(v, "chunked") {
			return 0, true, nil
		}
	}
	v, ok := headerValue(head, "content-length")
	if !ok {
		return -1, false, nil
	}
	n, convErr := strconv.Atoi(string(trimSpace(v)))
	if convErr != nil || n < 0 {
		return 0, false, errors.New("otlpmini: malformed Content-Length")
	}
	return n, false, nil
}

type errStatus int

func (e errStatus) Error() string {
	return "otlpmini: collector returned HTTP " + strconv.Itoa(int(e))
}
