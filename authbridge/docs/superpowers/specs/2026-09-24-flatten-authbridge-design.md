# Flattening `authbridge/` into the repo root

Status: design · 2026-09-24 · targets the whole repository layout

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
| Published install URL | **Shim at the old path.** The canonical one-liner may change; nothing may break. |
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
├── .github/  .claude/
│
└── authbridge/
    └── install.sh            # forwarding shim, the only survivor
```

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

## 6. Install compatibility

The hard requirement. The canonical URL becomes
`raw.githubusercontent.com/rossoctl/cortex/main/install.sh`.

`install.sh:472` fetches *another ref's copy of itself* to honour `--ref=`, and
the surrounding logic is deliberate: HTTP 200 runs that copy; 404 warns and runs
main's script while keeping `VERSION_REF` so the binary pin survives; any other
status calls `die` rather than silently substituting main.

The new root installer therefore tries **two paths in order**:

1. `${REPO}/${ref}/install.sh`
2. on 404 only, `${REPO}/${ref}/authbridge/install.sh`

| Invocation | Result |
|---|---|
| Old URL `main/authbridge/install.sh` | shim forwards to `main/install.sh`, args preserved |
| `--ref=<pre-flatten tag>` | (1) 404 → (2) 200 → pin intact |
| `--ref=<post-flatten ref>` | (1) 200; shim never involved |
| `--ref=<ref predating the script>` | both 404 → existing warn + main, pin preserved |
| transport error / 5xx | still `die`, unchanged |

Because (1) is tried first, the `--ref` machinery never reaches the shim for a
post-flatten ref. The shim's only job is a stale human URL, where forwarding to
main's current installer is the right answer.

The shim copies `install-demo.sh`'s existing pattern exactly: fetch to a temp
file, then `sh "$tmp" "$@"` — so a truncated download cannot execute as a
partial script, and arguments survive.

`install-demo.sh` is retargeted at `main/install.sh` directly rather than
double-hopping through the shim. It needs no shim of its own: its old URL,
`main/authbridge/install-demo.sh`, has zero references in this repository and
zero across public GitHub — checked, not assumed. It is itself a shim for a name
that predates it, so a shim for a shim would be protecting nothing.

**Removal criterion.** `install-demo.sh`'s comment promises removal "once the old
URL stops being fetched," which is unfalsifiable: `raw.githubusercontent.com` is
a CDN and exposes no fetch telemetry. Both shims get a concrete trigger instead
— removed at the next major version — stated in the comment.

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

One pull request, three commits, so the mechanical move can be reviewed apart
from the prose merges:

1. **`refactor: Flatten authbridge/ into the repo root`** — the sweep only:
   `git mv`, 12 module paths, 1,019 import occurrences, 97 non-Go files, 36
   workflow references, 6 dependabot directories, the install two-path lookup,
   the shim, the `.dockerignore`, `go.sum` regeneration. Driven by a script
   committed in the same PR so review is "re-run it and diff".
2. **`docs: Rehome the AuthBridge architecture doc`** — `authbridge/README.md` →
   `docs/architecture.md` plus inbound links.
3. **`docs: Merge the two CLAUDE.md files`** — the ~1,100-line reconciliation,
   reviewable as prose.

## 10. Verification

All Go commands with `GOWORK=off`, as CI runs them.

- `go mod tidy -diff` clean in **all 12 modules** — the gate that turned #1121's
  first push red
- `authlib`: `go build`, `go vet`, `go test -race`
- `cmd/abctl`, `cmd/authbridge-{proxy,envoy,praxis}`, `storage/redis`,
  `scripts/{profile-tags,readme-demo}`: build, vet, test
- `bash -n` and `shellcheck` on every moved shell script
- `sh install_test.sh`, extended with a case per row of the table in §6
- a real `docker build` of the proxy image, to prove the context change and the
  new `.dockerignore`
- `grep -rn 'cortex/authbridge'` returns nothing outside `docs/superpowers/` and
  the shim
- the old install URL resolves and installs
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
| An installer invocation breaks | §6 table, one `install_test.sh` case per row, gated by the existing `install-script` job |
| Docker context bloat | Root `.dockerignore` in the same commit; real `docker build` in §10 |
| A release tagged mid-flight publishes then unpublishes a module path | Do not tag until the PR has settled |
| Rollback | Pure rename: `git revert` the merge commit. Consumers are pinned and unaffected. |
| `git log --follow` noise on moved files | Accepted; inherent to any move, and precedent exists twice |

## 14. Note on this document

This spec lives at `authbridge/docs/superpowers/specs/` and will itself be moved
to `docs/superpowers/specs/` by the change it describes.
