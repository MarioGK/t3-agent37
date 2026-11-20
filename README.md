# t3-agent37

Run T3 Code in named Agent37 sandboxes. Each release has one executable per platform for Linux, macOS 13+, and Windows on x86-64 and ARM64.

Linux and macOS

```sh
curl -fsSL https://raw.githubusercontent.com/MarioGK/t3-agent37/main/install.sh | sh
```

Windows PowerShell

```powershell
irm https://raw.githubusercontent.com/MarioGK/t3-agent37/main/install.ps1 | iex
```

```sh
t3-agent37 setup
t3-agent37 setup --verify
t3-agent37 up
t3-agent37 ls
t3-agent37 down
```

`setup` opens Tailscale Trust credentials and walks through the Agent37 API key,
a Tailscale-ready template, OAuth tags, and CLIProxy. It reuses saved credentials
and saves new ones to an owner-only `runtime.json` beside `config.json`.
`setup --check` checks local settings without prompts or API calls.
`setup --verify` also checks the OAuth grant, MagicDNS, and HTTPS. Use
`--no-browser` to keep setup in the terminal.
With a policy-writing OAuth client, `setup --repair-tailscale` defines missing
enrollment tags and verifies access. Add `--dry-run` to preview.

`up` creates a sandbox with the lowest available number and serves T3 at its private
Tailscale HTTPS DNS name. It verifies Tailscale credentials before provisioning
and mints an ephemeral key when the sandbox needs to join. It automatically
registers with [configured T3 servers](docs/terrascale.md#link-t3-servers)
without prompts. Run `up <name>` to retry pending links.
On a workstation running T3 Code, bare `up` also opens the desktop pairing
request for the new sandbox. Confirm its private address in T3 Code.
The desktop displays the same sandbox name as `t3-agent37 ls`.
Use `up --no-desktop` for headless runs.

On another PC, join the same tailnet, run `t3-agent37 setup`, then run
`t3-agent37 link`. It wakes existing sandboxes as needed and opens each
pairing request in T3 Code. Use `link <name>` for one sandbox or
`link --dry-run` to preview. `connect <name> --pair` prints a fresh link.

`create dev --yes` creates a sandbox with an explicit name.
`down` permanently deletes the newest managed sandbox and its disk.
`down --dry-run` shows which sandbox it would delete.
`down <name>` stops a specific sandbox and keeps its disk.
`delete` opens a terminal picker. Use the arrow keys to select a sandbox and
confirm before deleting its disk. For scripts, use `delete <name>`. Add
`--dry-run` to preview.
`down` and `delete` unlink the sandbox from configured T3 servers.
`sync` clears stale links to stopped or deleted sandboxes. Use `sync --dry-run`
to see which links would be removed. Remove saved desktop entries under
**Settings → Connections**.

Run `t3-agent37 config` to edit the name prefix, default template, and provider.
The editor works offline. Provisioning flags override
the saved defaults. Settings are stored in `~/.config/t3-agent37/config.json` (Windows uses
`%APPDATA%\t3-agent37\config.json`). The prefix accepts 1 to 33 lowercase
letters, digits, or hyphens and must start and end with a letter or digit.

```json
{"name_prefix": "terra"}
```

Numbering starts at 1 for each prefix and reuses gaps after deletion.
If `terra-1`, `terra-2`, and `terra-4` exist, the next sandbox is `terra-3`.
Stopped sandboxes keep their names. `up --dry-run` checks the workspace when
an Agent37 key is configured and previews the name without reserving it.

[TerraScale template, CLIProxy, and Tailscale](docs/terrascale.md) · [Download the latest executable](https://github.com/MarioGK/t3-agent37/releases/latest)

Build from source with Go 1.27.4 or newer. The repository includes the [JetBrains Modern Go skill](.agents/skills/use-modern-go/SKILL.md).
