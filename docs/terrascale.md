# TerraScale Agent37 template

Use t3-agent37 0.2.25 or later. Run `t3-agent37 setup` and choose
`terrascale-agent37` if your workspace has access. The current Dockerfile
installs T3 Code `0.1.11-nightly.20261119.4954`.
You can also [publish your own copy](#publish-the-template).
Choose CLIProxy as the model provider, or use Automatic with the TerraScale
template. For Tailscale, define `tag:agent37` in your tailnet policy and grant your
T3 clients and other sandboxes access to that tag on TCP 443. Allow the
sandboxes to reach the configured T3 servers on their private HTTPS ports.
Setup opens **Trust credentials**.
Select **Credential → OAuth**, set **Auth Keys** to **Write**, and select
`tag:agent37`. For an existing OAuth client, enter its granted tags in the wizard.
Use commas between multiple tags. Enable MagicDNS and HTTPS certificates in the
tailnet DNS settings. [Trust credentials](https://console.tailscale.com/admin/settings/trust-credentials)
· [Tailnet DNS](https://login.tailscale.com/admin/dns)

```sh
t3-agent37 setup --verify
t3-agent37 create terrascale-dev --template terrascale-agent37 --yes
```

You can supply these variables from your secret store on the workstation.

| Variable | Value |
| --- | --- |
| `AGENT37_API_KEY` | Agent37 workspace API key |
| `T3_AGENT37_CLIPROXY_URL` | HTTPS base URL for CLIProxy |
| `T3_AGENT37_CLIPROXY_KEY` | CLIProxy client API key |
| `T3_AGENT37_TAILSCALE_CLIENT_ID`, `T3_AGENT37_TAILSCALE_CLIENT_SECRET` | OAuth client with `auth_keys` write access for `tag:agent37` |
| `T3_AGENT37_GITHUB_TOKEN` | Optional scoped GitHub.com credential for new sandboxes |

The CLI reads keys saved by setup in an owner-only `runtime.json` beside
`config.json`. Environment variables take precedence. For the Agent37 key,
the official `agent37 ssh setup` config also takes precedence over `runtime.json`.
Rerun `setup` to replace a saved OAuth credential. Leave both fields blank
to keep the saved pair.

`up` configures Codex, Claude Code, and OpenCode to use the sandbox's CLIProxy
key. Each uses GLM 5.3 in T3 and at the terminal. T3 selects OpenCode
`cliproxy/glm-5.3` for new threads. The key comes from the sandbox environment.
Set `T3_AGENT37_CLIPROXY_MODEL` before `create` to pick another default model
that CLIProxy supports across the three APIs. Run `t3-agent37 prepare terrascale-dev`
to configure a running sandbox and refresh its OpenCode model catalog.

`setup --verify` requests a Tailscale token for the configured tag set and
checks MagicDNS and HTTPS if the local Tailscale CLI is available. On an OAuth
authorization failure, it opens Trust credentials from an interactive
terminal. `--no-browser` keeps setup in the terminal. `setup --check` runs offline.
`setup --repair-tailscale` uses **Policy File Write** to define missing tags
with empty owner lists and retries OAuth verification. Existing tags and
access rules are preserved. Use `--dry-run` to validate and preview the change.
`create` and `up` verify OAuth before provisioning. The workstation keeps
the OAuth secret and sends the guest a single-use, preauthorized ephemeral key
valid for ten minutes. The CLI revokes the key on a failed join. T3 uses the
private `https://<hostname>.<tailnet>.ts.net` address.
Bare `up` opens the stock T3 Code pairing dialog on this PC. Confirm the
private address within five minutes. The CLI starts linking peers after the
confirmed client connects. A pending sandbox stays out of the mesh until you
finish pairing with `link <name>`. Use `link` to pair each managed sandbox in
turn, or `up --no-desktop` for unattended provisioning.
`connect <name> --pair` prints a link for manual pairing.

`up <count>` creates 1 to 100 new sandboxes with up to 16 provisioning at
once by default. Use `--parallel` to set a limit between 1 and 32. The batch
keeps successful sandboxes if others fail. Use `link <name>`
to retry a pending desktop pairing. The CLI starts approvals as sandboxes
become ready. T3 Code shows one approval at a time while other sandboxes keep
provisioning. One sync runs after the pairing attempts. Use `up <count> --no-desktop`
for headless provisioning. An existing numeric sandbox name can be started
with `start <name>`.

At 8 GB per sandbox, ten new sandboxes require 80 GB of running memory in
addition to the memory in use by existing sandboxes. Agent37 enforces the
[workspace memory limit](https://www.agent37.com/docs/agents-api/errors#sandbox-api-codes).

Batch preflight checks OAuth and reads the Agent37 inventory concurrently.
Each guest configures GitHub access while its private Tailscale endpoint comes
up. Both steps finish before desktop pairing. To measure scheduling with fixed
service delays and no cloud calls, run:

```sh
go test ./cmd/t3-agent37 -run '^$' -bench '^BenchmarkUpPipeline$' -benchtime=8x -count=4
```

`sleep [name]` suspends the sandbox and keeps its disk, Tailscale enrollment,
and T3 links. Omit the name to suspend the newest managed sandbox. Run
`up <name>` to wake it. `stop <name>` logs out the device and stops the sandbox
while keeping its disk. Use `up <name>` to restore peer links after a stop.

`down <name>` permanently deletes the named sandbox and its disk. Bare `down`
targets the newest managed sandbox. It tries to log out the sandbox's Tailscale
device first. If the guest is unavailable, the CLI can remove its exact tagged
device with Tailscale Devices Core Write. Without that grant, the ephemeral
device expires and the CLI reports pending cleanup.
`down` saves the target ID on the workstation before deleting it. Bare `down`
retries pending cleanup before selecting a newer sandbox. Use `down <name>` to
choose another sandbox or `down --pick` to confirm one in a menu, including after
an API key rotation. `down --all` shows progress for bulk deletion. Add
`--dry-run` to preview any form of `down`.
`down` unlinks deleted sandboxes from available T3 environments.
Cleanup warnings leave a successful sandbox deletion intact. JSON output
includes `"cleanup_pending": true` when links still need attention. Run `sync`
when an unavailable peer comes back online.
Inside the sandbox, use
`socks5h://127.0.0.1:1055` to reach tailnet services.

## Link T3 servers

To include fixed T3 servers in the mesh, save the peers in
`~/.config/t3-agent37/peers.json` or set `T3_AGENT37_PEERS_FILE`.

`create`, `up`, and `sync` establish links in both directions between
configured T3 servers and managed sandboxes in the workspace. They can wake
auto-sleeping sandboxes to link them. A sandbox stopped with `stop <name>`
returns to the mesh after `up <name>`. `sync` also removes links from available
peers to stopped or deleted sandboxes. For an ID Agent37 cannot find, `sync`
requires a local deletion record before removing the link. An ID from another
workspace can return the same 404. `sync --dry-run` checks configured servers
without using sandbox exec, which can wake a sleeping sandbox.

```json
[
  {"ssh": "dev", "url": "https://dev.example.ts.net:18456", "environmentId": "your-t3-environment-id"}
]
```

Use the environment ID from `/.well-known/t3/environment` on each peer and
an SSH alias that reaches the T3 owner. Each server must have Linked
Environments enabled. Registration runs over private Tailscale HTTPS and
adds approval-required links in both directions without opening the desktop
app. A pending peer leaves the sandbox ready to use. Run `sync` after
the peer recovers to add the missing links and reuse the working ones.

For a peer running inside Incus, an optional `incus` object directs the
owner command through an SSH host with Incus access. Set `ssh`, `instance`,
`user`, `uid`, and `gid` in the object. Use the T3 owner's account in the
guest, with its home at `/home/<user>`. The peer's top-level `url` still
points to its private Tailscale HTTPS address.

Set `T3_AGENT37_PEERS_FILE` to an empty string for a sandbox-only mesh.

## GitHub access

Bare `up` and `create` use the invoking workstation's active GitHub.com
CLI login. A sandbox can use the same GitHub permissions and the workstation's
global Git author name and email. GitHub SSH-style remotes use authenticated
HTTPS inside the sandbox. The login stays in the guest's private GitHub
CLI store until the sandbox disk is deleted. Use a repository-scoped
`T3_AGENT37_GITHUB_TOKEN` to limit the permissions.

Set `github_access` to `false` in `config` or use `--github=false` for a
sandbox that should launch without this handoff. Use `up <name> --github`
to configure a sandbox that already exists.

## Publish the template

The image includes T3, OpenCode, Codex, Claude Code, Tailscale, .NET 10 and 11, Go 1.27.4,
Rust 1.99.1 with rustfmt and Clippy, Python 3.12 with uv, and Node 24 with
mise. The Node pins are 24.21.0 with npm 11.18.0 and 24.22.1. The image also
includes Git, GitHub CLI, GCC, Make, Ruby, ShellCheck, actionlint, the TerraScale `ts`
CLI, PostgreSQL 16 client, NSS tools, and Playwright with Chromium. Set
`PLAYWRIGHT_BROWSERS_PATH=/opt/terrascale/playwright` to use the bundled browser.
For older sandboxes, the CLI can install missing `gh` and Claude Code binaries
from checksum-pinned downloads.

To publish your own copy, run this from the checkout with Agent37 CLI 0.4.1
or later. Use the published revision in `--template`.

```sh
agent37 templates build templates/terrascale --name terrascale-agent37 --default-port 3737
```

The Agent37 gateway uses port 3737 and T3 uses protected port 3773.
The image caches Git objects from the seven public TerraScale repositories
without credentials. Each sandbox still fetches current refs from GitHub.
Its checkouts remain usable independently of the image cache. Private
repositories are cloned at runtime using the workstation's GitHub handoff.
[Dockerfile](../templates/terrascale/Dockerfile)

The image includes a `ts-orch` built from the cached public
`ts-orchestrator` revision. The CLI reuses it when its build revision
matches the current checkout and builds it again when the revision changes.
