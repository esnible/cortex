package pipeline

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync/atomic"
)

// requestIDCounter is the fallback when crypto/rand is unavailable, so an id is
// always produced rather than an empty string that would silently disable
// pairing.
var requestIDCounter atomic.Uint64

// newRequestID returns a short, unique-per-process request identifier.
//
// Not a UUID on purpose: it exists to pair a request event with its response
// event in a session timeline, so it needs to be unique among in-flight
// requests and short enough to read in a terminal — not globally unique or
// cryptographically meaningful.
func newRequestID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "r" + strconv.FormatUint(requestIDCounter.Add(1), 36)
	}
	return hex.EncodeToString(b[:])
}

// RequestID returns a stable identifier for this request, generated on first
// use. Session events carry it so a consumer can pair a request event with its
// response event.
//
// Without it, pairing is positional — a UI matches a request row to whatever
// response row follows it — which silently misattributes whenever a client has
// more than one request in flight. That produced a real misdiagnosis: a plugin
// was blamed for a 400 that belonged to a concurrent request it never touched.
//
// Generated lazily rather than at Context construction so no listener can forget
// it; there are several construction sites and adding one more required field
// would be a standing trap. Contexts are single-goroutine by contract (plugins
// mutate Body, Headers and Extensions without locks), so the lazy write needs no
// synchronisation.
func (c *Context) RequestID() string {
	if c.requestID == "" {
		c.requestID = newRequestID()
	}
	return c.requestID
}
