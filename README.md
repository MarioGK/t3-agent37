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

Set `AGENT37_API_KEY` or use the key saved by `agent37 ssh setup` in
`~/.config/agent37/config.json`. Run `up` to create a sandbox with the next
numbered name and open T3 pairing.
T3 Code asks for confirmation the first time you add a sandbox.

```sh
t3-agent37 up
t3-agent37 ls
t3-agent37 down agent-1
t3-agent37 up agent-1
t3-agent37 connect agent-1
```

`create dev --yes` creates a sandbox with an explicit name.
`down` stops compute and keeps the disk. Add `--delete --yes` to erase it. `connect` reopens the local relay.

Set `name_prefix` in `~/.config/t3-agent37/config.json` (Windows uses
`%APPDATA%\t3-agent37\config.json`). The prefix accepts 1 to 33 lowercase
letters, digits, or hyphens and must start and end with a letter or digit.

```json
{"name_prefix": "terra"}
```

Each prefix has its own counter on this workstation. A failed create can leave
a gap. `up --dry-run` previews the next local name without reserving it.

[TerraScale template, CLIProxy, and Tailscale](docs/terrascale.md) · [Download the latest executable](https://github.com/MarioGK/t3-agent37/releases/latest)
