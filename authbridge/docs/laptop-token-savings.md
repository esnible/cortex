# Cut Claude Code token cost on your laptop

Cortex runs as a local proxy in front of Claude Code and strips tool
definitions your agent never calls out of every request. Claude Code sends the
full tool manifest on every turn — tens of thousands of tokens of JSON schema,
billed each time — and the manifest is built by the client, so the proxy is the
only place to trim it without changing every client.

Four steps, about two minutes.

## 1. Install the binaries

```sh
curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install-demo.sh \
  | AUTHBRIDGE_INSTALL_ONLY=1 sh
```

Puts `authbridge-proxy` and `abctl` in `~/.local/bin`. `INSTALL_ONLY` skips
starting the demo — you want a config that persists, which the next step writes.

The variable goes on `sh`, not on `curl`. Written the other way round
(`VAR=1 curl … | sh`) it only reaches `curl`, the script runs without it and
starts the demo — binding 47600-47602 and writing a `cortex-ca/demo.yaml`, so
step 3 then fails on the port clash.

## 2. Write a config

```sh
mkdir -p ~/.cortex
cat > ~/.cortex/config.yaml <<'YAML'
mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
stats:
  address: 127.0.0.1:47602
tls_bridge:
  mode: enabled
  ca_dir: "CA_DIR_PLACEHOLDER"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      # tool-prune must stay last: it rewrites the request body, and body
      # readers have to precede it to see the original bytes.
      - name: tool-prune
        config:
          remove: []
YAML
sed -i.bak "s|CA_DIR_PLACEHOLDER|$HOME/.cortex/ca|" ~/.cortex/config.yaml && rm ~/.cortex/config.yaml.bak
```

Keep this outside any `cortex-ca/` directory. `authbridge-proxy --demo`
regenerates `cortex-ca/demo.yaml` from a built-in template on startup — before
it binds ports, so even a start that fails on a port clash discards your edits.
Running with `--config` avoids that entirely.

## 3. Fill in the prune list and start

```sh
authbridge-proxy --config ~/.cortex/config.yaml &
abctl tools scan --write ~/.cortex/config.yaml
```

`tools scan` reads your own `~/.claude/projects/*.jsonl` transcripts and proposes
the built-in tools you have not called in 30 days. It only ever proposes tools it
recognises, and never one it has seen you call. The config is hot-reloaded; no
restart.

**What the scan cannot know is the future.** It reports what you have not used,
not what you will not need. If you start a kind of work that needs a pruned tool,
its definition is gone from the request and the model cannot call it — that is a
functional failure, not merely a smaller saving. Two things keep it cheap:

- **Rescan occasionally** (say monthly, or after your work changes shape) so the
  list tracks what you actually use. The list is only ever as current as the last
  scan.
- **Reach for `on_error: observe` when in doubt.** It measures the saving and
  changes nothing, so you can see what a list would be worth before trusting it.
  abctl marks those figures with `~`.

If a tool goes missing, the fix is to delete its name from `remove:` — the config
hot-reloads, so it comes back without a restart.

## 4. Point Claude Code at it

```sh
HTTPS_PROXY=http://localhost:47600 \
  NODE_EXTRA_CA_CERTS="$HOME/.cortex/ca/ca.crt" \
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  claude
```

Then watch what it saved:

```sh
abctl --endpoint http://localhost:47601
```

Plugin pane → `tool-prune` → `Metrics`.

What to expect, measured over 99 requests of one real session: **4–20% of the
prompt per turn, median 6%**. Two things move it, and neither is a defect:

- **How much of the manifest is yours to prune.** Requests carrying the full tool
  set saved 15–20%; most requests in that session offered a reduced set and saved
  4–6%.
- **How far into the conversation you are.** The removed bytes are a fixed size,
  so their share of a growing prompt falls — 13% early in that session, 4% by the
  end.

A single early turn can read ~24%, which is why a figure quoted from one request
is not the number to plan with.

Stop it with `pkill -f 'authbridge-proxy --config'`.

## Seeing the saving in money

`$ saved` and `$ saved / request` appear with no extra configuration, labelled
`default rates` to be clear they come from a built-in table rather than your own
account.

**Read that figure as a floor, not a measurement.** The built-in rates were
measured on a shared gateway that bills below vendor list. If your Claude Code
talks straight to Anthropic — which it does unless you have set
`ANTHROPIC_BASE_URL` — you are paying list, so the real saving is several times
what the column shows. Set your own rates to make it accurate; the number is
useful as-is only for confirming the plugin is working and comparing turns
against each other.

Token savings are reported per prompt-cache tier, never as one blended number:
providers charge ~1.25x the input rate for a cache write and ~0.1x for a cache
read, so identical saved bytes differ by more than 12x depending on cache state.

If you are on a different gateway, or the rates have moved, override them per
model — see
[`tool-prune-plugin.md`](./tool-prune-plugin.md#costing-it), which also has the
method for measuring your own from the gateway's cost headers.

## What this does and does not change

`/cost` and anything from the API response `usage` block **do** drop — the server
bills the request it received.

`/context` **does not**. It is computed client-side before the request leaves, and
the pruning happens downstream. So this saves money, not context window;
auto-compact still triggers at the same point. Recovering headroom needs
client-side settings (`--allowedTools`, disabling unused MCP servers).

If the Metrics pane stays empty and every event shows `tunnel`, Claude Code is not
trusting the bridge CA — check `NODE_EXTRA_CA_CERTS` points at the absolute path
above. The proxy also warns about this in its log after a few requests.
