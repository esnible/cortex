package main

import (
	"errors"
	"os"
	"path/filepath"

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
	if checkWritable(env, prof) != nil {
		// A read-only profile, such as a Nix home-manager link into /nix/store:
		// install.sh warns and carries on, so the run goes ahead with advice.
		p.done = true
		p.advice = &problem{reason: env.tilde(prof) + " is not writable; add " + env.tilde(env.binDir) + " to PATH yourself",
			fix: []string{exportLine(env.binDir)}}
		return p, nil
	}
	env.pathProfile, env.binOnPathNewTerms = prof, true
	p.verb, p.what, p.where = "add", env.tilde(env.binDir)+" to PATH", env.tilde(prof)+" (2 marked lines)"
	return p, nil
}

// apply copies an existing profile to <profile>.bak, as install.sh's cp does,
// then appends pathBlock. A symlinked profile is written through: the edit lands
// in the link's target and the .bak sits beside the link. A target that does not
// exist yet is created with mode 0644, and gets no .bak.
func (pathStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	prof := env.pathProfile
	target := linkTarget(prof)
	snap, err := snapshotFile(target)
	if err != nil {
		return "", undo{}, err
	}
	if snap.link != "" {
		// Still a link after linkTarget, so a loop: there is no file to append to,
		// and writing would replace the link with one of mode 0 (snap.mode).
		return "", undo{}, stepError{reason: env.tilde(prof) + " is a symlink loop; not editing it",
			detail: []string{"add it yourself:  " + exportLine(env.binDir)}}
	}
	bakSnap, err := snapshotFile(prof + ".bak")
	if err != nil {
		return "", undo{}, err
	}
	u := undo{label: env.tilde(prof), fn: func() error {
		return errors.Join(snap.restore(), bakSnap.restore())
	}, manual: "delete the two lines marked \"added by Cortex\" from " + env.tilde(prof)}
	mode := snap.mode
	if !snap.existed {
		mode = 0o644
	} else if err := writeFileAtomic(prof+".bak", snap.data, snap.mode); err != nil {
		return "", u, err
	}
	content := append(append([]byte{}, snap.data...), pathBlock(env.binDir)...)
	if err := writeFileAtomic(target, content, mode); err != nil {
		return "", u, err
	}
	return env.tilde(prof), u, nil
}

// linkTarget is the file a shell's >> on path would write: path itself, or what
// its links lead to. EvalSymlinks gives the kernel's answer whenever the file
// exists. It fails on a link to a missing file, so a dangling link is followed by
// hand: each relative target is joined to its link's directory with that
// directory's own links resolved, because the kernel resolves a linked directory
// before the ".." after it, and a textual join would not. A loop is still a link
// after maxHops, for apply to refuse.
func linkTarget(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	const maxHops = 40 // Linux's MAXSYMLINKS; macOS stops at 32
	p := path
	for range maxHops {
		link, err := os.Readlink(p)
		if err != nil {
			return p
		}
		if !filepath.IsAbs(link) {
			dir := filepath.Dir(p)
			if d, err := filepath.EvalSymlinks(dir); err == nil {
				dir = d
			}
			link = filepath.Join(dir, link)
		}
		p = link
	}
	return p
}
