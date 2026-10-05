//go:build socket

package otlpmini

import (
	"bufio"
	"net"
	"net/http"
	"testing"
)

// The socket exporter keeps one connection open across exports. Dialing per
// export allocated 8,272 bytes in the lneto TCP stack each time, which forced
// a GC roughly every 21 exports on the device.
func TestExportReusesOneConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan int, 4)
	go func() {
		conns := 0
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns++
			accepted <- conns
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					// Drain the body so the next request parses.
					buf := make([]byte, req.ContentLength)
					br.Read(buf)
					// No Connection: close, so the client may reuse this.
					c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
				}
			}(c)
		}
	}()

	reg, g, _ := newTestRegistry()
	exp := NewExporter("http://" + ln.Addr().String() + "/v1/metrics")
	defer exp.Close()

	for i := 0; i < 3; i++ {
		g.Set(float64(i))
		if _, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType()); err != nil {
			t.Fatalf("export %d: %v", i, err)
		}
	}

	// Exactly one connection should have been accepted for three exports.
	total := 0
	for len(accepted) > 0 {
		total = <-accepted
	}
	if total != 1 {
		t.Errorf("server accepted %d connections for 3 exports, want 1", total)
	}
}

// If the peer closes a kept-open connection, the next export must redial
// rather than report a failure.
func TestExportRecoversFromClosedConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				// Serve exactly one request, then hang up.
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				buf := make([]byte, req.ContentLength)
				br.Read(buf)
				c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
			}(c)
		}
	}()

	reg, g, _ := newTestRegistry()
	exp := NewExporter("http://" + ln.Addr().String() + "/v1/metrics")
	defer exp.Close()

	for i := 0; i < 3; i++ {
		g.Set(float64(i))
		if _, err := exp.Export(encode(t, reg, testStart, testNow), reg.ContentType()); err != nil {
			t.Fatalf("export %d failed after the peer hung up: %v", i, err)
		}
	}
}
