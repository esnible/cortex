# Rename `abctl` → `agentop`

**Date:** 2026-09-30
**Status:** approved, implementing

## Goal

The operator-facing CLI/TUI is renamed from `abctl` to `agentop` everywhere it is
current: the binary, its Go module and directory, its on-disk names, the release
tarballs, the installer, CI, and every current doc.

`abctl` shipped in a stable release (v0.7.0, 2026-09-24), so this is a change to an
interface on users' machines, not only to source.

## Decisions

**Clean break for the command.** There is no `abctl` alias. `scripts/install.sh` and
`make dev-install` delete a `~/.local/bin/abctl` they find, but only when the file
embeds this repo's module path (`github.com/rossoctl/cortex/(authbridge/)?cmd/abctl`):
judged by reading it, not by running it, so a build too old to know `--version` and a
different tool that happens to be called `abctl` are never executed, and the latter is
left alone. Reason for deleting it at all: a stale `abctl`
manages the same launchd label / systemd unit as `agentop`, and the service code already
documents that two managers disagreeing about the unit's stamp leaves it "live and
unmanageable" — observed on a real machine.

**No compatibility code for on-disk state either.** What v0.7.0 wrote, and what happens
to it:

| v0.7.0 artifact | After the rename |
|---|---|
| `AbctlVersion` / `X-AbctlVersion` unit stamp | Reads as unstamped, so `serviceIsCurrent` is false and `agentop service install` rewrites the unit. No code needed. |
| `~/.cortex/abctl-config.yaml` (events-table columns, filter) | Not read. Preferences start fresh in `agentop-config.yaml`. |
| `~/.cortex/abctl-events/` (yanked events) | Not read. New yanks go to `agentop-events/`. |
| `linger-enabled-by-abctl` (Linux) | Not read, so `agentop service uninstall` leaves that linger on. Harmless. |

`bobshell` and `bob` never shipped in a release. A block written by a dev-installed
`abctl configure bobshell enable` still calls `abctl` and will not match `agentop`'s
`disable`; it has to be removed by hand, then `agentop configure bobshell enable` run.

**The name collides with a published crate.** crates.io has an `agentop` (v1.0.0,
~225 downloads): a TUI that inspects Claude Code and Codex processes, installed by
`cargo install` into `~/.cargo/bin`. Accepted: ours installs to `~/.local/bin`, and
which one a shell runs is that shell's PATH order.

**Unchanged on purpose:**

- `docs/superpowers/` — dated design records describe what was designed at the time.
  Same treatment the Kagenti → Rossoctl rename gave them.
- The launchd label `io.rossoctl.cortex` and systemd unit `cortex.service`: neither
  contains `abctl`, and the unit runs `authbridge-proxy`, so the running service is not
  disturbed by the rename.
- Every frozen AuthBridge identifier listed in `CLAUDE.md`.
- `agentop claude-code` stays as the old spelling of `agentop configure claude-code`.

## Shape

One PR, reviewable commit by commit:

1. **Pure moves.** `git mv cmd/abctl cmd/agentop` and the three other `abctl`-named
   files (two `launch-abctl.sh` demo scripts, `demo-with-abctl.md`). Only path and
   module references change, so git records exact renames.
2. **Mechanical substitution.** Case-preserving `abctl`→`agentop`, `Abctl`→`Agentop`,
   `ABCTL`→`AGENTOP` across every tracked text file except `docs/superpowers/` and the
   generated `docs/assets/cortex-demo.svg`. The command is in the PR body, so a
   reviewer can re-run it and diff instead of reading it.
3. **Formatting fallout.** `gofmt` and column re-alignment where the two extra characters
   broke it — kept apart so commit 2 stays exactly reproducible.
4. **Hand changes.** Stale-binary cleanup in `install.sh` (with tests) and
   `dev-install`; the regenerated README demo SVG; the `CLAUDE.md` naming rule; a
   release-notes line.

## Risks worth checking by hand

- Release tarball names: `release-binaries.yaml` and `install.sh` must agree, or the
  first release after this has no downloadable `agentop` tarball.
- `ABCTL_SYSTEMD_TESTS=required` in `ci.yaml` is read by a test. Both sides must be
  renamed together, or the required gate silently becomes a skip.
- The on-disk names in the table above.

## Follow-ups (not in this PR)

- `rossoctl/rossoctl`: `docs/get-started/cli.md` and `docs/get-started/laptop.md`.
- Delete the stale `abctl_*` assets from the `main-latest` release by hand;
  `gh release upload --clobber` never prunes.
