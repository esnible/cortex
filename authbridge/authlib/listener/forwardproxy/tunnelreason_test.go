package forwardproxy

import (
	"net"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/tlsbridge"
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

// TestPassthroughReasonCoversEveryClassifyReason is a real tripwire, not a restated
// list. It iterates tlsbridge.ClassifyReasons — the vocabulary Classify itself uses —
// so adding a reason there, or renaming one, fails HERE instead of silently producing
// an unmapped "" downstream. An unmapped reason renders as an em dash, which reads as
// "bridged": the opposite of what happened.
func TestPassthroughReasonCoversEveryClassifyReason(t *testing.T) {
	if len(tlsbridge.ClassifyReasons) == 0 {
		t.Fatal("tlsbridge.ClassifyReasons is empty; this test would assert nothing")
	}
	for _, why := range tlsbridge.ClassifyReasons {
		got := passthroughReason(why)
		if got == "" {
			t.Errorf("Classify reason %q maps to \"\", which renders as a BRIDGED row", why)
			continue
		}
		if len(got) > pluginCellWidth {
			t.Errorf("reason %q is %d chars; it truncates in abctl's %d-wide PLUGIN cell, "+
				"so the timeline token stops matching the log token", got, len(got), pluginCellWidth)
		}
	}
}

// pluginCellWidth mirrors abctl's PLUGIN column width. Duplicated deliberately:
// authlib must not import the TUI, and a reason that does not fit is a defect in the
// reason, not in the column.
const pluginCellWidth = 18

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
