# Flattening `authbridge/` into the repo root

Status: implemented · 2026-09-24 · targets the whole repository layout

## 1. Problem

`authbridge/` holds 1,005 of the repository's 1,061 tracked files. It dates from
when this repo was `kagenti-extensions` and held several extensions; the only
other one, `client-registration`, has since moved into the operator. The
directory is now a level of nesting that separates nothing from nothing.

The cost is not aesthetic. It shows up in three places:

- **The public install command** carries it:
  `raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install.sh`.
- **Every Go module path** carries it: `github.com/rossoctl/cortex/authbridge/authlib`
  and eleven siblings.
- **Every path reference** carries it: 36 in workflows, 6 module directories in
  `dependabot.yml`, plus `Makefile`, `local-build-and-test.sh` and 97 non-Go
  files.

It also gets more expensive the longer it waits: the same sweep was 326 files at
the Kagenti→Rossoctl rename and 214 at rossocortex→cortex. It is 1,005 now.

## 2. Decisions

Four questions were settled before design:

| Question | Decision |
|---|---|
| Scope | **Structural only.** Paths lose the `/authbridge` segment; no package or binary is renamed. `authlib` stays `authlib`, `cmd/authbridge-proxy` keeps its name. |
| Published install URL | **No shim.** The canonical one-liner changes to `main/install.sh`; the old path 404s. See §6 — this was revised after measuring who actually references it. |
| `docs/` merge | **Flat union**, no reorganisation. |
| `README.md` / `CLAUDE.md` collisions | **Resolved inside the sweep**, in separate commits. |

A fifth constraint arrived during review and is load-bearing: **the installer
must not break in any form — the published one-liner may change, but no existing
invocation may fail.** Downstream repositories are permitted to break and will
be fixed afterwards.

One option was considered and is impossible: keeping the module paths while
moving the directories. Go resolves a submodule as `<repo>/<subdir>`, so the
path suffix must match the directory. Moving `authbridge/authlib` to `authlib`
*forces* the module path change.

## 3. Target layout

```
cortex/
├── README.md                 # product front page, unchanged
├── CLAUDE.md                 # merged: root (442) + authbridge (695)
├── CONTRIBUTING.md  SECURITY.md  LICENSE  Makefile
├── install.sh                # ← authbridge/install.sh (new canonical URL)
├── install_test.sh  install-demo.sh
├── local-build-and-test.sh  verify-spire-keycloak.sh  keycloak_sync.py
├── go.work                   # ← authbridge/go.work
├── requirements.txt  pyproject.toml  uv.lock  pyrightconfig.json
├── .gitignore  .pre-commit-config.yaml  .dockerignore   # .dockerignore is new
│
├── authlib/                  # ← authbridge/authlib (476 files)
├── cmd/
│   ├── abctl/                # 177 files
│   ├── authbridge-proxy/  authbridge-envoy/
│   ├── authbridge-cpex/   authbridge-praxis/
│   └── README.md
├── storage/redis/
├── proxy-init/
├── sparc-service/
├── lineage-attach/
├── demos/                    # 12 scenarios
├── docs/                     # union: 5 root + 42 authbridge = 47 files
├── scripts/                  # union: hooks/ + profile-tags/ + readme-demo/
├── tests/                    # now beside keycloak_sync.py
└── .github/  .claude/
```

There is no `authbridge/` directory afterwards. Nothing survives at that path.

`LOCAL_TESTING_GUIDE.md` is absent: PR #1126 removes it.

### Collision resolutions

- **`docs/`** — union. No filename overlaps; the two trees are complementary.
- **`scripts/`** — union. No overlaps.
- **`README.md`** — the root product page keeps the slot. `authbridge/README.md`
  (532 lines of Kubernetes architecture) becomes **`docs/architecture.md`**,
  linked from `docs/README.md` and from `docs/kubernetes.md`, which is a 59-line
  orientation page that complements rather than duplicates it.
- **`CLAUDE.md`** — merged into one root file. The nested copy existed because
  there was a subdirectory to scope it to; after the flatten there is not.

### Module path mapping

| Before | After |
|---|---|
| `…/cortex/authbridge/authlib` | `…/cortex/authlib` |
| `…/cortex/authbridge/cmd/abctl` | `…/cortex/cmd/abctl` |
| `…/cortex/authbridge/cmd/authbridge-{proxy,envoy,cpex,praxis}` | `…/cortex/cmd/authbridge-{…}` |
| `…/cortex/authbridge/storage/redis` | `…/cortex/storage/redis` |
| `…/cortex/authbridge/scripts/{profile-tags,readme-demo}` | `…/cortex/scripts/{…}` |
| `…/cortex/authbridge/demos/{echo,finance-sparc,ibac}` | `…/cortex/demos/{…}` |

## 4. What the move gets for free

The whole subtree moves up together, so paths *relative to it* are unchanged.
Verified against the tree:

- **`replace` directives.** `cmd/abctl/go.mod` has
  `replace …/authlib => ../../authlib`. `cmd/abctl` is two levels deep before and
  after, so `../../authlib` resolves correctly both times. No edit needed.
- **`go.work` `use` list.** `./authlib`, `./cmd/abctl` … are relative to
  `go.work`, which moves with them. No edit needed.
- **Dockerfile `COPY` paths.** Relative to the build context, which moves too.

## 5. The import rewrite is a safe prefix strip

Import paths take the form `cortex/authbridge/…`; the install URL takes the form
`cortex/main/authbridge/install.sh`. A substitution on `cortex/authbridge/`
cannot match the URL. That distinction is what lets one mechanical pass rewrite
1,019 import occurrences without touching the compatibility surface, and it was
checked against both real strings before being relied on.

### The exception: fixed-depth upward paths

§4's "relative paths survive" holds only for paths pointing *within* the moving
subtree. A path that climbs *out* of it counts levels from the repo root and
breaks, and no substitution on `authbridge/` can see it. There are eleven,
across ten files, each losing exactly one `../`:

| File | Site | Now | After |
|---|---|---|---|
| `scripts/readme-demo/main.go:22` | default `-out` | `../../../docs/assets/` | `../../docs/assets/` |
| `scripts/readme-demo/staleness_test.go:12` | `assetPath` | `../../../docs/assets/` | `../../docs/assets/` |
| `scripts/readme-demo/README.md:42` | example command's `-out` path | `../../../docs/assets/` | `../../docs/assets/` |
| `demos/github-issue/rbac/Makefile:59` | `AGENT_EXAMPLES_DIR` | `../../../../../agent-examples` | `../../../../agent-examples` |
| `demos/github-issue/rbac/Makefile:63` | `VENV` | `../../../../.venv` | `../../../.venv` |
| `demos/github-issue/aiac/Makefile:43` | `VENV` | `../../../../.venv` | `../../../.venv` |
| `scripts/profile-tags/guards_test.go:15` | `repoRoot` | `../../..` | `../..` |
| `install_test.sh:513` | `WORKFLOW` | `${SCRIPT_DIR}/../.github/workflows/release-binaries.yaml` | `${SCRIPT_DIR}/.github/workflows/release-binaries.yaml` |
| `demos/session-budget/hitl-with-claude-code.md:14` | `[qs]` link target | `../../../README.md#quick-start` | `../../README.md#quick-start` |
| `docs/laptop-token-savings.md:8` | "quick start" link target | `../../README.md#quick-start` | `../README.md#quick-start` |
| `proxy-init/README.md:189,227,229` | links to `build.yaml` and two `demos/` docs | `../../...` | `../...` |

The rule that decides which paths shorten: shorten only where the target did
**not** also move. `demos/github-issue/{rbac,aiac}/Makefile`'s
`-r ../../../requirements.txt` hint string looks like the same shape as the
`VENV` line just above it in each file, but `requirements.txt` moved from
`authbridge/` to the root *with* those Makefiles — the relative distance
between them is unchanged, so that hint stays exactly as it was and is
deliberately not a row in this table. `VENV`, `AGENT_EXAMPLES_DIR`, and every
row above point at targets that did not move together with the referencing
file, so those do shorten.

The `readme-demo` pair CI actually exercises (`main.go`, `staleness_test.go`)
matters most: CI's `go-ci-readme-demo` job runs a staleness
test that regenerates the demo SVG and compares it against the committed
`docs/assets/cortex-demo.svg`. Left unfixed, the generator writes above the
repository root and the job fails. That job is also why the SVG **must** be
regenerated in this change rather than after it: the committed asset shows the
install one-liner, and changing the one-liner makes the asset stale.

## 6. Install compatibility

The canonical URL becomes
`raw.githubusercontent.com/rossoctl/cortex/main/install.sh`. The old path is not
preserved.

### Two independent concerns, previously conflated

An earlier draft of this design kept a forwarding shim at
`authbridge/install.sh`, justified partly by the `--ref=` machinery. That was
wrong: **`--ref` and the published URL are unrelated problems.**

**`--ref` needs nothing from `main`.** `install.sh:472` fetches another ref's copy
of itself, and that copy lives in *that ref's own tree*. Moving the file on `main`
cannot affect what a tag contains, so every existing release keeps working.

What `--ref` does need is for the new installer to look in both places, because
refs from either era must resolve:

1. `${REPO}/${ref}/install.sh`
2. on 404 only, `${REPO}/${ref}/authbridge/install.sh`

| `--ref=` target | Result |
|---|---|
| post-flatten ref | (1) 200 |
| pre-flatten tag (every release today) | (1) 404 → (2) 200, `VERSION_REF` pin intact |
| ref predating the script entirely | both 404 → existing warn + main's script, pin preserved |
| transport error / 5xx | still `die`, unchanged — never silently substitute main |

### The old published URL: who breaks, and how

Measured rather than assumed. Every reference to `main/authbridge/install.sh`
across public GitHub is inside the rossoctl org — **no third parties**:

| Repo | Files | Action |
|---|---|---|
| `rossoctl/cortex` | 8 | updated in this PR |
| `rossoctl/rossoctl` | 2 — current `docs/get-started/laptop.md`, `docs/concepts/experiments/cost-control.md` | coordinated PR |
| `rossoctl/.github` | 3 — `versioned_docs/version-0.7/`, `version-0.8/` | left alone; archives are not rewritten |

So the only casualty is a reader following **archived v0.7/v0.8 docs**. The
failure mode there, measured: `curl -fsSL <404> | sh` gives curl exit 56 with 49
bytes on stderr, then `sh` reads empty stdin and exits **0**. An interactive user
sees `curl: (56) ... error: 404` and nothing installs — discoverable. Anything
scripted around the pipeline sees success having installed nothing, which is the
worse half.

That is accepted. Those archives pin old releases whose commands drift anyway,
and a permanent file at `authbridge/install.sh` would re-create the directory
this change exists to remove — with no falsifiable criterion for ever deleting
it, since `raw.githubusercontent.com` exposes no fetch telemetry.

`install-demo.sh` moves to the root and is retargeted at `main/install.sh`. Its
own old URL, `main/authbridge/install-demo.sh`, has zero references in this
repository and zero across public GitHub — checked. It is already a shim for an
older name.

Its stale comment promising removal "once the old URL stops being fetched" is
replaced with a concrete trigger, since that condition is unobservable.

## 7. New requirement: root `.dockerignore`

Four images build from `context: ./authbridge`, which today excludes the
repository root. After the flatten they build from `context: .`, and the repo has
no root `.dockerignore` — the only one is `sparc-service`'s. Without a new one,
Docker would ship `.git` (196 MB) and `.worktrees` (861 MB on a development
machine) to the daemon on every build. CI pays only the `.git` cost; a developer
running `local-build-and-test.sh` pays about a gigabyte.

The sweep adds a root `.dockerignore` covering `.git`, `.worktrees`, `bin`,
`dist`, `venv` and the language caches.

## 8. Sequencing: this lands last

Three pull requests from the same audit are open, and **all of them touch files
this sweep moves** — #1124 (12 files under `authbridge/`), #1125 (7), #1126 (1).
A rename of 1,005 files against an open PR produces conflicts that are not worth
resolving by hand in either direction.

So the flatten merges **after** #1124, #1125 and #1126, and is rebased on main
once they are in. This also makes §3's claim about `LOCAL_TESTING_GUIDE.md` true:
#1126 deletes it, so by the time the sweep runs there is nothing to move.

Anything else open against `authbridge/` when this is ready should either merge
first or accept a rebase.

## 9. Commit structure

One pull request, nineteen commits, not three. The core intent still landed
exactly as planned — one pure-sweep commit, one rehome commit, and three
prose-merge commits for `CLAUDE.md` — but this repo's convention is to commit
the dated design doc and plan doc themselves, then revise them in place across
review rounds and follow-up fixes, and that convention supplies the other
fourteen commits:

1. **`refactor: Flatten authbridge/ into the repo root`** (`afb49e9f`) — the
   sweep only: `git mv`, 12 module paths, 1,019 import occurrences, 97 non-Go
   files, 36 workflow references, 6 dependabot directories, the
   `.dockerignore`, `go.sum` regeneration. Driven by a script committed in the
   same PR so review is "re-run it and diff".
2. **`docs: Rehome the AuthBridge architecture doc`** (`d568d067`) —
   `authbridge/README.md` → `docs/architecture.md` plus inbound links.
3. **`docs: Bring the nine root-only sections of authbridge/CLAUDE.md into the
   root`** (`0eeab0b8`), **`docs: Reconcile the six overlapping sections
   against the tree`** (`2c02eb54`), and **`docs: Delete authbridge/CLAUDE.md,
   leaving no authbridge/ directory`** (`8c646f17`) — the ~1,100-line
   reconciliation, split into three reviewable passes rather than landing as
   one prose merge.

The install `--ref` two-path lookup that this list originally credited to the
sweep commit is its own commit, `feat(install): Resolve --ref against both
layouts` (`b65d15e6`), and lands *before* the sweep rather than inside it —
Task 1 has to exist before the sweep can move anything under it.

The remaining thirteen commits are the design doc (`566701e2`, `21db048d`),
the plan doc and the review rounds and pre-flight fixes it went through
(`1e1cc209`, `87461290`, `0bb6e9ee`, `ce997853`, `00dd4143`, `9bbf47de`,
`b5401450`, `da6ae2d8`), a fix commit closing four gaps the gates found after
the sweep landed (`c7268f1b`), a follow-up publishing `install.sh` at its new
canonical path (`11a8a9ab`), and the closing cleanup that deletes the
migration script once nothing needed it anymore (`be62b427`).

## 10. Verification

All Go commands with `GOWORK=off`, as CI runs them.

- `go mod tidy -diff` clean in **all 12 modules** — the gate that turned #1121's
  first push red
- `authlib`: `go build`, `go vet`, `go test -race`
- `cmd/abctl`, `cmd/authbridge-{proxy,envoy,praxis}`, `storage/redis`,
  `scripts/{profile-tags,readme-demo}`: build, vet, test
- `bash -n` and `shellcheck` on every moved shell script
- `sh install_test.sh`, extended with a case per row of the `--ref` table in §6.
  The two-path lookup is the testable part and must be covered against both a
  path that exists and one that 404s. The abandoned old *published* URL is not
  testable offline and is not a behaviour we are keeping, so it gets no case.
- a real `docker build` of the proxy image, to prove the context change and the
  new `.dockerignore`
- `grep -rn 'authbridge'` reviewed by hand: the only surviving matches should be
  the `authbridge-*` binary and image names (deliberate, §12), the `--ref`
  fallback path in `install.sh`, and `docs/superpowers/` archive text
- no `authbridge/` directory remains
- `rossoctl/operator` and `rossoctl-cli` built against the branch with a local
  `replace`, to record exactly what downstream will need — not to block

## 11. Downstream

`rossoctl/operator` (pinned `v0.0.0-20260915135208`) and `rossoctl-cli`
(`v0.0.0-20260902150554`) resolve their pins from the module proxy, where the old
directory still exists at those commits. Neither goes red when this merges; both
break when they next bump cortex, which is an import-path update. Permitted, and
to be fixed afterwards. `rossoctl-cli` already fails to compile against current
main for unrelated `tlsbridge` drift.

## 12. Out of scope

- Renaming `authlib`, the `authbridge-*` binaries, or the published images.
  "authbridge" survives as a component name; only the directory level goes.
- Reorganising `docs/` into subdirectories.
- The `docs/superpowers/` archive keeps its `cortex/authbridge/` strings: those
  are dated records, per the precedent set in #1125.
- Deleting or restructuring `demos/`.

## 13. Risks

| Risk | Mitigation |
|---|---|
| A missed path reference breaks CI after merge | Every workflow and dependabot entry enumerated in §3/§9; `grep` gate in §10 |
| A fixed-depth `../../../` path silently resolves outside the repo | All eleven enumerated in §5; invisible to a string sweep, so they are a named task rather than a side effect. The `readme-demo` pair is caught by CI's staleness job. |
| `--ref=<old tag>` stops resolving | Two-path lookup (§6), `install_test.sh` cases for both eras, gated by the existing `install-script` job |
| A reader of archived v0.7/v0.8 docs gets a 404 | **Accepted, not mitigated** (§6). Curl exits 56 visibly, but the pipeline exits 0 having installed nothing. Current docs in `rossoctl/rossoctl` get a coordinated PR; the archives do not. |
| Docker context bloat | Root `.dockerignore` in the same commit; real `docker build` in §10 |
| A release tagged mid-flight publishes then unpublishes a module path | Do not tag until the PR has settled |
| Rollback | Pure rename: `git revert` the merge commit. Consumers are pinned and unaffected. |
| `git log --follow` noise on moved files | Accepted; inherent to any move, and precedent exists twice |

## 14. Note on this document

This spec lived at `authbridge/docs/superpowers/specs/` and has itself been
moved to `docs/superpowers/specs/` by the change it describes.
