package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The guard table and the heap-profile shape are httpx.RegisterPprof's, pinned
// in its own package. This pins the viewer's wiring of it: pprofMux must mount
// the guarded surface, not an empty mux — a loopback Host is answered with a
// real profile and a rebound one with the guard's 403.
func TestPprofMuxMountsTheGuardedSurface(t *testing.T) {
	handler := pprofMux()

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.Host = "127.0.0.1:47603"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if resp := w.Result(); resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("GET /debug/pprof/heap on a loopback Host = %d %q, want 200 application/octet-stream",
			resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	req = httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.Host = "evil.example:47603"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Errorf("GET /debug/pprof/heap on a rebound Host = %d, want 403", w.Result().StatusCode)
	}
}
