# Cortex documentation

Cortex's documentation lives next to the code it describes. This directory holds
only the repo-level pieces.

## Start here

| If you want to… | Read |
|---|---|
| Install and run Cortex on a laptop | [root README](../README.md) |
| Understand the sidecar shapes and deployment | [`authbridge/README.md`](../authbridge/README.md) |
| Configure a plugin | [`authbridge/docs/plugin-catalog.md`](../authbridge/docs/plugin-catalog.md) |
| Write a plugin | [`authbridge/docs/plugin-reference.md`](../authbridge/docs/plugin-reference.md) and [`plugin-tutorial.md`](../authbridge/docs/plugin-tutorial.md) |
| Understand the pipeline internals and hot-reload | [`authbridge/docs/framework-architecture.md`](../authbridge/docs/framework-architecture.md) |
| Run a demo | [`authbridge/demos/README.md`](../authbridge/demos/README.md) |
| Use the `abctl` TUI | [`authbridge/cmd/abctl/README.md`](../authbridge/cmd/abctl/README.md) |

## In this directory

- [`proposals/`](proposals/) — design records for work that has since shipped.
  They are kept for the reasoning, not as current documentation; each carries a
  status line pointing at the doc that describes present behaviour.
- `assets/` — images used by the root README.

Dated implementation plans and design specs live in
[`authbridge/docs/superpowers/`](../authbridge/docs/superpowers/).
