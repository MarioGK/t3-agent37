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

Set `AGENT37_API_KEY`, then confirm pairing in T3 when creating a sandbox.
`t3-agent37 create --yes` prints the next name, such as `agent-1` or `agent-2`.

```sh
t3-agent37 create dev --yes
t3-agent37 down dev
t3-agent37 up dev
t3-agent37 connect dev
```

`down` stops compute and keeps the disk. Add `--delete --yes` to erase it. `connect` reopens the local relay.

Set `name_prefix` in `~/.config/t3-agent37/config.json` (Windows uses
`%APPDATA%\t3-agent37\config.json`). The prefix accepts 1 to 33 lowercase
letters, digits, or hyphens and must start and end with a letter or digit.

```json
{"name_prefix": "terra"}
```

Each prefix has its own counter on this workstation. A failed create can leave
a gap. `create --dry-run` previews the next local name without reserving it.

[TerraScale template, CLIProxy, and Tailscale](docs/terrascale.md) · [Download the latest executable](https://github.com/MarioGK/t3-agent37/releases/latest)
