package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathStepPlans(t *testing.T) {
	t.Run("already on PATH", func(t *testing.T) {
		env := newTestSetupEnv(t)
		env.pathEnv = "/usr/bin:" + env.binDir
		if p, _ := (pathStep{}).plan(env); !p.done || !env.binOnPath {
			t.Errorf("plan = %+v", p)
		}
	})
	t.Run("--no-modify-path advises", func(t *testing.T) {
		env := newTestSetupEnv(t)
		env.opts.noModifyPath = true
		p, _ := pathStep{}.plan(env)
		if !p.done || p.advice == nil || env.binOnPathNewTerms {
			t.Errorf("plan = %+v", p)
		}
	})
	t.Run("a shell with no profile advises", func(t *testing.T) {
		env := newTestSetupEnv(t)
		env.shell = "/usr/bin/fish"
		if p, _ := (pathStep{}).plan(env); !p.done || p.advice == nil {
			t.Errorf("plan = %+v", p)
		}
	})
	t.Run("the marker already present", func(t *testing.T) {
		env := newTestSetupEnv(t)
		writeExe(t, filepath.Join(env.home, ".zshrc"), pathBlock(env.binDir))
		if p, _ := (pathStep{}).plan(env); !p.done || p.advice != nil || !env.binOnPathNewTerms {
			t.Errorf("plan = %+v", p)
		}
	})
	t.Run("an edit", func(t *testing.T) {
		env := newTestSetupEnv(t)
		p, prob := pathStep{}.plan(env)
		if prob != nil || p.done || p.where != "~/.zshrc (2 marked lines)" {
			t.Errorf("plan = %+v %v", p, prob)
		}
	})
	// Nix home-manager links ~/.zshrc into the read-only /nix/store. install.sh
	// warns and carries on there, so setup advises rather than refusing to run.
	t.Run("a read-only profile advises", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write a 0444 file")
		}
		env := newTestSetupEnv(t)
		store := filepath.Join(env.home, "store", "zshrc")
		writeExe(t, store, "x\n")
		if err := os.Chmod(store, 0o444); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, store, filepath.Join(env.home, ".zshrc"))
		p, prob := pathStep{}.plan(env)
		if prob != nil {
			t.Fatalf("a read-only profile is a problem, not advice: %v", prob.reason)
		}
		if !p.done || p.advice == nil {
			t.Fatalf("plan = %+v, want done with advice", p)
		}
		if want := "~/.zshrc is not writable (permission denied); add ~/.local/bin to PATH yourself"; p.advice.reason != want {
			t.Errorf("advice = %q, want %q", p.advice.reason, want)
		}
		if len(p.advice.fix) != 1 || p.advice.fix[0] != exportLine(env.binDir) {
			t.Errorf("advice fix = %q, want the export line", p.advice.fix)
		}
		if env.binOnPathNewTerms || env.pathProfile != "" {
			t.Errorf("a read-only profile set binOnPathNewTerms=%v pathProfile=%q", env.binOnPathNewTerms, env.pathProfile)
		}
	})
	// apply renames a temp file into the target's directory, and writes the .bak
	// beside the profile, so a writable file in a read-only directory is advice too.
	for _, tc := range []struct{ name, readOnly string }{
		{"a writable link target in a read-only directory advises", "dotfiles"},
		{"a link from a read-only home advises", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root can write a read-only directory")
			}
			env := newTestSetupEnv(t)
			real := filepath.Join(env.home, "dotfiles", "zshrc")
			if tc.readOnly == "" {
				real = filepath.Join(t.TempDir(), "dotfiles", "zshrc")
			}
			writeExe(t, real, "x\n")
			mustSymlink(t, real, filepath.Join(env.home, ".zshrc"))
			dir := filepath.Join(env.home, tc.readOnly)
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			p, prob := pathStep{}.plan(env)
			if prob != nil || !p.done || p.advice == nil {
				t.Fatalf("plan = %+v %v, want done with advice", p, prob)
			}
			if !strings.Contains(p.advice.reason, " is not writable (permission denied); add ~/.local/bin to PATH yourself") {
				t.Errorf("advice = %q, want checkWritable's reason", p.advice.reason)
			}
		})
	}
}

func TestPathStepAppendsTheBlockAndUndoes(t *testing.T) {
	env := newTestSetupEnv(t)
	rc := filepath.Join(env.home, ".zshrc")
	if err := os.WriteFile(rc, []byte("alias ll='ls -l'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, prob := (pathStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	_, u, err := pathStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(rc)
	if string(b) != "alias ll='ls -l'\n"+pathBlock(env.binDir) {
		t.Errorf("profile = %q", b)
	}
	if bak, _ := os.ReadFile(rc + ".bak"); string(bak) != "alias ll='ls -l'\n" {
		t.Errorf(".bak = %q", bak)
	}
	if fi, _ := os.Stat(rc); fi.Mode().Perm() != 0o640 {
		t.Errorf("profile mode = %v, want 0640 kept", fi.Mode().Perm())
	}
	if !strings.Contains(u.manual, "~/.zshrc") {
		t.Errorf("undo manual = %q, want it to name ~/.zshrc", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(rc); string(b) != "alias ll='ls -l'\n" {
		t.Errorf("undo left %q", b)
	}
	if _, err := os.Stat(rc + ".bak"); err == nil {
		t.Error("undo left the .bak it created")
	}
}

// A write the directory refused changed nothing, so the undo puts back only the
// .bak it wrote: rewriting the unchanged profile there would fail as well.
func TestPathStepUndoAfterARefusedWriteRestoresOnlyWhatItWrote(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory")
	}
	env := newTestSetupEnv(t)
	dots := filepath.Join(env.home, "dotfiles")
	writeExe(t, filepath.Join(dots, "zshrc"), "x\n")
	rc := filepath.Join(env.home, ".zshrc")
	mustSymlink(t, filepath.Join(dots, "zshrc"), rc)
	if err := os.Chmod(dots, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dots, 0o755) })
	env.pathProfile = rc // past plan, which advises on the read-only directory
	_, u, err := pathStep{}.apply(env, nil)
	if err == nil {
		t.Fatal("writing into a read-only directory succeeded")
	}
	if _, err := os.Lstat(rc + ".bak"); err != nil {
		t.Fatalf("fixture: the .bak was not written before the refused write: %v", err)
	}
	if err := u.fn(); err != nil {
		t.Errorf("undo after a refused write reported %v, though nothing it wrote is left", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dots, "zshrc")); string(b) != "x\n" {
		t.Errorf("profile = %q", b)
	}
	if _, err := os.Lstat(rc + ".bak"); err == nil {
		t.Error("undo left the .bak it wrote")
	}
}

func TestPathStepCreatesAMissingProfileAndUndoRemovesIt(t *testing.T) {
	env := newTestSetupEnv(t)
	if _, prob := (pathStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	_, u, err := pathStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(env.home, ".zshrc")
	if b, _ := os.ReadFile(rc); string(b) != pathBlock(env.binDir) {
		t.Errorf("new profile = %q", b)
	}
	if _, err := os.Stat(rc + ".bak"); err == nil {
		t.Error("a .bak was made of a file that did not exist")
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rc); err == nil {
		t.Error("undo left a profile that did not exist")
	}
}

func TestPathStepWritesThroughASymlinkedProfile(t *testing.T) {
	env := newTestSetupEnv(t)
	real := filepath.Join(env.home, "dotfiles", "zshrc")
	writeExe(t, real, "x\n")
	rc := filepath.Join(env.home, ".zshrc")
	if err := os.Symlink(real, rc); err != nil {
		t.Fatal(err)
	}
	if _, prob := (pathStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	if _, _, err := (pathStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(rc); err != nil || l != real {
		t.Errorf("the link was replaced: %q %v", l, err)
	}
	if b, _ := os.ReadFile(real); string(b) != "x\n"+pathBlock(env.binDir) {
		t.Errorf("target = %q", b)
	}
}

// install.sh's >> on a link to a missing file creates that file through the
// link, and its cp makes no .bak of a file that is not there.
func TestPathStepAppendsThroughADanglingProfileLink(t *testing.T) {
	// A relative target resolved against the working directory instead of the
	// link's own directory would land here, not in the test's source tree.
	t.Chdir(t.TempDir())
	t.Run("a link to a missing file", func(t *testing.T) {
		env := newTestSetupEnv(t)
		rc := filepath.Join(env.home, ".zshrc")
		mustMkdir(t, filepath.Join(env.home, "dotfiles"))
		mustSymlink(t, "dotfiles/zshrc", rc)
		checkAppendsThroughLinks(t, env, filepath.Join(env.home, "dotfiles", "zshrc"),
			map[string]string{rc: "dotfiles/zshrc"})
	})
	t.Run("a link to a dangling link", func(t *testing.T) {
		env := newTestSetupEnv(t)
		rc := filepath.Join(env.home, ".zshrc")
		mid := filepath.Join(env.home, "dotfiles", "zshrc")
		mustMkdir(t, filepath.Join(env.home, "dotfiles"))
		mustSymlink(t, "dotfiles/zshrc", rc)
		mustSymlink(t, "zshrc.real", mid) // relative to dotfiles/, not to home
		checkAppendsThroughLinks(t, env, filepath.Join(env.home, "dotfiles", "zshrc.real"),
			map[string]string{rc: "dotfiles/zshrc", mid: "zshrc.real"})
	})
	t.Run("a link into a missing directory", func(t *testing.T) {
		env := newTestSetupEnv(t)
		rc := filepath.Join(env.home, ".zshrc")
		mustSymlink(t, "dotfiles/sub/zshrc", rc)
		checkAppendsThroughLinks(t, env, filepath.Join(env.home, "dotfiles", "sub", "zshrc"),
			map[string]string{rc: "dotfiles/sub/zshrc"})
		if _, err := os.Lstat(filepath.Join(env.home, "dotfiles")); err == nil {
			t.Error("undo left the directories made for the target")
		}
	})
}

func checkAppendsThroughLinks(t *testing.T, env *setupEnv, target string, links map[string]string) {
	t.Helper()
	rc := filepath.Join(env.home, ".zshrc")
	if _, prob := (pathStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	_, u, err := pathStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	for link, want := range links {
		if l, err := os.Readlink(link); err != nil || l != want {
			t.Errorf("the link %s was replaced: %q %v", link, l, err)
		}
	}
	if fi, err := os.Lstat(target); err != nil {
		t.Errorf("target missing: %v", err)
	} else if fi.Mode() != 0o644 {
		t.Errorf("target mode = %v, want a 0644 file", fi.Mode())
	}
	if b, _ := os.ReadFile(target); string(b) != pathBlock(env.binDir) {
		t.Errorf("target = %q", b)
	}
	if _, err := os.Lstat(rc + ".bak"); err == nil {
		t.Error("a .bak was made beside a dangling link")
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("undo left the target it created")
	}
	for link, want := range links {
		if l, err := os.Readlink(link); err != nil || l != want {
			t.Errorf("undo changed the link %s: %q %v", link, l, err)
		}
	}
}

// A link loop leads to no file, so there is nothing to append to: install.sh's
// >> fails. setup refuses it in plan, so the run stops before any step changes
// anything, and in readable words when the loop is in a directory link.
func TestPathStepRefusesAProfileLinkLoop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links [][2]string // link (under home) and its target text
	}{
		{"the profile links to itself", [][2]string{{".zshrc", ".zshrc"}}},
		{"through a directory that links to itself", [][2]string{{"loop", "loop"}, {".zshrc", "loop/zshrc"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestSetupEnv(t)
			for _, l := range tc.links {
				mustSymlink(t, l[1], filepath.Join(env.home, l[0]))
			}
			_, prob := pathStep{}.plan(env)
			if prob == nil || prob.reason != "~/.zshrc is a symlink loop; not editing it" {
				t.Fatalf("plan problem = %+v, want a symlink-loop refusal", prob)
			}
			if len(prob.fix) != 1 || prob.fix[0] != exportLine(env.binDir) {
				t.Errorf("fix = %q, want the export line", prob.fix)
			}
		})
	}
	// apply still refuses a loop that plan did not see.
	env := newTestSetupEnv(t)
	rc := filepath.Join(env.home, ".zshrc")
	mustSymlink(t, ".zshrc", rc)
	env.pathProfile = rc
	if _, _, err := (pathStep{}).apply(env, nil); err == nil {
		t.Error("apply wrote through a symlink loop")
	}
	if l, err := os.Readlink(rc); err != nil || l != ".zshrc" {
		t.Errorf("the looping link was replaced: %q %v", l, err)
	}
	if _, err := os.Lstat(rc + ".bak"); err == nil {
		t.Error("a .bak was made of a symlink loop")
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// The block must land in the file the shell opens. The kernel resolves a linked
// directory before the ".." that follows it, so joining ".." to a link's path as
// text lands elsewhere. hasPathMarker reads through the kernel and would then
// never see the marker, so each rerun would append again.
func TestPathStepFollowsTheKernelThroughLinkedDirectories(t *testing.T) {
	// A relative target resolved against the working directory lands here.
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name   string
		links  [][2]string // link (under home) and its target text, made in order
		exists bool        // the file the kernel reaches exists before apply
		want   string      // that file, under home
		stray  string      // where a textual join lands instead, under home
	}{
		{"a chain through a linked directory to an existing file",
			[][2]string{{"alias", "real/dots"}, {".zshrc", "alias/zshrc.link"}, {"real/dots/zshrc.link", "../shared/zshrc"}},
			true, "real/shared/zshrc", "shared/zshrc"},
		{"the same chain to a missing file",
			[][2]string{{"alias", "real/dots"}, {".zshrc", "alias/zshrc.link"}, {"real/dots/zshrc.link", "../shared/zshrc"}},
			false, "real/shared/zshrc", "shared/zshrc"},
		{"a link whose own text climbs out of a linked directory",
			[][2]string{{"alias", "real/dots"}, {".zshrc", "alias/../zshrc"}},
			true, "real/zshrc", "zshrc"},
		{"the same link to a missing file",
			[][2]string{{"alias", "real/dots"}, {".zshrc", "alias/../zshrc"}},
			false, "real/zshrc", "zshrc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestSetupEnv(t)
			mustMkdir(t, filepath.Join(env.home, "real", "dots"))
			mustMkdir(t, filepath.Join(env.home, "real", "shared"))
			want := filepath.Join(env.home, tc.want)
			before := ""
			if tc.exists {
				before = "x\n"
				writeExe(t, want, before)
			}
			for _, l := range tc.links {
				mustSymlink(t, l[1], filepath.Join(env.home, l[0]))
			}
			if _, prob := (pathStep{}).plan(env); prob != nil {
				t.Fatal(prob)
			}
			if _, _, err := (pathStep{}).apply(env, nil); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(want); string(b) != before+pathBlock(env.binDir) {
				t.Errorf("%s = %q, want the block appended there", tc.want, b)
			}
			if _, err := os.Lstat(filepath.Join(env.home, tc.stray)); err == nil {
				t.Errorf("the block went to a stray ~/%s", tc.stray)
			}
			if p, _ := (pathStep{}).plan(env); !p.done {
				t.Error("a rerun plans another edit: the marker is not in the file the shell reads")
			}
		})
	}
}
