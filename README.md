# t3-agent37

Create an Agent37 cloud machine, set up T3 Code on it, and add it to the
T3 desktop app. The cloud machine keeps running when you close the local
relay. Reopen the relay to continue the same threads.

## Install

Requires Python 3.11 or later, the T3 desktop app, and a funded Agent37
workspace. This release is tested on Linux. The pairing launcher also
supports macOS.

Download the wheel from the GitHub release and install it with uv:

```bash
gh release download v0.1.5 --repo MarioGK/t3-agent37 --pattern '*.whl'
uv tool install ./t3_agent37-0.1.5-py3-none-any.whl
t3-agent37 --version
```

## Create an environment

Load `AGENT37_API_KEY` from your secret store into the CLI process. The
key stays on your computer. Creating an instance can incur compute and
storage charges. Start with one environment.

```bash
t3-agent37 create terrascale-pilot --dry-run
t3-agent37 create terrascale-pilot --yes
```

The command creates or reuses a sandbox with the same name, installs the
checksum-pinned T3 runtime in the sandbox, registers a reboot hook, and
starts the local relay. Confirm the new environment in T3. If the app
doesn't open, paste the printed pairing URL in **Settings → Environments →
Add environment**. Keep the relay running while using T3 and protect the
one-time pairing link.

New sandboxes use Agent37's Default instance type with 4 vCPU and 8 GB
of RAM. Agent37 puts the sandbox to sleep after 15 minutes without
instance traffic. The CLI sets up OpenCode with the Agent37-managed
`agent37/default` model and selects it for new T3 threads. Each new sandbox
gets $1 in one-time model headroom from the existing workspace wallet.
Add the cloud checkout in T3 and start a thread.

To configure the same default on an existing sandbox, run
`t3-agent37 prepare terrascale-pilot`. The command leaves the sandbox running
for you to connect. It grants the initial model headroom once when migrating
from a different provider.

Codex and Claude Code are available through `--provider codex` and
`--provider claude`. They require their own account sign-in. Use
`t3-agent37 terminal terrascale-pilot` for the temporary terminal URL.
For Codex, complete `codex login --device-auth` in that terminal.

## TerraScale template

The [TerraScale Dockerfile](templates/terrascale/Dockerfile) bundles T3,
OpenCode, Codex, and Tailscale. Revision `@3` and later select the CLIProxy
profile and enroll in Tailscale when you create the sandbox with this CLI.
T3 loads text models from the authenticated CLIProxy catalog. It selects
`cliproxy/glm-5.3` for a fresh workspace. Set `T3_AGENT37_CLIPROXY_MODEL` to
a different catalog ID before creating it. Run `t3-agent37 prepare <name>`
to refresh the catalog on an existing CLIProxy sandbox.

Load these variables into the workstation CLI process from your secret store.

| Variable | Value |
| --- | --- |
| `T3_AGENT37_CLIPROXY_URL` | HTTPS base URL of your CLIProxy instance |
| `T3_AGENT37_CLIPROXY_KEY` | A dedicated API key accepted by CLIProxy |
| `T3_AGENT37_TAILSCALE_CLIENT_ID`, `T3_AGENT37_TAILSCALE_CLIENT_SECRET` | Tailscale OAuth client with `auth_keys` write access for `tag:agent37` |

The OAuth client stays on the workstation. The CLI sends a single-use,
ephemeral enrollment key to each sandbox and gives each sandbox a unique
Tailscale hostname. The userspace daemon joins without advertising routes
or accepting DNS changes, inbound tailnet traffic, or Tailscale SSH. T3
continues to use its protected Agent37 relay. Inside the sandbox, use
`socks5h://127.0.0.1:1055` for outbound connections to tailnet services.
Use `--no-tailscale` to create a CLIProxy sandbox without tailnet enrollment.

The image includes .NET 10 and 11, Go 1.27.4, Rust 1.99.1 with rustfmt and
Clippy, Python 3.12 with uv, and Node 24 with mise. The TerraScale JS SDK's
Node 24.21.0 and npm 11.18.0 and the docs site's Node 24.22.1 are already
installed in mise. Agent shells can run `dotnet`, `go`, `cargo`, and `python3.12`
on the standard `PATH`.

Build and test tools include Git, GCC, Make, Ruby, ShellCheck, actionlint, the
TerraScale `ts` CLI, PostgreSQL 16 client, NSS `certutil`, and the Linux tools
used by the local test harness. Playwright and Chromium are preinstalled.
Set `PLAYWRIGHT_BROWSERS_PATH=/opt/terrascale/playwright` for standalone
Playwright commands that use the bundled browser.

Publish it from this checkout with Agent37 CLI 0.4.1 or later and the API
key for the TerraScale workspace in the child process environment:

```bash
agent37 templates build templates/terrascale --name terrascale-agent37 --default-port 3737
t3-agent37 create terrascale-pilot --template terrascale-agent37@3 --yes
```

Use the revision returned by the build when publishing your own copy. Agent37's Codex
gateway uses port 3737. The CLI starts T3 on the separate protected port
3773. Register only the template context. The template contains no TerraScale checkouts or login
credentials. Give each instance access to its own repositories at runtime.

## Resume or stop

```bash
t3-agent37 list
t3-agent37 up terrascale-pilot
t3-agent37 connect terrascale-pilot
```

Each named sandbox has a persistent local port, so you can create and use
several simultaneously. Run one relay per environment. Use `--port` on the
first connection if you need to choose its address and `connect --pair` for
a fresh pairing link.
Stop the relay with Ctrl+C. To pause compute after work has finished, run
`t3-agent37 down terrascale-pilot`. For Tailscale-enabled sandboxes, `down`
logs out the ephemeral device before stopping compute, and `up` enrolls
again. The Agent37 disk continues to accrue storage charges. Review active
threads before stopping the instance.

Traffic routed through Agent37 wakes a sleeping sandbox. T3 background
traffic can keep it awake. The local relay uses loopback and connects
to Agent37's protected HTTPS and WebSocket endpoint.

[T3 remote access](https://github.com/pingdotgg/t3code/blob/main/docs/user/remote-access.md)
[Agent37 instance URLs](https://www.agent37.com/docs/agents-api/urls)
[Agent37 billing](https://www.agent37.com/docs/agents-api/billing)
