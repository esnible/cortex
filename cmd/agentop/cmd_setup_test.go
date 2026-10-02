package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

const stopBackgroundLine = "  Cortex runs without a supervisor here; stop it with: kill $(cat ~/.cortex/proxy.pid)\n"

// Beside a supervised Cortex the service step is done, with advice, while
// env.unsupervised stays true: only a proxy the run started gets the stop line.
func TestSetupEndingNamesTheStopCommandOnlyForAProxyItStarted(t *testing.T) {
	for _, started := range []bool{false, true} {
		var out bytes.Buffer
		env := &setupEnv{home: "/h", binDir: "/h/.local/bin", freshInstall: true, binOnPath: true,
			unsupervised: true, startedBackground: started}
		printSetupEnding(env, checklist.New(&out, false), 0)
		if got := strings.Contains(out.String(), stopBackgroundLine); got != started {
			t.Errorf("started a background proxy=%v, stop line printed=%v:\n%s", started, got, out.String())
		}
	}
}

func TestParseSetupFlagsTakesTheFrozenInterface(t *testing.T) {
	o, err := parseSetupFlags([]string{"--from", "/s", "--claude-code", "-y", "--no-service", "--install-only",
		"--no-modify-path", "--restart", "--handoff-bytes=2048", "--handoff-seconds=3", "--local"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	want := setupOptions{from: "/s", claudeCode: true, yes: true, noService: true, installOnly: true,
		noModifyPath: true, restart: true, handoffBytes: 2048, handoffSeconds: 3}
	if o != want {
		t.Errorf("parsed %+v\n  want %+v", o, want)
	}
	if !o.fromInstaller() {
		t.Error("handoff flags did not mark installer mode")
	}
}

func TestSetupConfirmFromDefaultsToYes(t *testing.T) {
	for in, want := range map[string]bool{"\n": true, "y\n": true, "YES\n": true, "n\n": false, "no\n": false, "": false, "later\n": false} {
		var w strings.Builder
		if got := setupConfirmFrom(strings.NewReader(in), &w); got != want {
			t.Errorf("answer %q → %v, want %v", in, got, want)
		}
		if w.String() != "  Continue? [Y/n] " {
			t.Errorf("prompt = %q", w.String())
		}
	}
}

func TestIsStagingDir(t *testing.T) {
	home := t.TempDir()
	staged := t.TempDir() // under os.TempDir()
	if !isStagingDir(staged, home) {
		t.Error("a dir under TMPDIR was not a staging dir")
	}
	if isStagingDir(os.TempDir(), home) {
		t.Error("TMPDIR itself read as a staging dir")
	}

	// A test's HOME is itself under TMPDIR, so everything in it reads as staged.
	// The rest move the system roots to a dir of their own.
	base := t.TempDir()
	sys, home := filepath.Join(base, "tmp"), filepath.Join(base, "home")
	saved := systemTempRoots
	systemTempRoots = func() []string { return []string{sys} }
	t.Cleanup(func() { systemTempRoots = saved })
	local := filepath.Join(home, ".cortex", "tmp", "stage")
	repo := filepath.Join(home, "src", "cortex", "bin")
	sibling := filepath.Join(base, "tmp-other") // shares the root's prefix, not its tree
	for _, d := range []string{filepath.Join(sys, "stage"), local, repo, sibling} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if !isStagingDir(filepath.Join(sys, "stage"), home) {
		t.Error("a dir under the system temp root was not a staging dir")
	}
	if !isStagingDir(local, home) {
		t.Error("~/.cortex/tmp/stage was not a staging dir")
	}
	if isStagingDir(repo, home) {
		t.Error("a source tree's bin dir read as a staging dir — setup would delete it")
	}
	if isStagingDir(filepath.Dir(local), home) {
		t.Error("~/.cortex/tmp itself read as a staging dir")
	}
	if isStagingDir(sibling, home) {
		t.Error("a sibling sharing the temp root's prefix read as a staging dir")
	}
}

func TestDownloadSizeNeverReadsZeroMB(t *testing.T) {
	for n, want := range map[int64]string{
		512:        "512 B",
		2048:       "2.0 kB",
		999_949:    "999.9 kB",
		999_950:    "1.0 MB",
		24_000_000: "24.0 MB",
	} {
		if got := downloadSize(n); got != want {
			t.Errorf("downloadSize(%d) = %q, want %q", n, got, want)
		}
	}
}
