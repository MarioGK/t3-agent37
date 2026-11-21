# t3-agent37

Run T3 Code in named Agent37 sandboxes. Each release has one executable per platform for Linux, macOS 13+, and Windows on x86-64 and ARM64.

Linux and macOS

```sh
curl -fsSL https://raw.githubusercontent.com/MarioGK/t3-agent37/main/install.sh | sh
```

The installer enables Bash, Fish, and Zsh completion for new shells.
Tab completes commands, flags, providers, workspace templates, and sandbox names.
Use `t3-agent37 completion --help` to generate scripts for custom shell setups.

Windows PowerShell

```powershell
irm https://raw.githubusercontent.com/MarioGK/t3-agent37/main/install.ps1 | iex
```

```sh
t3-agent37 setup
t3-agent37 setup --verify
t3-agent37 up
t3-agent37 up 10 --dry-run
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
links the managed sandboxes and [configured T3 servers](docs/terrascale.md#link-t3-servers)
to each other over Tailscale. Run `sync` to retry pending links.
Bare `up` opens the desktop pairing request and waits for the new sandbox to
connect before linking it to other environments. Confirm its private address
in T3 Code within five minutes. Run `link <name>` to retry a pending pairing.
The desktop displays the same sandbox name as `t3-agent37 ls`.
Use `up --no-desktop` for headless runs.
`up 10` creates ten new sandboxes in parallel using the next free names.
Confirm each desktop pairing in order. The CLI then syncs the mesh once.
`up 10 --dry-run` previews the batch. `up 10 --no-desktop` runs it headless.
Use `up <name>` to start an existing sandbox.
The terminal progress bar tracks sandbox readiness, T3 Code confirmation,
and mesh sync for single launches and batches.
New sandboxes default to **4 vCPU and 8 GB RAM**. Use `up --ram 16` for
one launch or `up 10 --ram 4` to size every sandbox in a batch. The CLI
selects the matching CPU count. Use `config --ram 16` to save a default or
`config --show-size` to check it. Both `up` and `config` accept `--cpu` with
`--ram`. The `config` menu also has a **Sandbox size** picker.
Agent37 Default supports 2 vCPU with 4 GB, or 4 vCPU with 8, 16, or 32 GB.
[Agent37 sizing](https://www.agent37.com/docs/agents-api/instance-sizing)

Use `ls --json` for scripts. Sandbox previews and `start`, `stop`, and `delete`
results keep their JSON output when piped.

With the TerraScale template and CLIProxy selected, `up` configures Codex,
Claude Code, and OpenCode with your CLIProxy key. All three use GLM 5.3 by
default. T3 Code starts with OpenCode selected.

New sandboxes inherit the workstation's GitHub.com CLI login and Git author
defaults. Use `up --github=false` to skip it, or set
`T3_AGENT37_GITHUB_TOKEN` to use a scoped GitHub token. Run
`up <name> --github` to retry access on an existing sandbox after a GitHub warning.

On another PC, join the same tailnet, run `t3-agent37 setup`, then run
`t3-agent37 link`. It wakes existing sandboxes as needed and opens each
pairing request in T3 Code. Use `link <name>` for one sandbox or
`link --dry-run` to preview. `connect <name> --pair` prints a fresh link.

`create dev --yes` creates a sandbox with an explicit name.
`down` permanently deletes the newest managed sandbox and its disk.
Retries keep the same sandbox while its deletion or T3 cleanup is pending.
Use `down --dry-run` to preview the target.
`down <name>` stops a specific sandbox and keeps its disk.
`delete` (or `del`) opens a terminal picker. Press `/` to filter the list, then
select a sandbox and confirm deletion. For scripts, use `delete <name>`. Add
`--dry-run` to preview.
Use `delete --all --dry-run` to preview every managed sandbox, then
`delete --all` to permanently delete them with a progress bar. The command
reports each result and retries pending cleanup by sandbox ID.
`down` and `delete` unlink the sandbox from reachable environments. If a
peer is offline, deletion succeeds and reports pending cleanup. Use
`delete <name>` to select a different sandbox while bare `down` is retrying.
`sync` repairs the mesh and clears confirmed stale links. It wakes auto-sleeping
sandboxes as needed. `sync --dry-run` previews configured servers without
waking sandboxes. Remove saved desktop entries under
**Settings → Connections**.

Run `t3-agent37 config` to edit the name prefix, template, provider, and GitHub default.
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
