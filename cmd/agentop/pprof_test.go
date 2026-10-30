package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// pprofMux's guards are the same defenses as the proxy's stats server (#1603)
// and the session API's erase guards: a page can rebind a DNS name to
// 127.0.0.1 and have the victim's browser read a loopback port, and the
// profiles expose the viewer's internals — the heap holds the session content
// it is displaying.
func TestPprofMuxRefusesBrowsers(t *testing.T) {
	handler := pprofMux()
	for _, tc := range []struct {
		name   string
		host   string
		origin string
		want   int
	}{
		{"rebound host", "evil.example:47603", "", http.StatusForbidden},
		{"loopback suffix", "127.0.0.1.evil.example", "", http.StatusForbidden},
		{"origin present", "localhost:47603", "https://evil.example", http.StatusForbidden},
		{"loopback", "localhost:47603", "", http.StatusOK},
		{"loopback ipv4", "127.0.0.1:47603", "", http.StatusOK},
		{"loopback ipv6", "[::1]:47603", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Result().StatusCode != tc.want {
				t.Errorf("GET /debug/pprof/heap Host %q Origin %q = %d, want %d",
					tc.host, tc.origin, w.Result().StatusCode, tc.want)
			}
		})
	}
}

// The refusal must keep the profile surface, not shadow it: a guarded request
// that passes returns a real heap profile, as `go tool pprof` reads it.
func TestPprofMuxServesAHeapProfile(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.Host = "127.0.0.1:47603"
	w := httptest.NewRecorder()

	pprofMux().ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content type = %q, want application/octet-stream", ct)
	}
	body := w.Body.Bytes()
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("body is %d bytes, want a gzip stream (pprof serves protobuf gzipped)", len(body))
	}
}
