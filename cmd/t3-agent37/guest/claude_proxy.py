"""Configure Claude Code to use the sandbox's CLIProxy connection."""

import hashlib
import os
from pathlib import Path
import platform
import shlex
import shutil
import tarfile
import tempfile
from urllib.parse import urlsplit
from urllib.request import urlopen


CLAUDE_VERSION = "2.1.329"
CLAUDE_MODEL = "glm-5.3"
CLAUDE_ARCHIVES = {
    "x86_64": (
        "x64", "fa7e25bb5b9b600a74d98d17fcc0149f1c3dc98e921c767bec5f88a16f95d30568"
        "16e888f68a5d1fa8287e37f5e205c908460c4cda64b3b1a7d62f1765328058",
    ),
    "aarch64": (
        "arm64", "2221b9ba6153a120955aa075944b31c702312a0c4f5f26770d153dd79bfc2780"
        "73e50dc9a6c94cd1ec359b9c9a4fe9455c13ef6319ac1208b45c8ebc5c7343d0",
    ),
}


def install_claude(root):
    prefix = root / "providers" / f"claude-{CLAUDE_VERSION}"
    binary = prefix / "claude"
    if binary.is_file():
        return binary

    target = CLAUDE_ARCHIVES.get(platform.machine())
    if target is None:
        raise RuntimeError("No Claude Code fallback is available for this platform")
    arch, checksum = target
    package = f"claude-code-linux-{arch}"
    url = f"https://registry.npmjs.org/@anthropic-ai/{package}/-/{package}-{CLAUDE_VERSION}.tgz"
    prefix.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".claude-", dir=prefix) as directory:
        staged = Path(directory)
        archive = staged / "claude.tgz"
        digest = hashlib.sha512()
        with urlopen(url, timeout=90) as response, archive.open("wb") as output:
            while chunk := response.read(1024 * 1024):
                digest.update(chunk)
                output.write(chunk)
                if output.tell() > 512 * 1024 * 1024:
                    raise RuntimeError("Claude Code release archive exceeds the size limit")
        if digest.hexdigest() != checksum:
            raise RuntimeError("Claude Code release checksum mismatch")
        with tarfile.open(archive) as release:
            member = release.getmember("package/claude")
            if not member.isfile():
                raise RuntimeError("Unexpected Claude Code release archive layout")
            executable = staged / "claude"
            with release.extractfile(member) as source, executable.open("wb") as output:
                shutil.copyfileobj(source, output)
            executable.chmod(0o700)
            executable.replace(binary)
    return binary


def claude_base_url(base_url):
    base_url = base_url.rstrip("/").removesuffix("/v1")
    url = urlsplit(base_url)
    if (url.scheme != "https" or not url.hostname or url.username is not None
            or url.password is not None or url.query or url.fragment
            or any(ord(character) < 33 for character in base_url)):
        raise RuntimeError("Claude Code needs an HTTPS CLIProxy base URL")
    return base_url


def configure_claude(root, base_url, model=CLAUDE_MODEL):
    base_url = claude_base_url(base_url)
    wrapper = root / "bin/claude"
    launcher = wrapper.resolve()
    binary = next((candidate for directory in os.get_exec_path()
                   if (candidate := Path(directory) / "claude").is_file()
                   and os.access(candidate, os.X_OK) and candidate.resolve() != launcher), None)
    if binary is None:
        binary = install_claude(root)

    script = f"""#!/bin/sh
set -eu
unset ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_CUSTOM_HEADERS
unset CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_BASE_URL={shlex.quote(base_url)}
export ANTHROPIC_AUTH_TOKEN="${{T3_AGENT37_CLIPROXY_KEY:?CLIProxy client key is missing}}"
export ANTHROPIC_MODEL={shlex.quote(model)}
export ANTHROPIC_DEFAULT_OPUS_MODEL="$ANTHROPIC_MODEL"
export ANTHROPIC_DEFAULT_SONNET_MODEL="$ANTHROPIC_MODEL"
export ANTHROPIC_DEFAULT_HAIKU_MODEL="$ANTHROPIC_MODEL"
export CLAUDE_CODE_SUBAGENT_MODEL="$ANTHROPIC_MODEL"
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
exec {shlex.quote(str(binary.absolute()))} "$@"
"""
    wrapper.parent.mkdir(parents=True, exist_ok=True)
    if not wrapper.is_file() or wrapper.read_text() != script:
        with tempfile.NamedTemporaryFile(mode="w", dir=wrapper.parent, delete=False) as output:
            temporary = Path(output.name)
            try:
                output.write(script)
                output.flush()
                temporary.chmod(0o700)
                temporary.replace(wrapper)
            finally:
                temporary.unlink(missing_ok=True)

    public_path = Path.home() / ".local/bin/claude"
    public_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        public_path.symlink_to(wrapper)
    except FileExistsError:
        pass
    return wrapper


def configure_claude_t3(settings, binary, model=CLAUDE_MODEL):
    instance = settings.setdefault("providerInstances", {}).setdefault("claudeAgent", {})
    instance.update(driver="claudeAgent", enabled=True)
    config = instance.setdefault("config", {})
    config["binaryPath"] = str(binary)
    models = config.setdefault("customModels", [])
    if model not in [entry if isinstance(entry, str) else entry.get("slug") for entry in models]:
        models.append(model)
    return settings


def configure_claude_settings(settings, base_url):
    # T3 reads this URL when checking provider status outside the Claude subprocess.
    settings.setdefault("env", {})["ANTHROPIC_BASE_URL"] = claude_base_url(base_url)
    return settings
