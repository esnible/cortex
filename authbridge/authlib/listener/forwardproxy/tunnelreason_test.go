package forwardproxy

import (
	"net"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// TestPassthroughReason pins the mapping onto Classify's own vocabulary. If
// Classify gains a reason and this is not extended, the event silently carries
// "" — which reads as "bridged" and is the opposite of the truth.
func TestPassthroughReason(t *testing.T) {
	for _, tc := range []struct{ why, want string }{
		{"port", pipeline.TunnelPassthroughPort},
		{"non-tls", pipeline.TunnelPassthroughNonTLS},
		{"skip", pipeline.TunnelPassthroughHost},
		{"", ""},
	} {
		if got := passthroughReason(tc.why); got != tc.want {
			t.Errorf("passthroughReason(%q) = %q, want %q", tc.why, got, tc.want)
		}
	}
}

// TestPassthroughReasonCoversEveryClassifyVerdict guards the gap the table above
// cannot: a reason Classify emits that nothing here maps. Kept as an explicit
// list so adding one to Classify fails here rather than degrading in the field.
func TestPassthroughReasonCoversEveryClassifyVerdict(t *testing.T) {
	for _, why := range []string{"port", "non-tls", "skip"} {
		if passthroughReason(why) == "" {
			t.Errorf("Classify reason %q maps to the empty string, which renders as a BRIDGED row", why)
		}
	}
}

// TestClientAddrNeverPanics: these helpers exist only to build a log line, so a
// nil conn must degrade rather than take the proxy down on a diagnostic path.
func TestClientAddrNeverPanics(t *testing.T) {
	if got := clientAddr(nil); got != "unknown" {
		t.Errorf("clientAddr(nil) = %q, want %q", got, "unknown")
	}
	if got := clientPort(nil); got != "<port>" {
		t.Errorf("clientPort(nil) = %q, want a template placeholder, got %q", got, got)
	}
}

// TestClientPortIsTheDiscriminator is the point of the whole change: the host
// cannot tell two clients apart because they all dial the same host, so the
// source port is what identifies the offender.
func TestClientPortIsTheDiscriminator(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer func() { _ = srv.Close() }()

	// srv's RemoteAddr is the client end — the same thing bridgeServe holds.
	addr := clientAddr(srv)
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("clientAddr = %q, want a loopback host:port", addr)
	}
	port := clientPort(srv)
	if port == "<port>" || !strings.HasSuffix(addr, ":"+port) {
		t.Errorf("clientPort = %q, not the port of %q", port, addr)
	}
	// It must be the CLIENT's ephemeral port, not the listener's: the listener
	// port is shared by every client and would identify nothing.
	if _, lport, _ := net.SplitHostPort(ln.Addr().String()); port == lport {
		t.Errorf("clientPort returned the listener port %q — that cannot discriminate between clients", port)
	}
}

// TestCANotBeforeWithoutBridge: the value appears in a log line on a failure
// path, so its absence must read as "unknown" rather than crash or print junk.
func TestCANotBeforeWithoutBridge(t *testing.T) {
	s := &Server{}
	if got := s.caNotBefore(); got != "unknown" {
		t.Errorf("caNotBefore with no bridge = %q, want %q", got, "unknown")
	}
}
