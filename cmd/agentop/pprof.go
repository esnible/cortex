package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
)

// pprofAddrEnv names the env var that serves Go's pprof endpoints from the
// viewer. Off unless set: the profiles expose the process's internals —
// allocation stack traces, argv, caller-started CPU/trace profiling — and the
// viewer's heap holds the session content it is displaying. Point it at a
// loopback address and read it with
// `go tool pprof http://127.0.0.1:47603/debug/pprof/heap`.
const pprofAddrEnv = "AGENTOP_PPROF_ADDR"

// startPprof serves the profile mux on addr. The returned shutdown closes the
// listener; process exit closes it too, so a caller that forgets the shutdown
// loses nothing but a port.
func startPprof(addr string) (shutdown func(), err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	// Handler is set, never nil: net/http/pprof's init also registers
	// /debug/pprof/* on http.DefaultServeMux, which a nil Handler would serve
	// ungated.
	srv := &http.Server{Handler: pprofMux()}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }, nil
}

// pprofMux is the guarded /debug/pprof surface: the handlers refuse any
// non-loopback Host and any request carrying Origin — the browser defenses of
// the proxy's stats server (#1603) and the session API's erase guards, copied
// rather than shared because this is a different binary.
func pprofMux() *http.ServeMux {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !loopbackHost(r.Host) {
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
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", guard(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", guard(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", guard(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", guard(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", guard(pprof.Trace))
	return mux
}

// loopbackHost reports whether hostport names exactly localhost, 127.0.0.1 or
// ::1, with or without a port. A twin of core/observe's unexported loopbackHost
// (itself a twin of sessionapi's): copied rather than shared because the
// binaries do not export it to each other. A short allowlist, not 127.0.0.0/8
// or a localhost suffix — the point is to refuse a name an attacker controls.
func loopbackHost(hostport string) bool {
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

// maybeServePprof starts the pprof listener when pprofAddrEnv is set, printing
// where it landed (or why it did not) on stderr, before the TUI takes the alt
// screen and stderr stops reaching the user. A refusal to bind is not fatal:
// the viewer's job does not depend on being profiled. The caller owns the
// returned shutdown and defers it — deferring here would stop the listener the
// moment this helper returns.
func maybeServePprof() (shutdown func()) {
	addr := os.Getenv(pprofAddrEnv)
	if addr == "" {
		return nil
	}
	stop, err := startPprof(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentop: not serving pprof on %s: %v\n", addr, err)
		return nil
	}
	fmt.Fprintf(os.Stderr, "agentop: serving pprof on http://%s/debug/pprof/\n", addr)
	return stop
}
