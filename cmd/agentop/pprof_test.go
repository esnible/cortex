package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// startPprof must land on loopback or refuse: the Host and Origin guards stop
// browsers, not a curl that sets Host: localhost, so a bind on every interface
// (":47603", "0.0.0.0:47603") would hand the heap — the session content the
// viewer is displaying — to anyone on the network. Checked on the bound
// address, not the string, so a name that resolves to loopback still works.
func TestStartPprofRefusesANonLoopbackBind(t *testing.T) {
	if _, _, err := startPprof("0.0.0.0:0"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("startPprof on 0.0.0.0:0 = %v, want a refusal naming loopback", err)
	}
}

// startPprof serves pprofMux, never DefaultServeMux (a nil Handler would) —
// pinned end to end over a real socket, which is also what makes the :0
// announce honest: the returned address is the one bound, port resolved.
func TestStartPprofServesTheGuardedMux(t *testing.T) {
	bound, stop, err := startPprof("127.0.0.1:0")
	if err != nil {
		t.Fatalf("startPprof on 127.0.0.1:0: %v", err)
	}
	defer stop()
	if strings.HasSuffix(bound, ":0") || !strings.HasPrefix(bound, "127.0.0.1:") {
		t.Errorf("bound address = %q, want 127.0.0.1:<real port>", bound)
	}

	for _, tc := range []struct {
		name string
		host string
		want int
	}{
		{"rebound Host", "evil.example", http.StatusForbidden},
		{"loopback Host", "127.0.0.1", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://"+bound+"/debug/pprof/heap", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = tc.host
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("GET /debug/pprof/heap Host %q = %d, want %d", tc.host, resp.StatusCode, tc.want)
			}
		})
	}
}
