package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// benchContextEvents is a session shaped like a real one: one-shot calls carrying a big context
// interleaved with a conversation whose message count grows.
func benchContextEvents(n int) []pipeline.SessionEvent {
	base := time.Now()
	out := make([]pipeline.SessionEvent, 0, n)
	for i := 0; len(out) < n; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		if i%4 == 0 {
			out = append(out, oneShot(fmt.Sprintf("o%d", i), at, 200_000)...)
		} else {
			out = append(out, conversation(fmt.Sprintf("c%d", i), at, 100+i, 400_000+i)...)
		}
	}
	return out[:n]
}

// THE SHAPE THAT MATTERS: one event arrives, and the sessions row loop asks every session for its
// gauge. rebuildSessionsTable runs on every streamed event and retention is unbounded, so a rescan
// here is O(events) per session per event — on the pane abctl opens on.
//
// This benchmark exists because that regression shipped once. The first version of the rule
// made two full passes and allocated a map per call; measured at 100k events it cost 6.10ms and
// 3.49MB per session per event, against ~14ns for the tail scan it replaced. Dropping the map and
// the dead paired-request check took the whole-slice cost to 0.51ms and no allocation, and folding
// the delta took the per-event cost off the session length entirely.
//
// Run it before changing sessionContextFor:
//
//	go test ./tui/ -run XXX -bench BenchmarkSessionContextPerEvent
func BenchmarkSessionContextPerEvent(b *testing.B) {
	for _, n := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("folded/%d", n), func(b *testing.B) {
			m := &model{events: map[string][]pipeline.SessionEvent{}}
			ids := make([]string, 10)
			for i := range ids {
				ids[i] = fmt.Sprintf("s%d", i)
				m.events[ids[i]] = benchContextEvents(n)
				_ = m.sessionContextFor(ids[i], nil) // warm, as a running TUI is
			}
			// THE SLICE IS BUILT ONCE, OUTSIDE THE LOOP, and the run is rewound instead.
			//
			// An earlier version appended the delta inside the b.N loop, which made the
			// measurement a function of how many iterations the sweep chose: the slice grew by a
			// turn every iteration, so a default -benchtime spent most of its time in append and
			// realloc — 152KB/op of it — and the per-event fold this benchmark exists to protect
			// was the small term. It only read correctly under -benchtime 20x, which is a
			// measurement you have to remember to ask for.
			//
			// Rewinding the winner's run to its pre-delta state is the same work with none of the
			// growth: every iteration folds exactly the arriving events for ids[0] and takes the
			// length-check hit for the other nine, which is the shape one streamed event has.
			// What it adds is one map store per iteration, constant and tens of nanoseconds.
			arriving := conversation("new", time.Now(), 999, 900_000)
			head := m.events[ids[0]]
			full := make([]pipeline.SessionEvent, 0, len(head)+len(arriving))
			full = append(append(full, head...), arriving...)
			warm := m.contextRun[ids[0]]
			m.events[ids[0]] = full
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.contextRun[ids[0]] = warm // as if the delta had only just landed
				for _, id := range ids {
					_ = m.sessionContextFor(id, nil)
				}
			}
		})
		b.Run(fmt.Sprintf("rescan/%d", n), func(b *testing.B) {
			evs := map[string][]pipeline.SessionEvent{}
			for i := 0; i < 10; i++ {
				evs[fmt.Sprintf("s%d", i)] = benchContextEvents(n)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, e := range evs {
					_ = pipeline.PromptContextOf(e)
				}
			}
		})
	}
}
