package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// pathStep is install.sh's offer_path_setup without its own prompt: the consent
// screen asks instead.
type pathStep struct{}

func (pathStep) name() string { return "PATH" }

func exportLine(binDir string) string { return `export PATH="` + binDir + `:$PATH"` }

func (pathStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "PATH"}
	if onPath(env.pathEnv, env.binDir) {
		env.binOnPath, env.binOnPathNewTerms = true, true
		p.done, p.doneMsg = true, env.tilde(env.binDir)
		return p, nil
	}
	if env.opts.noModifyPath {
		p.done = true
		p.advice = &problem{reason: env.tilde(env.binDir) + " is not on PATH — not editing dotfiles",
			fix: []string{exportLine(env.binDir)}}
		return p, nil
	}
	prof := pathProfile(env.shell, env.home)
	if prof == "" {
		p.done = true
		p.advice = &problem{reason: env.tilde(env.binDir) + " is not on PATH; add it for future sessions",
			fix: []string{exportLine(env.binDir)}}
		return p, nil
	}
	if hasPathMarker(prof) {
		env.binOnPathNewTerms = true
		p.done, p.doneMsg = true, "in "+env.tilde(prof)+"; new terminals only"
		return p, nil
	}
	target := linkTarget(prof)
	if linkLoops(target) {
		// Refused here, not in apply, so the run stops before any step changes
		// anything.
		return p, &problem{reason: env.tilde(prof) + " is a symlink loop; not editing it",
			fix: []string{exportLine(env.binDir)}}
	}
	// apply renames a temp file into the target's directory and writes the .bak
	// beside the profile, and install.sh's >> needs the profile itself writable.
	for _, w := range []string{prof, filepath.Dir(target), filepath.Dir(prof)} {
		if prob := checkWritable(env, w); prob != nil {
			// A read-only profile, such as a Nix home-manager link into /nix/store:
			// install.sh warns and carries on, so the run goes ahead with advice.
			p.done = true
			p.advice = &problem{reason: prob.reason + "; add " + env.tilde(env.binDir) + " to PATH yourself",
				fix: []string{exportLine(env.binDir)}}
			return p, nil
		}
	}
	env.pathProfile, env.binOnPathNewTerms = prof, true
	p.verb, p.what, p.where = "add", env.tilde(env.binDir)+" to PATH", env.tilde(prof)+" (2 marked lines)"
	return p, nil
}

// apply copies an existing profile to <profile>.bak, as install.sh's cp does,
// then appends pathBlock. A symlinked profile is written through: the edit lands
// in the link's target and the .bak sits beside the link. A target that does not
// exist yet is created with mode 0644, and gets no .bak; the undo removes any
// directory made for it. The append is a whole-file replace, through a temp file
// and a rename, so a hard link to the profile keeps the old content.
func (pathStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	prof := env.pathProfile
	target := linkTarget(prof)
	snap, err := snapshotFile(target)
	if err != nil {
		return "", undo{}, err
	}
	if snap.link != "" {
		// Still a link after linkTarget, so a loop that plan did not see: there is
		// no file to append to, and writing would replace the link with one of mode
		// 0 (snap.mode).
		return "", undo{}, stepError{reason: env.tilde(prof) + " is a symlink loop; not editing it",
			detail: []string{"add it yourself:  " + exportLine(env.binDir)}}
	}
	bakSnap, err := snapshotFile(prof + ".bak")
	if err != nil {
		return "", undo{}, err
	}
	madeDirs := missingDirs(filepath.Dir(target))
	var wroteBak, wroteTarget bool
	u := undo{label: env.tilde(prof), fn: func() error {
		// Only a file that was written is put back: rewriting one a refused write
		// left unchanged would fail where that write did.
		var errs []error
		if wroteTarget {
			errs = append(errs, snap.restore())
		}
		for _, d := range madeDirs {
			_ = os.Remove(d) // deepest first; only succeeds when empty, which is the point
		}
		if wroteBak {
			errs = append(errs, bakSnap.restore())
		}
		return errors.Join(errs...)
	}, manual: "delete the two lines marked \"added by Cortex\" from " + env.tilde(prof)}
	mode := snap.mode
	if !snap.existed {
		mode = 0o644
	} else {
		if err := writeFileAtomic(prof+".bak", snap.data, snap.mode); err != nil {
			return "", u, err
		}
		wroteBak = true
	}
	content := append(append([]byte{}, snap.data...), pathBlock(env.binDir)...)
	if err := writeFileAtomic(target, content, mode); err != nil {
		return "", u, err
	}
	wroteTarget = true
	return env.tilde(prof), u, nil
}

// missingDirs are dir and each parent of it that does not exist yet, deepest
// first: what MkdirAll(dir) would create. Lstat keeps a link that exists, even a
// dangling one, off the list, so the undo never removes a link.
func missingDirs(dir string) []string {
	var out []string
	for d := dir; filepath.Dir(d) != d; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		out = append(out, d)
	}
	return out
}

// linkTarget is the file a shell's >> on path would write: path itself, or what
// its links lead to. EvalSymlinks gives the kernel's answer whenever the file
// exists. It fails on a link to a missing file, so a dangling link is followed by
// hand, one link at a time. The link's directory and its target text are joined
// without cleaning, and EvalSymlinks resolves that up to its last element: the
// kernel resolves a linked directory before the ".." after it, and Clean would
// drop "dir/.." as text. Where that directory does not exist either, the join is
// cleaned as text. A loop ends as a link still, after maxHops, or as a path the
// kernel will not resolve, for linkLoops to report.
func linkTarget(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	const maxHops = 40 // Linux's MAXSYMLINKS; macOS stops at 32
	sep := string(filepath.Separator)
	p := path
	for range maxHops {
		link, err := os.Readlink(p)
		if err != nil {
			return p
		}
		if !filepath.IsAbs(link) {
			link = filepath.Dir(p) + sep + link
		}
		i := strings.LastIndex(link, sep)
		if dir, err := filepath.EvalSymlinks(link[:max(i, 1)]); err == nil {
			p = filepath.Join(dir, link[i+1:])
		} else {
			p = filepath.Clean(link)
		}
	}
	return p
}

// linkLoops reports whether path, as linkTarget left it, leads to no file: it is
// still a link, or the kernel stops resolving it (ELOOP).
func linkLoops(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return errors.Is(err, syscall.ELOOP)
	}
	return fi.Mode()&os.ModeSymlink != 0
}
