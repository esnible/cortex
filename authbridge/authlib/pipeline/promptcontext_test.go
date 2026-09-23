package pipeline

import (
	"fmt"
	"testing"
	"time"
)

// THE CASE THIS COLUMN WAS REPORTED FOR. Real interleaving from one live session: a conversation
// at ~1500 messages and 830-851k, with one-shots at 3 messages carrying 282k and 6k landing
// between its turns. Before this rule the gauge followed whichever spoke last, swinging 83% to
// 0.7% between adjacent turns.
func TestSessionContext_IgnoresOneShotsBetweenTurns(t *testing.T) {
	base := time.Now()
	at := func(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }
	var evs []SessionEvent
	for _, e := range [][]SessionEvent{
		conversation("c1", at(1), 1491, 830_000),
		oneShot("o1", at(2), 282_145),
		oneShot("o2", at(3), 282_493),
		conversation("c2", at(4), 1494, 835_000),
		oneShot("o3", at(5), 6_538),
		conversation("c3", at(6), 1509, 851_000),
		oneShot("o4", at(7), 7_000), // the most recent event of all
	} {
		evs = append(evs, e...)
	}

	if got, want := PromptContextOf(evs), 851_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — the conversation's latest turn, not the "+
			"one-shot that spoke after it", got, want)
	}
}

// A ONE-SHOT RUN OF ANY LENGTH MUST NOT WIN, which is why there is no window: the conversation
// goes silent while a subagent works, and that silence is structural rather than evidence it has
// gone. Thirty one-shots after the last conversation turn is past any last-N window.
func TestSessionContext_SurvivesALongSilence(t *testing.T) {
	base := time.Now()
	evs := conversation("c1", base, 700, 445_000)
	for i := 0; i < 30; i++ {
		evs = append(evs, oneShot(fmt.Sprintf("o%d", i),
			base.Add(time.Duration(i+1)*time.Minute), 186_870)...)
	}

	if got, want := PromptContextOf(evs), 445_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a silent conversation must not age out",
			got, want)
	}
}

// WITH NO ROLE STATED the most messages wins among conversation turns, and equal counts keep the
// most recent. This is the fallback rule, kept for a proxy that publishes no agentRole: the
// message count is then the only thing that separates the main thread from a subagent carrying
// its own tools, and it separates them only while the conversation is the longer of the two.
func TestSessionContext_UnstatedRoleMostMessagesWins(t *testing.T) {
	base := time.Now()
	var evs []SessionEvent
	for _, e := range [][]SessionEvent{
		conversation("main", base, 700, 445_000),
		conversation("sub", base.Add(time.Minute), 12, 40_000),       // a tool-carrying subagent
		conversation("main2", base.Add(2*time.Minute), 700, 448_000), // ties on messages
	} {
		evs = append(evs, e...)
	}

	if got, want := PromptContextOf(evs), 448_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d", got, want)
	}
}

// THE RESPONSE'S OWN MANIFEST IS THE FILTER, and the request's is deliberately not consulted.
//
// An earlier version required the paired request to carry tools too, justified as insurance.
// It was dead code: SnapshotInference is `c := *ext`, a shallow copy, and Tools is only appended
// while parsing the REQUEST — so both snapshots carry the same slice header off the same
// extension and cannot disagree. The pairing needed a map keyed by request id, which is what made
// this function allocate on every call, for a branch that could not be reached.
func TestSessionContext_JudgesTheResponsesOwnManifest(t *testing.T) {
	evs := []SessionEvent{{
		At: time.Now(), Phase: SessionResponse, Direction: Outbound,
		Inference: &InferenceExtension{
			Model: "claude-opus-5", Messages: make([]InferenceMessage, 50),
			Tools: toolsOf(27), InputTokens: 1_000, CacheReadTokens: 99_000,
		},
	}}

	if got, want := PromptContextOf(evs), 100_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a response with tools and tokens is a "+
			"candidate on its own account", got, want)
	}
}

// ONLY RESPONSES. A request snapshot carries no token counts, so it would be dropped anyway — but
// by accident of when SnapshotInference copies, not because the loop said so. This pins the
// phase check that makes the rule explicit: a request bearing tokens must still not count.
func TestSessionContext_IgnoresRequestEventsEvenWithCounts(t *testing.T) {
	inf := &InferenceExtension{
		Model: "claude-opus-5", Messages: make([]InferenceMessage, 900),
		Tools: toolsOf(27), InputTokens: 1_000, CacheReadTokens: 499_000,
	}
	evs := []SessionEvent{
		{At: time.Now(), Phase: SessionRequest, Direction: Outbound,
			Inference: inf},
	}

	if got := PromptContextOf(evs); got != 0 {
		t.Errorf("PromptContextOf = %d, want 0 — the prompt side is read off the response", got)
	}
}

// THE COMPACTION TRADEOFF THE FALLBACK STILL PAYS, pinned so it cannot be "fixed" by
// reintroducing the window that was ruled out.
//
// With no role stated, a compaction restarts the conversation at a low message count while the
// pre-compaction turn stays retained with 1500 of them, so the older, longer turn keeps winning
// and the gauge holds the old figure. A stale figure beats one that flips to a subagent's, and
// nothing in an unstated stream can tell the two apart. If this test starts failing because a
// recency rule was added to the FALLBACK, the silence problem is back with it — the main thread
// goes quiet while a subagent runs, and a last-N window fills with its traffic.
//
// TestSessionContext_AfterACompactionFollowsTheMainAgent is the same session with the role
// stated, and does not pay this.
func TestSessionContext_UnstatedRoleHoldsThePreCompactionFigure(t *testing.T) {
	base := time.Now()
	evs := conversation("before", base, 1500, 851_000)
	evs = append(evs, conversation("after", base.Add(time.Hour), 40, 62_000)...)

	if got, want := PromptContextOf(evs), 851_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — the stale-after-compaction tradeoff changed; "+
			"see the doc comment before accepting a new expectation here", got, want)
	}
}

// WITH NO MESSAGE COUNTS AT ALL, THE UNSTATED ARM IS LATEST-WINS — and `at` has to be the
// comparator that says so, ahead of tokens.
//
// This is the degenerate input the fallback actually meets on the wire, not a constructed one. A
// view=summary timeline that projects without setting MessageCount leaves messageCount() returning
// 0 for EVERY candidate, so all of them tie on msgs and whatever comes second decides the entire
// answer. The two candidates here are a compaction: the pre-compaction turn is large and early, the
// post-compaction turn is later and much smaller.
//
// Order tokens ahead of at and the larger figure wins, which pins 999,623 against a conversation
// that restarted at 400,249 — for the rest of the session, since nothing will ever outgrow it. That
// is the stale-figure failure this column exists to fix, reached through the one input where
// message count cannot rank anything. So tokens is a FINAL tie-break for pairs that agree on both
// msgs and at, never a substitute for at.
//
// Note this does not contradict TestSessionContext_UnstatedRoleHoldsThePreCompactionFigure above:
// there the counts are present and rank the turns, and holding the old figure is the accepted cost
// of having no role to read. Here there is nothing to rank by, and recency is all that is left.
func TestSessionContext_UnstatedWithNoCountsFollowsTheLatestTurn(t *testing.T) {
	base := time.Now()
	// msgs 0 on both: len(Messages) == 0 and MessageCount unset, which is exactly what
	// summarizeEvent produces on a proxy built before the counts landed.
	evs := conversation("pre-compaction", base, 0, 999_623)
	evs = append(evs, conversation("post-compaction", base.Add(time.Hour), 0, 400_249)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — with no counts to rank by the later turn wins; "+
			"a larger-context tie-break ahead of the timestamp pins the pre-compaction figure",
			got, want)
	}
}

// THE CASE THIS RULE WAS CHANGED FOR, with the reported session's own figures.
//
// c39dae31 compacted at 21:32:20 after a turn at 2,468 messages and 999,623 prompt tokens. Ten
// hours and 313 candidate turns later its conversation was at 952 messages and 400,249 tokens,
// and the gauge still drew the pre-compaction figure — 999,623 against a one-million window, a
// bar at 98.6% for a session with 600k of headroom. The message count could not recover for the
// rest of the session: it had 1,516 to climb back.
//
// With the role stated the rule is just "the main agent's latest turn", so the column follows the
// compaction on the very next turn.
func TestSessionContext_AfterACompactionFollowsTheMainAgent(t *testing.T) {
	base := time.Now()
	evs := mainAgent("pre-compaction", base, 2468, 999_623)
	evs = append(evs, mainAgent("post-compaction", base.Add(10*time.Hour), 952, 400_249)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — the gauge is holding a pre-compaction figure "+
			"for a conversation that restarted", got, want)
	}
}

// A SUBAGENT SPEAKING LAST MUST NOT TAKE THE COLUMN, and the message count is not what stops it.
//
// Figures from d6cfc02e, where the two populations overlap: the subagent reached 186 messages and
// 198,899 tokens while the main thread was at 288 and 217,121. Under latest-wins the subagent
// speaks last, so only the declared role keeps the gauge on the conversation. This is the test
// that fails if the role filter is dropped in favour of recency alone.
func TestSessionContext_IgnoresASubagentThatSpokeLast(t *testing.T) {
	base := time.Now()
	evs := mainAgent("main", base, 288, 217_121)
	evs = append(evs, subagent("sub", base.Add(time.Minute), 186, 198_899)...)

	if got, want := PromptContextOf(evs), 217_121; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a subagent took the column", got, want)
	}
}

// BOTH FILTERS ARE NEEDED, AND THE ONE-SHOT IS WHY.
//
// A one-shot is issued by the same CLI as the conversation, so it declares itself MAIN — the role
// does not exclude it, and the manifest has to. And it is not small: the security monitor carries
// a rendered transcript, measured at 421,220 prompt tokens against a true context of 998,334, so
// a size threshold would not separate them either.
func TestSessionContext_AStatedOneShotIsStillAOneShot(t *testing.T) {
	base := time.Now()
	evs := mainAgent("main", base, 952, 400_249)
	evs = append(evs, roled(oneShot("monitor", base.Add(time.Minute), 421_220),
		AgentRoleMain)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a one-shot that declares itself main took the "+
			"column; the tool manifest is what excludes it", got, want)
	}
}

// A STATED TURN DISPLACES AN UNSTATED FIGURE OUTRIGHT, however much longer the unstated one was.
//
// This is a proxy upgrade mid-session: what came before was chosen by a rule that cannot tell a
// subagent from a conversation, so it is not evidence about either. Taking the first stated turn
// on the spot is what makes the fix arrive on the next event rather than on the next 500.
func TestSessionContext_AStatedTurnDisplacesAnUnstatedFigure(t *testing.T) {
	base := time.Now()
	evs := conversation("unstated", base, 2468, 999_623)
	evs = append(evs, mainAgent("stated", base.Add(time.Hour), 952, 400_249)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a stated turn must displace a figure chosen by "+
			"the fallback", got, want)
	}
}

// ONCE A SESSION STATES ROLES, AN UNSTATED ROW IS NOT A CANDIDATE — it cannot be checked against
// the filter that matters, and a session whose proxy states roles has no reason to produce one.
// Trusting it would let a single unstated row hand the column to whatever sent it.
func TestSessionContext_AStatedSessionIgnoresUnstatedRows(t *testing.T) {
	base := time.Now()
	evs := mainAgent("stated", base, 952, 400_249)
	evs = append(evs, conversation("unstated", base.Add(time.Hour), 2468, 999_623)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — an unstated row took the column from a stated "+
			"session", got, want)
	}
}

// EXACT-TIMESTAMP TIES NOW RESOLVE DETERMINISTICALLY, by the larger context.
//
// The sequential form fell through to arrival order here: two unstated turns with equal message
// counts and the same timestamp gave 100k or 200k depending purely on which was folded first.
// That is the only input on which this reformulation disagrees with its predecessor, and a
// restore that folded a session's events in any other order would have inherited the
// non-determinism.
func TestPromptContextFold_ExactTimestampTiesAreDeterministic(t *testing.T) {
	at := time.Now()
	small := conversation("s", at, 600, 100_000)
	large := conversation("l", at, 600, 200_000)

	var a, b PromptContextFold
	a.AddAll(append(append([]SessionEvent{}, small...), large...))
	b.AddAll(append(append([]SessionEvent{}, large...), small...))

	if a.Tokens() != b.Tokens() {
		t.Errorf("fold order changed the answer: %d vs %d", a.Tokens(), b.Tokens())
	}
	if got := a.Tokens(); got != 200_000 {
		t.Errorf("tie resolved to %d, want 200000 — the larger context wins", got)
	}
}

// permsOf enumerates every permutation of [0,n), so a commutativity claim is made over ALL orders
// rather than over the handful an author happened to type out. n is 5 or 6 here.
func permsOf(n int) [][]int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	var out [][]int
	var rec func(k int)
	rec = func(k int) {
		if k == n {
			out = append(out, append([]int(nil), idx...))
			return
		}
		for i := k; i < n; i++ {
			idx[k], idx[i] = idx[i], idx[k]
			rec(k + 1)
			idx[k], idx[i] = idx[i], idx[k]
		}
	}
	rec(0)
	return out
}

// THE LAWS THE RESTORE PATH WILL RELY ON. Stated as tests because the spec claims them: if the
// fold is not commutative and associative, replay order changes a persisted figure.
//
// TWO FIXTURES, BECAUSE ONE CANNOT REACH BOTH ARMS. The first version of this test carried a
// single stated candidate, so stated dominance decided every permutation on its own and neither
// arm's internal ordering was ever the comparator that settled the answer — the sequential latch
// this reformulation replaced would have passed it unchanged, which is no test of the
// reformulation at all. So: one fixture whose answer is settled INSIDE the stated arm by
// (at, tokens), and one with no stated candidate anywhere, whose answer is settled inside the
// unstated arm by (msgs, at, tokens).
//
// AND ASSOCIATIVITY, NOT ONLY COMMUTATIVITY. Permuting turns one at a time cannot distinguish the
// two laws: a restore combines whole FOLDS, so the grouped form — ((A,B),C) against (A,(B,C)) —
// is the shape the persisted figure actually depends on.
func TestPromptContextFold_IsACommutativeMonoid(t *testing.T) {
	base := time.Now()

	for _, tc := range []struct {
		name  string
		want  int
		turns [][]SessionEvent
	}{
		{
			// THE STATED ARM DECIDES HERE. Dominance over the unstated turn settles only which
			// CLASS wins; (at, tokens) has to settle the rest — m2 beats m1 on at, and beats m3
			// on tokens at an equal at.
			name: "stated candidates ranked among themselves",
			want: 400_249,
			turns: [][]SessionEvent{
				conversation("c1", base, 1491, 830_000),                // unstated, and much larger
				oneShot("o1", base.Add(time.Minute), 282_000),          // no manifest: makes no claim
				mainAgent("m1", base.Add(2*time.Minute), 108, 217_121), // stated, earlier
				subagent("s1", base.Add(3*time.Minute), 186, 198_899),  // states a role, filtered out
				mainAgent("m2", base.Add(5*time.Minute), 40, 400_249),  // stated and LATEST: wins
				mainAgent("m3", base.Add(5*time.Minute), 30, 300_000),  // ties m2 on at, smaller
			},
		},
		{
			// THE UNSTATED ARM DECIDES HERE, because nothing states a role at all — the case the
			// fixture above cannot reach, since one stated candidate is enough to end every
			// comparison on dominance. All three of its comparators are load-bearing: u3 loses on
			// msgs though it is both later and far larger, u4 loses on at at an equal count, and
			// u2 takes it from u1 on tokens at an equal (msgs, at).
			name: "no stated candidate at all",
			want: 500_000,
			turns: [][]SessionEvent{
				conversation("u1", base.Add(8*time.Minute), 1509, 400_000),
				conversation("u2", base.Add(8*time.Minute), 1509, 500_000),
				conversation("u3", base.Add(20*time.Minute), 1491, 999_000),
				conversation("u4", base.Add(time.Minute), 1509, 900_000),
				oneShot("o1", base.Add(30*time.Minute), 282_000),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns := tc.turns

			// foldOf folds whole turns, in the order given. n counts arrivals rather than
			// content, so it is reset out of every comparison.
			foldOf := func(order []int) PromptContextFold {
				var f PromptContextFold
				for _, i := range order {
					f.AddAll(turns[i])
				}
				f.ResetFolded()
				return f
			}

			// combine is the fold's binary operation applied to two FOLDS rather than to a fold
			// and an event: the same max over the same production comparator Add uses, with the
			// same zero-fold identity. It is not a second copy of the rule — better() is the
			// rule, and the assertion that a grouped combine equals the event-by-event fold is
			// what pins this helper to Add. A restore-then-continue needs exactly this operation.
			combine := func(a, b PromptContextFold) PromptContextFold {
				switch {
				case a.tokens == 0:
					return b
				case b.tokens == 0:
					return a
				case better(b.current(), a.current()):
					return b
				default:
					return a
				}
			}

			ident := make([]int, len(turns))
			for i := range ident {
				ident[i] = i
			}
			want := foldOf(ident)

			var zero PromptContextFold
			if want == zero {
				t.Fatal("fixture folds to the zero value; this test proves nothing")
			}
			if got := want.Tokens(); got != tc.want {
				t.Fatalf("fixture folds to %d, want %d — the fixture no longer exercises the arm "+
					"this case exists for; read the comment before changing the expectation",
					got, tc.want)
			}

			// Commutativity, over every permutation of the turns.
			for _, order := range permsOf(len(turns)) {
				if got := foldOf(order); got != want {
					t.Fatalf("order %v gave %+v, want %+v — the fold is not commutative",
						order, got, want)
				}
			}

			// Associativity, over every 3-way contiguous grouping of every permutation. The
			// second assertion is the one that keeps combine honest: a grouped combine must
			// agree with folding the same turns event by event.
			for _, order := range permsOf(len(turns)) {
				for i := 1; i < len(order)-1; i++ {
					for j := i + 1; j < len(order); j++ {
						a, b, c := foldOf(order[:i]), foldOf(order[i:j]), foldOf(order[j:])
						left, right := combine(combine(a, b), c), combine(a, combine(b, c))
						if left != right {
							t.Fatalf("order %v grouped at %d,%d: ((A,B),C) = %+v but "+
								"(A,(B,C)) = %+v — the fold is not associative",
								order, i, j, left, right)
						}
						if left != want {
							t.Fatalf("order %v grouped at %d,%d folded to %+v, want %+v — "+
								"combining group folds disagrees with folding the events",
								order, i, j, left, want)
						}
					}
				}
			}

			// Identity, both ways it gets used: folding no events, and combining a zero fold.
			withNothing := foldOf(ident)
			withNothing.AddAll(nil)
			withNothing.ResetFolded()
			if withNothing != want {
				t.Error("folding nothing changed the answer; zero is not the identity")
			}
			if combine(want, zero) != want || combine(zero, want) != want {
				t.Error("combining a zero fold changed the answer; zero is not the identity")
			}
		})
	}
}

// NIL IS "NOTHING KNOWN", and it must survive the round trip as an ABSENT field rather than as a
// zero object. contextGauge renders 0 as an em dash and a real figure as a track; a
// {"tokens":0} on the wire would assert a figure the server does not have.
func TestPromptContextFold_PublishIsNilWhenNothingIsKnown(t *testing.T) {
	var f PromptContextFold
	if got := f.Publish(); got != nil {
		t.Errorf("a zero fold published %+v, want nil", got)
	}
	f.AddAll(oneShot("o1", time.Now(), 282_000)) // no manifest: makes no claim
	if got := f.Publish(); got != nil {
		t.Errorf("a one-shot-only session published %+v, want nil", got)
	}
	f.AddAll(conversation("c1", time.Now(), 600, 500_000))
	got := f.Publish()
	if got == nil || got.Tokens != 500_000 {
		t.Fatalf("published %+v, want tokens=500000", got)
	}
}

// THE HOLE A BARE max LEFT OPEN, and the reason PromptContext carries Stated at all.
//
// abctl attaches to a proxy predating agentRole, folds unstated turns, and lands on the
// documented stale-fallback figure — 700k held from before a compaction. The proxy is then
// upgraded; abctl keeps running, because contextRun outlives everything but a pod switch. The
// new proxy publishes a STATED 200k, the correct latest main-agent turn. max(700k, 200k) pins
// the unsound figure permanently.
func TestMerge_StatedBeatsUnstatedHoweverLarge(t *testing.T) {
	stated := &PromptContext{Tokens: 200_000, Stated: true, At: time.Now()}
	unstated := &PromptContext{Tokens: 700_000, Stated: false, At: time.Now()}

	for _, tc := range []struct {
		name string
		a, b *PromptContext
	}{
		{"stated first", stated, unstated},
		{"unstated first", unstated, stated},
	} {
		if got := Merge(tc.a, tc.b); got.Tokens != 200_000 {
			t.Errorf("%s: merged to %d, want 200000 — a figure from a rule that cannot see "+
				"subagents is not evidence about the conversation", tc.name, got.Tokens)
		}
	}
}

// NIL IS THE IDENTITY, which is what lets the client merge without version detection: an old
// proxy sends no field, and that is a valid operand rather than a case to branch on.
func TestMerge_NilIsTheIdentity(t *testing.T) {
	x := &PromptContext{Tokens: 500_000, Stated: true, At: time.Now()}
	if got := Merge(nil, x); got != x {
		t.Errorf("Merge(nil, x) = %+v, want x", got)
	}
	if got := Merge(x, nil); got != x {
		t.Errorf("Merge(x, nil) = %+v, want x", got)
	}
	if got := Merge(nil, nil); got != nil {
		t.Errorf("Merge(nil, nil) = %+v, want nil", got)
	}
}

// Both stated: the later turn wins, which is the rule the column follows.
func TestMerge_BothStatedTakesTheLater(t *testing.T) {
	early := &PromptContext{Tokens: 900_000, Stated: true, At: time.Now()}
	late := &PromptContext{Tokens: 200_000, Stated: true, At: early.At.Add(time.Minute)}
	if got := Merge(early, late); got.Tokens != 200_000 {
		t.Errorf("merged to %d, want 200000 — latest-wins, not largest", got.Tokens)
	}
}

// THE COARSENING IS CONFINED TO msgs, and it does NOT degrade to "largest wins".
//
// With the message count unpublished, the timestamp is what remains of the fold's unstated order —
// and it is the comparator that matters. Taking the larger figure here would pin a pre-compaction
// context over the turn that followed it, which is the staleness this column exists to avoid; the
// first draft of the design specified exactly that and Task 4 caught it.
func TestMerge_NeitherStatedTakesTheLaterTurnNotTheLarger(t *testing.T) {
	at := time.Now()
	later := &PromptContext{Tokens: 100_000, At: at}
	earlierButBigger := &PromptContext{Tokens: 700_000, At: at.Add(-time.Hour)}

	for _, tc := range []struct {
		name string
		a, b *PromptContext
	}{
		{"later first", later, earlierButBigger},
		{"bigger first", earlierButBigger, later},
	} {
		if got := Merge(tc.a, tc.b); got.Tokens != 100_000 {
			t.Errorf("%s: merged to %d, want 100000 — the later turn wins; taking the larger "+
				"figure would hold a pre-compaction context forever", tc.name, got.Tokens)
		}
	}

	// Only a genuine timestamp tie falls through to the larger figure.
	tied := &PromptContext{Tokens: 500_000, At: at}
	if got := Merge(later, tied); got.Tokens != 500_000 {
		t.Errorf("on an exact tie merged to %d, want 500000", got.Tokens)
	}
}

// THE SECOND MONOID, which the spec claims separately from the fold's. Merge is what the client
// and any future restore call, so its laws are load-bearing independently.
//
// THE VALUE SET SPANS BOTH CLASSES on purpose — two stated members and two unstated ones, plus
// nil — so the laws are exercised across the dominance arm AND inside each class's own (At,
// Tokens) ordering, rather than only where dominance settles it.
func TestMerge_IsACommutativeMonoid(t *testing.T) {
	at := time.Now()
	vals := []*PromptContext{
		nil,
		{Tokens: 100_000, At: at},
		{Tokens: 700_000, At: at.Add(-time.Hour)},
		{Tokens: 200_000, Stated: true, At: at},
		{Tokens: 900_000, Stated: true, At: at.Add(-time.Minute)},
	}
	for _, x := range vals {
		for _, y := range vals {
			if Merge(x, y) != Merge(y, x) {
				t.Errorf("not commutative for %+v, %+v", x, y)
			}
			for _, z := range vals {
				if Merge(Merge(x, y), z) != Merge(x, Merge(y, z)) {
					t.Errorf("not associative for %+v, %+v, %+v", x, y, z)
				}
			}
		}
	}
}

// Nothing to say is zero, which the gauge renders as a dash rather than an empty track.
func TestSessionContext_ZeroWhenNothingCanBeSaid(t *testing.T) {
	base := time.Now()
	for _, tc := range []struct {
		name   string
		events []SessionEvent
	}{
		{"no events at all", nil},
		{"no inference on any event", []SessionEvent{
			{Phase: SessionRequest}, {Phase: SessionResponse},
		}},
		// Every request a one-shot: there is no conversation to report on, and reporting a
		// one-shot's own context is the defect this rule exists to fix.
		{"one-shots only", append(oneShot("o1", base, 60_000), oneShot("o2", base, 61_000)...)},
		// A conversation whose response reported no token counts at all — through exchange
		// directly, because conversation() takes a context and cannot express zero.
		{"a conversation with no prompt counts", exchange("c1", base, 40, 27, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptContextOf(tc.events); got != 0 {
				t.Errorf("PromptContextOf = %d, want 0", got)
			}
		})
	}
}
