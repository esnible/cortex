package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

func configEnv(t *testing.T) *setupEnv {
	t.Helper()
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "cortex"), cortexStub("127.0.0.1:1"))
	return env
}

func asStepError(err error, se *stepError) bool { return errors.As(err, se) }

func TestConfigStepWritesAFreshConfigAndUndoes(t *testing.T) {
	env := configEnv(t)
	p, prob := configStep{}.plan(env)
	if prob != nil || p.verb != "write" || !env.configFresh {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	_, u, err := configStep{}.apply(env, nil)
	if err != nil {
		t.Fatalf("apply on a fresh install: %v", err)
	}
	if pending, err := configMigrationPending(env.configPath()); err != nil || pending {
		t.Errorf("after apply, pending=%v err=%v — the step must leave it migrated", pending, err)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env.cortexDir); err == nil {
		t.Error("undo left the ~/.cortex this run created")
	}
}

// The undo removes ~/.cortex only when this run created it. An empty one that was
// already there is empty again after the undo, so only that guard keeps it.
func TestConfigStepUndoKeepsAnEmptyCortexDirThatExisted(t *testing.T) {
	env := configEnv(t)
	if err := os.Mkdir(env.cortexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if p, prob := (configStep{}).plan(env); prob != nil || p.verb != "write" {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	_, u, err := configStep{}.apply(env, nil)
	if err != nil {
		t.Fatalf("apply on a fresh install: %v", err)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env.configPath()); err == nil {
		t.Error("undo left the config the step wrote")
	}
	if _, err := os.Stat(env.cortexDir); err != nil {
		t.Error("undo removed a ~/.cortex that existed before")
	}
}

func TestConfigStepMigratesAnOldConfigAndUndoesByteForByte(t *testing.T) {
	env := configEnv(t)
	old := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:47600\n"
	writeExe(t, env.configPath(), old)
	if err := os.Chmod(env.configPath(), 0o600); err != nil {
		t.Fatal(err)
	}
	p, prob := configStep{}.plan(env)
	if prob != nil || p.verb != "update" || !env.configWillChange {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	_, u, err := configStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(env.configPath()); bytes.Equal(b, []byte(old)) {
		t.Error("the config was not migrated")
	}
	if want := "restore ~/.cortex/config.yaml from ~/.cortex/config.yaml.before-agentop-migrate"; !strings.Contains(u.manual, want) {
		t.Errorf("undo manual = %q, want it to contain %q", u.manual, want)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(env.configPath()); string(b) != old {
		t.Errorf("undo left %q", b)
	}
	for _, f := range []string{".before-agentop-migrate", ".before-agentop-pricing"} {
		if _, err := os.Stat(env.configPath() + f); err == nil {
			t.Errorf("undo left %s", f)
		}
	}
	if _, err := os.Stat(env.cortexDir); err != nil {
		t.Error("undo removed a ~/.cortex that existed before")
	}
}

// A Bob pricing failure only warns, as it does in service install: an unpriced Bob
// exposes nothing. insertBobEndpoint refuses a flow-style pricing block. With the
// pins present the Bob step is the only one with work; with them missing the pins
// still go in, which is what makes the undo's restore observable.
func TestConfigStepWarnsWhenBobCannotBePriced(t *testing.T) {
	pins := "listener:\n  bind_loopback_only: true\n  health_addr: 127.0.0.1:47604\n" +
		"  transparent_proxy_addr: 127.0.0.1:47603\n"
	for _, tc := range []struct{ name, body string }{
		{"pins present", "mode: proxy-sidecar\n" + pins + "pricing: {endpoints: []}\n"},
		{"pins missing", "mode: proxy-sidecar\nlistener:\n  roles: [forward]\npricing: {endpoints: []}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := configEnv(t)
			writeExe(t, env.configPath(), tc.body)
			if err := os.Chmod(env.configPath(), 0o600); err != nil {
				t.Fatal(err)
			}
			// What the pins migration alone makes of the file: the Bob step must add
			// nothing to it.
			pinsOnly := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(pinsOnly, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := migrateConfig(pinsOnly, io.Discard); err != nil {
				t.Fatal(err)
			}
			want, _ := os.ReadFile(pinsOnly)

			detail, u, err := configStep{}.apply(env, nil)
			if err != nil {
				t.Fatalf("a Bob pricing error failed the step: %v", err)
			}
			if !strings.Contains(detail, "IBM Bob left unpriced (pricing: is written in flow style") {
				t.Errorf("detail = %q, want it to say Bob is left unpriced, and why", detail)
			}
			if b, _ := os.ReadFile(env.configPath()); !bytes.Equal(b, want) {
				t.Errorf("the Bob step changed the file: %q, want %q", b, want)
			}
			if _, err := os.Stat(env.configPath() + ".before-agentop-pricing"); err == nil {
				t.Error("the refused Bob step left a .before-agentop-pricing")
			}
			if err := u.fn(); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(env.configPath()); string(b) != tc.body {
				t.Errorf("undo left %q", b)
			}
			if _, err := os.Stat(env.configPath() + ".before-agentop-migrate"); err == nil {
				t.Error("undo left .before-agentop-migrate")
			}
		})
	}
}

// A failed pin migration on a config that binds a listener on every interface
// fails the step, as service install refuses to supervise it. Neither config can
// take the pins: one has no listener: block, the other a flow-style one. Without
// bind_loopback_only, the preset binds health and the session API on every
// interface, so a config with no listener: block always lands here.
func TestConfigStepRefusesAFailedPinMigrationThatLeavesListenersExposed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no listener block", "mode: proxy-sidecar\n"},
		{"flow-style listener", "mode: proxy-sidecar\nlistener: {roles: [forward]}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := configEnv(t)
			writeExe(t, env.configPath(), tc.body)
			if err := os.Chmod(env.configPath(), 0o600); err != nil {
				t.Fatal(err)
			}
			_, u, err := configStep{}.apply(env, nil)
			var se stepError
			if !asStepError(err, &se) {
				t.Fatalf("a failed pin migration with exposed listeners: err = %#v, want a step error naming them", err)
			}
			if !strings.Contains(se.reason, "health_addr") || !strings.Contains(se.reason, "on every interface") {
				t.Errorf("reason = %q, want it to name the exposed health_addr", se.reason)
			}
			if !strings.Contains(strings.Join(se.detail, "\n"), "bind_loopback_only: true") {
				t.Errorf("detail = %q, want service install's remedy", se.detail)
			}
			if u.fn == nil {
				t.Fatal("a failed apply returned no undo")
			}
			if err := u.fn(); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(env.configPath()); string(b) != tc.body {
				t.Errorf("undo left %q", b)
			}
		})
	}
}

// A failed pin migration on a config already bound to loopback only warns, as in
// service install, and the Bob migration still runs. The pins cannot go in: one
// listener block is flow style, and the other's indented comment makes the pinned
// result fail to parse after migrateConfig has written its backup.
func TestConfigStepWarnsOnAFailedPinMigrationWhenNothingIsExposed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"flow-style listener", "mode: proxy-sidecar\nlistener: {bind_loopback_only: true}\n"},
		{"pins would not parse", "mode: proxy-sidecar\nlistener:\n    # note\n  bind_loopback_only: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := configEnv(t)
			writeExe(t, env.configPath(), tc.body)
			if err := os.Chmod(env.configPath(), 0o600); err != nil {
				t.Fatal(err)
			}
			if exposed := wildcardListeners(env.configPath()); len(exposed) != 0 {
				t.Fatalf("the fixture exposes %v, so it tests nothing", exposed)
			}
			probe := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(probe, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := migrateConfig(probe, io.Discard); err == nil {
				t.Fatal("migrateConfig accepts the fixture, so it tests nothing")
			}
			detail, u, err := configStep{}.apply(env, nil)
			if err != nil {
				t.Fatalf("a failed pin migration with nothing exposed failed the step: %v", err)
			}
			if !strings.Contains(detail, "listener pins not added (") {
				t.Errorf("detail = %q, want it to say the pins were not added", detail)
			}
			cfg, err := config.Load(env.configPath())
			if err != nil {
				t.Fatal(err)
			}
			if priced, err := bobPriced(cfg); err != nil || !priced {
				t.Errorf("after a pins warning, Bob priced=%v err=%v — the Bob migration must still run", priced, err)
			}
			if err := u.fn(); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(env.configPath()); string(b) != tc.body {
				t.Errorf("undo left %q", b)
			}
			for _, f := range []string{".before-agentop-migrate", ".before-agentop-pricing"} {
				if _, err := os.Stat(env.configPath() + f); err == nil {
					t.Errorf("undo left %s", f)
				}
			}
		})
	}
}

// wildcardListeners finds nothing exposed in a config that will not load, so the
// step must refuse such a config itself before the warn-or-refuse decision, as
// service install does. A fresh install is the path plan does not check.
func TestConfigStepRefusesAWrittenConfigThatWillNotLoad(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\nmkdir -p \"$HOME/.cortex\"\n"+
		"printf 'listener:\\n  roles: [forward\\n' > \"$HOME/.cortex/config.yaml\"\n")
	if p, prob := (configStep{}).plan(env); prob != nil || p.verb != "write" {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	_, u, err := configStep{}.apply(env, nil)
	var se stepError
	if !asStepError(err, &se) || !strings.Contains(se.reason, "will not load") {
		t.Errorf("a written config that will not load: err = %#v, want a will-not-load step error", err)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env.cortexDir); err == nil {
		t.Error("undo left the ~/.cortex this run created")
	}
}

func TestConfigStepDoneWhenCurrentAndRefusesABrokenOne(t *testing.T) {
	env := configEnv(t)
	writeExe(t, env.configPath(), "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n")
	if _, err := migrateConfig(env.configPath(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateBobPricing(env.configPath(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if p, prob := (configStep{}).plan(env); prob != nil || !p.done {
		t.Errorf("a current config planned a change: %+v %v", p, prob)
	}
	writeExe(t, env.configPath(), "listener:\n  roles: [forward\n")
	if _, prob := (configStep{}).plan(env); prob == nil {
		t.Error("a config that will not load raised no problem")
	}
}

func TestConfigStepReportsAWriteConfigFailure(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\necho 'no home' >&2\nexit 1\n")
	if _, prob := (configStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	_, _, err := configStep{}.apply(env, nil)
	var se stepError
	if !asStepError(err, &se) || se.reason != "cortex --local --write-config failed" || len(se.detail) == 0 || se.detail[len(se.detail)-1] != "no home" {
		t.Errorf("err = %#v", err)
	}
}

// The service step restarts for new listener pins, which a running proxy takes
// only at start, and not for Bob's rate, which it reloads: the config step tells
// the two apart when planning and when applying.
func TestConfigStepFlagsNewPinsButNotBobsRate(t *testing.T) {
	pins := "listener:\n  bind_loopback_only: true\n  health_addr: 127.0.0.1:47604\n" +
		"  transparent_proxy_addr: 127.0.0.1:47603\n"
	for _, tc := range []struct {
		name, body string
		pins       bool
	}{
		{"pins missing", "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:47600\n", true},
		{"only Bob's rate missing", "mode: proxy-sidecar\n" + pins, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := configEnv(t)
			writeExe(t, env.configPath(), tc.body)
			if err := os.Chmod(env.configPath(), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, prob := (configStep{}).plan(env); prob != nil || !env.configWillChange || env.configPinsPending != tc.pins {
				t.Fatalf("plan: %v willChange=%v pinsPending=%v, want pinsPending=%v",
					prob, env.configWillChange, env.configPinsPending, tc.pins)
			}
			if _, _, err := (configStep{}).apply(env, nil); err != nil || env.configPinsChanged != tc.pins {
				t.Errorf("apply: %v pinsChanged=%v, want %v", err, env.configPinsChanged, tc.pins)
			}
		})
	}
}
