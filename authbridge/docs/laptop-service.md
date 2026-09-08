# Running Cortex: start, stop, and remove it

Cortex runs as a service, so there is nothing to launch by hand and nothing to keep
in a terminal. Everything below is `abctl service`; run it with no action to see the
list.

```sh
abctl service status      # is it running, and is it answering?
abctl service start       # start it
abctl service stop        # stop it, and keep it stopped
abctl service restart     # stop and start
abctl service install     # set it up in the first place (the installer does this)
abctl service uninstall   # stop it and remove the service
```

**Never use `kill` or `pkill` to stop it.** The proxy is supervised, so killing it gets
it restarted within a couple of seconds, which looks like a process refusing to die.
`abctl service stop` is the stop that works.

That restart is the point, though, and it is worth seeing once:

```sh
kill -9 $(pgrep -f 'authbridge-proxy --config')   # comes back within ~2s
abctl service status                              # healthy again
```

On macOS you will see **two** `authbridge-proxy` processes: a supervisor (the one
launchd starts, holding no ports) and the proxy itself. launchd does not restart user
agents added mid-session — verified across `KeepAlive`, `StartInterval` and
`RunAtLoad` — so the supervisor is what makes crash recovery work. On Linux there is
one process; systemd handles it.

## Re-running the installer is safe

The one-liner is how you upgrade, so it is meant to be run repeatedly. When nothing has
changed it changes nothing: it does not re-download binaries already at that version, and
`service install` reports `Already current` and leaves the running proxy alone rather
than restarting it.

That last part matters — a restart cuts every attached Claude Code session, because
`HTTPS_PROXY` is fixed in each session's environment at startup and cannot fall back to a
direct connection. When a restart genuinely is needed, install says how many connections
it is about to cut.

To restart deliberately: `abctl service restart`.

## `abctl: command not found`

The installer puts both binaries in `~/.local/bin`. If that is not on your PATH it
offers to add it to your shell profile; new terminals pick it up, and for the one you
are in:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

To undo, delete the two lines the installer marked in your profile.

## Restricted environments (sandboxes, no launchd session)

Some environments cannot manage services at all — a sandboxed shell, a session without
a usable launchd domain, CI. `abctl service install` detects this before writing
anything and tells you so, rather than failing at `launchctl bootstrap` with
`Input/output error`.

Cortex still runs there; it just is not supervised:

```sh
authbridge-proxy --local     # in its own terminal, or backgrounded
abctl                        # the viewer, as usual
```

What you give up: no restart after a crash, and nothing brings it back at login. Stop
it with `kill $(pgrep -f authbridge-proxy)` — there is no service to stop.

Two assumptions that do not hold in such environments, and what happens:

| Assumption | If it does not hold |
|---|---|
| `launchctl` can manage the user domain | `service install` stops early and prints the command above |
| `$HOME` is your login home | launchd never scans `$HOME/Library/LaunchAgents`, so the service cannot start at login. `service install` warns and continues; crash recovery still works while you are logged in |

## Is it working?

```sh
abctl service status
```

`installed:` names the unit file, `healthy:` names the endpoint that answered. If it
says `NOT answering`, the proxy is loaded but not serving — check `~/.cortex/proxy.log`.

To see traffic rather than status, run `abctl` with no arguments.

## Start and stop

```sh
abctl service start
abctl service stop
```

A stop persists: Cortex stays down across logouts and reboots until you start it
again. That is deliberate — a stop that quietly undoes itself at your next login is
worse than none.

`stop` also reports how many connections it cut, because a Claude Code session that is
already running cannot recover on its own: `HTTPS_PROXY` is fixed in its environment
when it starts, so it has no way to fall back to a direct connection. Restart any
session that begins failing to connect.

## Three ways to turn it off

They are different, so pick deliberately:

### Pause it

```sh
abctl service stop
```

Claude Code fails while Cortex is stopped, because its settings still point at the
proxy. Either start Cortex again or unwire Claude Code (below).

**A Claude Code session that is already running cannot recover on its own.**
`HTTPS_PROXY` is fixed in its environment when it starts, so it has no way to fall
back to a direct connection, and `claude-code disable` cannot reach it. Restart any
session that starts failing to connect. `service stop` tells you how many
connections it cut, for exactly this reason.

Use `abctl service stop`, not `kill` or `pkill` — the supervisor restarts the
process within seconds, which looks like it refusing to die.

### Unwire Claude Code

```sh
abctl claude-code disable
```

This removes only the keys Cortex added to `~/.claude/settings.json`
(`HTTPS_PROXY`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, and the CA variables
`NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` / `GIT_SSL_CAINFO` / `REQUESTS_CA_BUNDLE` /
`CURL_CA_BUNDLE`) and leaves anything else in that file alone. Claude Code goes
straight to the API again. Restart `claude` to pick it up.

There are several CA variables because anything Claude Code spawns inherits
`HTTPS_PROXY` and so must also be able to verify the bridge. They do not all get
the same file: `NODE_EXTRA_CA_CERTS` **extends** Node's trust store, so it gets
`ca.crt`, while the rest **replace** the trust store and get `bundle.crt` — the CA
followed by this machine's public roots. Pointing a replacing variable at `ca.crt`
would leave that tool trusting one private CA and nothing else, which breaks every
direct TLS call it makes.

Cortex keeps running; nothing sends traffic to it. `abctl claude-code enable` puts it
back.

### Remove it

```sh
abctl claude-code disable     # 1. unwire Claude Code
abctl service uninstall       # 2. stop it and remove the service
rm -rf ~/.cortex              # 3. config, CA, logs
rm -f ~/.local/bin/abctl ~/.local/bin/authbridge-proxy
```

Order matters for the first two: `claude-code disable` needs to read the config that
step 3 deletes.

#### Check nothing is left

```sh
abctl claude-code status                    # should say "not enabled"
pgrep -fl authbridge-prox                   # should print nothing
ls ~/.cortex 2>/dev/null                    # should print nothing
```

The CA that step 3 removes was only ever trusted through the CA variables in
`~/.claude/settings.json` — Cortex never adds it to the system or login keychain, so
there is nothing to clean up there. `bundle.crt` lives in the same directory and is
derived from `ca.crt` plus a copy of the public roots, so removing `~/.cortex` takes
it with them; it holds no private key and grants nothing on its own.

#### If `abctl` is already gone

The service can be removed by hand:

```sh
# macOS
launchctl bootout "gui/$(id -u)/io.rossoctl.cortex"
rm -f ~/Library/LaunchAgents/io.rossoctl.cortex.plist

# Linux
systemctl --user disable --now cortex.service
rm -f ~/.config/systemd/user/cortex.service
```

Then delete the three Cortex keys from the `env` block of
`~/.claude/settings.json` yourself.
