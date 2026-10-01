# Rename the `authbridge-*` sidecar binaries → `cortex*`

**Date:** 2026-09-30
**Status:** approved, implementing

## Goal

The four sidecar binaries take the product's name. Their directories, Go modules, binary
names, release tarballs, logger names and the binary inside each image change:

| Before | After |
|---|---|
| `authbridge-proxy` (`cmd/`, module, binary, `authbridge-proxy{,-lite,-sessionbudget}_<ver>_…` tarballs) | `cortex` (`cortex{,-lite,-sessionbudget}_<ver>_…`) |
| `authbridge-envoy` | `cortex-envoy` |
| `authbridge-cpex` | `cortex-cpex` |
| `authbridge-praxis` | `cortex-praxis` |

`agentop service install` looks for `cortex` beside itself, then on `PATH`, then at
`~/.local/bin/cortex`.

## What keeps the old spelling, and why

The same strings name things this repo does not own, and those stay:

- **The operator's container name `authbridge-proxy`** (`AuthBridgeProxyContainerName` in
  `rossoctl/operator`). The operator sets only `Args` on that container, never `Command`,
  so the image's `entrypoint.sh` chooses the binary — renaming the binary inside the
  image is invisible to it. Every `kubectl … -c authbridge-proxy`, the demos' sidecar
  detection, and agentop's pod-picker set (`cmd/agentop/cluster`) address the container.
- **Image names** `authbridge`, `authbridge-envoy`, `authbridge-lite`, `authbridge-cpex`:
  the operator selects images by name.
- **Demo-owned container names**, such as `demos/hr-cpex`'s `authbridge-cpex` container,
  kept so a demo's manifests and its `kubectl -c` lines stay in step without touching
  either.
- `AUTHBRIDGE_*`, `x-authbridge-*`, `/etc/authbridge`, and the ConfigMaps — already
  frozen, and none of them spells a binary name.

`CLAUDE.md`'s frozen table loses its binaries row and gains the container name.

## Clean break

Same policy as the abctl rename: no alias, and no old name recognised.

- **Process identity matches the new basename exactly.** `agentop`'s `runningPID` and
  `install.sh`'s `proxy_running` compare the basename of `ps -o comm=` to `cortex`. Not a
  substring: macOS reports the full path, and `cortex` is part of the repo, the product
  and `~/.cortex`, so `Contains("cortex")` would match a process under any checkout of
  this repo — and `stopPID` signals what it matches.
- **A proxy started by hand before the upgrade is not recognised.** Stop it first. One
  run by the service is unaffected: launchd and systemd tear it down by label.
- **`~/.local/bin/authbridge-proxy` is deleted** by `install.sh` and `make dev-install`
  only when it is ours (it embeds `github.com/rossoctl/cortex/(authbridge/)?cmd/authbridge-proxy`)
  **and** no installed service unit still names it — otherwise `--install-only` would
  leave launchd starting a deleted binary.
- In clusters, the sidecar's logger name changes from `authbridge-proxy` to `cortex`; the
  container name does not.

## Method

1. **Pure moves** — `scripts` below, `cx-moves.sh`.
2. **Curated substitution** — `cx-subst.pl` below. It masks the keep-list first, then
   renames `authbridge-proxy` everywhere unmasked (it is never an image name), and renames
   the other three only where a binary is meant: their own `cmd/` directories, the
   ignore files, lines that say "binary", and the ext_proc binary beside Envoy.
   Reproducible: run both scripts from the parent of the moves commit.
3. **Hand-read fallout** — prose the rules misjudge (about fifteen lines that name a
   binary without saying so), brace and glob paths (`cmd/authbridge-{…}`), alignment
   (`cortex` is ten characters shorter), articles, two test assertions the shorter name
   weakened.
4. **Hand changes** — exact-basename process checks, stale-binary removal with its unit
   guard, the frozen table, a release-notes line, the README demo SVG.

## Risks

- `cortex` is also the binary of the Prometheus long-term-storage project. agentop looks
  beside itself before `PATH`, and the process check is an exact basename, so a collision
  needs that tool installed and found first. Accepted.
- PR CI does not build images (`build.yaml` runs on tags and `main`), so each Dockerfile
  and `entrypoint.sh` is built and started locally before merge.
- `cortex-praxis --version` prints `cortex`: praxis printed `authbridge-proxy` before,
  a pre-existing copy of the proxy's line. Left as it was.

## Follow-ups (not in this PR)

- `rossoctl/operator`: comments and docs that call the binary `authbridge-proxy`
  (`config/defaults.go`, `config/types.go`, the AgentRuntime type docs, `authbridge-webhook.md`).
- `rossoctl/rossoctl`: docs that name the binary; `rossoctl-cli`'s comment pointing at
  `cmd/authbridge-proxy/plugins_sparcplugin.go`.

## Appendix: the scripts

### cx-moves.sh

```sh
#!/bin/sh
# Pure moves: the four cmd/ dirs, plus every path-shaped reference to them.
set -eu
git mv cmd/authbridge-proxy  cmd/cortex
git mv cmd/authbridge-envoy  cmd/cortex-envoy
git mv cmd/authbridge-cpex   cmd/cortex-cpex
git mv cmd/authbridge-praxis cmd/cortex-praxis
git grep -lzE 'cmd/authbridge-(proxy|envoy|cpex|praxis)' -- ':!docs/superpowers' \
  | xargs -0 perl -pi -e 's#cmd/authbridge-proxy#cmd/cortex#g; s#cmd/authbridge-(envoy|cpex|praxis)#cmd/cortex-$1#g'
# cmd/README.md links its siblings relatively.
perl -pi -e 's#\[`authbridge-proxy/`\]\(authbridge-proxy/\)#[`cortex/`](cortex/)#; s#\[`authbridge-(envoy|cpex|praxis)/`\]\(authbridge-\1/\)#[`cortex-$1/`](cortex-$1/)#' cmd/README.md
# ci.yaml's cmd matrix is keyed by directory name.
perl -pi -e "s#^(\s+)- authbridge-proxy\$#\${1}- cortex#; s#^(\s+)- authbridge-(envoy|praxis)\$#\${1}- cortex-\$2#; s#matrix\.binary == 'authbridge-proxy'#matrix.binary == 'cortex'#g; s#matrix\.binary == 'authbridge-envoy'#matrix.binary == 'cortex-envoy'#g; s#^(\s+)authbridge-proxy\) profiles=#\${1}cortex) profiles=#; s#^(\s+)authbridge-envoy\) profiles=#\${1}cortex-envoy) profiles=#" .github/workflows/ci.yaml
```

### cx-subst.pl

```perl
#!/usr/bin/perl
# Rename the sidecar binaries; keep the container and image names that share their spelling.
#   git grep -lzIE 'authbridge-(proxy|envoy|cpex|praxis)' -- ':!docs/superpowers' ':!docs/assets/cortex-demo.svg' \
#     | xargs -0 perl -i cx-subst.pl
use strict; use warnings;
my $N = qr/authbridge-(?:proxy|envoy|cpex|praxis)/;
while (<>) {
  my @keep; my $hold = sub { push @keep, $_[0]; "\x00$#keep\x00" };
  # 1. Image names: registry paths, tags, loads, and prose naming an image.
  s{((?:ghcr\.io/rossoctl/cortex/|localhost/|library/|docker-image |image: )$N)}{$hold->($1)}ge;
  s{($N)(?=:[\w\$])}{$hold->($1)}ge;
  s{($N)(?=`?(?: \(?combined\)?)? image)}{$hold->($1)}ge;
  # 2. Container names: the operator's authbridge-proxy, demo-owned ones, the picker's set.
  s{($N)(?=": \{)|(?<="name": ")($N)|(?<=- name: )($N)|(?<=ContainerName = ")($N)|(?<=-c )($N)|(?<=:-)($N)(?=\})}{$hold->($1 // $2 // $3 // $4 // $5 // $6)}ge;
  s{(?<=\^\()($N)}{$hold->($1)}ge;
  s{(?<=container ")($N)|($N)(?=`?\s+(?:container|sidecar|logs)\b)|(?<=\+ )(authbridge-proxy)(?=\))|(?<=" \+ ")($N)}{$hold->($1 // $2 // $3 // $4)}ge;
  # Lines that list the sidecar container per mode keep every name on them.
  s{($N)(?=`? in proxy-sidecar)}{$hold->($1)}ge;
  s{($N)}{$hold->($1)}ge if /envoy-proxy|proxy-sidecar \(default\)|\bSIDECAR\b/ || /container name/i;
  # 3. authbridge-proxy is never an image, so it is the binary wherever it is left.
  s/authbridge-proxy/cortex/g;
  # 4. The others mostly name an image or a sidecar; rename them only where a binary is meant.
  # Beside Envoy in its image, authbridge-envoy is the ext_proc binary.
  s/(?<=Envoy \+ )authbridge-envoy/cortex-envoy/g;
  if ($ARGV =~ m{(^|/)\.(git|docker)ignore$|^cmd/cortex(-\w+)?/} || /\bbinar(?:y|ies)\b/) {
    s/authbridge-(envoy|cpex|praxis)/cortex-$1/g;
  }
  s/\x00(\d+)\x00/$keep[$1]/g;
  print;
} continue { close ARGV if eof }
```
