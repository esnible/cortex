# OpenCode

[OpenCode](https://opencode.ai/) is a supported agent. Cortex recognises its User-Agent,
groups its traffic by OpenCode's own session id and parses its inference.
`agentop configure opencode` routes it through Cortex and takes that out again.

One fact shapes all of it: OpenCode's traffic does not leave from the `opencode` you run.
The first client starts one background service per user, `opencode serve --service`,
and that service sends all of OpenCode's traffic, for every session. Later clients reuse
it, and it outlives them. It keeps the environment it started with. So proxy variables
in your shell, or `agentop exec -- opencode`, reach it only when that command is the one
that starts it.

## Enable and revert

```sh
agentop configure opencode enable    # route OpenCode's service through Cortex
agentop configure opencode status    # what is set, and what the running service uses
agentop configure opencode disable   # take it out again
```

**What `enable` writes.** The service keeps an environment of its own, the `env` block
of OpenCode's `service.json`. That file is `$XDG_CONFIG_HOME/opencode/service.json`, or
`~/.config/opencode/service.json` when `XDG_CONFIG_HOME` is unset. When the service
starts, those values take precedence over the environment it inherited, so the routing
goes there and not into a shell profile. `enable` sets nine variables in that block,
each with its own `opencode service set env NAME VALUE` call. agentop never edits the
file itself. These are the nine variables `agentop exec` sets, chosen for the reasons
[its section](../../cmd/agentop/README.md#running-one-command-through-cortex-agentop-exec)
gives. The addresses come from `~/.cortex/config.yaml`:

| Variable | Value |
|---|---|
| `HTTPS_PROXY` `HTTP_PROXY` `https_proxy` `http_proxy` | Cortex's forward proxy URL |
| `NODE_EXTRA_CA_CERTS` | `ca.crt`, the bridge CA |
| `SSL_CERT_FILE` `GIT_SSL_CAINFO` `REQUESTS_CA_BUNDLE` `CURL_CA_BUNDLE` | `bundle.crt`, the bridge CA plus the platform roots |

`enable` refuses to run when the TLS bridge is disabled or has no `ca_dir`, because
OpenCode would then have no CA to trust. It also refuses to overwrite a value someone
else set, such as a corporate proxy, and prints the `opencode service unset env` command
that removes it. The only values it replaces are Cortex's own, such as a proxy at an
older Cortex address. On macOS it also prints that Go tools (`go`, `gh`) use the
keychain, not `SSL_CERT_FILE`; see
[Go tools on macOS](../laptop-service.md#go-tools-on-macos-need-the-keychain-not-a-variable).

**The record.** The first `enable` records what the nine variables held in
`~/.cortex/opencode-state.json`. Any value `enable` replaced was Cortex's, so the record
stores it as absent: putting an old Cortex address back would leave OpenCode on Cortex
after `disable`. As a result, the record normally holds nothing to restore.

**What `disable` does.** It changes only Cortex's values, unsetting each one or
restoring a value found in the record. A value that is not Cortex's, even one set after
`enable`, stays in place, and `disable` names it (`<KEY> left as "<v>": it is not a
value Cortex set.`) along with the command that removes it. `disable` does not need
Cortex's config. Without it, `disable` recognises Cortex's values by their shape: a
proxy on `localhost:476…` or `127.0.0.1:476…`, or a CA path under `.cortex/`. Run it
before you [remove Cortex](../laptop-service.md#remove-it). A service started with these
variables sends its requests to Cortex's address whether or not anything is listening
there.

**What `status` says.** It prints the nine variables, then `enabled` if every one holds
exactly the value `enable` writes, or `not fully enabled (N of 9 set)` if not. A proxy
written another way, such as `localhost` instead of `127.0.0.1`, counts as not set here.
Then comes one line about the running service. It answers two questions: is the proxy
the service uses now Cortex's, and would the proxy its service environment gives it on a
restart be Cortex's? The line is one of:

- it is using Cortex;
- it is running with its old environment, and `opencode service restart` would put it
  on Cortex;
- it is using Cortex, but its service environment does not route it there, so a restart
  would take it off Cortex. A service started under `agentop exec` looks like this, and
  `enable` keeps it on Cortex;
- it is not using Cortex;
- it is not running;
- it could not check, because the service's environment cannot be read or
  `opencode service status` failed.

If the Cortex config cannot be read, `status` prints the variables and the service's pid
and proxy, with no verdict. `status` changes nothing.

**Restarting.** None of the three commands restarts the service, because
`opencode service restart` ends every OpenCode session using it. A running service picks
up `enable` or `disable` only when it restarts. Both commands tell you when the running
service still has its old environment.

`enable` and `disable` both show what they will change and ask before doing it. `--yes`
skips the question. If you decline, or there is no terminal to ask on, they change
nothing and exit 3. `--config PATH` reads a different Cortex config. `--opencode BIN`
names the CLI when it is neither on `PATH` nor in `~/.opencode/bin`.

**Undoing it by hand**, without agentop: check what the service environment holds, unset
each of the nine that holds Cortex's value, delete the record, and restart the service.

```sh
opencode service get env
opencode service unset env HTTPS_PROXY
opencode service unset env HTTP_PROXY
opencode service unset env https_proxy
opencode service unset env http_proxy
opencode service unset env NODE_EXTRA_CA_CERTS
opencode service unset env SSL_CERT_FILE
opencode service unset env GIT_SSL_CAINFO
opencode service unset env REQUESTS_CA_BUNDLE
opencode service unset env CURL_CA_BUNDLE
rm -f ~/.cortex/opencode-state.json
opencode service restart   # ends every OpenCode session using the service
```

**`agentop exec -- opencode` instead.** This routes OpenCode only when it is the command
that starts the service. The service then keeps `exec`'s environment, so later clients
that reuse it go through Cortex too, until the service restarts. A service that is
already running without Cortex is not changed. In that case `exec` warns that the
service is not using Cortex and gives `enable` and the restart command. It also prints a
note when it cannot check the service. Two cases get no check at all:
`opencode service …` commands, and `opencode` run through a wrapper such as
`env opencode`.

## CA trust

Cortex's TLS bridge decrypts OpenCode's HTTPS using certificates it forges from its own
CA, so the service has to trust that CA. This was tested on OpenCode 2.0.21 on macOS,
with each variable set only in the service environment. `NODE_EXTRA_CA_CERTS` alone was
enough, and so was `SSL_CERT_FILE` alone: with either one, OpenCode Zen's inference was
decrypted and recorded. `enable` sets both. `NODE_EXTRA_CA_CERTS` adds `ca.crt` to the
runtime's own roots. The bundle variables replace a tool's roots instead of adding to
them, so they get `bundle.crt` (the CA plus the platform roots) rather than `ca.crt`
alone. `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE` and `CURL_CA_BUNDLE` are for programs the
service runs, such as `git`, Python and `curl`. Whether every OpenCode tool passes the
service's environment to the commands it runs was not checked.

**Missing trust does not look like an error.** OpenCode refuses the forged certificate,
Cortex passes that host through, and OpenCode's retry succeeds as an opaque tunnel.
OpenCode keeps working normally. The gap is in Cortex: the inference appears only as
`CONNECT` rows marked `tunnel`, with no parsed inference, no token counts or cost, and no
`ses_…` session, because the session header travels inside the encrypted request.
Cortex's log names the host:

```sh
grep client-hung-up ~/.cortex/proxy.log
```

For OpenCode Zen this finds a `WARN tls-bridge passthrough host=opencode.ai
reason=client-hung-up` line; another provider shows its own host. The reason is
`client-hung-up`, not the `client-rejected-ca` that
[Everything shows as `tunnel`](../laptop-service.md#everything-shows-as-tunnel-and-no-plugin-ever-runs)
is written around. `client-hung-up` records a client that closed the connection
mid-handshake without sending a TLS alert, which is what OpenCode does here. That page
lists `client-hung-up` as usually needing no action. When it repeats for OpenCode's
inference host, missing trust is the likely cause. The fix is to put the CA variables in
the environment the service starts with: run `enable`, then `opencode service restart`.

## What Cortex shows for it

**Its own agent.** OpenCode's TUI, its service and its inference client all send the
same User-Agent, `opencode/<channel>/<version>/<client>` (for example
`opencode/latest/2.0.21/cli`). Cortex recognises it as `opencode` and takes the version
from the field after the channel. OpenCode therefore gets its own row in agentop's
agents pane instead of sharing `Other`.

**Its own sessions.** On its inference requests the service sends OpenCode's session id
(`ses_…`, the id that appears in the TUI's URLs) in `X-Opencode-Session-Id`. Each
OpenCode session is a bucket named with that id. The header is in the default
`session.id_headers`. If you set that list yourself, include it, because naming any
header replaces the built-in list.

**Typed inference.** These requests are parsed in the OpenAI dialect (model, messages,
tools and the response):

- requests to OpenCode Zen's `/zen/v1/chat/completions`;
- requests to OpenAI-compatible providers on `/v1/chat/completions` or
  `/chat/completions`, LiteLLM among them.

A provider on Anthropic's `/v1/messages` is parsed in that dialect instead. The path has
to match exactly. A provider mounted under a prefix of its own, or any other endpoint
under `/zen`, is recorded with its method and path but not parsed.

**Tokens and cost.** Token counts come from the usage block in the response. A streamed
OpenAI-dialect response includes one only when the request asked for it
(`stream_options.include_usage`). Whether OpenCode asks every provider for it was not
checked. Cost needs either a reported figure or a rate. A LiteLLM gateway's
`X-Litellm-Response-Cost` header is used as the figure. Otherwise the tokens are priced
at a rate, and Cortex bundles rates only for Anthropic's models. Any other model, Zen's
included, is reported unpriced until you add a `pricing:` entry for it.
[Finding traffic that is not priced](../pricing.md#finding-traffic-that-is-not-priced)
shows how to find the endpoint and model to add.

**Tool pruning is unsupported.** The `tool-prune` plugin acts on any request whose path
ends in `/v1/chat/completions`, Zen's included, and removes the tools named in its
`remove` list. That list comes from `agentop tools scan`, which reads only Claude Code's
transcripts and proposes only Claude Code's built-in tool names, so nothing builds a
list for OpenCode. A list written by hand would be applied to OpenCode's requests; that
was not tested.

**Header-less traffic.** On a laptop, `session.process_attribution` is on by default,
and a request with no session header is filed by the process that sent it. The
service's own header-less requests and tunnels, and those of the commands it runs, are
filed under its sessions; [Known issues](#known-issues) says which one. When
OpenCode's TUI talks to the service through Cortex, those requests are forwarded
without being recorded once the service has named a session. Before that, they are
recorded. See
[How traffic is grouped into sessions](../laptop-service.md#how-traffic-is-grouped-into-sessions).

## Verified depth

Tested live with OpenCode 2.0.21 on macOS 26.6 (arm64) and Cortex built from the change
that added this page, using OpenCode Zen's models, which ran without a key.

Not tested live:

- **Linux.** agentop's code paths and the process lookup's are unit-tested on Linux in a
  container, but OpenCode itself was not run there.
- **Other OpenCode versions.** The User-Agent shape and the session header were captured
  from 2.0.21.
- **Providers other than OpenCode Zen**, LiteLLM included.
  [#941](https://github.com/rossoctl/cortex/issues/941) has one user's report of a
  LiteLLM run, started with `agentop exec` under its earlier name, in which token counts
  showed.

## Known issues

- **One service holds several sessions.** The service's header-less traffic goes under
  whichever of its sessions was most recently active. Two sessions whose tool calls
  overlap can take each other's rows. A new session's header-less traffic goes under
  the previous session until the new session's first request names it.
- **Before the service names its first session**, its requests go to a pending bucket,
  `pending:opencode@<pid>.<start>`, once one of them has identified the process as
  OpenCode by its User-Agent. The bucket is named for the topmost OpenCode process in
  the service's chain of parents, usually the service itself. The first session the
  service names adopts the bucket, unless that session already holds events. In that
  case the bucket stays a row of its own.
- **The service keeps the environment it started with.** `enable` and `disable` reach a
  running service only when it restarts, and `agentop exec -- opencode` reaches it only
  when that command starts it ([Enable and revert](#enable-and-revert)).
- **Probes of local model servers that are not running.** The service probes local model
  servers on every cycle; on 2.0.21 these included `:1234` (LM Studio's port) and
  `:8000`. With nothing listening there, each probe is recorded in the service's session
  as a request with no response. Once [#1223](https://github.com/rossoctl/cortex/pull/1223)
  merges, each probe instead appears as a `502` response row with `upstream_refused`,
  every cycle.
- **`X-Session-Id` is not read.** OpenCode also sends its session id in that header. The
  name is generic, so Cortex reads `X-Opencode-Session-Id` instead. Adding
  `X-Session-Id` to `session.id_headers` would group traffic from any client that sends
  it.
