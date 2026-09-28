package session

import (
	"strings"
	"unicode"

	"github.com/rossoctl/cortex/core/capabilities"
	"github.com/rossoctl/cortex/core/pipeline"
)

const emptySessionTitle = "(empty session)"

// maxTitleLen caps a title, in RUNES. A user message is unbounded — the largest measured
// event carries ~190KB — and this is served on every /v1/sessions response, whose size is
// already the constraint that `?before=` exists for.
//
// NOT A DISPLAY-COLUMN BUDGET: 80 runes of CJK occupy 160 columns, so a client still has to
// truncate by width. Runes, not bytes, so a multi-byte title is not cut mid-character.
//
// Deliberately the same 80 as core/observe/claude.MaxTitleLen, and deliberately NOT that
// constant: that one is documented as the cap on a HARVESTED title and is exported so
// cmd/abctl's tests can hold a contract with the harvester. Importing it here would make the
// session store depend on the transcript harvester for a number, and would couple two caps
// that answer to different consumers. If they ever need to differ, they can.
const maxTitleLen = 80

// Title sources, best first. Within one rank the last match in event order wins.
const (
	rankRename    = 0
	rankUserQuery = 1
	rankUserMsg   = 2
	rankNone      = 3
)

const renamePrefix = "<command-name>/rename</command-name>"

// sessionTitle names a session from its own events: a /rename envelope if one is present,
// else a <user_query> block, else the last user message carrying text.
//
// SANITIZATION IS PART OF ACCEPTANCE, not a final flourish. A candidate that sanitizes to ""
// names nothing, and accepting one used to lock its rank and discard the real title behind it:
// sessionTitle([userEvent("the real ask"), userEvent("   ")]) answered "". The raw-content
// emptiness tests inside titleFrom cannot see this — whitespace and control runes are non-empty
// until sanitizeTitle folds them away — so the test has to happen after the fold.
//
// HERE RATHER THAN PER CANDIDATE, which is a cost decision: sanitizing every candidate as it is
// produced measured 326µs→651µs on BenchmarkListSessions_Title/user-text, because rank 2 is
// always beatable and so every user message of every event becomes a candidate. Sanitizing at
// the moment a candidate would REPLACE the incumbent runs it once per improvement — at most
// three times for a session, since rank only ever descends.
func sessionTitle(events []pipeline.SessionEvent) string {
	if len(events) == 0 {
		return emptySessionTitle
	}
	best, bestRank := "", rankNone
	for i := len(events) - 1; i >= 0; i-- {
		// Reverse, so the FIRST hit at a rank is the LAST one in event order — hence
		// `>=` rather than `>`: an earlier event must not displace it at equal rank.
		r, t := titleCandidate(&events[i])
		if t == "" || r >= bestRank {
			continue
		}
		// Fold BEFORE accepting, so a whitespace-only candidate does not claim the rank. It is
		// not merely that the title would be blank — claiming rank 0 would also break the walk
		// and hide a real /rename in an earlier event.
		t = sanitizeTitle(t)
		if t == "" {
			continue
		}
		best, bestRank = t, r
		if bestRank == rankRename {
			break // nothing can outrank it
		}
	}
	if bestRank == rankNone {
		return ""
	}
	return best
}

// titleCandidate returns the best-ranked title this event's user messages offer.
//
// ONE REVERSE SCAN, NO RETRIES. The previous shape picked a candidate by quickRank and retried
// the whole scan whenever titleFrom refused the pick, which was quadratic in messages-per-event
// on exactly the reminder-bearing path this file added: measured 31.9µs at n=50 rising to 7.20ms
// at n=800, a clean 4x per doubling and ~1088x the cost of the same event with no demotions. It
// runs under the store's read lock, whose writer side is Append on the proxy's request path.
//
// Reverse, so the first acceptance at any rank is the LAST such message in event order — the
// rule stated at the top of this file. That replaces the old forward loop's `<=` tie-break,
// which was subtly wrong in the other direction: see the /rename case in #6 below.
//
// TWO CLASSES OF MESSAGE, because quickRank is only an upper bound and a bound is not a rank:
//
//   - quickRank says rank 0 or 1 (rare): the guess is SETTLED EAGERLY by calling titleFrom, and
//     whatever rank comes back is used. A guess of 0 or 1 that titleFrom demotes to 2 must not
//     be recorded as 0 or 1 — that is #4 (an unterminated <user_query> guesses 1 and settles at
//     2) and #5 (a demoted-but-non-empty pick discarded a better-ranked message in the same
//     event). Eager settling costs one titleFrom per rank-0/1 message, and those are rare
//     precisely because the tags are.
//   - quickRank says rank 2 (the majority): DEFERRED. Calling titleFrom here is what cost +70%,
//     so the scan only remembers the index and settles it at the end if nothing better appeared.
//     A deferred rank-2 message can only settle at 2 or at rankNone, never better — a message
//     with no <user_query> and no reminder-hidden /rename prefix has nothing for the strip to
//     uncover. That is the one direction of quickRank's bound that does hold, and it is the only
//     direction this relies on.
func titleCandidate(e *pipeline.SessionEvent) (int, string) {
	if e.Inference == nil {
		return rankNone, ""
	}
	msgs := e.Inference.Messages
	bestRank, bestTitle := rankNone, ""
	// deferredHead is the index one past the newest rank-2 guess not yet settled. The deferred
	// candidates are scanned lazily below, latest first, because a rank-2 guess can still settle
	// at rankNone — a message that is nothing but reminders guesses 2 and names nothing — and the
	// next one back must then get its turn. Storing a single index lost exactly that case.
	deferredHead := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != capabilities.RoleUser {
			continue
		}
		switch quickRank(msgs[i].Content) {
		case rankNone:
			continue
		case rankUserMsg:
			if deferredHead < 0 {
				deferredHead = i + 1
			}
			continue
		}
		// A rank-0 or rank-1 guess. Settle it now; titleFrom may demote it to 2 or refuse it.
		r, t := titleFrom(msgs[i].Content)
		if t == "" || r >= bestRank {
			continue
		}
		bestRank, bestTitle = r, t
		if bestRank == rankRename {
			return bestRank, bestTitle // nothing in this event can outrank it
		}
	}
	// Settle the deferred rank-2 candidates only if nothing better was found. A settled rank-2
	// from a demoted rank-0/1 guess is no better than these, and these are LATER in event order
	// whenever both exist — the scan is reverse, so a deferred index recorded first was seen
	// later in event order. Prefer them, per the last-match rule.
	//
	// ONE PASS TOTAL ACROSS BOTH LOOPS, which is what keeps this linear: each message is visited
	// at most once here, walking back from the newest deferred candidate, and the walk stops at
	// the first one that names something.
	if deferredHead >= 0 && bestRank >= rankUserMsg {
		for i := deferredHead - 1; i >= 0; i-- {
			// No quickRank here. Re-classifying costs a second scan of every message, which is
			// what a first cut of this did and it showed: +45% on prose. titleFrom is the
			// authority anyway, and a message it names is a valid rank-2 candidate whatever the
			// guess would have been — a rank-0/1 message that reaches this point was already
			// settled and rejected by the loop above.
			if msgs[i].Role != capabilities.RoleUser {
				continue
			}
			if r, t := titleFrom(msgs[i].Content); t != "" {
				return r, t
			}
			// Named nothing (nothing but reminders). Keep walking back — an earlier message in
			// this same event may still title it, which is the within-event fall-through
			// TestSessionTitle_DemotedPickFallsBackWithinEvent pins.
		}
	}
	return bestRank, bestTitle
}

// quickRank guesses a message's rank without stripping reminders, cheaply enough to run on
// every message of every event.
//
// A LOWER BOUND ON COST, NOT A RANK. It is reliable in ONE direction only: a message it calls
// rank 2 cannot settle better than 2, because it contains neither <user_query> nor a /rename
// prefix anywhere, and stripping reminders cannot introduce either. Callers may defer such a
// message and settle it later.
//
// IN THE OTHER DIRECTION IT OVER-PROMISES, and the comment here used to claim otherwise —
// "never worse" was false in two ways, both now covered by tests:
//
//   - rank 1 for an UNTERMINATED <user_query>: Contains finds the open tag, but between() needs
//     a closing tag and returns "", so titleFrom falls through to rank 2.
//   - rank 0 or 1 for a message whose payload is empty once extracted (`<command-args></command-args>`,
//     `<user_query> </user_query>`), which settles at rankNone.
//
// So a rank-0 or rank-1 guess MUST be settled by titleFrom before its rank is recorded.
//
// Contains for BOTH tags, not HasPrefix for the /rename envelope. HasPrefix systematically
// under-rated a reminder-prefixed /rename to rank 2, which is how a later genuine /rename lost
// to an earlier one inside the same event. The prefix test still lives in titleFrom, where the
// reminders have already been stripped and a prefix is the right question.
func quickRank(content string) int {
	if content == "" {
		return rankNone
	}
	if strings.HasPrefix(content, renamePrefix) {
		return rankRename
	}
	if strings.Contains(content, "<user_query>") {
		return rankUserQuery
	}
	// A REMINDER AT THE FRONT is the only thing that can displace a /rename envelope from the
	// front, which is why the HasPrefix above misses one and this exists. Without it a later
	// reminder-prefixed /rename lost to an earlier bare one inside the same event, against this
	// file's stated last-match-wins rule.
	//
	// BOTH TESTS ARE ANCHORED, and that is a cost decision as much as a correctness one. An
	// unanchored Contains for the envelope — 37 bytes, scanned over the whole message to answer
	// "no" for almost every message — cost +45% on 500-byte prose (325µs→472µs on a 300-turn
	// session), and gating it on an unanchored Contains for the reminder cost the same, because
	// the gate scans the whole message too. Anchoring the gate restores parity. `<` is not a
	// usable discriminator: ordinary prose carrying a code snippet has one, and an IndexByte('<')
	// fast path measured 906µs on exactly that fixture while looking like 248µs on a fixture
	// whose prose had no '<' at all.
	//
	// What this deliberately does NOT catch is an envelope behind PROSE rather than behind a
	// reminder ("as I said, <command-name>/rename</command-name>…"). titleFrom tests a prefix too,
	// so such a message never named a session at rank 0 under any version of this code; quickRank
	// guessing rank 2 for it agrees with what titleFrom would settle on.
	if strings.HasPrefix(content, reminderOpen) && strings.Contains(content, renamePrefix) {
		return rankRename
	}
	return rankUserMsg
}

// titleFrom ranks one user message and returns the title it offers.
//
// <system-reminder> blocks are excised first, before every rank arm rather than only the
// rank-2 one. All three arms need it and two are wrong without it: a reminder nested INSIDE a
// <user_query> would ride along into the title, and a <user_query> nested inside a REMINDER —
// one quoting an earlier turn is enough — would be mistaken for the real ask. The /rename arm
// tests a PREFIX, so a reminder in front of an envelope hides it entirely.
//
// THE STRIP IS NOT FREE, which is why titleCandidate does not call this on every message.
// Excising a tag that can sit anywhere means scanning the whole message and building a new
// string; calling it per message cost +70% on BenchmarkListSessions_Title/user-text, which walks
// ~45k messages of ~500 bytes. quickRank screens instead, and this settles the rank of the few
// messages whose guess is not reliable — see titleCandidate for which those are.
func titleFrom(content string) (int, string) {
	content = stripReminders(content)
	if strings.HasPrefix(content, renamePrefix) {
		// A /rename envelope is machinery, not prose, so it yields its ARGUMENT OR NOTHING
		// and never falls through to a lower rank. Falling through was the first attempt and
		// it was worse than the bug it avoided: the generic rank-2 arm below then took the
		// whole literal "<command-name>/rename</command-name><command-args></command-args>"
		// as the title. Returning rankNone lets the walk reach a real title behind it, which
		// is the thing an argument-less /rename must not defeat.
		if t := between(content, "<command-args>", "</command-args>"); t != "" {
			return rankRename, t
		}
		return rankNone, ""
	}
	if t := between(content, "<user_query>", "</user_query>"); t != "" {
		return rankUserQuery, t
	}
	// A user-role message whose payload was a tool result or an image flattens to "" (see
	// pipeline.InferenceMessage.ContentBytes) — it is the last message of every agentic turn
	// and must not be chosen as the title. A message that was nothing BUT reminders reaches
	// here as "" too, and falls through by the same route: rankNone, so the walk finds a real
	// title behind it.
	if content != "" {
		return rankUserMsg, content
	}
	return rankNone, ""
}

const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
)

// stripReminders excises every <system-reminder>…</system-reminder> span and returns what
// surrounds them. The harness injects these blocks into the user turn it is attached to, so
// without this the block itself becomes the title and the real prompt — which sits after it —
// is never reached.
//
// SPLICES rather than keeping only the tail: of the user turns in local transcripts that carry
// a reminder alongside real prose, 67 of 68 have text on BOTH sides of the block. The seam is
// left as-is; sanitizeTitle folds "before\n" + "\nafter" to one space later.
//
// Called once at the top of titleFrom, so every rank arm sees content with the blocks already
// gone — see the comment there for why that matters to all three and not just rank 2.
//
// NESTED BLOCKS ARE MATCHED BY DEPTH, so an inner block does not terminate the outer one. The
// non-nesting version leaked the outer closing tag and dropped the text between the two open
// tags: "prose <sr>a<sr>b</sr> outer-tail</sr> real ask" left
// "prose  outer-tail</system-reminder> real ask" — a stray tag in a title, and the reason to
// track depth rather than pair each open with the next close. "<sr>a<sr>b</sr>c" now excises the
// whole span and yields "", where before it yielded "c".
//
// An UNTERMINATED open tag is left alone, matching the policy between() fixes for <user_query>:
// the tag name can appear in ordinary prose (someone discussing this very code), so the whole
// message stays a reasonable title rather than being cut at a tag that closes nothing. With depth
// tracking that extends to a block whose closes do not balance its opens: the remainder from the
// outermost unbalanced open is kept verbatim, tags and all.
//
// Matched literally in lowercase, which is the only form the harness emits into an inference
// payload. core/observe/claude canonicalises tag names because a capitalised variant reached
// its filter from a transcript; that is a different producer, and importing its machinery here
// would be the wrong dependency direction for a string scan.
//
// Runs on RAW content, before sanitizeTitle — folding U+0085 to a space would turn
// "<\u0085system-reminder>" into a non-tag and hide the block from this scan.
func stripReminders(s string) string {
	// The common case is no reminder at all, and this runs on every message of every event: a
	// session whose best rank is 2 can never terminate the reverse walk early, since rank 2 is
	// always beatable. One Index over the message beats building a string for each.
	if !strings.Contains(s, reminderOpen) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := strings.Index(s, reminderOpen)
		if i < 0 {
			break
		}
		// Walk forward from the open tag, tracking depth, so a nested open tag consumes its own
		// close rather than letting the inner close terminate the outer block.
		depth, rest := 1, s[i+len(reminderOpen):]
		for depth > 0 {
			nextOpen := strings.Index(rest, reminderOpen)
			nextClose := strings.Index(rest, reminderClose)
			if nextClose < 0 {
				break // unbalanced from here on
			}
			if nextOpen >= 0 && nextOpen < nextClose {
				depth++
				rest = rest[nextOpen+len(reminderOpen):]
				continue
			}
			depth--
			rest = rest[nextClose+len(reminderClose):]
		}
		if depth > 0 {
			break // unterminated: keep the remainder verbatim, tags and all
		}
		b.WriteString(s[:i])
		s = rest
	}
	b.WriteString(s)
	return b.String()
}

// between returns the text bracketed by open and closing, or "" if either is absent.
//
// Hand-parsed rather than a regexp: matching a tag pair wants a backreference, which RE2
// lacks. A local copy of core/observe/claude's unexported helper — importing a transcript
// harvester into the session store for a string helper is the wrong dependency direction.
// Unlike the original this one does not TrimSpace; sanitizeTitle trims at the end.
func between(s, open, closing string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// sanitizeTitle folds a title to a single line: every whitespace or control rune becomes at
// most one U+0020, and the result is clipped to maxTitleLen runes.
//
// Applied ONCE, to the winner — not per candidate, which would be O(content) on every hit.
// Rank never depends on whitespace, so comparing raw strings is safe. And applied to the
// EXTRACTED title, never the haystack: folding U+0085 to a space turns "<\u0085command-name>"
// into "< command-name>", which is no longer a tag.
func sanitizeTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // drops leading whitespace
	for _, r := range s {
		// TWO PREDICATES, and each catches runes the other misses. pipeline.IsControlRune covers
		// C0/C1/DEL plus the BIDI and zero-width runes that reorder or hide their surroundings.
		// unicode.IsSpace covers \n\r\t, the exotic spaces (U+00A0, U+3000, the U+2000 block) —
		// and, most importantly for a single-line guarantee, U+2028 LINE SEPARATOR and U+2029
		// PARAGRAPH SEPARATOR, which are the runes most likely to break a title across lines and
		// are NOT control runes. Both FOLD rather than drop, so "one\ntwo" does not become
		// "onetwo".
		if unicode.IsSpace(r) || pipeline.IsControlRune(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return clipTitle(strings.TrimRight(b.String(), " "))
}

// clipTitle caps s at maxTitleLen runes.
//
// A PLAIN RUNE CUT, which can sever a combining mark from its base character — "e" plus a
// combining acute cut between the two leaves a dangling accent on whatever now precedes it.
// core/observe/claude's clipTitle is exempt because its scrubRunes drops every combining
// mark, modifier and regional indicator first; this sanitizer keeps them, so "café" survives
// as "café" and the cut is the price. Measuring in grapheme clusters instead needs a
// segmentation library core/ does not have, and the cut lands at rune 80 of a title that is
// almost never that long.
//
// TrimRight runs again here because the cut can land just after a folded space.
func clipTitle(s string) string {
	r := []rune(s)
	if len(r) <= maxTitleLen {
		return s
	}
	return strings.TrimRight(string(r[:maxTitleLen]), " ")
}
