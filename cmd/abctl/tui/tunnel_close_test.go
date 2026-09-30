package tui

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// Tests for #1199: an opaque tunnel now records a response row when it closes, carrying
// a status, how long the tunnel stayed open, and the bytes it moved each way.

// A close row carries the Tunnel marker too, so the fold must key on the open only.
// Otherwise a close followed by a request to the same host — a client that tunnels and
// then makes a plain call — would swallow that request into a row keyed on it, and the
// close would vanish from the timeline.
func TestBuildEventRows_TunnelCloseNeverFolds(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com:443",
			Tunnel: true, RequestID: "t"},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "api.example.com:443",
			Tunnel: true, RequestID: "t", StatusCode: 200},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com", RequestID: "r"},
	}
	rows := buildEventRows(events)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 — nothing here is a bridged pair", len(rows))
	}
	for i, r := range rows {
		if r.tunnel != nil {
			t.Errorf("row %d folded a tunnel into itself", i)
		}
	}
}

// What makes the close row useful with no abctl-side pairing work: it shares the open's
// RequestID, and the # column pairs on that across whatever landed in between. Pinned
// because the proxy side of #1199 relies on it rather than adding anything here.
func TestComputeEventPairs_TunnelCloseJoinsItsOpen(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Phase: pipeline.SessionRequest, Host: "kube:6443", Tunnel: true, RequestID: "tun"},
		{Phase: pipeline.SessionRequest, Host: "github.example.com", RequestID: "a"},
		{Phase: pipeline.SessionResponse, Host: "github.example.com", RequestID: "a", StatusCode: 200},
		{Phase: pipeline.SessionResponse, Host: "kube:6443", Tunnel: true, RequestID: "tun", StatusCode: 200},
	}
	rows := buildEventRows(events)
	ids, _ := computeEventPairs(rows)
	if ids[rows[0].event] != ids[rows[3].event] {
		t.Errorf("tunnel open is exchange %d and its close %d; want one exchange",
			ids[rows[0].event], ids[rows[3].event])
	}
	if ids[rows[0].event] == ids[rows[1].event] {
		t.Error("the tunnel and the request that interleaved with it share an exchange number")
	}
}

// A tunnel's DURATION is how long it stayed open, which for `kubectl logs -f` is
// minutes. "252.00s" makes a reader do the division; minutes and seconds do not.
func TestDurationCell_MinutesAndHours(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{12 * time.Millisecond, "12ms"},
		{1200 * time.Millisecond, "1.20s"},
		{59990 * time.Millisecond, "59.99s"},
		{time.Minute, "1m00s"},
		{102450 * time.Millisecond, "1m42s"},
		{252 * time.Second, "4m12s"},
		{time.Hour + 2*time.Minute + 5*time.Second, "1h02m"},
	} {
		if got := durationCell(pipeline.SessionEvent{Duration: tc.d}); got != tc.want {
			t.Errorf("durationCell(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestBytesCell(t *testing.T) {
	for _, tc := range []struct {
		name     string
		up, down int64
		want     string
	}{
		{"not a tunnel close", 0, 0, ""},
		{"both ways", 4210, 18230, "↑4.2k ↓18.2k"},
		{"small counts stay exact", 5, 7, "↑5 ↓7"},
		// Counts are absent on the wire when zero, so one side at zero is shown as 0
		// once the other proves this row carries counts at all.
		{"nothing sent", 0, 512, "↑0 ↓512"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bytesCell(pipeline.SessionEvent{BytesUp: tc.up, BytesDown: tc.down}); got != tc.want {
				t.Errorf("bytesCell(up=%d, down=%d) = %q, want %q", tc.up, tc.down, got, tc.want)
			}
		})
	}
}

// Off by default: only opaque tunnels' close rows have a figure, so on by default it
// would spend 15 columns of every terminal on a mostly blank column.
func TestBytesColumn_IsOptInAndSortsByTotal(t *testing.T) {
	var col *eventColumn
	for i := range eventColumns {
		if eventColumns[i].id == colBytes {
			col = &eventColumns[i]
		}
	}
	if col == nil {
		t.Fatal("no BYTES column")
	}
	if col.defaultOn {
		t.Error("BYTES is on by default; it should be opt-in")
	}
	if col.sortKey == nil {
		t.Fatal("BYTES has no sort key")
	}
	small := col.sortKey(cellContext{row: eventRow{event: &pipeline.SessionEvent{BytesUp: 1, BytesDown: 1}}})
	big := col.sortKey(cellContext{row: eventRow{event: &pipeline.SessionEvent{BytesDown: 1000}}})
	if !small.less(big) {
		t.Error("BYTES does not sort by the total carried")
	}
}
