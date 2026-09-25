# CLAUDE.md - Rossoctl Extensions

This file provides context for Claude (AI assistant) when working with the `cortex` monorepo.

## AI Assistant Instructions

- **Always work in your own git worktree — never in the shared top-level checkout.**
  Several Claude Code sessions run against this repo at once. They share one object
  store, which is fine, but a shared *working tree* is not: `git checkout` in one
  session rewrites files under another, and two sessions' uncommitted edits land in one
  index. Before starting work:

  ```sh
  # Fetch into a ref only you write, so nothing can move it underneath you.
  git fetch https://github.com/rossoctl/cortex.git main:refs/base/<topic>
  git worktree add .worktrees/<topic> -b <branch> refs/base/<topic>
  ```

  Then stay in that directory. Leave the top-level checkout alone — treat it as a
  reference copy someone else may be using. When the branch is merged or abandoned, tear
  down all three things you created:

  ```sh
  git worktree remove .worktrees/<topic>   # --force if untracked files remain
  git branch -d <branch>
  git update-ref -d refs/base/<topic>
  ```

  `remove` cleans up its own bookkeeping, so `git worktree prune` is not needed here —
  that is for a worktree directory someone deleted by hand. What `remove` does leave is
  the branch, and a leftover branch is enough to make the next `worktree add -b` of that
  name fail. Deleting the worktree directory instead of removing it is worse: the branch
  stays checked out indefinitely.

  When `branch -d` answers *not fully merged*, that is usually not what happened. It
  judges reachability from the HEAD of whichever tree you run it in, which is normally
  the top-level checkout you were told not to touch — so it is simply behind. PRs land
  here as merge commits, so bring `main` up to date and `-d` will accept the branch.
  Save `-D` for work you really are discarding: it drops unpushed commits silently.

  Three things learned the hard way:
  - **Fetch into your own ref; never branch from `FETCH_HEAD`.** `FETCH_HEAD` is
    per-worktree, but this bootstrap has to run in the shared top-level checkout, so
    every session fetching there writes that one file — and a fetch landing between your
    reading it and your using it hands you a different commit than the one you checked.
    That is how `CLAUDE.md` got reverted mid-session to a pre-rename state. Nothing but
    you writes `refs/base/<topic>`, so there is no window to lose and no verification
    step to remember. Do not use `refs/worktree/` for this — git reserves that namespace
    for per-worktree refs.
  - **A refused `worktree add` does not always mean another session holds the branch.**
    `-b <branch>` also fails when the branch merely exists with nothing checking it out —
    which is what a `worktree remove` without the matching `branch -d` leaves behind.
    `git worktree list` says who actually holds what; the error text does not. When it
    does show a live worktree on that branch, the refusal is a feature: pick another
    name, do not force past it.
  - **Worktrees do not isolate the running Cortex.** One `~/.cortex/config.yaml`, one
    launchd label, one proxy on `:47600`, and every session's `HTTPS_PROXY` points at
    it. `abctl service restart` always replaces that instance and cuts every attached
    session. `abctl service install` only does so when it has something to change or a
    running proxy to adopt — with nothing to do it prints `Already current` and leaves
    the proxy alone. `--ref=main` is an `install.sh` flag, not an `abctl` one; it picks
    which installer script runs, so whether it interrupts anything depends on what that
    install then finds. Coordinate before any of it.

- **Use `Assisted-By` for attribution** — never add `Co-Authored-By`, `Generated with Claude Code`, or similar trailers. See [Commit Attribution Policy](#commit-attribution-policy) below.

## Repository Overview

**cortex** contains Kubernetes security extensions for the [Rossoctl](https://github.com/rossoctl/rossoctl) ecosystem. It provides **zero-trust authentication** for Kubernetes workloads through transparent token exchange and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

The sidecar injection webhook lives in a separate repo: [rossoctl/operator](https://github.com/rossoctl/operator).

**GitHub:** `github.com/rossoctl/cortex`
**Container registry:** `ghcr.io/rossoctl/cortex/<image-name>`
**License:** Apache 2.0

## Top-Level Directory Structure

```
cortex/
├── authbridge/               # Authentication bridge components
│   ├── authlib/              #   Shared auth building blocks (Go module)
│   │   ├── plugins/          #     Every plugin; each owns its own config
│   │   │   ├── jwtvalidation/#       JWKS-backed JWT verifier (validation/)
│   │   │   └── tokenexchange/#       RFC 8693 exchange client + token cache
│   │   ├── bypass/           #     Path pattern matcher
│   │   ├── spiffe/           #     SPIFFE credential sources
│   │   ├── routing/          #     Host-to-audience router
│   │   ├── auth/             #     HandleInbound + HandleOutbound composition
│   │   └── config/           #     Mode presets, YAML config, validation
│   ├── cmd/authbridge-proxy/ #   proxy-sidecar mode (default): HTTP forward + reverse
│   │   │                     #   proxies, full plugin set including parsers
│   │   ├── main.go
│   │   ├── Dockerfile        #     proxy-sidecar combined image
│   │   └── entrypoint.sh
│   ├── cmd/authbridge-envoy/ #   envoy-sidecar mode: ext_proc gRPC server, full plugin set
│   │   ├── main.go
│   │   ├── Dockerfile        #     envoy-sidecar combined image
│   │   └── entrypoint.sh
│   │                         #   (the authbridge-lite image is this proxy
│   │                         #    binary built with the `lite` profile's tags)
│   ├── cmd/authbridge-cpex/  #   proxy-sidecar + cpex plugin (cgo, links libcpex_ffi.a)
│   ├── cmd/authbridge-praxis/#   proxy-sidecar behind a Praxis proxy config. PAUSED —
│   │                         #   kept and kept compiling, but ships in no image
│   │                         #   and registers no plugins. Do not delete.
│   ├── cmd/abctl/            #   Terminal UI over the session API (the largest
│   │                         #   component after authlib). Released as a binary.
│   ├── proxy-init/           #   iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes)
│   │   ├── init-iptables.sh
│   │   ├── Dockerfile.init
│   │   ├── Makefile
│   │   └── README.md
│   ├── docs/                 #   Plugin + framework reference; superpowers/{plans,specs}
│   ├── scripts/              #   profile-tags (build-tag resolver), readme-demo
│   ├── storage/redis/        #   Redis driver for the storage.Store interface
│   ├── sparc-service/        #   Python SPARC reflection service (own image)
│   ├── lineage-attach/       #   OTel shim + scripts for lineage propagation
│   ├── demos/                #   12 demo scenarios — see authbridge/demos/README.md
│   └── keycloak_sync.py      #   Declarative Keycloak sync tool
├── docs/                     # Repo-level proposals + assets
├── tests/                    # Python tests (keycloak_sync)
├── .github/
│   ├── workflows/            # CI/CD (ci.yaml, build.yaml, security-scans, scorecard, spellcheck)
│   └── ISSUE_TEMPLATE/       # Bug report, feature request, epic templates
├── .pre-commit-config.yaml   # Pre-commit hooks (whitespace, yaml/json, ruff; no Go hooks)
└── CLAUDE.md                 # This file
```

## Major Components

### 1. AuthBridge Binaries (Go)

**Sidecar binaries** providing transparent traffic interception for both inbound JWT validation and outbound OAuth 2.0 token exchange (RFC 8693). All sidecar binaries but `authbridge-praxis` pin one deployment shape and refuse a mismatching `mode:` at boot; mode is no longer selected at runtime. The `authbridge-lite` **image** is a build variant of the proxy binary (not a separate binary) — see below.

**Library:** `authbridge/authlib/` (shared)
**Language:** Go 1.26.5 (`authbridge/go.work` and the nine workspace modules; the
three self-contained `demos/*` modules are still on 1.24)
**Detailed guide:** [`authbridge/CLAUDE.md`](authbridge/CLAUDE.md)

**Binaries:**
- `cmd/authbridge-proxy/` — proxy-sidecar mode (default): HTTP forward + reverse proxies, full plugin set (jwt-validation, token-exchange, a2a-parser, mcp-parser, inference-parser). No Envoy / no gRPC.
- `cmd/authbridge-envoy/` — envoy-sidecar mode: ext_proc gRPC server hooked into Envoy, full plugin set.
- `cmd/authbridge-cpex/` — proxy-sidecar plus the `cpex` plugin. Needs cgo; links `libcpex_ffi.a` from a pinned CPEX release.
- `cmd/authbridge-praxis/` — proxy-sidecar rendered into a Praxis proxy config. **Paused, not abandoned:** it ships in no image, has no demo, and registers no plugins, but it is deliberately kept and deliberately kept compiling (it appears in the `ci.yaml` binary matrix for exactly that reason). Do not propose deleting it; it consumes `config.Config`, so shared-config refactors have to keep it building.
- `cmd/abctl/` — the terminal UI over the session API (`:9094`). Not a sidecar; released as a standalone binary and the component the root README leads with.
- `authbridge-lite` (image, **not** a separate binary) — `cmd/authbridge-proxy` built with the `lite` profile, a sidecar minimum (jwt-validation, token-exchange, litellm-budget-track, static-inject; parsers and OPA dropped). Every plugin is opt-in: they live in `cmd/*/plugins_<name>.go` files gated by `//go:build include_plugin_<name>`, and profiles are defined in `authbridge/scripts/profile-tags`.

**Common:**
- `authlib/` — shared auth library (JWT validation, token exchange, caching, routing, all listener implementations, all plugins).
- `proxy-init/init-iptables.sh` — traffic interception setup (Istio ambient mesh compatible). Used by envoy-sidecar mode (`redirect`) and by proxy-sidecar mode's `enforce-redirect` egress guard.
- `proxy-init/Dockerfile.init` — proxy-init container image.

**Ports (envoy-sidecar):** 15123 (outbound), 15124 (inbound), 9090 (ext-proc), 9901 (admin)
**Ports (proxy-sidecar / lite):** 8080 (reverse proxy), 8081 (forward proxy), 9091 (health), 9093 (stats), 9094 (session API)

### 2. Client Registration

Keycloak client registration for workloads is handled by the
**operator** (separate repo) — see `operator/docs/operator-managed-client-registration.md`.
The operator creates a Secret with `client-id.txt` + `client-secret.txt`
and the webhook mounts it at `/shared/` in the workload pod. The
in-pod `client-registration` sidecar that previously lived in this
repo has been removed.

## How the Components Work Together

The operator (in a separate repo) injects AuthBridge sidecars
into workload pods. Default deployment shape (proxy-sidecar mode):

```
         ┌────────────────────────────────────┐
         │            WORKLOAD POD            │
         │                                    │
         │  authbridge-proxy ──► SPIRE Agent  │  (in-process
         │    - spiffe.Provider reads SVIDs   │   Workload API
         │      over the Workload API and     │   client; shaped
         │      mirrors them under /opt/      │   by the `spiffe:`
         │    - Reverse proxy: inbound JWT    │   config block)
         │    - Forward proxy: outbound       │
         │      token exchange                │
         │       │                            │
         │  Your Application                  │
         │    (HTTP_PROXY → forward proxy)    │
         └────────────────────────────────────┘

         The operator also creates a Secret with client-id +
         client-secret and mounts it at /shared/.

         For envoy-sidecar mode, replace authbridge-proxy with
         the authbridge-envoy image (Envoy + ext_proc) and add a
         proxy-init container for iptables.
```

There is no `spiffe-helper` sidecar and no `SPIRE_ENABLED` gate. SPIRE
credentials are fetched in-process by `authlib/spiffe`'s Provider.

## AuthBridge Binaries

Sidecar binaries, one Dockerfile each; the `authbridge-lite` image is a build variant of the proxy binary (proxy Dockerfile + the `lite` profile's tags):

| Binary | Mode | Listeners | Plugins |
|--------|------|-----------|---------|
| `cmd/authbridge-proxy/` | proxy-sidecar (default) | HTTP forward + reverse proxies | full (incl. parsers) |
| `cmd/authbridge-envoy/` | envoy-sidecar | gRPC ext_proc on :9090 | full (incl. parsers) |
| `cmd/authbridge-cpex/` | proxy-sidecar | HTTP forward + reverse proxies | full + `cpex` (cgo) |
| `cmd/authbridge-praxis/` | proxy-sidecar | HTTP (Praxis-rendered) | **none** — paused, see above |
| `authbridge-lite` _(image: proxy + `lite` profile)_ | proxy-sidecar | HTTP forward + reverse proxies | sidecar-minimum plugin set (see `authbridge/scripts/profile-tags`) |

`cmd/abctl/` is also a Go module here but is not a sidecar — it is the operator-facing TUI over the session API.

**Go modules** (12 in total; `authbridge/go.work` links 9 of them — the three
self-contained `demos/*` modules are outside the workspace. `go-tidy-check` in
`ci.yaml` does cover all 12: it discovers them with `find`, not from `go.work`):
- `authbridge/authlib/` — pure library: validation, exchange, cache, bypass, spiffe, routing, auth, config, all listener implementations, all plugins. **Consumed outside this repo**, so removing exported API here is a cross-repo change.
- `authbridge/cmd/authbridge-{proxy,envoy,cpex,praxis}/` — thin main packages that import authlib and start the listeners they need; they import no plugin package directly. (The `authbridge-lite` image is `authbridge-proxy` built with the `lite` profile.)
- `authbridge/cmd/abctl/` — the TUI; also released as a standalone binary.
- `authbridge/storage/redis/`, `authbridge/scripts/{profile-tags,readme-demo}/`, and the self-contained `authbridge/demos/{echo,finance-sparc,ibac}/`.
- `authbridge/go.work` — workspace linking authlib + the binaries for local development.

**Config format:** YAML with `${ENV_VAR}` expansion, mode presets, and startup validation. The `mode` field must match the binary for all but `authbridge-praxis`, which pins no mode.

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-* | Pre-commit; `go fmt`/`go vet`/build/test for authlib, both `scripts/*` and the `cmd/*` matrix; `go mod tidy -diff` for all 12 modules; Python tests. Note `go fmt` rewrites rather than fails, so it does not gate |
| `build.yaml` | Tag push (`v*`) or manual | Multi-arch Docker builds for all six matrix images: proxy-init, authbridge (proxy-sidecar combined), authbridge-envoy (envoy-sidecar combined), authbridge-lite (proxy Dockerfile built with the `lite` profile from `authbridge/scripts/profile-tags`), authbridge-cpex, and sparc-service (Python). Every Go image passes `GO_BUILD_TAGS` naming a profile — plugins are all opt-in, so an image built without tags registers none |
| `security-scans.yaml` | PR to main | Dependency review, shellcheck, YAML lint, Hadolint, Bandit, Trivy, CodeQL |
| `scorecard.yaml` | Weekly / push to main | OpenSSF Scorecard security health metrics |
| `spellcheck_action.yml` | PR | Spellcheck on markdown files |

### PR Title Convention

PR titles must follow the format:

```
<Prefix>: <Subject starting with uppercase>
```

The CI title check (`deepakputhraya/action-pr-title`) is **case-sensitive** and
requires a **capitalized** prefix from this set: `Build`, `Chore`, `CI`, `Docs`,
`Feat`, `Fix`, `Perf`, `Refactor`, `Revert`, `Style`, `Test`, `Feature`,
`Bug fix`, `Proposal`, `Breaking change`, `Other`, `Other/Misc`.

Note: commit *message* examples elsewhere in this doc use lowercase
(`feat:` / `fix:`) — that is fine for commits, but the PR *title* check rejects
lowercase prefixes. Use `Fix:` / `Feat:` / `Docs:` in PR titles.

## Container Images

All images are pushed to `ghcr.io/rossoctl/cortex/` from
`.github/workflows/build.yaml`:

| Image | Source | Description |
|-------|--------|-------------|
| **`authbridge`** | **`authbridge/cmd/authbridge-proxy/Dockerfile`** | **proxy-sidecar image (default mode): authbridge-proxy, full plugin set incl. parsers. No Envoy.** |
| `authbridge-envoy` | `authbridge/cmd/authbridge-envoy/Dockerfile` | envoy-sidecar combined image: Envoy + authbridge-envoy (ext_proc, full plugin set) |
| `authbridge-lite` | `authbridge/cmd/authbridge-proxy/Dockerfile` (+ `GO_BUILD_TAGS` from the `lite` profile) | proxy-sidecar image with a sidecar-minimum plugin set (see `authbridge/scripts/profile-tags`). A build variant of `authbridge`, not a separate binary; not yet referenced by the operator's default config |
| `authbridge-cpex` | `authbridge/cmd/authbridge-cpex/Dockerfile` | proxy-sidecar build with the CPEX plugin. Two tag sources: the literal `cpex` tag, always required because it gates `cmd/authbridge-cpex/main.go`, plus the plugin tags its Dockerfile appends from `GO_BUILD_TAGS` (resolved from the `cpex` profile in `build.yaml`) — `cpex` alone registers no plugins. Links `libcpex_ffi.a` from a pinned CPEX release (CGO_ENABLED=1). Routes hooks through the CPEX framework (APL DSL + named CPEX policy plugins). FFI ABI version is read from `authbridge/cmd/authbridge-cpex/CPEX_FFI_VERSION` |
| `proxy-init` | `authbridge/proxy-init/Dockerfile.init` | Alpine + iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes) |
| `sparc-service` | `authbridge/sparc-service/Dockerfile` | Python SPARC reflection service (FastAPI wrapper around the ALTK pre-tool reflection component), called by the `sparc` plugin |

None of these images bundle `spiffe-helper`, and `SPIRE_ENABLED` no
longer gates anything. SPIRE credentials are fetched in-process by
`authlib/spiffe`'s Provider.

The legacy `authbridge-unified`, `authbridge-light`, `client-registration`,
`spiffe-helper`, `auth-proxy`, and `demo-app` standalone images have
been removed from CI (the auth-proxy / demo-app source is still in-tree
for the standalone quickstart). Older release tags continue to publish
the old images.

## Pre-commit Hooks

Install: `pre-commit install`

Hooks:
- `trailing-whitespace`, `end-of-file-fixer`, `check-added-large-files` (max 1024KB), `check-yaml`, `check-json`, `check-merge-conflict`, `mixed-line-ending`
- `ai-assisted-by-trailer` — Rewrites `Co-Authored-By` to `Assisted-By` (commit-msg stage)
- `ruff`, `ruff-format` — Python linting/formatting on `authbridge/` files

**There are no Go hooks** — `gofmt` and `go vet` run nowhere in pre-commit. In
`ci.yaml` both run, but only one of them can fail:

- `go vet ./...` **is** a gate, on 7 of the 12 modules: `authlib`, both
  `scripts/*`, and the `cmd/{authbridge-proxy,authbridge-envoy,abctl,authbridge-praxis}`
  matrix. Not vetted anywhere: `cmd/authbridge-cpex` (deliberately excluded — it
  needs CGO and a pinned `libcpex_ffi.a`, so `build.yaml` covers it via the image
  build), `storage/redis`, and the three `demos/*` modules.
- `go fmt ./...` is **not** a gate. `go fmt` is `gofmt -l -w`: it rewrites the
  checkout and exits 0, so formatting drift cannot fail a build.

Formatting drift therefore reaches main — a few files are gofmt-dirty there
today, including three under `authlib`, where `go fmt` demonstrably runs on
every PR. Run `gofmt -l` yourself before pushing, and `go vet` too if you
touched one of the five unvetted modules.

## Languages and Tech Stack

| Area | Technology |
|------|------------|
| AuthBridge sidecar binaries | Go 1.26.5, envoy-control-plane, lestrrat-go/jwx |
| abctl (TUI) | Go 1.26.5, bubbletea |
| keycloak_sync.py / setup scripts | Python 3.12, python-keycloak (`>=7.1.1,<8`) |
| sparc-service | Python 3.10+, FastAPI, agent-lifecycle-toolkit |
| Proxy | Envoy v1.37.1 (pinned by digest in `cmd/authbridge-envoy/Dockerfile`) |
| Traffic interception | iptables (via init container) |
| Identity | SPIFFE/SPIRE (JWT-SVIDs) |
| Auth provider | Keycloak (OAuth2/OIDC, token exchange RFC 8693) |
| Packaging | Docker |
| CI | GitHub Actions |

## External Dependencies and Services

| Service | Required | Purpose |
|---------|----------|---------|
| Kubernetes | Yes | Target platform (v1.25+ recommended) |
| [operator](https://github.com/rossoctl/operator) | Yes | Injects AuthBridge sidecars into workload pods |
| Keycloak | Yes | OAuth2/OIDC provider, token exchange |
| SPIRE | Optional | SPIFFE identity (JWT-SVIDs) for workloads |

## ConfigMaps and Secrets Expected at Runtime

When the operator injects sidecars, the target namespace needs these resources:

| Resource | Kind | Used by | Keys |
|----------|------|---------|------|
| `authbridge-config` | ConfigMap | authbridge | `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `PLATFORM_CLIENT_IDS` (optional), `TOKEN_URL` (optional, derived from KEYCLOAK_URL+KEYCLOAK_REALM), `ISSUER` (optional, derived or explicit for split-horizon DNS), `DEFAULT_OUTBOUND_POLICY` (optional, defaults to `passthrough`). Inbound audience validation uses `CLIENT_ID` from `/shared/client-id.txt`. Target audience and scopes are configured per-route in `authproxy-routes`. |
| `keycloak-admin-secret` | Secret | operator (ClientRegistrationReconciler) | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `authproxy-routes` | ConfigMap (optional) | authbridge | `routes.yaml` -- per-host token exchange rules (see authbridge/CLAUDE.md for format) |
| `spiffe-helper-config` | ConfigMap (legacy, unused) | (none) | Retained only for older deployments; authbridge does not read it |
| `envoy-config` | ConfigMap | envoy-proxy | Envoy YAML configuration |

**Note:** `authproxy-routes` is optional. Without it, all outbound traffic passes through unchanged (the default policy is `passthrough`). Only create it when the agent needs to call services that require token exchange. Set `DEFAULT_OUTBOUND_POLICY: "exchange"` in `authbridge-config` to restore the legacy behavior.

## Common Development Tasks

### Building Everything Locally

The repo-root `local-build-and-test.sh` orchestrates every image
the platform needs (`spiffe-idp-setup` from rossoctl, plus
`authbridge`, `authbridge-envoy`, `authbridge-lite`, `proxy-init`
from this repo) and loads them into a Kind cluster:

```bash
ROSSOCTL_DIR=../rossoctl ./local-build-and-test.sh
```

To build a single image directly:

```bash
# proxy-init (iptables init container, envoy-sidecar mode)
cd authbridge/proxy-init && make docker-build-init

# Combined sidecars (proxy-sidecar default / envoy-sidecar)
cd authbridge && podman build -f cmd/authbridge-proxy/Dockerfile -t authbridge:latest .
cd authbridge && podman build -f cmd/authbridge-envoy/Dockerfile -t authbridge-envoy:latest .
# authbridge-lite: same proxy Dockerfile, built with the `lite` profile
# from authbridge/scripts/profile-tags. Plugins are all opt-in, so
# GO_BUILD_TAGS is required — omitting it registers no plugins.
cd authbridge && podman build -f cmd/authbridge-proxy/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . lite)" \
  -t authbridge-lite:latest .
```

### Running the Full Demo

1. Set up a Kind cluster with SPIRE + Keycloak (use [Rossoctl installer](https://www.rossoctl.dev/docs/overview/quickstart))
2. Deploy the webhook via [operator](https://github.com/rossoctl/operator)
3. See the [AuthBridge demos index](authbridge/demos/README.md) for a recommended learning path:
   - **Getting started**: `authbridge/demos/weather-agent/demo-ui.md` (inbound validation, UI deployment)
   - **Full flow**: `authbridge/demos/github-issue/demo-ui.md` (token exchange + scope-based access)
   - **Routes config reference**: `authbridge/demos/token-exchange-routes/README.md` (single + multi-target route patterns)

### Adding a New Component Image to CI

1. Add entry to `.github/workflows/build.yaml` matrix (`image_config` array)
2. Provide `name`, `context`, and `dockerfile` fields
3. Image will be pushed to `ghcr.io/rossoctl/cortex/<name>`

## Code Style and Conventions

### Go Code
- Run `gofmt -l` before pushing. **It is not enforced anywhere:** pre-commit has
  no Go hooks, and `ci.yaml`'s `go fmt ./...` is `gofmt -l -w`, which rewrites and
  exits 0. `go vet` *is* gated, but only on 7 of the 12 modules — see the
  Pre-commit Hooks section for which five are uncovered.
- Run per-module with `GOWORK=off`, as every CI Go job except the `authlib` one
  does, so each module resolves its own `replace` directives instead of pulling in
  workspace siblings.
- If a change deletes a package or its last import of a dependency, also run
  `go mod tidy -diff` in every module — `ci.yaml`'s `go-tidy-check` gates on it,
  and `build`/`vet`/`test` all pass while it fails.

### Python Code (keycloak_sync.py, demo setup scripts)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies in `authbridge/requirements.txt` — `python-keycloak>=7.1.1,<8`.
  Note `ci.yaml`'s Python job still pip-installs `python-keycloak==5.3.1`, two
  majors behind, so CI is not testing the version the project declares.

### Kubernetes Manifests
- Example deployment YAMLs in `authbridge/demos/*/k8s/`

### Shell Scripts
- `set -euo pipefail` (strict mode)
- Extensive inline documentation (especially `init-iptables.sh`)

## Important Cross-Component Relationships

1. **Envoy Proxy UID:** Envoy runs as UID 1337. The `proxy-init` iptables rules exclude this UID from redirection to prevent loops. The `authbridge` container also runs as UID 1337.

2. **Shared Volume Contract:** The sidecar and the operator communicate through files:
   - `/opt/svid.pem`, `/opt/svid_key.pem`, `/opt/svid_bundle.pem` — written by authbridge's in-process `spiffe.Provider` mirror when `spiffe.mirror_files` is on (the default), for external readers (e2e probes, debugging). The hot path reads SVIDs from memory, not these files.
   - `/opt/jwt_svid.token` — same mirror, but written only when a plugin requests a JWT-SVID for an audience (today: `token-exchange` with `identity.type: spiffe`). An X.509-only workload never produces it.
   - `/shared/client-id.txt` — operator-created Secret mount, read by authbridge (`jwt-validation`'s `audience_file`)
   - `/shared/client-secret.txt` — operator-created Secret mount, read by authbridge (`token-exchange`)

3. **Port Coordination:** Envoy listens on 15123 (outbound) and 15124 (inbound). The ext-proc listens on 9090. The `proxy-init` iptables rules redirect to these ports.

## Gotchas and Known Issues

1. **Multiple Go modules:** The repo has 12 Go modules under `authbridge/` — `authlib/`, each `cmd/*/`, `storage/redis/`, both `scripts/*/`, and the three self-contained `demos/*/` ones. `authbridge/go.work` links the first nine; the `demos/*` modules are deliberately outside the workspace. Local commands from a specific module directory should typically set `GOWORK=off` (as every CI Go job but `authlib`'s does) so the module resolves its own `replace` directives instead of pulling in workspace siblings.

2. **Avoid committing venvs:** Virtual environment directories (e.g. `authbridge/proxy-init/quickstart/venv/`) should be gitignored (the repo's `.gitignore` has a `venv` pattern). Do not create and commit new virtual environments under version control.

3. **Envoy config not embedded:** The envoy-proxy sidecar mounts `envoy-config` ConfigMap at `/etc/envoy`. This ConfigMap must exist in the target namespace before workloads are created.

4. **Outbound policy is passthrough by default:** AuthBridge defaults to passing outbound traffic through unchanged. Token exchange only happens for hosts explicitly listed in the `authproxy-routes` ConfigMap. Target audience and scopes are configured per-route in `authproxy-routes`.

5. **Route host patterns use short service names:** The `host` field in `authproxy-routes` matches against the HTTP `Host` header, which is typically just the short Kubernetes service name (e.g., `github-tool-mcp`), not the FQDN. Glob patterns (`*`) are supported but the most common case is a plain service name.

## DCO Sign-Off (Mandatory)

All commits **must** include a `Signed-off-by` trailer (Developer Certificate of Origin).
Always use the `-s` flag when committing:

```sh
git commit -s -m "feat: Add new feature"
```

This adds a line like `Signed-off-by: Your Name <your@email.com>` to the commit message.
PRs without DCO sign-off will fail CI checks. To retroactively sign-off existing commits:

```sh
git rebase --signoff main
```

## Orchestration

This repo includes orchestrate skills for enhancing related repositories.
Run `/orchestrate <repo-url>` to start.

| Skill | Description |
|-------|-------------|
| `orchestrate` | Entry point — scan, plan, execute phases |
| `orchestrate:scan` | Assess repo structure, CI, security gaps |
| `orchestrate:plan` | Create phased enhancement plan |
| `orchestrate:precommit` | Add pre-commit hooks and linting |
| `orchestrate:tests` | Add test infrastructure |
| `orchestrate:ci` | Add CI/CD workflows |
| `orchestrate:security` | Add security governance files |
| `orchestrate:replicate` | Bootstrap skills into target repo |
| `orchestrate:review` | Review all orchestration PRs before merge |

Skills management:

| Skill | Description |
|-------|-------------|
| `skills` | Skills router — create, validate, scan |
| `skills:write` | Create or edit skills with proper structure |
| `skills:validate` | Validate skill format and naming |
| `skills:scan` | Audit repo for skill gaps |

## Commit Attribution Policy

When creating git commits, do NOT use `Co-Authored-By` trailers for AI attribution.
Instead, use `Assisted-By` to acknowledge AI assistance without inflating contributor stats:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Never add `Co-authored-by`, `Made-with`, or similar trailers that GitHub parses as co-authorship.

### PR Bodies

PR descriptions should end with the same `Assisted-By` trailer:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Do not use `🤖 Generated with [Claude Code](https://claude.com/claude-code)` or similar.

A `commit-msg` hook in `scripts/hooks/commit-msg` enforces this automatically for commits.
Install it via pre-commit:

```sh
pre-commit install --hook-type pre-commit --hook-type commit-msg
```
