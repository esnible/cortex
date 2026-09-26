package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// assetPath is the committed asset the README points at. GitHub cannot build the
// SVG, so it has to be in the tree.
const assetPath = "../../docs/assets/cortex-demo.svg"

// usageState matches the drawn body of the usage-chart beat, which is dropped
// before comparing.
//
// The chart plots a ten-minute window ENDING NOW on a continuous axis, so every
// bar's column is a function of the current second and the whole plot slides
// between runs. Minute-aligning the fixture does not help: the axis is continuous,
// not bucket-indexed. That is data placement rather than a UI contract, so it is
// excluded here and the pane's contract is asserted instead by
// TestCapture_EveryBeatShowsWhatTheStoryboardClaims, which checks its summary
// labels render. The state's own keyframes are outside this match and stay
// compared, so the beat cannot silently lose its slot on the timeline.
var usageState = regexp.MustCompile(`(?s)<g class="st st\d+[^"]*lbl-usage[^"]*">.*?</g>`)

// normalize elides the usage chart and nothing else.
//
// An earlier version also masked every digit run, as a backstop for ages whose
// length changes with capture speed. That went too far: it made CONTEXT(1M) and
// CONTEXT(2M) compare equal, so the check could not see a renamed column, a
// reformatted figure or a changed count — most of what it exists to catch. The
// capturer now canonicalises ages and timestamps at fixed width instead, which
// removes the need for masking entirely.
func normalize(b []byte) string {
	return usageState.ReplaceAllString(string(b), `<g class="lbl-usage-elided"/>`)
}

// TestCommittedAssetIsCurrent fails when the committed SVG no longer matches what
// demo.yaml and the current abctl produce.
//
// A fabricated demo is only worth having if it cannot quietly drift from the
// product it claims to show; this converts a TUI change that would have made the
// asset a lie into a failing check.
func TestCommittedAssetIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("read committed asset: %v", err)
	}

	fresh := filepath.Join(t.TempDir(), "fresh.svg")
	if err := run("demo.yaml", fresh); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	got, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}

	if normalize(committed) != normalize(got) {
		t.Errorf(`%s is stale.

Regenerate it:
    go -C scripts/readme-demo run .

(committed %d bytes, regenerated %d bytes)`, assetPath, len(committed), len(got))
	}
}

// TestCommittedAssetIsWithinBudget keeps the README from growing a megabyte of
// markup unnoticed.
func TestCommittedAssetIsWithinBudget(t *testing.T) {
	info, err := os.Stat(assetPath)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 500 << 10
	if info.Size() > budget {
		t.Errorf("asset is %d KB, over the %d KB budget", info.Size()>>10, budget>>10)
	}
}
