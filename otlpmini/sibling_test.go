package otlpmini

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestSiblingPath(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/v1/metrics", "/v1/logs"},
		{"/otlp/v1/metrics", "/otlp/v1/logs"},
		{"/", "/v1/logs"},
		{"/otlp", "/otlp/v1/logs"},
	} {
		if got := siblingPath(c.in, "logs"); got != c.want {
			t.Errorf("siblingPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Metrics and logs go to different paths on the same collector. With the
// socket build they also share one kept-open connection.
func TestExportToSendsToSiblingPath(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	conns := map[string]bool{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		conns[r.RemoteAddr] = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	srv.Start()
	defer srv.Close()

	e := NewExporter(srv.URL + "/v1/metrics")
	defer e.Close()
	if _, err := e.Export([]byte("{}"), "application/json"); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := e.ExportTo(e.SiblingPath("logs"), []byte("{}"), "application/json"); err != nil {
		t.Fatalf("ExportTo: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/v1/metrics" || paths[1] != "/v1/logs" {
		t.Errorf("paths = %v", paths)
	}
	if len(conns) != 1 {
		t.Errorf("used %d connections, want 1 (kept-open connection shared across signals)", len(conns))
	}
}
