package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// LoopbackHost is the one loopback-Host predicate behind the session API's
// erase guards, the stats server's pprof endpoints and the agentop viewer's —
// three call sites that each used to carry their own copy. The table is the
// union of what those sites' tests pinned, so a regression here is caught once
// instead of per binary. It is an allowlist on purpose: see the function's doc.
func TestLoopbackHost(t *testing.T) {
	for _, tc := range []struct {
		hostport string
		want     bool
	}{
		{"localhost", true},
		{"localhost:9094", true},
		{"LOCALHOST:9094", true},
		{"127.0.0.1", true},
		{"127.0.0.1:47603", true},
		{"::1", true},
		{"[::1]:9094", true},
		{"", false},
		{"evil.example:47603", false},
		{"localhost.evil.example", false},
		{"127.0.0.1.evil.example", false},
		{"localhost.", false},
		{"127.0.0.2:9094", false},
		{"0177.0.0.1:80", false},
		{"127.1:80", false},
		{"::1.evil.example", false},
		{"[::ffff:127.0.0.1]:80", false},
	} {
		if got := LoopbackHost(tc.hostport); got != tc.want {
			t.Errorf("LoopbackHost(%q) = %v, want %v", tc.hostport, got, tc.want)
		}
	}
}

// RegisterPprof's guards are the browser defenses: a page can rebind a DNS
// name to 127.0.0.1 and have the victim's browser read a loopback port, and
// the profiles expose the process's internals — heap contents, argv, and a
// caller's ability to start CPU profiling. The Host allowlist and the Origin
// refusal each stop one of those shapes.
func TestRegisterPprofRefusesBrowsers(t *testing.T) {
	mux := http.NewServeMux()
	RegisterPprof(mux)
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

			mux.ServeHTTP(w, req)

			if w.Result().StatusCode != tc.want {
				t.Errorf("GET /debug/pprof/heap Host %q Origin %q = %d, want %d",
					tc.host, tc.origin, w.Result().StatusCode, tc.want)
			}
		})
	}
}

// The refusal must keep the profile surface, not shadow it: a guarded request
// that passes returns a real heap profile, as `go tool pprof` reads it.
func TestRegisterPprofServesAHeapProfile(t *testing.T) {
	mux := http.NewServeMux()
	RegisterPprof(mux)
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.Host = "127.0.0.1:47603"
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

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
