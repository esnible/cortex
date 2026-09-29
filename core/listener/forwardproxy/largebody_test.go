package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// responseBodyProbe declares ReadsBody, which is what makes
// Pipeline.NeedsResponseBody true — the same shape as the three protocol
// parsers (inference-parser, mcp-parser, a2a-parser), each of which
// declares ReadsBody and nothing else. It records the length it was
// handed so a test can tell "buffered and parsed" from "streamed past".
type responseBodyProbe struct {
	sawResponseBodyLen int
	responseCalls      int
	writes             bool // declare WritesResponseBody, like cpex and sparc
}

func (p *responseBodyProbe) Name() string { return "response-body-probe" }
func (p *responseBodyProbe) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true, WritesResponseBody: p.writes}
}
func (p *responseBodyProbe) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseBodyProbe) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.responseCalls++
	p.sawResponseBodyLen = len(pctx.ResponseBody)
	return pipeline.Action{Type: pipeline.Continue}
}

// lastFrameProbe is a StreamingResponder recording the terminal frame, where
// inference-parser settles a header-priced response that has no body to parse.
type lastFrameProbe struct{ sawLast bool }

func (p *lastFrameProbe) Name() string { return "last-frame-probe" }
func (p *lastFrameProbe) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (p *lastFrameProbe) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *lastFrameProbe) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *lastFrameProbe) OnResponseFrame(_ context.Context, _ *pipeline.Context, _ []byte, last bool) pipeline.Action {
	p.sawLast = p.sawLast || last
	return pipeline.Action{Type: pipeline.Continue}
}

// assertRelayedUnbuffered: OnResponse ran once, on an empty body rather than a
// truncated one, and the terminal frame still arrived.
func assertRelayedUnbuffered(t *testing.T, probe *responseBodyProbe, frames *lastFrameProbe) {
	t.Helper()
	if probe.responseCalls != 1 || probe.sawResponseBodyLen != 0 || !frames.sawLast {
		t.Errorf("probe = %+v, last frame = %v; want 1 OnResponse on 0 body bytes and a last=true frame",
			*probe, frames.sawLast)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// serveOversizedBody writes maxBodySize+1 bytes of a non-SSE content type.
// When declareLength is false the handler leaves Content-Length unset, so
// Go serves it chunked and resp.ContentLength is -1 — the case a
// header-based short-circuit cannot see.
func serveOversizedBody(t *testing.T, declareLength bool) (*httptest.Server, int) {
	t.Helper()
	size := maxBodySize + 1
	payload := make([]byte, 32<<10)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if declareLength {
			w.Header().Set("Content-Length", strconv.Itoa(size))
		}
		w.WriteHeader(http.StatusOK)
		remaining := size
		for remaining > 0 {
			n := len(payload)
			if n > remaining {
				n = remaining
			}
			if _, err := w.Write(payload[:n]); err != nil {
				return
			}
			remaining -= n
		}
	}))
	return srv, size
}

func proxyGet(t *testing.T, srv *Server, url string) *http.Response {
	t.Helper()
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestForwardProxy_OversizedDeclaredLengthStreamsThrough is the regression
// test for the bug that made `claude update` fail behind the laptop proxy:
// a 226MB binary download over a bridged (non-passthrough) host was answered
// 502 {"error":"response body too large"}.
//
// There is no stream to preserve here and nothing was downgraded — the
// incremental path is gated on Content-Type: text/event-stream, so a
// non-SSE response has only ever had the buffered path available. Any
// ReadsBody plugin therefore turns every large download into a 502, even
// though the plugin could not have parsed application/octet-stream.
//
// Content-Length is present and accurate, so the size is knowable before a
// byte of body is read. The assertion is that the caller gets all the bytes.
func TestForwardProxy_OversizedDeclaredLengthStreamsThrough(t *testing.T) {
	backend, size := serveOversizedBody(t, true)
	defer backend.Close()

	probe, frames := &responseBodyProbe{}, &lastFrameProbe{}
	p, err := pipeline.New([]pipeline.Plugin{probe, frames})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}

	resp := proxyGet(t, srv, backend.URL+"/claude-code-releases/2.1.284/darwin-arm64/claude")

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body %q), want 200", resp.StatusCode, body)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("reading proxied body: %v", err)
	}
	if n != int64(size) {
		t.Errorf("proxied %d bytes, want %d", n, size)
	}
	// The plugin must see an empty body rather than a truncated one: a
	// partial body is indistinguishable from a real short response and
	// would make a parser emit nonsense. Mirrors the SSE passthrough
	// contract, where a ReadsBody-only plugin also gets an empty body.
	assertRelayedUnbuffered(t, probe, frames)

	// The declared length alone decides: a body that fails after a few bytes
	// is never read before the headers go out, so it is not a 502.
	srv.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, ContentLength: maxBodySize + 1,
			Body: io.NopCloser(io.MultiReader(strings.NewReader("abc"), failingReader{}))}, nil
	})}
	if resp := proxyGet(t, srv, "http://declared.example/x"); resp.StatusCode != http.StatusOK {
		t.Errorf("declared oversize with a failing body: status = %d, want 200 (no read before headers)", resp.StatusCode)
	}
}

// TestForwardProxy_OversizedWithResponseWriterStill502s: a WritesResponseBody
// plugin enforces on the body, so relaying it an empty one would fail open.
func TestForwardProxy_OversizedWithResponseWriterStill502s(t *testing.T) {
	for _, declared := range []bool{true, false} {
		backend, _ := serveOversizedBody(t, declared)
		p, err := pipeline.New([]pipeline.Plugin{&responseBodyProbe{writes: true}})
		if err != nil {
			t.Fatal(err)
		}
		srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}
		if resp := proxyGet(t, srv, backend.URL+"/x"); resp.StatusCode != http.StatusBadGateway {
			t.Errorf("declared=%v: status = %d, want 502", declared, resp.StatusCode)
		}
		backend.Close()
	}
}

// TestForwardProxy_OversizedUnknownLengthStreamsThrough covers the other
// half: a chunked response declares no length, so the size only becomes
// known once the cap is already exceeded mid-read. Discovering it late must
// still not fail the request — the bytes already read belong to the caller.
func TestForwardProxy_OversizedUnknownLengthStreamsThrough(t *testing.T) {
	backend, size := serveOversizedBody(t, false)
	defer backend.Close()

	probe, frames := &responseBodyProbe{}, &lastFrameProbe{}
	p, err := pipeline.New([]pipeline.Plugin{probe, frames})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}

	resp := proxyGet(t, srv, backend.URL+"/chunked")

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body %q), want 200", resp.StatusCode, body)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("reading proxied body: %v", err)
	}
	if n != int64(size) {
		t.Errorf("proxied %d bytes, want %d", n, size)
	}
	// The one branch where a 10MB prefix IS in memory, so handing it over is
	// the realistic regression.
	assertRelayedUnbuffered(t, probe, frames)
}

// TestForwardProxy_UndersizedBodyStillBuffers guards the other direction:
// the fix must not stop buffering the bodies plugins exist to read. A
// normal-sized response still reaches the plugin whole.
func TestForwardProxy_UndersizedBodyStillBuffers(t *testing.T) {
	payload := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	defer backend.Close()

	probe := &responseBodyProbe{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}

	resp := proxyGet(t, srv, backend.URL+"/mcp")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != payload {
		t.Errorf("proxied body = %q, want %q", body, payload)
	}
	if probe.sawResponseBodyLen != len(payload) {
		t.Errorf("plugin saw %d body bytes, want %d", probe.sawResponseBodyLen, len(payload))
	}
}
