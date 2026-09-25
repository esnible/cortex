# Flatten `authbridge/` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move all 1,006 files under `authbridge/` up to the repository root, dropping the `/authbridge` segment from 12 Go module paths and every path reference, leaving no `authbridge/` directory behind.

**Architecture:** A mechanical rename driven by a committed script, so review is "re-run it and diff" rather than reading 1,006 renames. Paths *within* the moving subtree (`replace` directives, `go.work` use list, Dockerfile `COPY`) survive untouched because the subtree moves together; paths that climb *out* of it are enumerated and fixed by hand. One PR, five commits.

**Tech Stack:** Go 1.26.5 (12 modules, `go.work`), POSIX sh (`install.sh` + its test harness), GitHub Actions, Docker.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-24-flatten-authbridge-design.md`

## Global Constraints

- **Structural only.** No package, binary, or image is renamed. `authlib` stays `authlib`; `cmd/authbridge-proxy` keeps its name; `authbridge-*` images keep theirs.
- **No shim.** The old install URL `main/authbridge/install.sh` is abandoned and 404s. No `authbridge/` directory survives.
- **Canonical install URL becomes** `https://raw.githubusercontent.com/rossoctl/cortex/main/install.sh`.
- **`--ref=` must resolve for both eras** via a two-path lookup: `${ref}/install.sh`, then on 404 only `${ref}/authbridge/install.sh`.
- **All Go commands run with `GOWORK=off`**, as CI does.
- **Every module must pass `go mod tidy -diff`** — `build`/`vet`/`test` all pass while it fails.
- **`docs/superpowers/` keeps its `cortex/authbridge/` strings.** Dated records, per #1125.
- **This lands after #1124, #1125 and #1126 merge**, rebased on main.
- Commits: `git commit -s` (DCO) and end the message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.

---

### Task 1: Teach `install.sh` the two-path `--ref` lookup

Done **before** the move, because it is correct either way: `${ref}/install.sh` simply 404s today and the fallback finds the current path. That makes it independently testable and keeps Task 2 purely mechanical.

**Files:**
- Modify: `authbridge/install.sh:472` (the `url=` assignment inside the bootstrap block)
- Modify: `authbridge/install_test.sh` (extend `with_bootstrap`, add two cases)
- Test: `authbridge/install_test.sh` (the repo's only test harness for this script)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `install.sh` resolves `--ref` against both layouts. Task 2 moves this file to the root unchanged. Task 5 depends on the canonical URL constant being correct.

The existing harness stubs `curl` with **one** HTTP code for any URL:

```sh
with_bootstrap() { # want_ref http_code_for_script_fetch newest_release_output [run_as=pipe|file]
```

Testing a two-path lookup needs per-URL codes, so the harness gains a fourth positional before `run_as`.

- [ ] **Step 1: Write the failing tests**

In `authbridge/install_test.sh`, change `with_bootstrap`'s signature to take a second HTTP code for the legacy path, defaulting to the first so every existing call site keeps its meaning:

```sh
with_bootstrap() { # want_ref http_new http_legacy newest_release [run_as=pipe|file]
	_want=$1; _http=$2; _httplegacy=${3:-$2}; _newest=$4; _runas=${5:-pipe}
```

Update the curl stub inside it to branch on the URL:

```sh
		printf 'curl() {\n'
		printf '  _out=""; _prev=""; _u=""\n'
		# shellcheck disable=SC2016 # literal on purpose: expanded by the probe, not here.
		printf '  for _a in "$@"; do [ "${_prev}" = "-o" ] && _out="${_a}"; case "$_a" in http*) _u="$_a" ;; esac; _prev="${_a}"; done\n'
		# shellcheck disable=SC2016 # same.
		printf '  case "${_u}" in *//authbridge/install.sh|*/authbridge/install.sh) _c="%s" ;; *) _c="%s" ;; esac\n' "${_httplegacy}" "${_http}"
		# shellcheck disable=SC2016 # same.
		printf '  [ "${_c}" != "200" ] || [ -z "${_out}" ] || printf "#!/bin/sh\\nprintf \\"REEXECED\\\\n\\"\\nexit 0\\n" > "${_out}"\n'
		# shellcheck disable=SC2016 # same.
		printf '  printf "%%s" "${_c}"\n'
		printf '}\n'
```

Then add the two new cases next to the existing bootstrap checks:

```sh
# A ref from AFTER the flatten: /install.sh exists, so the legacy path is never
# consulted and that ref's own script runs with its own pin.
check "--ref=v9.9.9 with /install.sh present: script and install both v9.9.9" \
	"script=v9.9.9 version=v9.9.9" "$(with_bootstrap v9.9.9 200 404 v0.7.0-alpha.7)"

# A ref from BEFORE the flatten: /install.sh 404s, the legacy
# /authbridge/install.sh answers, and the pin still belongs to that ref. Without
# the fallback this silently degraded to main's script.
check "--ref=v0.8.0 falls back to the legacy path: script and install both v0.8.0" \
	"script=v0.8.0 version=v0.8.0" "$(with_bootstrap v0.8.0 404 200 v0.7.0-alpha.7)"
```

Existing calls now pass their single code as both, so `with_bootstrap v0.5.0 404 …` becomes "both paths 404" — which is exactly what that test's comment already describes. Add the third argument to each existing call site so the positions line up:

```sh
check "API unreachable, no --ref: script=main, nothing to install" \
	"script=main version=" "$(with_bootstrap "" 000 000 "")"
check "--ref=main: script=main, install main" \
	"script=main version=main" "$(with_bootstrap main 000 000 v0.7.0-alpha.7)"
check "--ref=CHANNEL_TAG behaves as --ref=main" \
	"script=main version=main" "$(with_bootstrap "${CHANNEL_TAG}" 000 000 v0.7.0-alpha.7)"
check "--ref=v0.5.0 with a 404 script: script=main, install v0.5.0" \
	"script=main version=v0.5.0" "$(with_bootstrap v0.5.0 404 404 v0.7.0-alpha.7)"
```

- [ ] **Step 2: Run the tests to verify the two new ones fail**

```bash
sh authbridge/install_test.sh
```

Expected: the two new checks FAIL (`install.sh` still fetches only the legacy path, so `--ref=v9.9.9` with `/install.sh`→200 still reports `script=main`). Every pre-existing check PASSES.

- [ ] **Step 3: Implement the two-path lookup**

Replace the single `url=` line at `authbridge/install.sh:472` and the fetch that follows it. Keep the existing status handling verbatim — 200 runs it, 404 warns and keeps the pin, anything else dies:

```sh
		boot=$(mktemp)
		# Two paths, because a pinned ref may predate the flatten. Try the current
		# layout first so a post-flatten ref never pays for the legacy probe; fall
		# back only on a clean 404, never on a transport error.
		url="https://raw.githubusercontent.com/${REPO}/${want_ref}/install.sh"
		http=$(curl -sSL -o "${boot}" -w '%{http_code}' "${url}" 2>/dev/null) || http="000"
		[ -n "${http}" ] || http="000"
		if [ "${http}" = "404" ]; then
			url="https://raw.githubusercontent.com/${REPO}/${want_ref}/authbridge/install.sh"
			http=$(curl -sSL -o "${boot}" -w '%{http_code}' "${url}" 2>/dev/null) || http="000"
			[ -n "${http}" ] || http="000"
		fi
```

Update the 404 warning below it so the message names both attempts rather than one path:

```sh
			warn "${want_ref} has no install.sh at either path (HTTP 404); continuing with the copy from main"
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
sh authbridge/install_test.sh
```

Expected: all checks PASS, including the two new ones.

- [ ] **Step 5: Lint the script the way CI does**

```bash
bash -n authbridge/install.sh && shellcheck authbridge/install.sh authbridge/install_test.sh
```

Expected: no output from either.

- [ ] **Step 6: Commit**

```bash
git add authbridge/install.sh authbridge/install_test.sh
git commit -s -m "feat(install): Resolve --ref against both layouts

install.sh fetches another ref's copy of itself to honour --ref=, at a path that
is about to change. Try \${ref}/install.sh first and fall back to
\${ref}/authbridge/install.sh on a clean 404 only, so refs from either side of
the flatten resolve and a pinned pre-flatten tag keeps running its own script
instead of silently degrading to main's.

Correct before the move as well as after: the new path simply 404s today and the
fallback finds the current one, which is why this lands first and separately.

A transport failure still dies rather than falling back — the distinction the
existing code is careful about, preserved here.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: The mechanical sweep

**Files:**
- Create: `scripts/flatten-authbridge.sh` (the migration script, committed for review; deleted in Task 6)
- Create: `.dockerignore`
- Move: all 1,006 files under `authbridge/` up one level
- Modify: 12 `go.mod` module lines; 423 Go files (909 import occurrences); `.github/workflows/{ci,build,release-binaries,dependabot-tidy,security-scans}.yaml` (36 refs); `.github/dependabot.yml` (6 directories); `Makefile`; `local-build-and-test.sh`; `.gitignore`

**Interfaces:**
- Consumes: Task 1's `install.sh` (moved unchanged).
- Produces: every module at its final path. Tasks 3–5 operate on the moved tree. `authlib`'s module path is `github.com/rossoctl/cortex/authlib`.

- [ ] **Step 1: Write the migration script**

Create `scripts/flatten-authbridge.sh`:

```sh
#!/bin/sh
# One-shot migration: move authbridge/* to the repo root and drop the segment
# from every module path and path reference. Committed so review is "re-run it
# and diff" rather than reading 1,006 renames. Deleted in the final commit.
set -eu

cd "$(git rev-parse --show-toplevel)"

# 1. Move everything up a level. docs/ and scripts/ merge with their root
#    namesakes; neither has a filename collision. README.md and CLAUDE.md are
#    left in place for Tasks 3 and 4 — they DO collide.
for entry in authbridge/*; do
	name=${entry#authbridge/}
	case "${name}" in
		README.md|CLAUDE.md) continue ;;
		docs|scripts)
			# Merge, do not replace.
			for inner in "${entry}"/*; do
				git mv "${inner}" "${name}/$(basename "${inner}")"
			done
			rmdir "${entry}"
			;;
		*) git mv "${entry}" "${name}" ;;
	esac
done
# Dotfiles are not matched by authbridge/* above.
for entry in authbridge/.*; do
	case "${entry}" in authbridge/.|authbridge/..) continue ;; esac
	[ -e "${entry}" ] || continue
	git mv "${entry}" "${entry#authbridge/}"
done

# 2. Module paths.
for mod in $(find . -name go.mod -not -path './.git/*'); do
	dir=$(dirname "${mod}")
	old=$(awk '/^module /{print $2; exit}' "${mod}")
	new=$(printf '%s' "${old}" | sed 's|/cortex/authbridge/|/cortex/|')
	[ "${old}" = "${new}" ] || (cd "${dir}" && go mod edit -module "${new}")
done

# 3. Import paths. A one-segment prefix strip. Safe against the install URL,
#    which reads cortex/main/authbridge/ and cannot match cortex/authbridge/.
find . -name '*.go' -not -path './.git/*' -print0 \
	| xargs -0 sed -i '' 's|github.com/rossoctl/cortex/authbridge/|github.com/rossoctl/cortex/|g'

# 4. replace directives point INTO the subtree, so their relative paths are
#    unchanged — but the module path on the left side is not.
find . -name go.mod -not -path './.git/*' -print0 \
	| xargs -0 sed -i '' 's|github.com/rossoctl/cortex/authbridge/|github.com/rossoctl/cortex/|g'

# 5. Path references outside Go. Two exclusions, both deliberate:
#    - docs/superpowers/: dated records, per spec §12.
#    - install.sh: its --ref fallback deliberately PROBES the old location
#      (${ref}/authbridge/install.sh). Rewriting that makes both lookup paths
#      identical and silently breaks --ref for every pre-flatten tag. Its two
#      usage comments are hand-edited in step 5b instead.
git ls-files -z -- '*.yaml' '*.yml' '*.md' '*.sh' 'Makefile' '*.toml' '*.json' \
	| grep -zv '^docs/superpowers/' \
	| grep -zv '^install\.sh$' \
	| xargs -0 sed -i '' \
		-e 's|authbridge/authlib|authlib|g' \
		-e 's|authbridge/cmd/|cmd/|g' \
		-e 's|authbridge/storage/|storage/|g' \
		-e 's|authbridge/scripts/|scripts/|g' \
		-e 's|authbridge/proxy-init|proxy-init|g' \
		-e 's|authbridge/sparc-service|sparc-service|g' \
		-e 's|authbridge/lineage-attach|lineage-attach|g' \
		-e 's|authbridge/demos/|demos/|g' \
		-e 's|authbridge/docs/|docs/|g' \
		-e 's|authbridge/install|install|g' \
		-e 's|authbridge/keycloak_sync.py|keycloak_sync.py|g' \
		-e 's|authbridge/requirements.txt|requirements.txt|g' \
		-e 's|context: ./authbridge$|context: .|' \
		-e 's|directory: /authbridge/|directory: /|g'

# 6. Tidy every module.
for mod in $(find . -name go.mod -not -path './.git/*'); do
	(cd "$(dirname "${mod}")" && GOWORK=off go mod tidy)
done
```

Note `sed -i ''` is the BSD/macOS spelling; on GNU sed use `sed -i`. Run on whichever the executing machine has and say which in the PR.

- [ ] **Step 2: Run it**

```bash
sh scripts/flatten-authbridge.sh
```

Expected: no errors. `authbridge/` now contains only `README.md` and `CLAUDE.md`.

- [ ] **Step 3: Fix the seven fixed-depth upward paths by hand**

These climb *out* of the moved subtree, so no substitution sees them. Each loses exactly one `../`:

```
scripts/readme-demo/main.go:22          filepath.Join("..","..","..","docs",…)  -> "..","..","docs"
scripts/readme-demo/staleness_test.go:12 "../../../docs/assets/cortex-demo.svg" -> "../../docs/assets/cortex-demo.svg"
demos/github-issue/rbac/Makefile:59     ../../../../../agent-examples           -> ../../../../agent-examples
demos/github-issue/rbac/Makefile:63     ../../../../.venv                       -> ../../../.venv
demos/github-issue/rbac/Makefile:85     -r ../../../requirements.txt            -> -r ../../requirements.txt
demos/github-issue/aiac/Makefile:43     ../../../../.venv                       -> ../../../.venv
demos/github-issue/aiac/Makefile:71     -r ../../../requirements.txt            -> -r ../../requirements.txt
```

- [ ] **Step 4: Add the root `.dockerignore`**

Four images move from `context: ./authbridge` to `context: .`, which newly includes `.git` (196 MB) and `.worktrees` (861 MB on a dev machine). Create `.dockerignore`:

```
# Build contexts became the repo root when authbridge/ was flattened, so the
# things that were previously outside every context are now inside one.
.git
.worktrees
bin
dist
venv
.venv
__pycache__
.pytest_cache
.ruff_cache
.mypy_cache
*.test
node_modules
```

- [ ] **Step 5: Verify nothing references the old layout**

```bash
grep -rn 'cortex/authbridge' --include='*.go' --include='*.mod' . ; echo "exit=$?"
grep -rn 'authbridge/' --exclude-dir=.git --exclude-dir=docs . | grep -v 'authbridge-' ; echo "exit=$?"
ls authbridge/
```

Expected: the first two print nothing (`exit=1` from grep means no matches). `ls authbridge/` shows only `README.md` and `CLAUDE.md`, both handled next.

- [ ] **Step 6: Run every gate CI runs**

```bash
export GOWORK=off
for m in authlib cmd/abctl cmd/authbridge-proxy cmd/authbridge-envoy \
         cmd/authbridge-praxis storage/redis scripts/profile-tags scripts/readme-demo; do
  (cd "$m" && go mod tidy -diff && go build ./... && go vet ./...) || echo "FAILED: $m"
done
(cd authlib && go test -race ./...)
(cd scripts/readme-demo && go test -count=1 ./...)
sh install_test.sh
shellcheck install.sh install_test.sh local-build-and-test.sh verify-spire-keycloak.sh
# CI's python-test job. Easy to forget because this repo is 95% Go — and
# forgetting it is how a dead `parents[1]/"authbridge"` path in
# tests/test_keycloak_sync.py survived the first pass of this very sweep.
pytest tests/ -v -x --ignore=tests/e2e
```

Expected: every module clean. `scripts/readme-demo`'s staleness test may fail here — that is Task 5's job; note it and continue.

- [ ] **Step 7: Prove the Docker context change works**

```bash
docker build -f cmd/authbridge-proxy/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . full)" \
  -t authbridge:flatten-check .
```

Expected: builds. Watch the "transferring context" line — it should be tens of MB, not hundreds. If it reports hundreds, `.dockerignore` is not being honoured.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -s -m "refactor: Flatten authbridge/ into the repo root

authbridge/ held 1,006 of 1061 tracked files and separated nothing from
nothing: it dates from when this repo was kagenti-extensions and held more than
one extension. The level was carried by the public install URL, all 12 Go module
paths, 36 workflow references and 6 dependabot module directories.

Driven by scripts/flatten-authbridge.sh, committed here so review is re-run it
and diff rather than reading 1,006 renames.

Three things needed no edit, because the subtree moved together and they point
within it: the replace directives' relative paths, go.work's use list, and the
Dockerfile COPY paths.

Seven paths did, because they climb OUT of the subtree and no substitution on
'authbridge/' can see them: readme-demo's generator output path and its
staleness assertion, plus five in the github-issue demo Makefiles. Each lost one
'../'.

The import rewrite is a one-segment prefix strip and cannot touch the install
URL: imports read cortex/authbridge/, the URL reads cortex/main/authbridge/.

Adds a root .dockerignore, which is now load-bearing rather than hygiene. Four
images moved from context: ./authbridge to context: ., so .git (196MB) and
.worktrees (861MB on a dev machine) went from outside every build context to
inside one.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: Rehome the architecture doc

**Files:**
- Move: `authbridge/README.md` → `docs/architecture.md`
- Modify: `docs/README.md` (add it to the index), `docs/kubernetes.md` (link to it)

**Interfaces:**
- Consumes: Task 2's moved tree.
- Produces: `authbridge/` contains only `CLAUDE.md`.

The root `README.md` is the 87-line product front page; `authbridge/README.md` is 532 lines of Kubernetes architecture. Different audiences, so the front page keeps the slot.

- [ ] **Step 1: Move it**

```bash
git mv authbridge/README.md docs/architecture.md
```

- [ ] **Step 2: Add it to the docs index**

In `docs/README.md`'s "Start here" table, replace the row pointing at `../authbridge/README.md` with:

```markdown
| Understand the sidecar shapes and deployment | [`architecture.md`](architecture.md) |
```

- [ ] **Step 3: Link it from the Kubernetes page**

`docs/kubernetes.md` is a 59-line orientation page that complements rather than duplicates the deep dive. Add to its "Start here" section:

```markdown
For the full deployment architecture — sidecar shapes, container inventory, the
end-to-end token flow — see [`architecture.md`](architecture.md).
```

- [ ] **Step 3b: Retarget the eleven links that point at this file**

Task 2's sweep rewrote `authbridge/README.md` → `README.md` in these, which is the
wrong document — the root README is the product page, this is the architecture
deep dive. Task 2's fix round restored them to `authbridge/README.md`; now that
the file has become `docs/architecture.md`, point them there. Three anchors
(`#download-prebuilt-binaries`, `#deployment-modes`, `#build-tag-plugin-selection`)
exist only in this document, so getting the target wrong leaves them dead:

```
authlib/plugins/README.md:57              demos/github-issue/demo-manual.md:1204
cmd/abctl/README.md:13                    demos/weather-agent/demo-ui.md:847
demos/README.md:172                       demos/weather-agent/demo-with-abctl.md:44
demos/github-issue/demo.md:92             docs/kubernetes.md:44
demos/github-issue/demo-ui.md:1217        docs/kubernetes.md:45
```

Mind the relative depth: each needs the correct number of `../` to reach
`docs/architecture.md` from its own location, which differs per file.

```bash
grep -rn 'authbridge/README' --exclude-dir=.git --exclude-dir=docs/superpowers . || echo "none left"
```

- [ ] **Step 4: Verify every inbound link still resolves**

```bash
for f in $(git ls-files "*.md"); do
  d=$(dirname "$f")
  grep -oE '\]\(([^)#h][^)]*)\)' "$f" | sed 's/](//;s/)$//' | while read -r l; do
    [ -e "$d/$l" ] || echo "BROKEN in $f -> $l"
  done
done
grep -rn 'authbridge/README' --exclude-dir=.git --exclude-dir=docs/superpowers . || echo "no stale refs"
```

Expected: no `BROKEN` lines, no stale refs.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -s -m "docs: Rehome the AuthBridge architecture doc

Flattening authbridge/ collides its README.md with the repo's. They are not the
same kind of document: the root one is the 87-line product front page, this is
532 lines of Kubernetes deployment architecture. The front page keeps the slot
and the deep dive becomes docs/architecture.md, indexed from docs/README.md and
linked from docs/kubernetes.md — the 59-line orientation page it complements.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: Merge the two `CLAUDE.md` files

**Files:**
- Modify: `CLAUDE.md` (442 lines, absorbs the other)
- Delete: `authbridge/CLAUDE.md` (695 lines)

**Interfaces:**
- Consumes: Tasks 2 and 3.
- Produces: no `authbridge/` directory at all.

The nested file existed because there was a subdirectory to scope it to. After the flatten there is not, so one file serves the whole repo.

- [ ] **Step 1: Merge, resolving the overlaps rather than concatenating**

Both files carry a directory tree, a binaries list, a commit-attribution policy, and a DCO section. Keep one of each. Section-by-section resolution:

| Section | Action |
|---|---|
| Repository overview / directory tree | Keep root's, updated to the flattened layout |
| Binaries table | Keep the nested file's — it is the more detailed, and #1125 corrected it |
| Plugin reference, session API, hot-reload, mTLS, gotchas | Keep the nested file's wholesale; root has no equivalent |
| CI/CD workflows, container images, ConfigMaps | Keep root's |
| Worktree protocol, commit attribution, DCO | Keep root's; drop the nested duplicates |
| `## Orchestration`, skills tables | Keep root's |

Where the two disagree on a fact, the nested file is generally the fresher one — but check the claim against the tree rather than trusting either.

- [ ] **Step 2: Delete the nested file and the now-empty directory**

```bash
git rm authbridge/CLAUDE.md
```

- [ ] **Step 3: Verify the result describes the tree that exists**

```bash
ls authbridge 2>&1 | head -1          # expect: No such file or directory
grep -n 'authbridge/' CLAUDE.md | grep -v 'authbridge-'   # expect: nothing
for d in authlib cmd demos docs scripts storage proxy-init sparc-service lineage-attach tests; do
  grep -q "$d" CLAUDE.md || echo "MISSING from the tree block: $d"
done
# Strip the #anchor before testing existence — a link like
# ](../README.md#quick-start) names a real file, and testing the whole string
# reports it broken. That bug made an earlier version of this check emit ~68
# false positives on an unchanged tree, which is worse than no check: a gate
# that always fails is a gate everyone learns to skip.
for f in $(grep -oE '\]\([^)#h][^)]*\)' CLAUDE.md | sed 's/](//;s/)$//;s/#.*$//'); do
  [ -e "$f" ] || echo "BROKEN link -> $f"
done
```

Expected: `authbridge` gone, no stale path references, nothing missing, no broken links.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -s -m "docs: Merge the two CLAUDE.md files into one

The nested authbridge/CLAUDE.md existed because there was a subdirectory to
scope it to. After the flatten there is not, and two files describing one tree
is how they drifted apart — #1125 had to correct both for the same wrong claims
about spiffe-helper, pre-commit hooks and the demo list.

Resolved section by section rather than concatenated: the root file's repository
overview, CI/CD, container images, worktree protocol and attribution policy; the
nested file's binaries table, plugin reference, session API, hot-reload and
gotchas, which have no root equivalent. Facts checked against the tree where the
two disagreed.

Leaves no authbridge/ directory.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 5: Publish the new one-liner and regenerate the demo asset

**Files:**
- Modify: `README.md`, `CONTRIBUTING.md`, `install.sh` (its own usage comments at lines 4 and 164), `install-demo.sh`, `.github/workflows/release-binaries.yaml`, `scripts/readme-demo/demo.yaml`
- Modify: `docs/assets/cortex-demo.svg` (regenerated, not hand-edited)

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces: the published command matches the file's location, and CI's staleness gate passes.

`scripts/readme-demo`'s test regenerates the SVG and compares it against the committed asset. The asset renders the install one-liner, so changing the one-liner makes it stale and that job fails until it is regenerated. This task is therefore required, not cosmetic.

- [ ] **Step 1: Verify the sweep replaced every published spelling, and fix what it missed**

Task 2's substitution already covers `README.md`, `CONTRIBUTING.md`, `install-demo.sh`, `release-binaries.yaml` and `demo.yaml`, so expect most of this to be done:

```bash
grep -rn 'main/authbridge/install.sh' --exclude-dir=.git --exclude-dir=docs . || echo "all replaced"
```

`install.sh` is excluded from that sweep rule, because its `--ref` fallback must keep probing the old path (see Task 2 Step 1's comment). It has four legacy references and they are not all the same kind:

```bash
grep -n 'authbridge/install.sh' install.sh
```

- **`:4` and `:164`** — the header usage line and the `--help` example. Hand-edit both to `https://raw.githubusercontent.com/rossoctl/cortex/main/install.sh`.
- **`:485`** — the `--ref` fallback probe. **Leave it alone.** It exists to find pre-flatten refs.
- **`:1278`** — an advisory URL inside the `abctl service` version-mismatch `die`, interpolating a *tag*: `${REPO}/${version}/authbridge/install.sh`. Correct for a pre-flatten tag, a 404 for a post-flatten one, and it cannot do a two-path lookup because it is printed advice rather than a fetch.

Fix `:1278` by deleting the URL guess rather than repairing it. Task 1 made `--ref=` resolve either layout, so the advice can delegate to the mechanism instead of duplicating it:

```sh
			v*)
				die "the ${version} abctl has no 'service' command, which this installer needs
  in order to start Cortex. Either re-run this installer pinned to that release,
  which resolves its installer in either layout:
    --ref=${version}
  or install newer binaries with this script:
    --ref=<newer tag>"
				;;
```

That is strictly more robust than any hardcoded path: it is correct for both eras with no version boundary to encode, and it stays correct if the layout ever changes again.

- [ ] **Step 1b: Confirm only the probe remains**

```bash
grep -n 'authbridge/install.sh' install.sh
```

Expected: exactly one hit, line ~485, the fallback probe.

- [ ] **Step 1b: Consolidate the duplicated binaries content in `CLAUDE.md`**

The merge left the binaries enumerated three times and three sections sharing the
name "AuthBridge Binaries": `### 1. AuthBridge Binaries (Go)` (~L187),
`## AuthBridge Binaries` (~L254), and
`### AuthBridge Binaries (cmd/authbridge-{proxy,envoy}/)` (~L312). Two predate this
series; the merge added the third. Task 4's constraint was "one binaries list", so
this is a regression against it, not merely inherited clutter.

Keep the table at ~L254 as the single enumeration. Fold the ~L187 bullets into
prose that points at it, and retitle ~L312 — its current heading names only 2 of
the 5 binaries, which is actively wrong now that cpex, praxis and abctl exist.

Also at ~L308 and ~L342, two near-verbatim copies of "YAML with `${ENV_VAR}`
expansion, mode presets, and startup validation." Keep one.

Verify afterwards:

```bash
grep -c 'AuthBridge Binaries' CLAUDE.md          # expect 1, or 2 with distinct titles
grep -c 'ENV_VAR}` expansion, mode presets' CLAUDE.md   # expect 1
```

- [ ] **Step 1c: Add the three demos missing from `demos/README.md`**

PR #1125's body claimed it fixed this and it only fixed `CLAUDE.md`. `context-guru`,
`echo` and `mtls` are still absent from the demo index — verify with:

```bash
for d in $(ls -d demos/*/ | xargs -n1 basename); do
  grep -q "$d" demos/README.md || echo "MISSING $d"
done
```

Add one row each, matching the file's existing style. Not strictly part of the
flatten, folded in because a merged PR asserting a fix it did not make is worse
than a slightly wider scope here, and the alternative is a separate PR for three
lines.

- [ ] **Step 2: Replace `install-demo.sh`'s unfalsifiable removal promise**

Its comment says it will go away "once the old URL stops being fetched." `raw.githubusercontent.com` is a CDN and exposes no fetch telemetry, so that condition can never be evaluated. Give it a checkable one:

```sh
# Removed at the next major version, not "when fetches stop" — raw.githubusercontent.com
# exposes no fetch telemetry, so that condition could never be evaluated.
```

- [ ] **Step 3: Confirm the asset is now stale**

```bash
cd scripts/readme-demo && GOWORK=off go test -count=1 -run Staleness ./...
```

Expected: FAIL, reporting the committed SVG differs — the one-liner changed.

- [ ] **Step 4: Regenerate it**

```bash
cd scripts/readme-demo && GOWORK=off go run .
```

Writes `docs/assets/cortex-demo.svg` via the `-out` default, which Task 2 Step 3 corrected from three levels up to two.

- [ ] **Step 5: Confirm the asset is fresh and the suite passes**

```bash
cd scripts/readme-demo && GOWORK=off go test -count=1 ./...
git diff --stat docs/assets/cortex-demo.svg
```

Expected: PASS, and the SVG shows as modified.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -s -m "docs: Publish install.sh at its new path

The canonical one-liner becomes
raw.githubusercontent.com/rossoctl/cortex/main/install.sh. The old path is
abandoned rather than shimmed: every reference to it across public GitHub is
inside the rossoctl org, so there are no third parties to protect, and a
permanent file at authbridge/install.sh would re-create the directory this
series exists to delete with no falsifiable condition for ever removing it.

Regenerates docs/assets/cortex-demo.svg, which is required and not cosmetic: the
asset renders the one-liner, and CI's readme-demo job regenerates it and
compares, so a changed command fails that job until the asset follows.

Also replaces install-demo.sh's promise to disappear 'once the old URL stops
being fetched' with a condition that can actually be checked.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 6: Remove the migration script and record downstream follow-ups

**Files:**
- Delete: `scripts/flatten-authbridge.sh`
- Modify: `docs/superpowers/specs/2026-09-24-flatten-authbridge-design.md` (status line)

**Interfaces:**
- Consumes: Tasks 2–5.
- Produces: the final tree.

- [ ] **Step 1: Delete the one-shot script**

```bash
git rm scripts/flatten-authbridge.sh
```

It existed so reviewers could re-run it; once merged it is a loaded gun pointed at a directory that no longer exists.

- [ ] **Step 2: Mark the spec implemented**

Change its status line to `Status: implemented · 2026-09-24`, and note that the spec has itself moved to `docs/superpowers/specs/` — the change it describes moved it.

- [ ] **Step 3: Record what downstream needs**

Build both consumers against the branch to capture the exact change they need, then put it in the PR body. Not a gate: per the spec, downstream may break and is fixed afterwards.

```bash
git rev-parse HEAD   # note this SHA for the PR body
# For each of rossoctl/operator and rossoctl-cli, in a scratch clone:
#   go mod edit -replace github.com/rossoctl/cortex/authbridge/authlib=<this worktree>/authlib
#   GOWORK=off go build ./... 2>&1 | head
# Record the failing import paths; they are the diff those repos will need.
```

- [ ] **Step 4: Final full verification**

```bash
export GOWORK=off
for m in authlib cmd/abctl cmd/authbridge-proxy cmd/authbridge-envoy \
         cmd/authbridge-praxis storage/redis scripts/profile-tags scripts/readme-demo; do
  (cd "$m" && go mod tidy -diff && go build ./... && go vet ./...) || echo "FAILED: $m"
done
(cd authlib && go test -race ./...)
sh install_test.sh
ls authbridge 2>&1 | head -1    # expect: No such file or directory
```

Expected: all clean; no `authbridge` directory.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -s -m "chore: Retire the flatten migration script

It was committed so the 1,006-file rename could be reviewed by re-running it
rather than read line by line. That job is done, and a one-shot script that
rewrites module paths in a directory which no longer exists is only a hazard.

Marks the design spec implemented. The spec has itself moved to
docs/superpowers/specs/ — by the change it describes.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage.** §3 target layout → Tasks 2–4. §3 collision resolutions → Task 2 (docs, scripts), Task 3 (README), Task 4 (CLAUDE). §3 module mapping → Task 2 Step 1. §4 free wins → Task 2 Step 1, asserted in the commit message. §5 fixed-depth paths → Task 2 Step 3, all seven. §6 `--ref` two-path → Task 1. §6 abandoned URL + `install-demo.sh` → Task 5. §7 `.dockerignore` → Task 2 Step 4, proven in Step 7. §8 sequencing → Global Constraints. §9 commit structure → Tasks 2–6 (five commits, plus Task 1's, which lands first and separately). §10 verification → Task 2 Step 6, Task 6 Step 4. §11 downstream → Task 6 Step 3. §12 out of scope → nothing renames a binary or image. §13 risks → each mitigation appears as a step.

**Placeholder scan.** No TBD/TODO. Every code step carries the actual content. The `sed -i ''` portability note is a real instruction, not a deferral.

**Type consistency.** `with_bootstrap`'s signature is defined once in Task 1 Step 1 as `want_ref http_new http_legacy newest_release [run_as]`, and all six call sites in that task use four or five positional arguments consistently. The `url`/`http`/`boot` variable names in Task 1 Step 3 match the existing block they replace.

**One gap found and closed while reviewing:** the plan originally left the demo SVG regeneration implicit. It is gated by CI's `go-ci-readme-demo` staleness test and cannot be deferred, so it is now Task 5 Steps 3–5 with the failing-first check included.
