"""Route the installed Codex CLI and its T3 app-server through CLIProxy."""

import json
import os
from pathlib import Path
import re
from selectors import DefaultSelector, EVENT_READ
import shutil
import stat
import subprocess
import tempfile
import time
import tomllib


DEFAULT_MODEL = "glm-5.3"
SOL_MODEL = "gpt-6.2-sol"
PROVIDER = "t3_agent37_cliproxy"


def write_config(binary, home, values):
    # Use Codex's TOML editor to preserve settings outside this batch.
    messages = [
        {"id": 1, "method": "initialize", "params": {
            "clientInfo": {"name": "t3-agent37", "version": "1"},
        }},
        {"method": "initialized"},
        {"id": 2, "method": "config/batchWrite", "params": {"edits": [
            {"keyPath": key, "mergeStrategy": "replace", "value": value}
            for key, value in values.items()
        ]}},
    ]
    payload = "".join(json.dumps(message) + "\n" for message in messages).encode()
    with subprocess.Popen(
        [binary, "app-server", "--listen", "stdio://"], cwd=Path.home(),
        env={**os.environ, "CODEX_HOME": str(home)}, stdin=subprocess.PIPE,
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
    ) as process, DefaultSelector() as selector:
        try:
            selector.register(process.stdout, EVENT_READ)
            process.stdin.write(payload)
            process.stdin.flush()
            deadline = time.monotonic() + 15
            pending = bytearray()
            while selector.select(max(0, deadline - time.monotonic())):
                chunk = os.read(process.stdout.fileno(), 65536)
                if not chunk:
                    break
                pending.extend(chunk)
                while b"\n" in pending:
                    line, _, pending = pending.partition(b"\n")
                    reply = json.loads(line)
                    if reply.get("id") == 2:
                        if reply.get("result", {}).get("status") == "ok":
                            return
                        raise RuntimeError("Codex app-server could not save the CLIProxy configuration")
                if time.monotonic() >= deadline:
                    break
        except (BrokenPipeError, ValueError):
            raise RuntimeError("Codex app-server could not configure CLIProxy") from None
        finally:
            process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
    raise RuntimeError("Codex app-server did not confirm the CLIProxy configuration")


def config_contents(path):
    try:
        return path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return None


def replace_config(binary, path, original, values, expected):
    with tempfile.TemporaryDirectory(prefix=".t3-agent37-codex-", dir=path.parent) as directory:
        stage = Path(directory) / "config.toml"
        stage.write_text(original or "", encoding="utf-8")
        write_config(binary, stage.parent, values)
        contents = stage.read_text(encoding="utf-8")
        try:
            updated = tomllib.loads(contents)
        except ValueError:
            # Codex 0.167 can put a leading comment inside the brackets when
            # it expands an inline model_providers table. Repair that header.
            contents = re.sub(
                r"(?m)^\[((?:[ \t]*(?:\#[^\n]*)?\n)+)"
                r'''([ \t]*(?:model_providers|"model_providers"|'model_providers')[ \t]*\])''',
                r"\1[\2", contents,
            )
            try:
                updated = tomllib.loads(contents)
            except ValueError:
                raise RuntimeError("Codex produced an invalid CLIProxy configuration") from None
            stage.write_text(contents, encoding="utf-8")
        if updated != expected:
            raise RuntimeError("Codex did not preserve the existing configuration")
        if config_contents(path) != original:
            raise RuntimeError("Codex configuration changed during setup. Retry setup")
        stage.chmod(0o600 if original is None else stat.S_IMODE(path.stat().st_mode))
        stage.replace(path)


def configure(base_url, model=DEFAULT_MODEL, catalog_models=()):
    binary = shutil.which("codex")
    if binary is None:
        raise RuntimeError("The CLIProxy sandbox template must include Codex on PATH")

    home = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex").resolve()
    home.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = (home / "config.toml").resolve()
    try:
        original = config_contents(path)
        current = tomllib.loads(original or "")
    except ValueError:
        raise RuntimeError("Codex configuration is invalid") from None

    provider = {
        "name": "CLIProxyAPI",
        "base_url": base_url,
        "env_key": "T3_AGENT37_CLIPROXY_KEY",
        "wire_api": "responses",
        "requires_openai_auth": False,
    }
    providers = current.get("model_providers", {})
    if not isinstance(providers, dict):
        raise RuntimeError("Codex model_providers must be a TOML table")
    defaults = {"model": model, "model_provider": PROVIDER}
    expected = current | defaults | {"model_providers": providers | {PROVIDER: provider}}
    if current != expected:
        values = defaults | {f"model_providers.{PROVIDER}": provider}
        replace_config(binary, path, original, values, expected)

    # Both model IDs are known to work with CLIProxy's Responses API when
    # present. Other catalog entries may only implement Chat Completions.
    custom_models = [model]
    for candidate in (DEFAULT_MODEL, SOL_MODEL):
        if candidate != model and candidate in catalog_models:
            custom_models.append(candidate)
    return {
        "setupMode": "existing",
        "binaryPath": os.path.abspath(binary),
        "homePath": str(home),
        "customModels": custom_models,
    }
