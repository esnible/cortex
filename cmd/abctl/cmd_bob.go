package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rossoctl/cortex/core/tlsbridge"
)

const (
	// bobSettingsRel is where IBM Bob keeps its user settings on macOS. Bob is a
	// VS Code fork, so this is VS Code's own layout with Bob's product name in it.
	bobSettingsRel = "Library/Application Support/IBM Bob/User/settings.json"

	// bobProxyKey is the one setting this command writes. VS Code's settings
	// document is FLAT — dotted keys are literal top-level keys, not a nested
	// object — so this is the whole path, unlike claude-code's "env" block.
	bobProxyKey = "http.proxy"

	// bobCACommonName is the subject every generated bridge CA carries. A literal
	// rather than an import: bridgeCACommonName lives in package main of
	// cmd/authbridge-proxy and is not importable from here. darwinGoNote already
	// carries the same copy for the same reason.
	bobCACommonName = "authbridge-tls-bridge-ca"

	// bobSystemKeychain is machine-wide trust, which is the right scope for a
	// proxy that every process on the box is pointed at.
	bobSystemKeychain = "/Library/Keychains/System.keychain"
)

const bobUsage = `abctl configure bob — route IBM Bob through Cortex via its settings.json

Usage:
  abctl configure bob enable  [--yes] [--settings PATH] [--config PATH]
  abctl configure bob disable [--yes] [--settings PATH] [--config PATH]
  abctl configure bob status  [--settings PATH] [--config PATH]

Flags:
  --yes            do not prompt for confirmation
  --settings PATH  IBM Bob's settings file. Defaults, on macOS only, to
                   ~/Library/Application Support/IBM Bob/User/settings.json
  --config PATH    Cortex config to read the proxy address from
                   (default ~/.cortex/config.yaml)

IBM Bob is a VS Code fork, so it has a settings file and one flat key decides
where its networking goes: "http.proxy". enable sets it to Cortex's forward
proxy, read from ~/.cortex/config.yaml so the address always matches the proxy
that is actually running. Only that one key is written — every other setting is
left exactly as it was, and the first write copies the original to
settings.json.bak and never overwrites that copy.

disable removes "http.proxy" ONLY when it points at Cortex. A value that does
not — a corporate proxy, say — is reported and left alone, because a proxy abctl
did not write is not abctl's to delete.

The proxy is only half of it. Bob's requests are terminated by Cortex's TLS
bridge, so Bob must also trust the bridge CA, and that is an OS trust-store
change abctl does not make for you: it needs sudo, and a tool that silently
escalates to alter machine-wide trust is not one you can audit. enable prints
the command that does it, disable prints how to undo it, status prints how to
check it.

"abctl configure bobshell" is a different thing and the two are independent.
That one defines a "bob" shell function so typing "bob" runs through Cortex;
this one configures what the editor does on its own.

Comments: VS Code permits them in settings.json and this command does not. It
reads the file as strict JSON and refuses a commented one by name rather than
rewriting it, because a rewrite would silently delete the comments.

Exit status: 0 applied, already correct, or reported; 3 declined — at the prompt
or because there was no terminal to ask on; 1 something went wrong; 2 a usage
error.
`

// bobConfirm prompts before a write. A var so tests can substitute it: `go test`
// inherits the terminal it was launched from, so an unstubbed prompt blocks
// waiting on a human.
//
// Deliberately a separate var from bobShellConfirm rather than a reuse of it.
// They are identical today, but sharing one would mean a test stubbing one
// verb's prompt silently disarms the other command's too — and a test that
// cannot fail is worse than a duplicated three-line closure.
var bobConfirm = func(path, what string, stdout io.Writer) bool {
	fmt.Fprintf(stdout, "%s %s\n", what, path)
	return confirm(stdout)
}

// runBob dispatches `abctl configure bob`. Returns the process exit code.
func runBob(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, bobUsage)
		return 2
	}
	action := args[0]
	// Before anything else: --help asks for this command's usage, and reading it
	// as an action name would send someone looking for the command list to the one
	// branch that refuses to print it. Same split as bobshell's and claude-code's.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, bobUsage)
		return 0
	}

	// The verb is validated HERE, before any environment or filesystem work, for
	// the reason runBobShell documents at length: a step below can answer
	// successfully on its own — bobSettingsPath's off-macOS arm prints advice —
	// so validating the action last would report success for a verb that does not
	// exist. Nothing downstream can reach this check, so it comes first.
	switch action {
	case "enable", "disable", "status":
	default:
		fmt.Fprintf(stderr, "abctl: unknown bob action %q (enable, disable, status)\n", action)
		return 2
	}

	fs := flag.NewFlagSet("configure bob "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	settingsPath := fs.String("settings", "", "IBM Bob settings file")
	// Registered for all three verbs, status included: status reports whether the
	// value still matches the proxy the config names now, which needs the config.
	cortexCfgPath := fs.String("config", "", "Cortex config file")
	// --yes for enable and disable ONLY. status writes nothing, so it has nothing
	// to confirm; registering it unconditionally would make `status --yes` parse
	// and be silently ignored, which is the same "accepted and did nothing" that
	// `status extra` is already a usage error for. Read back into a plain bool
	// below so the call sites cannot nil-deref.
	var yesFlag *bool
	if action == "enable" || action == "disable" {
		yesFlag = fs.Bool("yes", false, "do not prompt for confirmation")
	}
	// The FlagSet's own usage would print a bare header and a short flag list;
	// this command's usage is the useful answer, and suppressing it here keeps
	// -h's single copy on stdout below.
	fs.Usage = func() {}
	if err := fs.Parse(args[1:]); err != nil {
		// -h and --help arrive as flag.ErrHelp, and asking for help is not a usage
		// error: stdout and 0. Any other parse failure is real, and Parse has
		// already named it on stderr.
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, bobUsage)
			return 0
		}
		return 2
	}
	yes := yesFlag != nil && *yesFlag
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "abctl: bob %s takes no arguments (got %q)\n", action, fs.Arg(0))
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "abctl: cannot determine your home directory: %v\n", err)
		return 1
	}
	if *cortexCfgPath == "" {
		*cortexCfgPath = filepath.Join(home, cortexCfgRel)
	}
	if *settingsPath == "" {
		p, perr := bobSettingsPath(home)
		if perr != nil {
			// 2, not 1: the missing fact is one the caller can supply on the command
			// line, which makes this a usage error rather than a failure. Unlike
			// bobshell's off-platform arm there is no block of text to paste that
			// would constitute an answer on its own.
			fmt.Fprintf(stderr, "abctl: %v.\n  Pass --settings PATH to point at it.\n", perr)
			return 2
		}
		*settingsPath = p
	}

	switch action {
	case "enable":
		// enable derives the CA path itself, from the config it already requires.
		return bobEnable(*settingsPath, *cortexCfgPath, yes, stdout, stderr)
	case "disable":
		return bobDisable(*settingsPath, bobCAPath(*cortexCfgPath, home), yes, stdout, stderr)
	default:
		return bobStatus(*settingsPath, *cortexCfgPath, bobCAPath(*cortexCfgPath, home), stdout)
	}
}

// bobSettingsPath is where IBM Bob's settings live, for the platforms where that
// is known.
//
// macOS only, deliberately. ~/.config/IBM Bob/User/settings.json is the VS Code
// convention on Linux and would be the obvious guess, but it is unverified for
// this app — and writing a proxy setting into a file nothing reads is a silent
// no-op that leaves the user no reason to look there. bobShellRCPath makes the
// same call for an unrecognised shell: claim only what you know, and say so
// otherwise. --settings covers every platform, so nothing is blocked.
//
// home is a parameter rather than read from the environment, so the mapping is
// testable and runBob reads $HOME exactly once.
func bobSettingsPath(home string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("I only know where IBM Bob keeps its settings on macOS, and this is %s", runtime.GOOS)
	}
	return filepath.Join(home, bobSettingsRel), nil
}

// bobIsCortexProxy reports whether an http.proxy value is one abctl wrote,
// structurally: loopback host, port in Cortex's 476xx block, http scheme.
//
// Deliberately NOT isCortexValue, which is the same question asked with
// strings.Contains. That form matches a substring anywhere, so
// "http://corp.example.com/?next=127.0.0.1:47600" and the hostname
// "localhost:47600.evil.com" both satisfy it. In claude-code the consequence of
// a false positive is REFUSING to overwrite, so it fails safe; here the
// consequence is delete, and the direction of failure inverts — a false positive
// silently removes someone's corporate proxy. Parsing and comparing Hostname()
// and Port() whole cannot be fooled by a path or a suffixed host.
//
// It also reads nothing from the config, which is what lets disable keep working
// after forward_proxy_addr moves, after wantedFromLoaded rewrites a bind address
// to "localhost", and — the case that decides it — when the config is gone
// entirely. Someone who has uninstalled Cortex and wants Bob working again must
// not find the off switch broken by the absence of the thing being switched off.
//
// The residual false positive is an unrelated loopback proxy of the user's own
// on a 476xx port. Accepted: that is a deliberate collision with Cortex's
// documented port block, disable names the exact value in the prompt before
// removing it, and writeSettings has already saved a .bak. Exact comparison
// against the derived value still earns its place in status, which reports drift
// rather than acting on it.
func bobIsCortexProxy(val string) bool {
	u, err := url.Parse(strings.TrimSpace(val))
	if err != nil || u.Host == "" {
		return false
	}
	// http only: it is the only scheme enable ever writes, so anything else is
	// someone else's value.
	if u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
	default:
		return false
	}
	return strings.HasPrefix(u.Port(), "476")
}

// bobTrustNote is the trust-store step abctl does NOT take, printed for the user
// to run.
//
// Printed rather than executed on purpose. Adding a root to the System keychain
// needs sudo, and a tool that escalates on its own to change machine-wide trust
// is not one anybody can audit after the fact — the user should see the exact
// command before it runs. Same reasoning as darwinGoNote, which prints its
// keychain command too.
//
// caPath is ca.crt — the single bridge CA — and NOT bundle.crt, even though
// bundle.crt is the file most of abctl's other CA messages name. The keychain is
// ADDITIVE, so it wants one certificate; bundle.crt exists for the tools whose CA
// setting REPLACES their trust store, and it holds the bridge CA followed by
// every platform root (129 certificates on this machine).
// `add-trusted-cert -r trustRoot` on that file would install explicit root-trust
// settings for ~128 unrelated public CAs machine-wide, which is both far broader
// than intended and not undone by the single delete-certificate below. See
// core/tlsbridge/bundle.go's header, which states which file is for which job.
func bobTrustNote(caPath string) string {
	q := shellQuote(caPath)
	if runtime.GOOS == "darwin" {
		return "Bob must also trust the bridge CA, or every https request fails verification.\n" +
			"  That is a machine-wide trust change needing sudo, so abctl does not make it\n" +
			"  for you — run:\n\n" +
			"    sudo security add-trusted-cert -d -r trustRoot \\\n" +
			"      -k " + bobSystemKeychain + " " + q + "\n\n" +
			"  That is the single bridge CA, not the " + tlsbridge.TrustBundleName + " in the same\n" +
			"  directory: the keychain adds to what it already trusts, so it wants one\n" +
			"  certificate. The bundle carries every platform root as well, and trusting it\n" +
			"  as a root would change how this machine treats certificates that have\n" +
			"  nothing to do with Cortex.\n\n"
	}
	// Suggestive by design. The exact route differs per distribution and a
	// confidently wrong command is worse than a named one plus a caveat.
	return "Bob must also trust the bridge CA, or every https request fails verification.\n" +
		"  Add " + q + " to this system's trusted certificate store. The exact\n" +
		"  step depends on the distribution; the usual ones are:\n\n" +
		"    Debian/Ubuntu: copy it into /usr/local/share/ca-certificates/ (renamed to\n" +
		"                   end in .crt) and run sudo update-ca-certificates\n" +
		"    Fedora/RHEL:   copy it into /etc/pki/ca-trust/source/anchors/ and run\n" +
		"                   sudo update-ca-trust\n\n" +
		"  Some applications keep their own trust store and need it added there too.\n" +
		"  That is the single bridge CA, not the " + tlsbridge.TrustBundleName + " beside it, which\n" +
		"  additionally carries every platform root.\n\n"
}

// bobUntrustNote is the undo for bobTrustNote, and says plainly that it is
// optional: a CA left in the store signs nothing but Cortex's own forged leaves,
// and removing it is tidiness rather than a fix.
func bobUntrustNote(caPath string) string {
	if runtime.GOOS == "darwin" {
		// The keychain has to be named: an add to the System keychain is not undone
		// by a delete that defaults to the login one. -t drops the trust settings
		// add-trusted-cert created along with the certificate itself.
		return "Optionally, remove the bridge CA from the keychain as well:\n\n" +
			"    sudo security delete-certificate -c " + bobCACommonName + " \\\n" +
			"      -t " + bobSystemKeychain + "\n\n" +
			"  Safe to leave in place if you expect to re-enable: it only validates\n" +
			"  certificates Cortex itself issues, so nothing else starts being trusted\n" +
			"  because it is there.\n\n"
	}
	return "Optionally, remove " + shellQuote(caPath) + " from this system's\n" +
		"  trusted certificate store — delete the copy you added (under\n" +
		"  /usr/local/share/ca-certificates/ or /etc/pki/ca-trust/source/anchors/) and\n" +
		"  re-run update-ca-certificates / update-ca-trust.\n\n" +
		"  Safe to leave in place if you expect to re-enable: it only validates\n" +
		"  certificates Cortex itself issues.\n\n"
}

// bobVerifyNote is status's suggestion, and is only ever printed — running
// verify-cert here would turn a report into an action, and status writes nothing.
//
// Nothing is printed off macOS: there is no portable equivalent to suggest, and
// inventing one is the confidently-wrong-command failure bobTrustNote's non-mac
// arm already hedges against.
func bobVerifyNote(caPath string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return "To check that this machine trusts the bridge CA:\n\n" +
		"    security verify-cert -c " + shellQuote(caPath) + "\n\n" +
		"  Not run here — status reports, it does not act. A failure does not by itself\n" +
		"  mean Bob is misconfigured: the CA only matters for hosts the bridge\n" +
		"  terminates, and it is trusted separately from the proxy setting above.\n"
}

// bobCAPath is the bridge CA path for a message, best-effort.
//
// Best-effort because disable and status must keep working when the config does
// not: a user who has uninstalled Cortex still needs the off switch, and
// refusing to print a cleanup hint because the config that named the CA is gone
// would be a worse answer than printing the conventional path. Only enable
// requires the config, and it checks that for itself.
func bobCAPath(cortexCfgPath, home string) string {
	if want, _, err := wantedFromConfig(cortexCfgPath); err == nil && want[envCACerts] != "" {
		return want[envCACerts]
	}
	return filepath.Join(home, ".cortex", "ca", "ca.crt")
}

func bobEnable(settingsPath, cortexCfgPath string, yes bool, stdout, stderr io.Writer) int {
	want, cfg, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	// Same gate as claude-code and exec, and for the same reason: with the bridge
	// off Cortex terminates no TLS, so pointing Bob at the proxy would send every
	// https request into something that cannot answer it.
	if !bridgeEnabled(cfg) {
		fmt.Fprintf(stderr, "abctl: %v\n", errBridgeDisabled(cortexCfgPath))
		return 1
	}
	caPath := want[envCACerts]
	if caPath == "" {
		fmt.Fprintf(stderr, "abctl: %s has no tls_bridge.ca_dir, so there is no CA for IBM Bob to\n"+
			"  trust; every https request would fail certificate verification. Enable the\n"+
			"  TLS bridge first.\n", cortexCfgPath)
		return 1
	}
	proxy := want[envProxy]

	doc, err := readSettings(settingsPath)
	if err != nil {
		// readSettings names the file and says to fix or move it. The clause about
		// comments is bob's own: Bob is a VS Code fork, VS Code permits comments in
		// settings.json and writes them back, so a commented file is the likeliest
		// way to arrive here — and the generic "not valid JSON" does not hint at it.
		fmt.Fprintf(stderr, "abctl: %v.\n"+
			"  If Bob has saved comments in it, that is why: VS Code allows them and this\n"+
			"  command reads strict JSON. Remove them, or point --settings elsewhere —\n"+
			"  abctl will not rewrite the file to strip them.\n", err)
		return 1
	}

	switch existing := doc[bobProxyKey].(type) {
	case nil:
		// Not set. Nothing to weigh.
	case string:
		if existing == proxy {
			fmt.Fprintf(stdout, "Already enabled: %s routes IBM Bob through Cortex.\n", settingsPath)
			return 0
		}
		if !bobIsCortexProxy(existing) {
			// Refuse rather than overwrite: the overwhelmingly likely owner of a
			// foreign value is a corporate proxy the user needs, and this command
			// keeps no record that could restore it.
			fmt.Fprintf(stderr, "abctl: %s already sets %q to %q, which is not a Cortex proxy.\n"+
				"  Leaving it alone — remove or change it yourself if you want Cortex there\n"+
				"  instead.\n", settingsPath, bobProxyKey, existing)
			return 1
		}
		// Ours but stale — the proxy moved. Falls through to the write.
	default:
		// A non-string (a number, an object) is not something this command wrote and
		// not something it can compare. Same refusal as a foreign string.
		fmt.Fprintf(stderr, "abctl: %s sets %q to a non-string value (%T).\n"+
			"  Leaving it alone — fix it yourself if you want Cortex there instead.\n",
			settingsPath, bobProxyKey, existing)
		return 1
	}

	// Enabling before Cortex's first start is legitimate — the proxy generates the
	// CA on boot — but the trust command printed below would fail on a missing
	// file, so say so now rather than let it be discovered at the sudo prompt.
	if _, serr := os.Stat(caPath); serr != nil {
		fmt.Fprintf(stdout, "Note: %s does not exist yet.\n"+
			"  Cortex creates it on first start. Start Cortex, then run the trust command\n"+
			"  below — until the CA is trusted, Bob's https requests fail verification.\n\n",
			caPath)
	}

	fmt.Fprintf(stdout, "Sets in %s:\n  %q: %q\n", settingsPath, bobProxyKey, proxy)
	fmt.Fprintf(stdout, "Nothing else in the file changes; a copy is kept as %s.bak\n\n", settingsPath)
	if !yes && !bobConfirm(settingsPath, "Write to", stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	doc[bobProxyKey] = proxy
	if werr := writeSettings(settingsPath, doc); werr != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", werr)
		return 1
	}

	// Deliberately not "all Bob traffic now goes through Cortex": http.proxy is what
	// VS Code's own networking and its extension host read, and it is the only
	// documented lever — but an extension bundling its own HTTP client can bypass
	// it. Claiming coverage abctl cannot deliver is how a user stops looking for the
	// real reason something is unparsed.
	fmt.Fprintf(stdout, "\nEnabled. %q in %s now points at Cortex — this is the setting\n"+
		"VS Code forks read for their own networking and their extension host.\n\n",
		bobProxyKey, settingsPath)
	// Restart, rather than a claim either way about live pickup: writeSettings is
	// temp+rename so Bob never sees a half-written file, but whether Bob re-reads a
	// proxy change without restarting is not something this command has verified.
	fmt.Fprint(stdout, "Restart Bob so it re-reads its settings.\n\n")
	fmt.Fprint(stdout, bobTrustNote(caPath))
	fmt.Fprintf(stdout, "Undo with: abctl configure bob disable\n")
	return 0
}

func bobDisable(settingsPath, caPath string, yes bool, stdout, stderr io.Writer) int {
	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}

	existing, ok := doc[bobProxyKey]
	if !ok {
		fmt.Fprintf(stdout, "Not enabled: %s sets no %q. Nothing to do.\n", settingsPath, bobProxyKey)
		return 0
	}
	s, isString := existing.(string)
	if !isString || !bobIsCortexProxy(s) {
		// Exit 0, not 1. "Remove it only if it points at Cortex" is the contract, and
		// this file already satisfies it — there is nothing for the user to fix, so
		// reporting a failure would be wrong.
		fmt.Fprintf(stdout, "%s sets %q to %v, which is not a Cortex proxy.\n"+
			"  Left alone — abctl removes only values it would have written.\n",
			settingsPath, bobProxyKey, existing)
		return 0
	}

	fmt.Fprintf(stdout, "Removes from %s:\n  %q: %q\n", settingsPath, bobProxyKey, s)
	fmt.Fprintf(stdout, "Nothing else in the file changes; a copy is kept as %s.bak\n\n", settingsPath)
	if !yes && !bobConfirm(settingsPath, "Write to", stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	delete(doc, bobProxyKey)
	if werr := writeSettings(settingsPath, doc); werr != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", werr)
		return 1
	}

	fmt.Fprintf(stdout, "\nDisabled. IBM Bob no longer routes through Cortex.\n")
	fmt.Fprint(stdout, "Restart Bob so it re-reads its settings.\n\n")
	fmt.Fprint(stdout, bobUntrustNote(caPath))
	return 0
}

func bobStatus(settingsPath, cortexCfgPath, caPath string, stdout io.Writer) int {
	// Always exit 0: "not enabled" is a successful report, the same call
	// claudeCodeStatus and bobShellStatus make. A non-zero status here would make
	// `abctl configure bob status` unusable in a shell conditional for anything but
	// "is it on".
	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stdout, "not enabled (%v)\n", err)
		return 0
	}

	switch existing := doc[bobProxyKey].(type) {
	case nil:
		fmt.Fprintf(stdout, "  %q (unset)\nnot enabled in %s\n", bobProxyKey, settingsPath)
	case string:
		fmt.Fprintf(stdout, "  %q=%s\n", bobProxyKey, existing)
		switch {
		case !bobIsCortexProxy(existing):
			fmt.Fprintf(stdout, "not enabled in %s (that is not a Cortex proxy)\n", settingsPath)
		default:
			// Drift is reported, never acted on. wantedFromConfig is consulted only
			// here, and its failure is not this report's failure: the value is still
			// a Cortex proxy whatever the config says, so an unreadable config
			// downgrades the answer rather than breaking it.
			want, _, werr := wantedFromConfig(cortexCfgPath)
			switch {
			case werr != nil:
				fmt.Fprintf(stdout, "enabled in %s (could not read %s to compare: %v)\n",
					settingsPath, cortexCfgPath, werr)
			case want[envProxy] != existing:
				fmt.Fprintf(stdout, "enabled in %s, but %s now names %s —\n"+
					"  re-run `abctl configure bob enable` to move it.\n",
					settingsPath, cortexCfgPath, want[envProxy])
			default:
				fmt.Fprintf(stdout, "enabled in %s\n", settingsPath)
			}
		}
	default:
		fmt.Fprintf(stdout, "  %q=%v\nnot enabled in %s (that value is a %T, not a string)\n",
			bobProxyKey, existing, settingsPath, existing)
	}

	if note := bobVerifyNote(caPath); note != "" {
		fmt.Fprint(stdout, "\n"+note)
	}
	return 0
}
