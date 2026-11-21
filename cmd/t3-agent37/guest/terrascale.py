"""Prepare the checked-out TerraScale workspace for work in a disposable guest."""

import json
import os
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


def install_orchestrator(orchestrator, home):
    destination = home / ".local/bin/ts-orch"
    revision = run(["git", "rev-parse", "HEAD"], cwd=orchestrator)
    installed = destination if destination.exists() else Path("/usr/local/bin/ts-orch")
    if installed.is_file() and os.access(installed, os.X_OK):
        try:
            build_info = run(["go", "version", "-m", str(installed)], cwd=home)
            if ("\tbuild\tvcs.revision=" + revision in build_info and
                    "\tbuild\tvcs.modified=false" in build_info):
                return
        except subprocess.CalledProcessError:
            pass
    env = {**os.environ, "GOTOOLCHAIN": "local", "GIT_TERMINAL_PROMPT": "0"}
    with tempfile.TemporaryDirectory(prefix=".ts-orch-", dir=destination.parent) as stage:
        binary = Path(stage) / "ts-orch"
        run(["go", "build", "-trimpath", "-buildvcs=true", "-ldflags=-s -w", "-o", str(binary),
             "./cmd/terrascale-orchestrator"], cwd=orchestrator, timeout=165, env=env)
        binary.replace(destination)


def prepare(workspace_path):
    home = Path.home().resolve()
    stage = "skills"
    try:
        workspace = (home / workspace_path.removeprefix("~/")).resolve(strict=True)
        if not workspace_path.startswith("~/") or workspace == home or not workspace.is_relative_to(home):
            return stage
        orchestrator = workspace / "ts-orchestrator"
        install_links(workspace, home)
        stage = "toolchain"
        for tool in ("git", "gh", "dotnet", "go", "node", "npm", "python3", "rustc", "codex", "claude", "opencode", "ts"):
            if shutil.which(tool) is None:
                return stage
        run(["dotnet", "--version"], cwd=workspace / "terrascale")
        stage = "mise"
        prepare_mise(home, orchestrator)
        stage = "orchestrator"
        install_orchestrator(orchestrator, home)
    except (OSError, ValueError, KeyError, subprocess.SubprocessError):
        return stage
    return "ready"


if __name__ == "__main__":
    print(json.dumps({"stage": prepare(sys.argv[1])}))
