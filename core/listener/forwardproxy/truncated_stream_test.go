package forwardproxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/pipeline"
)

// abortingStream answers 200, writes one flushed chunk, then resets the TCP
// connection with SO_LINGER=0 — an upstream that breaks a streamed response
// partway. contentType picks the arm the proxy takes: text/event-stream hits
// the SSE relays, anything else the unbuffered io.Copy.
func abortingStream(contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}
}

// readViaProxy fetches target through the proxy and returns the error the
// CLIENT saw while reading the body — the thing #1543 is about: a truncated
// upstream stream must reach the client as an error, not a clean end of body.
func readViaProxy(t *testing.T, proxy *httptest.Server, target string) error {
	t.Helper()
	c := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := c.Get(target)
	if err != nil {
		// A failure to even get headers is a client-visible error too —
		// the plain-body arm's reset can land before the client reads the
		// flushed headers. What must NOT happen is a clean GET plus a
		// clean body read off a truncated stream.
		return err
	}
	defer resp.Body.Close()
	_, readErr := io.ReadAll(resp.Body)
	return readErr
}

// TestForwardProxy_TruncatedStream_ReachesClientAsError: every relay arm the
// forward proxy takes for an open-ended (chunked) body — the SSE relay with a
// streaming responder, the SSE passthrough with none, and the plain unbuffered
// copy — used to return normally when the upstream broke mid-body, so net/http
// wrote the terminating chunk and the client read a COMPLETE 200 off a cut-off
// stream (#1543). Each arm must instead abort the client's connection, and the
// response row must carry the failure instead of a clean 200.
func TestForwardProxy_TruncatedStream_ReachesClientAsError(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		plugins     []pipeline.Plugin
	}{
		{name: "sse passthrough, no plugins", contentType: "text/event-stream"},
		{name: "sse relay, streaming responder", contentType: "text/event-stream", plugins: []pipeline.Plugin{newStreamingProbe(false)}},
		{name: "plain body, unbuffered copy", contentType: "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(abortingStream(tc.contentType))
			defer up.Close()
			proxy, store := failureProxy(t, nil, tc.plugins...)

			if readErr := readViaProxy(t, proxy, up.URL); readErr == nil {
				t.Error("truncated upstream stream reached the client as a clean end of body")
			}

			row := onlyResponseEvent(t, store)
			if row.Error == nil {
				t.Fatalf("response row for a truncated stream carries no error (status %d) — agentop hides the failure", row.StatusCode)
			}
			if row.Error.Kind != "upstream_error" {
				t.Errorf("row error kind = %q, want upstream_error", row.Error.Kind)
			}
			if row.Error.Code != "200" {
				t.Errorf("row error code = %q, want the upstream's 200 as error.code", row.Error.Code)
			}
		})
	}
}

// The control: an upstream that ends its stream cleanly produces a clean read
// AND a row with no error. Pins that the fix above did not start aborting
// healthy streams.
func TestForwardProxy_CleanStreamStillReadsClean(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
	}))
	defer up.Close()
	proxy, store := failureProxy(t, nil, newStreamingProbe(false))

	if readErr := readViaProxy(t, proxy, up.URL); readErr != nil {
		t.Errorf("clean upstream stream reached the client as an error: %v", readErr)
	}
	row := onlyResponseEvent(t, store)
	if row.Error != nil {
		t.Errorf("clean stream's row carries an error: %+v", row.Error)
	}
}

// skip_hosts promises no pipeline and no session row — but not a lying
// connection: a truncated skipped stream must still abort the client's
// connection, while recording nothing.
func TestForwardProxy_TruncatedSkippedStreamAbortsButRecordsNothing(t *testing.T) {
	up := httptest.NewServer(abortingStream("text/event-stream"))
	defer up.Close()
	skip := skiphostMustMatch(t, up.URL)
	proxy, store := failureProxy(t, skip)

	if readErr := readViaProxy(t, proxy, up.URL); readErr == nil {
		t.Error("truncated skipped stream reached the client as a clean end of body")
	}
	for _, ev := range allEvents(t, store) {
		if ev.Phase == pipeline.SessionResponse {
			t.Errorf("skip_hosts recorded a response row: %+v", ev)
		}
	}
}

// skiphostMustMatch builds a matcher covering the host of rawURL.
func skiphostMustMatch(t *testing.T, rawURL string) *skiphost.Matcher {
	t.Helper()
	u := mustParseURL(rawURL)
	m, err := skiphost.New([]string{u.Hostname()})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}
	return m
}
