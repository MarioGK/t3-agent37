"""Prepare the checked-out TerraScale workspace for work in a disposable guest."""

import gzip
import hashlib
import json
import os
import platform
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import tomllib


def run(args, cwd=None, timeout=10, env=None):
    return subprocess.run(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                          timeout=timeout, check=True, text=True).stdout.strip()


def link(source, target):
    if target.is_symlink() and target.resolve() == source:
        return
    target.symlink_to(source)


def install_links(workspace, home):
    skills = workspace / "skills"
    helpers = workspace / "bin"
    for source_dir, destinations in (
        (skills, (".agents/skills", ".claude/skills", ".codex/skills", ".config/opencode/skills")),
        (helpers, (".local/bin",)),
    ):
        if not source_dir.is_dir() or source_dir.is_symlink():
            raise ValueError("Missing workspace tools")
        sources = [item for item in source_dir.iterdir() if not item.is_symlink() and
                   ((source_dir == skills and (item / "SKILL.md").is_file()) or
                    (source_dir == helpers and item.is_file() and os.access(item, os.X_OK)))]
        if not sources:
            raise ValueError("Empty workspace tools")
        for destination in destinations:
            folder = home / destination
            if not folder.resolve().is_relative_to(home):
                raise ValueError("Workspace tool links must stay inside the sandbox home")
            folder.mkdir(parents=True, exist_ok=True)
            for source in sources:
                link(source, folder / source.name)


def prepare_mise(home, orchestrator):
    pin = tomllib.loads((orchestrator / ".mise.toml").read_text())["tools"]["go"]
    if not isinstance(pin, str) or not re.fullmatch(r"\d+\.\d+\.\d+", pin, re.ASCII):
        raise ValueError("Invalid TerraScale Go version")
    goroot, version = run(["go", "env", "GOROOT", "GOVERSION"], cwd=home).splitlines()
    if version != "go" + pin:
        raise ValueError("The template Go version does not match the workspace")
    data = Path(os.environ.get("MISE_DATA_DIR", home / ".local/share/mise")).resolve()
    installation = data / "installs" / "go" / pin
    if not installation.exists():
        if data == Path("/opt/terrascale/mise"):
            if Path(goroot).resolve() != Path("/usr/local/go"):
                raise ValueError("Unexpected Go installation")
            run(["sudo", "-n", "install", "-d", "--", str(installation.parent)])
            try:
                run(["sudo", "-n", "ln", "-s", "--", "/usr/local/go", str(installation)])
            except subprocess.CalledProcessError:
                if installation.resolve() != Path("/usr/local/go"):
                    raise
        elif data.is_relative_to(home):
            run(["mise", "link", "go@" + pin, goroot], cwd=home)
        else:
            raise ValueError("Unexpected Mise data directory")
    run(["mise", "trust", str(orchestrator / ".mise.toml")], cwd=home)
    selected = run(["mise", "exec", "--", "go", "env", "GOVERSION"], cwd=orchestrator)
    if selected != version:
        raise ValueError("Mise selected another Go version")


MAX_ORCHESTRATOR_SIZE = 200 * 1024 * 1024
ORCHESTRATOR_CACHE = Path("/opt/terrascale/orch-go-cache")


def workspace_root(home, workspace_path):
    workspace = (home / workspace_path.removeprefix("~/")).resolve(strict=True)
    if not workspace_path.startswith("~/") or workspace == home or not workspace.is_relative_to(home):
        raise ValueError("Invalid TerraScale workspace path")
    return workspace


def build_info(binary, home):
    return dict(line.removeprefix("\tbuild\t").partition("=")[::2]
                for line in run(["go", "version", "-m", str(binary)], cwd=home).splitlines()
                if line.startswith("\tbuild\t"))


def orchestrator_target(orchestrator):
    revision = run(["git", "rev-parse", "HEAD"], cwd=orchestrator)
    platform_arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if not platform_arch or platform.system() != "Linux":
        return None
    if not re.fullmatch(r"[0-9a-f]{40}", revision) or run(
            ["git", "status", "--porcelain", "--untracked-files=normal"], cwd=orchestrator):
        return None
    version = run(["go", "env", "GOVERSION"], cwd=orchestrator)
    return {"revision": revision, "arch": platform_arch, "go_version": version}


def install_orchestrator(orchestrator, home, request_shared=False):
    destination = home / ".local/bin/ts-orch"
    revision = run(["git", "rev-parse", "HEAD"], cwd=orchestrator)
    installed = destination if destination.exists() else Path("/usr/local/bin/ts-orch")
    if installed.is_file() and os.access(installed, os.X_OK):
        try:
            info = build_info(installed, home)
            if info.get("vcs.revision") == revision and info.get("vcs.modified") == "false":
                return None
        except subprocess.CalledProcessError:
            pass
    env = {**os.environ, "GOTOOLCHAIN": "local", "GIT_TERMINAL_PROMPT": "0"}
    build_cache, modules = (ORCHESTRATOR_CACHE / name for name in ("build", "modules"))
    if build_cache.is_dir() and modules.is_dir():
        try:
            target = orchestrator_target(orchestrator)
            if target is not None:
                cached_env = {**env, "GOCACHE": str(build_cache), "GOMODCACHE": str(modules), "CGO_ENABLED": "0"}
                build_orchestrator(orchestrator, destination, cached_env, timeout=25, target=target)
                return None
        except (OSError, ValueError, subprocess.SubprocessError):
            # New dependencies or a template mismatch can make a cached build
            # take longer. Reuse the normal shared or local build in that case.
            pass
    if request_shared:
        try:
            target = orchestrator_target(orchestrator)
            if target is not None:
                return target
        except (OSError, subprocess.SubprocessError):
            pass
    build_orchestrator(orchestrator, destination, env)


def build_orchestrator(orchestrator, destination, env, timeout=165, target=None):
    with tempfile.TemporaryDirectory(prefix=".ts-orch-", dir=destination.parent) as stage:
        binary = Path(stage) / "ts-orch"
        run(["go", "build", "-trimpath", "-buildvcs=true", "-ldflags=-s -w", "-o", str(binary),
             "./cmd/terrascale-orchestrator"], cwd=orchestrator, timeout=timeout, env=env)
        if target is not None:
            verify_build_target(binary, destination.parent, target)
        binary.replace(destination)


def install_shared(workspace_path, filename, digest):
    home = Path.home().resolve()
    if not re.fullmatch(r"ts-orch-[0-9a-f]{32}\.gz", filename) or not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise ValueError("Invalid shared orchestrator artifact")
    compressed = home / ".local/share/t3-agent37" / filename
    try:
        orchestrator = workspace_root(home, workspace_path) / "ts-orchestrator"
        target = orchestrator_target(orchestrator)
        if target is None:
            raise ValueError("Shared build requires a clean checkout")
        destination = home / ".local/bin/ts-orch"
        with tempfile.TemporaryDirectory(prefix=".ts-orch-", dir=destination.parent) as directory:
            staged = Path(directory) / "ts-orch"
            checksum = hashlib.sha256()
            size = 0
            with gzip.open(compressed, "rb") as source, staged.open("xb") as output:
                while data := source.read(1024 * 1024):
                    size += len(data)
                    if size > MAX_ORCHESTRATOR_SIZE:
                        raise ValueError("Shared orchestrator exceeds the size limit")
                    checksum.update(data)
                    output.write(data)
            if checksum.hexdigest() != digest:
                raise ValueError("Shared orchestrator checksum mismatch")
            verify_build_target(staged, home, target)
            staged.chmod(0o755)
            staged.replace(destination)
    finally:
        compressed.unlink(missing_ok=True)


def verify_build_target(binary, cwd, target):
    info = build_info(binary, cwd)
    if (info.get("vcs.revision") != target["revision"] or info.get("vcs.modified") != "false"
            or info.get("GOOS") != "linux" or info.get("GOARCH") != target["arch"]):
        raise ValueError("Orchestrator build targets another revision or platform")


def prepare(workspace_path, request_shared=False):
    home = Path.home().resolve()
    stage = "skills"
    try:
        workspace = workspace_root(home, workspace_path)
        orchestrator = workspace / "ts-orchestrator"
        install_links(workspace, home)
        stage = "toolchain"
        for tool in ("git", "gh", "dotnet", "go", "node", "npm", "python3", "rustc", "codex", "claude", "opencode", "ts"):
            if shutil.which(tool) is None:
                return {"stage": stage}
        product = workspace / "terrascale"
        run(["dotnet", "--version"], cwd=product)
        if (product / ".config/dotnet-tools.json").is_file():
            run(["dotnet", "tool", "restore", "--disable-parallel"], cwd=product, timeout=45)
        stage = "mise"
        prepare_mise(home, orchestrator)
        stage = "orchestrator"
        target = install_orchestrator(orchestrator, home, request_shared)
        if target is not None:
            return {"stage": "share_orchestrator", **target}
    except (OSError, ValueError, KeyError, subprocess.SubprocessError):
        return {"stage": stage}
    return {"stage": "ready"}


if __name__ == "__main__":
    workspace, *mode = sys.argv[1:]
    if mode and mode[0] == "install-shared":
        try:
            install_shared(workspace, *mode[1:])
            response = {"stage": "ready"}
        except (OSError, EOFError, ValueError, subprocess.SubprocessError):
            response = {"stage": "orchestrator"}
    else:
        response = prepare(workspace, mode == ["request-shared"])
    print(json.dumps(response))
