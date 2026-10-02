package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// `agentop configure opencode` routes OpenCode through Cortex for good. The process you
// run is the wrong place for that: OpenCode's background service sends every session's
// traffic, outlives the client that started it, and keeps an environment of its own in
// service.json, which it takes over the one it inherited (see opencode.go). So enable
// writes the variables there, and only through the opencode CLI.

const (
	// opencodeStateRel records what each variable held in OpenCode's service environment
	// before enable, so disable can put it back. Not clientstate.RelPath: that file is
	// Claude Code's, and cortex reads it back to check Claude Code's CA.
	opencodeStateRel = ".cortex/opencode-state.json"
	// opencodeStateSettings is the record's Settings field. Claude Code's record names
	// the settings file it describes; OpenCode's environment is reached through its
	// CLI rather than a file agentop writes, so the record names that instead.
	opencodeStateSettings = "opencode service env"
)

// openCodeKeys are the variables enable sets, in the order it sets them: the nine
// `agentop exec` gives a child, for the reasons execProxyVars and bundleKeys give.
// Not CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC, which only Claude Code reads.
var openCodeKeys = append([]string{envProxy, "HTTP_PROXY", "https_proxy", "http_proxy", envCACerts}, bundleKeys...)

// openCodeCanonicalKey is the name wantedFromConfig and isCortexValue know k by. Both
// know the proxy only as HTTPS_PROXY, the one spelling Claude Code needs.
func openCodeCanonicalKey(k string) string {
	switch k {
	case "HTTP_PROXY", "https_proxy", "http_proxy":
		return envProxy
	}
	return k
}

// opencodeConfirm prompts before enable or disable changes the service environment.
// Its own var, for the reasons claudeCodeConfirm gives: tests replace it, and stubbing
// it must not disarm another command's prompt.
var opencodeConfirm = confirm

const openCodeUsage = `agentop configure opencode — route OpenCode through Cortex via its background service

Usage:
  agentop configure opencode enable  [--yes] [--config PATH] [--opencode BIN]
  agentop configure opencode disable [--yes] [--config PATH] [--opencode BIN]
  agentop configure opencode status  [--config PATH] [--opencode BIN]

OpenCode's traffic does not leave from the opencode you run. One background
service per user, "opencode serve --service", sends every session's requests;
the first client starts it and later ones reuse it. The service keeps an
environment of its own, which it takes over the one it inherited, so that is
where the routing goes. Every change is made with the opencode CLI
("opencode service set env" and "opencode service unset env").

enable sets the nine variables "agentop exec" sets, reading the addresses from
~/.cortex/config.yaml so they match the proxy:

  HTTPS_PROXY  HTTP_PROXY  https_proxy  http_proxy      the forward proxy URL
  NODE_EXTRA_CA_CERTS                                   ca.crt
  SSL_CERT_FILE  GIT_SSL_CAINFO                         bundle.crt: the bridge CA
  REQUESTS_CA_BUNDLE  CURL_CA_BUNDLE                    plus the platform roots

"agentop exec --help" says why each gets which file. enable refuses to overwrite a
value someone else set and leaves every other variable alone. The first run
records what the nine held in ~/.cortex/opencode-state.json, so disable puts back
a value you had rather than removing it. status shows the nine and the running
service, and changes nothing.

Neither enable nor disable restarts a running service, because that ends every
OpenCode session using it. A running service keeps the environment it started
with, so each says when the service is running with its old environment;
"opencode service restart" applies the change.

Note: once its service has started with these, OpenCode needs Cortex running —
its requests go to the proxy address. "agentop configure opencode disable" is the
off switch.

Exit status: 0 applied or already correct, 3 declined (or no terminal to ask
on), 1 something went wrong, 2 a usage error.

Flags:
  --yes            do not prompt for confirmation (enable and disable)
  --config PATH    Cortex config to read addresses from (default ~/.cortex/config.yaml)
  --opencode BIN   the opencode CLI (default: opencode on PATH, else ~/.opencode/bin/opencode)
`

func runOpenCode(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, openCodeUsage)
		return 2
	}
	action := args[0]
	// Help before the verb check, and the verb before anything touches the machine:
	// the same order as runBob, for its reasons.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, openCodeUsage)
		return 0
	case "enable", "disable", "status":
	default:
		fmt.Fprintf(stderr, "agentop: unknown opencode action %q (enable, disable, status)\n", action)
		return 2
	}

	fs := flag.NewFlagSet("configure opencode "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cortexCfgPath := fs.String("config", "", "Cortex config file")
	bin := fs.String("opencode", "", "the opencode CLI")
	// Not for status, which changes nothing: accepting --yes there would be a flag
	// that parses and does nothing.
	var yesFlag *bool
	if action != "status" {
		yesFlag = fs.Bool("yes", false, "do not prompt for confirmation")
	}
	fs.Usage = func() {}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, openCodeUsage)
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentop: opencode %s takes no arguments (got %q)\n", action, fs.Arg(0))
		return 2
	}
	yes := yesFlag != nil && *yesFlag

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "agentop: cannot determine your home directory: %v\n", err)
		return 1
	}
	if *cortexCfgPath == "" {
		*cortexCfgPath = filepath.Join(home, cortexCfgRel)
	}
	if *bin == "" {
		found, ferr := findOpenCode()
		if ferr != nil {
			fmt.Fprintf(stderr, "agentop: %v; name it with --opencode PATH\n", ferr)
			return 1
		}
		*bin = found
	}
	statePath := filepath.Join(home, opencodeStateRel)

	switch action {
	case "enable":
		return openCodeEnable(*bin, *cortexCfgPath, statePath, yes, stdout, stderr)
	case "disable":
		return openCodeDisable(*bin, *cortexCfgPath, statePath, yes, stdout, stderr)
	default:
		return openCodeStatus(*bin, *cortexCfgPath, stdout, stderr)
	}
}

// openCodeValues spreads wantedFromConfig's values over the nine keys: its proxy URL
// into all four proxy spellings, the rest by name. A key with no value there, the CA
// keys of a config without a ca_dir, is left out.
func openCodeValues(want map[string]string) map[string]string {
	out := make(map[string]string, len(openCodeKeys))
	for _, k := range openCodeKeys {
		if v := want[openCodeCanonicalKey(k)]; v != "" {
			out[k] = v
		}
	}
	return out
}

// openCodeWanted is what enable sets, after the two refusals claude-code enable makes,
// for the reasons planClaudeCodeEnable gives: a bridge that is off, or no CA to trust.
func openCodeWanted(cortexCfgPath string) (map[string]string, error) {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		return nil, err
	}
	if !bridgeEnabled(cfg) {
		return nil, errBridgeDisabled(cortexCfgPath)
	}
	if _, ok := want[envCACerts]; !ok {
		return nil, fmt.Errorf("%s has no tls_bridge.ca_dir, so OpenCode has no CA to trust;\n"+
			"  requests would fail certificate verification. Enable the TLS bridge first.", cortexCfgPath)
	}
	return openCodeValues(want), nil
}

// openCodeServiceEnv is OpenCode's service environment as `opencode service get env`
// prints it: a JSON object of strings, {} when empty.
func openCodeServiceEnv(bin string) (map[string]string, error) {
	out, err := openCodeRun(bin, "service", "get", "env")
	if err != nil {
		return nil, err
	}
	var env map[string]string
	// A nil map with no error is JSON null, which is not an object either.
	if err := json.Unmarshal([]byte(out), &env); err != nil || env == nil {
		return nil, fmt.Errorf("opencode service get env printed %q, which is not a JSON object of strings", out)
	}
	return env, nil
}

// openCodePlan is what enable would change, worked out without changing anything.
type openCodePlan struct {
	bin     string
	env     map[string]string // the service environment before the change
	want    map[string]string // the nine values enable sets
	changes []string          // keys whose value differs, in openCodeKeys order; none = already enabled
}

// planOpenCodeEnable runs the checks that can refuse an enable. An error is the
// refusal, worded to follow "agentop: ".
func planOpenCodeEnable(bin, cortexCfgPath string) (openCodePlan, error) {
	want, err := openCodeWanted(cortexCfgPath)
	if err != nil {
		return openCodePlan{}, err
	}
	env, err := openCodeServiceEnv(bin)
	if err != nil {
		return openCodePlan{}, err
	}
	// Refuse a value someone else set, most likely a corporate proxy or CA, as
	// claude-code does. Asked under the canonical name, so a lowercase proxy an earlier
	// enable wrote for an older Cortex address is not taken for someone else's.
	for _, k := range openCodeKeys {
		if cur, ok := env[k]; ok && cur != want[k] && !isCortexValue(openCodeCanonicalKey(k), cur) {
			return openCodePlan{}, fmt.Errorf("%s is already set to %q in OpenCode's service environment.\n"+
				"  Refusing to overwrite a value you set. Remove it first: opencode service unset env %s", k, cur, k)
		}
	}
	pl := openCodePlan{bin: bin, env: env, want: want}
	for _, k := range openCodeKeys {
		if env[k] != want[k] {
			pl.changes = append(pl.changes, k)
		}
	}
	return pl, nil
}

// applyOpenCodeEnable records what the keys held, then sets each one that differs.
// Failing to record is a warning, as it is for claude-code: the change still happens,
// and disable then removes the keys rather than restoring them.
func applyOpenCodeEnable(pl openCodePlan, statePath string, stderr io.Writer) error {
	st := managedState{Settings: opencodeStateSettings, Prior: map[string]*string{}}
	for _, k := range openCodeKeys {
		if v, ok := pl.env[k]; ok {
			st.Prior[k] = &v
		} else {
			st.Prior[k] = nil
		}
	}
	if err := writeState(statePath, st); err != nil {
		fmt.Fprintf(stderr, "agentop: could not record OpenCode's prior values (%v); disable will remove\n"+
			"  these variables rather than restore any you had set yourself\n", err)
	}
	for i, k := range pl.changes {
		if _, err := openCodeRun(pl.bin, "service", "set", "env", k, pl.want[k]); err != nil {
			if i == 0 {
				return err
			}
			return fmt.Errorf("%w\n  Already set: %s. agentop configure opencode disable puts back what was there",
				err, strings.Join(pl.changes[:i], ", "))
		}
	}
	return nil
}

func openCodeEnable(bin, cortexCfgPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	pl, err := planOpenCodeEnable(bin, cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(pl.changes) == 0 {
		fmt.Fprintln(stdout, "Already enabled: OpenCode's service environment routes it through Cortex.")
		reportOpenCodeService(bin, pl.want[envProxy], true, false, stdout)
		return 0
	}
	fmt.Fprintln(stdout, "Sets in OpenCode's background-service environment (opencode service set env):")
	for _, k := range pl.changes {
		fmt.Fprintf(stdout, "  %s=%s\n", k, pl.want[k])
	}
	fmt.Fprint(stdout, "Nothing else changes; agentop configure opencode disable puts back what was there.\n\n")
	if !yes && !opencodeConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	if err := applyOpenCodeEnable(pl, statePath, stderr); err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Enabled — OpenCode's service uses Cortex from its next start.")
	reportOpenCodeService(bin, pl.want[envProxy], true, false, stdout)
	return 0
}

// openCodeDisablePlan is what disable would change, worked out without changing anything.
type openCodeDisablePlan struct {
	bin     string
	env     map[string]string
	present []string // managed keys set in the service environment, in openCodeKeys order; none = nothing to do
}

func planOpenCodeDisable(bin string) (openCodeDisablePlan, error) {
	env, err := openCodeServiceEnv(bin)
	if err != nil {
		return openCodeDisablePlan{}, err
	}
	pl := openCodeDisablePlan{bin: bin, env: env}
	for _, k := range openCodeKeys {
		if _, ok := env[k]; ok {
			pl.present = append(pl.present, k)
		}
	}
	return pl, nil
}

// applyOpenCodeDisable puts back what enable recorded, unsets what it added, and
// deletes the record. It returns the keys restored to a value the user had set.
//
// The record is deleted only once every key is done, so a failure part way leaves it
// for the next disable, which then finishes the job.
func applyOpenCodeDisable(pl openCodeDisablePlan, statePath string, stderr io.Writer) ([]string, error) {
	st, sterr := readState(statePath)
	if sterr != nil {
		// Proceed, as claude-code does: the user asked for this off. But say what is
		// about to be lost.
		fmt.Fprintf(stderr, "agentop: cannot read the record of what you had before enabling (%v).\n"+
			"  Falling back to removing these keys outright. If you had set any of them\n"+
			"  yourself before running enable, that value is not recoverable from here —\n"+
			"  check opencode service get env afterwards.\n\n", sterr)
	}
	var restored []string
	for _, k := range pl.present {
		var err error
		if prior := openCodePrior(st, k); prior != nil {
			_, err = openCodeRun(pl.bin, "service", "set", "env", k, *prior)
			if err == nil {
				restored = append(restored, k)
			}
		} else {
			// Absent before enable, or no usable record: removing it is all that is left.
			_, err = openCodeRun(pl.bin, "service", "unset", "env", k)
		}
		if err != nil {
			return nil, fmt.Errorf("%w\n  Run agentop configure opencode disable again to finish", err)
		}
	}
	_ = os.Remove(statePath)
	return restored, nil
}

// openCodePrior is the value st recorded for k before enable, or nil when it recorded
// none: k was absent, or st is not an OpenCode record.
func openCodePrior(st *managedState, k string) *string {
	if st == nil || st.Settings != opencodeStateSettings {
		return nil
	}
	return st.Prior[k]
}

func openCodeDisable(bin, cortexCfgPath, statePath string, yes bool, stdout, stderr io.Writer) int {
	pl, err := planOpenCodeDisable(bin)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(pl.present) == 0 {
		fmt.Fprintln(stdout, "Nothing to do: none of the Cortex variables are set in OpenCode's service environment.")
		return 0
	}
	fmt.Fprintf(stdout, "This will remove from OpenCode's service environment: %s\n\n", strings.Join(pl.present, ", "))
	if !yes && !opencodeConfirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	restored, err := applyOpenCodeDisable(pl, statePath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	if len(restored) > 0 {
		fmt.Fprintf(stdout, "\nRestored to the value(s) you had before: %s\n", strings.Join(restored, ", "))
	}
	fmt.Fprintln(stdout, "\nDisabled. OpenCode's service no longer routes through Cortex from its next start.")

	// The running service is compared with Cortex's proxy. When the config cannot be
	// read, the HTTPS_PROXY just taken out stands in for it: that is what a service
	// still running with its old environment uses.
	cortexProxy := pl.env[envProxy]
	if want, _, werr := wantedFromConfig(cortexCfgPath); werr == nil {
		cortexProxy = want[envProxy]
	}
	reportOpenCodeService(bin, cortexProxy, false, false, stdout)
	return 0
}

// openCodeStatus prints the nine keys, whether they route OpenCode through this Cortex,
// and the running service. It changes nothing.
func openCodeStatus(bin, cortexCfgPath string, stdout, stderr io.Writer) int {
	env, err := openCodeServiceEnv(bin)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	for _, k := range slices.Sorted(slices.Values(openCodeKeys)) {
		if v, ok := env[k]; ok {
			fmt.Fprintf(stdout, "  %s=%s\n", k, v)
		} else {
			fmt.Fprintf(stdout, "  %s (unset)\n", k)
		}
	}
	want, _, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		// Without Cortex's addresses there is nothing to judge the values by, so the
		// keys and the service are reported as they are. Not a failure: status reports.
		fmt.Fprintf(stdout, "cannot compare with Cortex's config: %v\n", err)
		reportOpenCodeService(bin, "", false, true, stdout)
		return 0
	}
	vals := openCodeValues(want)
	// Set means set to Cortex's value. A variable left at an older Cortex address is
	// shown above but not counted: a service started with it would not reach this one.
	set := 0
	for _, k := range openCodeKeys {
		if w, ok := vals[k]; ok && env[k] == w {
			set++
		}
	}
	enabled := set == len(openCodeKeys)
	if enabled {
		fmt.Fprintln(stdout, "enabled")
	} else {
		fmt.Fprintf(stdout, "not fully enabled (%d of %d set)\n", set, len(openCodeKeys))
	}
	reportOpenCodeService(bin, vals[envProxy], enabled, true, stdout)
	return 0
}

// reportOpenCodeService says how the running service compares with the environment
// just configured. It never restarts the service: that would end every OpenCode
// session using it.
//
// cortexProxy is Cortex's proxy URL, "" when it is not known. enabled is whether the
// service environment now routes OpenCode through it. status asks for a line in every
// case; enable and disable say only what needs doing or what was confirmed.
func reportOpenCodeService(bin, cortexProxy string, enabled, status bool, stdout io.Writer) {
	svc, err := probeOpenCodeService(bin)
	if err == nil && svc.Running {
		err = svc.EnvErr
	}
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "Could not check OpenCode's running service (%v); restart it to be sure: opencode service restart\n", err)
	case !svc.Running:
		if status {
			fmt.Fprintln(stdout, "OpenCode's background service is not running.")
		}
	case cortexProxy == "":
		if !status {
			return
		}
		if svc.Proxy == "" {
			fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is running with no proxy.\n", svc.PID)
		} else {
			fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is running with proxy %s.\n", svc.PID, svc.Proxy)
		}
	case sameProxy(svc.Proxy, cortexProxy) != enabled:
		fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is running with its old environment.\n"+
			"  It picks this up when it restarts: opencode service restart   # ends every OpenCode session using it\n", svc.PID)
	case enabled:
		fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is using Cortex.\n", svc.PID)
	case status:
		fmt.Fprintf(stdout, "OpenCode's background service (pid %d) is not using Cortex.\n", svc.PID)
	}
}
