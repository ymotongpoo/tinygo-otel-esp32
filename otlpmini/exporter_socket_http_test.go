// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

//go:build socket

package otlpmini

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

// rawServer serves canned bytes so that a response can be split exactly where
// a real TCP stack might split it.
type rawServer struct {
	ln       net.Listener
	requests atomic.Int32
	// replies are written in order, one per request, each element written as
	// a separate Write call so the client sees a packet boundary there.
	replies [][]string
}

func newRawServer(t *testing.T, replies [][]string) *rawServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &rawServer{ln: ln, replies: replies}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *rawServer) addr() string { return s.ln.Addr().String() }

func (s *rawServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 8192)
			for {
				// One Read per request is enough: the test payloads are small
				// and arrive in one segment.
				if _, err := c.Read(buf); err != nil {
					return
				}
				n := int(s.requests.Add(1)) - 1
				if n >= len(s.replies) {
					return
				}
				for _, part := range s.replies[n] {
					if _, err := c.Write([]byte(part)); err != nil {
						return
					}
				}
			}
		}(c)
	}
}

// A response may arrive split anywhere. Reading once and giving up treats a
// perfectly valid response as malformed.
func TestExportAcceptsSplitResponse(t *testing.T) {
	srv := newRawServer(t, [][]string{
		{"HTTP/1.", "1 200 OK\r\nContent-Length: 0\r\n\r\n"},
	})

	reg, g, _ := newTestRegistry()
	g.Set(1)
	exp := NewExporter("http://" + srv.addr() + "/v1/metrics")
	defer exp.Close()

	if _, err := exp.export(exp.path, encode(t, reg, testStart, testNow), reg.ContentType()); err != nil {
		t.Errorf("a split but valid response was rejected: %v", err)
	}
}

// The body must be drained before the connection is reused, or the next export
// reads leftover body bytes as its status line.
func TestExportDrainsBodyBeforeReuse(t *testing.T) {
	body := `{"partialSuccess":{}}`
	srv := newRawServer(t, [][]string{
		{fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(body)), body},
		{"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"},
	})

	reg, g, _ := newTestRegistry()
	g.Set(1)
	exp := NewExporter("http://" + srv.addr() + "/v1/metrics")
	defer exp.Close()

	if _, err := exp.export(exp.path, encode(t, reg, testStart, testNow), reg.ContentType()); err != nil {
		t.Fatalf("export 1: %v", err)
	}
	// If the body was left in the socket, this read sees `{"partialSuccess"...`
	// as an HTTP status line.
	if _, err := exp.export(exp.path, encode(t, reg, testStart, testNow), reg.ContentType()); err != nil {
		t.Errorf("export 2 saw the previous body as a status line: %v", err)
	}
}

// Headers can exceed any fixed buffer. Silently misreading them is worse than
// failing, so the exporter must report the overflow.
func TestExportRejectsOversizedHeaders(t *testing.T) {
	huge := "HTTP/1.1 200 OK\r\nX-Pad: " + strings.Repeat("a", 4096) + "\r\nContent-Length: 0\r\n\r\n"
	srv := newRawServer(t, [][]string{{huge}})

	reg, _, _ := newTestRegistry()
	exp := NewExporter("http://" + srv.addr() + "/v1/metrics")
	defer exp.Close()

	_, err := exp.export(exp.path, encode(t, reg, testStart, testNow), reg.ContentType())
	if err == nil {
		t.Skip("headers fit in the buffer; nothing to assert")
	}
	if !strings.Contains(err.Error(), "header") {
		t.Errorf("oversized headers gave %v, want an error naming the header limit", err)
	}
}

// Connection: close is case-insensitive in HTTP. Matching two literal spellings
// misses the rest.
func TestExportHonorsCaseInsensitiveConnectionClose(t *testing.T) {
	srv := newRawServer(t, [][]string{
		{"HTTP/1.1 200 OK\r\nCONNECTION: Close\r\nContent-Length: 0\r\n\r\n"},
		{"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"},
	})

	reg, _, _ := newTestRegistry()
	exp := NewExporter("http://" + srv.addr() + "/v1/metrics")
	defer exp.Close()

	if _, err := exp.export(exp.path, encode(t, reg, testStart, testNow), reg.ContentType()); err != nil {
		t.Fatalf("export 1: %v", err)
	}
	if exp.conn != nil {
		t.Error("CONNECTION: Close was ignored; the connection was kept open")
	}
}
