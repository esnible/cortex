package httpx

import (
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
)

// LoopbackHost reports whether hostport names exactly localhost, 127.0.0.1 or
// ::1, with or without a port. Not 127.0.0.0/8 as a whole and not anything
// ending in localhost — the point is to refuse a name an attacker controls,
// and a short list cannot be matched by one.
//
// ONE definition for the loopback-Host checks (#1608): the session API's erase
// guards, the stats server's pprof endpoints and the agentop viewer each
// carried their own copy, and a fix to one — closing a rebinding shape, adding
// an address form — reached the others only by re-editing every copy.
func LoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// RegisterPprof mounts Go's net/http/pprof handlers under /debug/pprof/ on mux,
// each behind the browser defenses: a request whose Host is not exactly a
// loopback name (LoopbackHost — this defeats DNS rebinding) or that carries an
// Origin header (a browser always sends one; a no-cors GET needs no preflight)
// is answered 403. The profiles expose the process's internals — allocation
// stack traces, argv, and a caller's ability to start CPU/trace profiling — so
// callers mount them only on loopback-bound servers: the stats server behind
// listener.bind_loopback_only, agentop behind AGENTOP_PPROF_ADDR.
//
// Side effect of the import: net/http/pprof's init also registers
// /debug/pprof/* on http.DefaultServeMux in every binary linking this
// package. Nothing in this tree serves that mux — an http.Server built with a
// nil Handler would, and would expose these ungated, so none may be.
//
// ONE definition for the guarded surface (#1608): the stats server (#1603)
// and the agentop viewer each carried their own copy of the guard and the five
// registrations.
func RegisterPprof(mux *http.ServeMux) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !LoopbackHost(r.Host) {
				http.Error(w, "pprof is served only for a loopback Host", http.StatusForbidden)
				return
			}
			if r.Header.Get("Origin") != "" {
				http.Error(w, "pprof is refused from a browser: the request carries an Origin header", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/debug/pprof/", guard(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", guard(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", guard(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", guard(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", guard(pprof.Trace))
}
