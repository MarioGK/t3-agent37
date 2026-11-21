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
t3-agent37 sleep
t3-agent37 down --dry-run
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
in T3 Code within five minutes. If T3 accepts the pairing but fails to connect,
the CLI opens one fresh confirmation request. Run `link <name>` to retry a
pairing that still needs attention.
The desktop displays the same sandbox name as `t3-agent37 ls`.
Use `up --no-desktop` for headless runs.
`up 10` creates ten new sandboxes in parallel using the next free names.
Larger batches use up to 16 workers by default. Set `--parallel` to choose
between 1 and 32, for example `up 24 --parallel 24`.
T3 Code prompts for each sandbox as it becomes ready while the others keep
provisioning. The CLI checks for confirmation with a 100 ms delay, waits
longer if Agent37 rate-limits the checks, and syncs the mesh once at the end.
It prepares the pairing code while HTTPS becomes ready and opens it after
the sandbox and its projects pass verification.
`up 10 --dry-run` previews the batch. `up 10 --no-desktop` runs it headless.
Use `up <name>` to start an existing sandbox.
The terminal progress bar starts with the launch checks and tracks sandbox
readiness, T3 Code approvals, and mesh sync for single launches and batches.
It reaches 100% when all required steps finish successfully.
New sandboxes default to **4 vCPU and 8 GB RAM**. Use `up --ram 16` for
one launch or `up 10 --ram 4` to size every sandbox in a batch. The CLI
selects the matching CPU count. Use `config --ram 16` to save a default or
`config --show-size` to check it. Both `up` and `config` accept `--cpu` with
`--ram`. The `config` menu also has a **Sandbox size** picker.
Agent37 Default supports 2 vCPU with 4 GB, or 4 vCPU with 8, 16, or 32 GB.
[Agent37 sizing](https://www.agent37.com/docs/agents-api/instance-sizing)

Use `ls --json` for scripts. Sandbox previews and `start`, `stop`, `sleep`, and `down`
results keep their JSON output when piped.

With the TerraScale template and CLIProxy selected, `up` configures Codex,
Claude Code, and OpenCode with your CLIProxy key. All three use GLM 5.3 by
default. T3 Code starts with OpenCode selected.

New sandboxes inherit the workstation's GitHub.com CLI login and Git author
defaults. Use `up --github=false` to skip it, or set
`T3_AGENT37_GITHUB_TOKEN` to use a scoped GitHub token. Run
`up <name> --github` to retry access on an existing sandbox after a GitHub warning.

To clone repositories and register them in T3 Code on launch, put
`workspace.json` beside `config.json` or set `T3_AGENT37_WORKSPACE_FILE`
to its path.

```json
{
  "path": "~/Projects/Example",
  "repositories": [
    {"repo": "example/web"},
    {"repo": "example/api", "ref": "main", "directory": "backend"}
  ]
}
```

An optional `root_repository` checks out a repository at `path` itself.
The CLI clones up to four child repositories at a time while the sandbox joins
Tailscale. It verifies their T3 projects before requesting desktop pairing.
When an existing workspace starts, the CLI fast-forwards its tracked GitHub
branches before checking the projects. It preserves local branches, pinned
tags, and conflicting files.
Use `up --workspace=false` to skip the playbook for a launch. Set
`T3_AGENT37_TIMINGS=1` to print the duration of each stage.

With `TerraScale/workspace` as `root_repository`, `up` also reads its
committed `repos.txt`, installs the workspace's skills and helper links,
checks the .NET and Mise Go toolchains, and builds `ts-orch` from the cloned
revision. T3 pairing waits for the tools and projects to be ready.

On another PC, join the same tailnet, run `t3-agent37 setup`, then run
`t3-agent37 link`. It wakes existing sandboxes as needed and opens each
pairing request in T3 Code. Use `link <name>` for one sandbox or
`link --dry-run` to preview. `connect <name> --pair` prints a fresh link.

`create dev --yes` creates a sandbox with an explicit name.
`sleep [name]` suspends the sandbox and keeps its disk and T3 links. Omit the
name for the newest managed sandbox. Run `up <name>` to wake it.
`stop <name>` shuts it down and keeps its disk.

**`down <name>` permanently deletes the named sandbox and its disk.** Bare
`down` deletes the newest managed sandbox. `down --pick` opens a picker with
confirmation, and `down --all` deletes all managed sandboxes with a progress
bar. Add `--dry-run` to any form to preview the targets.
`down` removes T3 links on reachable peers and reports pending cleanup for
offline peers. Retries keep the saved sandbox ID while cleanup is pending.
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
