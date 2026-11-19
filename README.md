# t3-agent37

Run T3 Code in named Agent37 sandboxes. Each release has one executable per platform for Linux, macOS, and Windows on x86-64 and ARM64.

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
t3-agent37 setup --check
t3-agent37 up
t3-agent37 ls
t3-agent37 down
```

`setup` walks through the Agent37 API key, a Tailscale-ready template, a scoped
Tailscale OAuth client, and any CLIProxy settings. It reuses existing credentials
and saves new ones to an owner-only `runtime.json` beside `config.json`.
`setup --check` checks local settings without prompts or API calls.

`up` creates a sandbox with the next numbered name and serves T3 at its private
Tailscale HTTPS DNS name. For a separate desktop connection,
`connect <name> --pair` prints a link that T3 Code asks you to confirm.

`create dev --yes` creates a sandbox with an explicit name.
`down` permanently deletes the newest managed sandbox and its disk.
`down --dry-run` shows which sandbox it would delete.
`down <name>` stops a specific sandbox and keeps its disk.
`delete` opens a terminal picker. Use the arrow keys to select a sandbox and
confirm before deleting its disk. For scripts, use `delete <name>`. Add
`--dry-run` to preview. `connect` prints the verified Tailscale address.

Run `t3-agent37 config` to edit the name prefix, default template, and provider.
The editor works offline. Provisioning flags override
the saved defaults. Settings are stored in `~/.config/t3-agent37/config.json` (Windows uses
`%APPDATA%\t3-agent37\config.json`). The prefix accepts 1 to 33 lowercase
letters, digits, or hyphens and must start and end with a letter or digit.

```json
{"name_prefix": "terra"}
```

Each prefix has its own counter on this workstation. A failed create can leave
a gap. `up --dry-run` previews the next local name without reserving it.

[TerraScale template, CLIProxy, and Tailscale](docs/terrascale.md) · [Download the latest executable](https://github.com/MarioGK/t3-agent37/releases/latest)
