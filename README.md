# t3-agent37

Create an Agent37 cloud machine, set up T3 Code on it, and add it to the
T3 desktop app. The cloud machine keeps running when you close the local
relay. Reopen the relay to continue the same threads.

## Install

Requires Python 3.11 or later, the T3 desktop app, and a funded Agent37
workspace. This release is tested on Linux. The pairing launcher also
supports macOS.

Download the wheel from the private GitHub release and install it with uv:

```bash
gh release download v0.1.1 --repo MarioGK/t3-agent37 --pattern '*.whl'
uv tool install ./t3_agent37-0.1.1-py3-none-any.whl
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

The default Agent37 template includes Codex. Open the temporary terminal
URL below, run `codex login --device-auth`, and approve sign-in yourself.
Then add the cloud checkout in T3 and start a thread with Codex.

```bash
t3-agent37 terminal terrascale-pilot
```

For a different agent, pass `--provider claude` or `--provider opencode`.
The CLI selects the matching Agent37 image and checks the provider on the
instance before pairing. Sign in using that provider's instructions.

## TerraScale template

The [TerraScale Dockerfile](templates/terrascale/Dockerfile) includes the
pinned T3 binary, Codex, Node 24, Go 1.27, the .NET 10 and 11 SDKs, Git, and
Python. A new sandbox can skip the T3 download. The template uses Agent37's
Codex base and persistent home directly.

Publish it from this checkout with Agent37 CLI 0.4.1 or later and the API
key for the TerraScale workspace in the child process environment:

```bash
agent37 templates build templates/terrascale --name terrascale-agent37 --default-port 3773
t3-agent37 create terrascale-pilot --template terrascale-agent37@1 --yes
```

Replace `@1` with the revision returned by the build. Register only the
template context. The template contains no TerraScale checkouts or login
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
`t3-agent37 down terrascale-pilot`. The Agent37 disk continues to accrue
storage charges. Review active threads before stopping the instance.

Version 0.1.1 uses explicit start and stop. Wake-on-send and cached
threads during sleep are follow-up milestones. Native T3 pairing remains
in place, and the relay uses only loopback and Agent37's protected HTTPS
and WebSocket endpoint.

[T3 remote access](https://github.com/pingdotgg/t3code/blob/main/docs/user/remote-access.md)
[Agent37 instance URLs](https://www.agent37.com/docs/agents-api/urls)
[Agent37 billing](https://www.agent37.com/docs/agents-api/billing)
