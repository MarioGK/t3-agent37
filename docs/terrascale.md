# TerraScale Agent37 template

With `AGENT37_API_KEY` set, create a sandbox from the private template.

```sh
t3-agent37 create terrascale-dev --template terrascale-agent37@3 --yes
```

Supply these variables from your secret store on the workstation.

| Variable | Value |
| --- | --- |
| `T3_AGENT37_CLIPROXY_URL` | HTTPS base URL for CLIProxy |
| `T3_AGENT37_CLIPROXY_KEY` | CLIProxy client API key |
| `T3_AGENT37_TAILSCALE_CLIENT_ID`, `T3_AGENT37_TAILSCALE_CLIENT_SECRET` | OAuth client with `auth_keys` write access for `tag:agent37` |

The CLI also reads these keys from an owner-only `runtime.json` beside
`config.json`. Environment variables take precedence.

T3 loads the CLIProxy text model catalog and defaults to `cliproxy/glm-5.3`.
Set `T3_AGENT37_CLIPROXY_MODEL` before `create` to pick another model ID.
Run `t3-agent37 prepare terrascale-dev` to refresh the catalog.

`create` and `up` enroll the sandbox with a single-use ephemeral Tailscale key
and serve T3 at its private `https://<hostname>.<tailnet>.ts.net` address.
`connect <name> --pair` prints a pairing link for that address.
`down` logs out the device before deleting the newest managed sandbox.
`down <name>` logs out the device and stops that sandbox. Inside the sandbox, use
`socks5h://127.0.0.1:1055` to reach tailnet services.
Run `t3-agent37 config` to set a Tailscale-ready default template.
Enrollment credentials are required before provisioning starts.

```json
{"name_prefix": "terra", "template": "terrascale-agent37@3", "tailscale": true}
```

## Link T3 servers

To link a Tailscale-enabled sandbox on `create` and `up`, save the peers in
`~/.config/t3-agent37/peers.json` or set `T3_AGENT37_PEERS_FILE`.

```json
[
  {"ssh": "dev", "url": "https://dev.example.ts.net:18456", "environmentId": "your-t3-environment-id"}
]
```

Use the environment ID from `/.well-known/t3/environment` on each peer and
an SSH alias that reaches the T3 owner. Each server must have Linked
Environments enabled. The sandbox joins with private Tailscale HTTPS Serve.
The CLI adds both directions with approval-required access and reuses
reachable links without opening the desktop app. Set `T3_AGENT37_PEERS_FILE`
to an empty string to skip sync.

The image includes T3, OpenCode, Codex, Tailscale, .NET 10 and 11, Go 1.27.4,
Rust 1.99.1 with rustfmt and Clippy, Python 3.12 with uv, and Node 24 with
mise. The Node pins are 24.21.0 with npm 11.18.0 and 24.22.1. The image also
includes Git, GCC, Make, Ruby, ShellCheck, actionlint, the TerraScale `ts`
CLI, PostgreSQL 16 client, NSS tools, and Playwright with Chromium. Set
`PLAYWRIGHT_BROWSERS_PATH=/opt/terrascale/playwright` to use the bundled browser.

To publish your own copy, run this from the checkout with Agent37 CLI 0.4.1
or later. Use the published revision in `--template`.

```sh
agent37 templates build templates/terrascale --name terrascale-agent37 --default-port 3737
```

The Agent37 gateway uses port 3737 and T3 uses protected port 3773.
Give each sandbox access to its repositories at runtime. [Dockerfile](../templates/terrascale/Dockerfile)
