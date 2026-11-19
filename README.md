# t3-agent37

Run T3 Code in named Agent37 sandboxes.

```sh
curl -fsSL https://raw.githubusercontent.com/MarioGK/t3-agent37/main/install.sh | sh
```

Load `AGENT37_API_KEY` and confirm pairing in T3 after `create`.

```sh
t3-agent37 create dev --yes
t3-agent37 down dev
t3-agent37 up dev
t3-agent37 connect dev
```

`down` stops compute and keeps the sandbox disk. `connect` reopens the local relay.

[TerraScale template, CLIProxy, and Tailscale](docs/terrascale.md) · [Releases](https://github.com/MarioGK/t3-agent37/releases)
