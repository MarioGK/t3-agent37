#!/usr/bin/env python3
"""Unified guest startup script for T3 Code on Agent37 sandboxes."""

from concurrent.futures import ThreadPoolExecutor, as_completed
from contextlib import contextmanager
from copy import deepcopy
from datetime import date, datetime, time as dt_time
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import tomllib
import uuid
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener, urlopen

FALLBACK_VERSION = "0.1.12-nightly.20261123.5138"
T3_CHECKSUMS = {
    "x86_64": ("x64", "77974bc1679da7e078370985fd4ed23ac479c5beec7a90b33668c9d6e89cf441"),
    "aarch64": ("arm64", "28fccd13573672160fd9d5496932ed3ff2f49f834cf2a99deb13b6d85b4866b8"),
}
GH_VERSION = "2.105.0"
GH_CHECKSUMS = {
    "x86_64": ("amd64", "79e7ab5db738c48777b696e374f8b428f0647debfb424f44d2f426370407fe2f"),
    "aarch64": ("arm64", "b1278c75107423d818f1937fd92cafc9e5e9b606631568f68d7fb816f5885a02"),
}
CLAUDE_VERSION = "2.1.329"
CLAUDE_ARCHIVES = {
    "x86_64": ("x64", "fa7e25bb5b9b600a74d98d17fcc0149f1c3dc98e921c767bec5f88a16f95d3056816e888f68a5d1fa8287e37f5e205c908460c4cda64b3b1a7d62f1765328058"),
    "aarch64": ("arm64", "2221b9ba6153a120955aa075944b31c702312a0c4f5f26770d153dd79bfc278073e50dc9a6c94cd1ec359b9c9a4fe9455c13ef6319ac1208b45c8ebc5c7343d0"),
}

OPENCODE_VERSION = "1.19.5"
MANAGED_MODEL = "agent37/default"
DEFAULT_CLIPROXY_MODEL = "glm-5.3"
SOL_MODEL = "gpt-6.2-sol"
CLIPROXY_FALLBACK_MODELS = (
    DEFAULT_CLIPROXY_MODEL,
    SOL_MODEL,
    "claude-sonnet-5-5",
    "claude-opus-5-5",
    "gpt-6.1-sol",
    "gemini-4-neon-xhigh",
)
CLAUDE_FALLBACK_MODELS = (
    DEFAULT_CLIPROXY_MODEL,
    "claude-sonnet-5-5",
    "claude-opus-5-5",
    "claude-sonnet-5",
    "claude-opus-5",
)
CODEX_PROVIDER = "t3_agent37_cliproxy"
MEDIA_PREFIXES = ("gpt-image-", "grok-imagine-", "grok-tts", "grok-voice-")

ROOT = Path.home() / ".local/share/t3-agent37"
DATA = ROOT / "data"
TAILSCALE_ROOT = ROOT / "tailscale"
PUBLIC_GIT_CACHE = Path("/opt/terrascale/git-cache")
PUBLIC_WORKTREE_SNAPSHOTS = Path("/opt/terrascale/worktree-snapshots")
ORCHESTRATOR_CACHE = Path("/opt/terrascale/orch-go-cache")

# Agent37 may keep the gateway port assigned at creation even after a template change.
PORT = 4373 if os.environ.get("AGENT37_GATEWAY_PORT") == "3773" else 3773
HOST = os.environ.get("T3_AGENT37_BIND", "0.0.0.0")
DESCRIPTOR = f"http://127.0.0.1:{PORT}/.well-known/t3/environment"

HOSTNAME_RE = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?")
LABEL_RE = re.compile(r"[a-z0-9][a-z0-9-]{0,42}")
REPO_RE = re.compile(r"[A-Za-z0-9-]+/[A-Za-z0-9_.-]+")
TOKEN_RE = re.compile(r"[A-Za-z0-9_.-]{1,4096}")
STAGE_RE = re.compile(r"launch-[a-f0-9]{32}")
MODEL_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:/()-]{0,159}")
BARE_TOML_KEY_RE = re.compile(r"^[A-Za-z0-9_-]+$")
AUTH_ENV_STRIP = {"GH_DEBUG", "DEBUG", "GIT_TRACE", "GIT_TRACE_CURL"}


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class TailnetError(RuntimeError):
    pass


class EnrollmentRequired(TailnetError):
    pass


def select_runtime(entrypoint, image_versions, fallback):
    if entrypoint.is_symlink():
        binary = entrypoint.readlink()
        runtime = binary.parent
        if (binary.name == "t3" and runtime.parent == image_versions
                and (runtime / ".verified").is_file() and binary.is_file()):
            return runtime
    return fallback


RUNTIME = select_runtime(
    Path("/usr/local/bin/t3"), Path("/opt/t3-agent37/versions"), ROOT / "versions" / FALLBACK_VERSION
)
VERSION = RUNTIME.name
T3 = RUNTIME / "t3"


def script_path():
    for candidate in (ROOT / "startup.py", Path("/opt/t3-agent37/startup.py")):
        if candidate.is_file():
            return candidate
    return Path(__file__).resolve()


def local_opener():
    return build_opener(ProxyHandler({}), NoRedirect())


def atomic_write(path, content, mode=None):
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, delete=False) as out:
        tmp = Path(out.name)
        try:
            out.write(content)
            out.flush()
            if mode is not None:
                tmp.chmod(mode)
            tmp.replace(path)
        finally:
            tmp.unlink(missing_ok=True)


def update_json(path, update):
    path.parent.mkdir(parents=True, exist_ok=True)
    before = json.loads(path.read_text(encoding="utf-8")) if path.exists() else {}
    after = update(deepcopy(before))
    if before != after:
        atomic_write(path, json.dumps(after, indent=2) + "\n")


def download_verified(url, dest, algo, expected, max_bytes, timeout=90, label="archive"):
    digest = hashlib.new(algo)
    with urlopen(url, timeout=timeout) as response, dest.open("wb") as output:
        while chunk := response.read(1024 * 1024):
            digest.update(chunk)
            output.write(chunk)
            if output.tell() > max_bytes:
                raise RuntimeError(f"{label} exceeds the size limit")
    if digest.hexdigest() != expected:
        raise RuntimeError(f"{label} checksum mismatch")


# ---------------------------------------------------------------------------
# Pure-Python TOML writer (replaces codex app-server JSON-RPC)
# ---------------------------------------------------------------------------

def _toml_key(key):
    return key if BARE_TOML_KEY_RE.match(key) else json.dumps(key)


def _toml_value(value):
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        return "nan" if math.isnan(value) else ("inf" if value > 0 else "-inf") if math.isinf(value) else repr(value)
    if isinstance(value, str):
        return json.dumps(value)
    if isinstance(value, (datetime, date, dt_time)):
        return value.isoformat()
    if isinstance(value, list):
        return "[" + ", ".join(_toml_value(item) for item in value) + "]"
    if isinstance(value, dict):
        return "{ " + ", ".join(f"{_toml_key(k)} = {_toml_value(v)}" for k, v in value.items()) + " }"
    raise TypeError(f"Unsupported TOML value: {type(value).__name__}")


def dump_toml(data):
    lines = []

    def emit(table, prefix):
        scalars = [(k, v) for k, v in table.items() if not isinstance(v, dict)]
        subtables = [(k, v) for k, v in table.items() if isinstance(v, dict)]
        if prefix and (scalars or not subtables):
            if lines:
                lines.append("")
            lines.append("[" + ".".join(_toml_key(part) for part in prefix) + "]")
        lines.extend(f"{_toml_key(k)} = {_toml_value(v)}" for k, v in scalars)
        for key, sub in subtables:
            emit(sub, (*prefix, key))

    emit(data, ())
    return ("\n".join(lines) + "\n") if lines else ""


# ---------------------------------------------------------------------------
# T3 runtime, daemon, and provider configuration
# ---------------------------------------------------------------------------

def unpack_runtime(package, destination):
    for member in package:
        rel = Path(member.name)
        if rel.is_absolute() or ".." in rel.parts or not (member.isdir() or member.isfile()):
            raise RuntimeError("Unexpected file in T3 release archive")
        target = destination / rel
        if member.isdir():
            target.mkdir(parents=True, exist_ok=True)
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        with package.extractfile(member) as src, target.open("xb") as out:
            shutil.copyfileobj(src, out)
        target.chmod(0o755 if member.mode & 0o111 else 0o644)


def install_runtime():
    if (RUNTIME / ".verified").is_file() and T3.is_file():
        return
    arch, checksum = T3_CHECKSUMS[platform.machine()]
    name = f"t3-{VERSION}-linux-{arch}.tar.gz"
    RUNTIME.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".install-", dir=RUNTIME.parent) as tmp:
        stage = Path(tmp)
        archive = stage / name
        download_verified(f"https://github.com/pingdotgg/t3code/releases/download/v{VERSION}/{name}",
                          archive, "sha256", checksum, 512 << 20, label="T3 release archive")
        unpacked = stage / "unpacked"
        unpacked.mkdir()
        with tarfile.open(archive) as pkg:
            unpack_runtime(pkg, unpacked)
        children = list(unpacked.iterdir())
        if len(children) != 1 or not (children[0] / "t3").is_file():
            raise RuntimeError("Unexpected T3 release layout")
        (children[0] / ".verified").touch()
        children[0].rename(RUNTIME)


def install_hook():
    hook = Path.home() / ".agent37/hooks/post-restart.sh"
    before = hook.read_text(encoding="utf-8") if hook.exists() else "#!/bin/sh\n"
    line = f"python3 -B {shlex.quote(str(script_path()))} boot"
    lines = [e for e in before.splitlines()
             if not (("t3-agent37/remote.py" in e or "t3-agent37/startup.py" in e) and e.rstrip().endswith(" boot"))]
    desired = "\n".join([*(lines or ["#!/bin/sh"]), line]) + "\n"
    if not hook.exists() or desired != before:
        atomic_write(hook, desired, mode=0o700)
    else:
        hook.chmod(0o700)


def descriptor():
    try:
        with local_opener().open(DESCRIPTOR, timeout=2) as response:
            result = json.load(response)
    except (URLError, ValueError, OSError):
        return None
    if result.get("serverVersion", "").lstrip("v") != VERSION:
        raise RuntimeError(f"A different T3 server is already listening on port {PORT}")
    return result


def claude_base_url(base_url):
    base_url = base_url.rstrip("/").removesuffix("/v1")
    u = urlsplit(base_url)
    if (u.scheme != "https" or not u.hostname or u.username is not None
            or u.password is not None or u.query or u.fragment or any(ord(c) < 33 for c in base_url)):
        raise RuntimeError("Claude Code needs an HTTPS CLIProxy base URL")
    return base_url


def boot():
    ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
    with (ROOT / "server.log").open("ab") as log:
        subprocess.Popen([sys.executable, "-B", str(script_path()), "serve"],
                         stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)


def serve():
    ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
    lock = (ROOT / "server.lock").open("a")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return
    os.environ["PATH"] = os.pathsep.join(dict.fromkeys([str(ROOT / "bin"), str(Path.home() / ".local/bin"), *os.get_exec_path()]))
    if (ROOT / "bin/claude").is_file() and os.environ.get("T3_AGENT37_CLIPROXY_URL"):
        os.environ["ANTHROPIC_BASE_URL"] = claude_base_url(os.environ["T3_AGENT37_CLIPROXY_URL"])
    active_proxy = ROOT / "peer-proxy-active"
    if (ROOT / "peer-proxy").is_file():
        os.environ.update(NODE_USE_ENV_PROXY="1", HTTPS_PROXY="http://127.0.0.1:1055", NO_PROXY="127.0.0.1,localhost,::1")
        active_proxy.touch()
    else:
        active_proxy.unlink(missing_ok=True)
    os.set_inheritable(lock.fileno(), True)
    os.execv(T3, [str(T3), "serve", "--host", HOST, "--port", str(PORT),
                  "--base-dir", str(DATA), "--mode", "web", "--no-browser"])


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
    prefix.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".claude-", dir=prefix) as tmp:
        stage = Path(tmp)
        archive = stage / "claude.tgz"
        download_verified(f"https://registry.npmjs.org/@anthropic-ai/{package}/-/{package}-{CLAUDE_VERSION}.tgz",
                          archive, "sha512", checksum, 512 << 20, label="Claude Code release archive")
        with tarfile.open(archive) as release:
            member = release.getmember("package/claude")
            if not member.isfile():
                raise RuntimeError("Unexpected Claude Code release archive layout")
            exe = stage / "claude"
            with release.extractfile(member) as src, exe.open("wb") as out:
                shutil.copyfileobj(src, out)
            exe.chmod(0o700)
            exe.replace(binary)
    return binary


def configure_claude(root, base_url, model=DEFAULT_CLIPROXY_MODEL):
    base_url = claude_base_url(base_url)
    wrapper = root / "bin/claude"
    launcher = wrapper.resolve()
    binary = next((c for d in os.get_exec_path()
                   if (c := Path(d) / "claude").is_file() and os.access(c, os.X_OK) and c.resolve() != launcher), None)
    binary = binary or install_claude(root)
    script = (
        "#!/bin/sh\nset -eu\n"
        "unset ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_CUSTOM_HEADERS\n"
        "unset CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY\n"
        f"export ANTHROPIC_BASE_URL={shlex.quote(base_url)}\n"
        'export ANTHROPIC_AUTH_TOKEN="${T3_AGENT37_CLIPROXY_KEY:?CLIProxy client key is missing}"\n'
        f"export ANTHROPIC_MODEL={shlex.quote(model)}\n"
        'export ANTHROPIC_DEFAULT_OPUS_MODEL="$ANTHROPIC_MODEL"\n'
        'export ANTHROPIC_DEFAULT_SONNET_MODEL="$ANTHROPIC_MODEL"\n'
        'export ANTHROPIC_DEFAULT_HAIKU_MODEL="$ANTHROPIC_MODEL"\n'
        'export CLAUDE_CODE_SUBAGENT_MODEL="$ANTHROPIC_MODEL"\n'
        "export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\n"
        f'exec {shlex.quote(str(binary.absolute()))} "$@"\n'
    )
    if not wrapper.is_file() or wrapper.read_text(encoding="utf-8") != script:
        atomic_write(wrapper, script, mode=0o700)
    alias = Path.home() / ".local/bin/claude"
    alias.parent.mkdir(parents=True, exist_ok=True)
    try:
        alias.symlink_to(wrapper)
    except FileExistsError:
        pass
    return wrapper


def configure_claude_settings(settings, base_url):
    settings.setdefault("env", {})["ANTHROPIC_BASE_URL"] = claude_base_url(base_url)
    return settings


def configure_claude_t3(settings, binary, model=DEFAULT_CLIPROXY_MODEL):
    inst = settings.setdefault("providerInstances", {}).setdefault("claudeAgent", {})
    inst.update(driver="claudeAgent", enabled=True)
    models = inst.setdefault("config", {}).setdefault("customModels", [])
    inst["config"]["binaryPath"] = str(binary)
    if model not in [e if isinstance(e, str) else e.get("slug") for e in models]:
        models.append(model)
    return settings


def configure_codex(base_url, model=DEFAULT_CLIPROXY_MODEL, catalog_models=()):
    binary = shutil.which("codex")
    if binary is None:
        raise RuntimeError("The CLIProxy sandbox template must include Codex on PATH")
    home = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex").resolve()
    home.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = (home / "config.toml").resolve()
    try:
        original = path.read_text(encoding="utf-8") if path.exists() else None
        current = tomllib.loads(original or "")
    except ValueError:
        raise RuntimeError("Codex configuration is invalid") from None

    provider = {"name": "CLIProxyAPI", "base_url": base_url, "env_key": "T3_AGENT37_CLIPROXY_KEY",
                "wire_api": "responses", "requires_openai_auth": False}
    providers = current.get("model_providers", {})
    if not isinstance(providers, dict):
        raise RuntimeError("Codex model_providers must be a TOML table")
    expected = current | {"model": model, "model_provider": CODEX_PROVIDER,
                          "model_providers": providers | {CODEX_PROVIDER: provider}}
    if current != expected:
        rendered = dump_toml(expected)
        if tomllib.loads(rendered) != expected:
            raise RuntimeError("Could not encode Codex CLIProxy configuration")
        atomic_write(path, rendered, mode=0o600 if original is None else stat.S_IMODE(path.stat().st_mode))

    custom_models = [model, *(c for c in (DEFAULT_CLIPROXY_MODEL, SOL_MODEL) if c != model and c in catalog_models)]
    return {"setupMode": "existing", "binaryPath": os.path.abspath(binary),
            "homePath": str(home), "customModels": custom_models}


def _cliproxy_endpoint():
    base = os.environ.get("T3_AGENT37_CLIPROXY_URL", "").rstrip("/")
    key = os.environ.get("T3_AGENT37_CLIPROXY_KEY")
    explicit_model = os.environ.get("T3_AGENT37_CLIPROXY_MODEL", "").strip()
    u = urlsplit(base)
    if u.scheme != "https" or not u.hostname or u.username or u.password or u.query or u.fragment or not key:
        raise RuntimeError("CLIProxy needs an HTTPS base URL and client key")
    return (base if base.endswith("/v1") else base + "/v1"), key, explicit_model


def cli_proxy_profile(prefetched_models=None):
    base, key, explicit_model = _cliproxy_endpoint()
    if prefetched_models is not None:
        raw_ids = [e for e in prefetched_models if isinstance(e, str)]
    else:
        try:
            req = Request(base + "/models", headers={"Authorization": f"Bearer {key}"})
            with build_opener(NoRedirect()).open(req, timeout=15) as resp:
                catalog = json.load(resp)
        except HTTPError as err:
            err.close()
            raise RuntimeError(f"CLIProxy model catalog returned HTTP {err.code}") from None
        except (URLError, OSError, ValueError):
            raise RuntimeError("CLIProxy model catalog is unavailable") from None
        if not isinstance(catalog, dict) or not isinstance(catalog.get("data"), list):
            raise RuntimeError("CLIProxy model catalog has an unexpected format")
        raw_ids = [e["id"] for e in catalog["data"] if isinstance(e, dict) and isinstance(e.get("id"), str)]

    models = {m: {"name": m} for m in raw_ids if MODEL_ID_RE.fullmatch(m) and not m.startswith(MEDIA_PREFIXES)}
    if not models:
        raise RuntimeError("CLIProxy catalog returned no usable models")
    if explicit_model:
        if explicit_model not in models:
            raise RuntimeError("The requested default model is absent from the CLIProxy catalog")
        model = explicit_model
    else:
        model = next((m for m in CLIPROXY_FALLBACK_MODELS if m in models), next(iter(models)))
    return f"cliproxy/{model}", {
        "name": "CLIProxyAPI",
        "options": {"baseURL": base, "apiKey": "{env:T3_AGENT37_CLIPROXY_KEY}",
                    "timeout": 300000, "headerTimeout": 300000, "chunkTimeout": 300000},
        "models": models,
    }


def configure_managed_providers(provider, cliproxy_models=None, preboot_mode=False):
    profile = "cliproxy" if provider == "cliproxy" else "agent37"
    if profile == "cliproxy":
        if preboot_mode:
            base_url, _, explicit_model = _cliproxy_endpoint()
            model_id = explicit_model or DEFAULT_CLIPROXY_MODEL
            claude_model_id = model_id
            model = f"cliproxy/{model_id}"
            catalog_hint = [model_id, *([SOL_MODEL] if os.environ.get("T3_AGENT37_CLIPROXY_HAS_SOL") == "1" else [])]
            config = None
        else:
            _, _, explicit_model = _cliproxy_endpoint()
            model, config = cli_proxy_profile(cliproxy_models)
            base_url, model_id, catalog_hint = config["options"]["baseURL"], model.removeprefix("cliproxy/"), config["models"]
            claude_model_id = (explicit_model if explicit_model else
                               next((m for m in CLAUDE_FALLBACK_MODELS if m in catalog_hint), model_id))
        claude = configure_claude(ROOT, base_url, claude_model_id)
        codex = configure_codex(base_url, model_id, catalog_hint)
        claude_home = Path(os.environ.get("CLAUDE_CONFIG_DIR") or Path.home() / ".claude").expanduser()
        update_json(claude_home / "settings.json", lambda s: configure_claude_settings(s, base_url))
    else:
        if not os.environ.get("AGENT37_MANAGED_TOKEN") or not os.environ.get("AGENT37_LLM_PROXY_URL"):
            raise RuntimeError("The sandbox has no Agent37 managed model credentials")
        model = MANAGED_MODEL
        config = {"name": "Agent37",
                  "options": {"baseURL": "{env:AGENT37_LLM_PROXY_URL}", "apiKey": "{env:AGENT37_MANAGED_TOKEN}"},
                  "models": {"default": {"name": "Agent37 Default"}}}
    if config is not None:
        config["npm"] = "@ai-sdk/openai-compatible"

    binary = shutil.which("opencode")
    if binary is None:
        if preboot_mode:
            return
        prefix = ROOT / "providers" / OPENCODE_VERSION
        binary = prefix / "node_modules/.bin/opencode"
        if not binary.is_file():
            prefix.mkdir(parents=True, exist_ok=True)
            subprocess.run(["npm", "install", "--prefix", str(prefix), "--no-audit", "--no-fund",
                            f"opencode-ai@{OPENCODE_VERSION}"], check=True, stdout=subprocess.DEVNULL, timeout=120)

    def configure_opencode(settings):
        settings.setdefault("provider", {})[profile] = config
        if not settings.get("model") or (profile == "cliproxy" and settings["model"] == MANAGED_MODEL):
            settings["model"] = model
        return settings

    def configure_t3(settings):
        instances = settings.setdefault("providerInstances", {})
        oc = instances.setdefault("opencode", {"driver": "opencode"})
        oc["enabled"] = True
        oc.setdefault("config", {})["binaryPath"] = str(binary)
        if profile == "cliproxy":
            configure_claude_t3(settings, claude, claude_model_id)
            inst = instances.setdefault("codex", {})
            inst.update(driver="codex", enabled=True)
            cfg = inst.setdefault("config", {})
            existing = cfg.get("customModels") or []
            known = {e if isinstance(e, str) else (e.get("slug") if isinstance(e, dict) else None) for e in existing}
            cfg.update(codex, customModels=[*existing, *(m for m in codex["customModels"] if m not in known)])
        default = settings.get("defaultModelSelection") or {}
        prev = default.get("instanceId", default.get("provider"))
        if not prev or (prev == "codex" and profile == "agent37") or (profile == "cliproxy" and default.get("model") == MANAGED_MODEL):
            settings["defaultModelSelection"] = {"instanceId": "opencode", "model": model}
        return settings

    if config is not None:
        config_home = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
        update_json(config_home / "opencode/opencode.json", configure_opencode)
    update_json(DATA / "userdata/settings.json", configure_t3)


def provider_ready(provider, cliproxy_models=None, preboot_mode=False):
    if provider in {"opencode", "cliproxy"}:
        configure_managed_providers(provider, cliproxy_models, preboot_mode=preboot_mode)
        return True
    cmd = {"codex": ["codex", "login", "status"], "claude": ["claude", "auth", "status"]}
    if provider not in cmd or shutil.which(provider) is None:
        raise RuntimeError("The selected provider CLI must be installed on PATH")
    return True if preboot_mode else subprocess.run(cmd[provider], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15).returncode == 0


def machine_info_with_label(contents, name):
    lines = [line for line in contents.splitlines() if not line.strip().startswith("PRETTY_HOSTNAME=")]
    return "\n".join([f'PRETTY_HOSTNAME="{name}"', *lines]) + "\n"


def save_environment_label(name):
    if not LABEL_RE.fullmatch(name):
        return False
    path = Path("/etc/machine-info")
    try:
        previous = path.read_text(encoding="utf-8") if path.exists() else ""
        desired = machine_info_with_label(previous, name)
        if desired == previous:
            return True
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=ROOT) as staged:
            staged.write(desired)
            staged.flush()
            cmd = ["/usr/bin/install", "-m", "0644", "--", staged.name, str(path)]
            if os.geteuid() != 0:
                cmd = ["sudo", "-n", *cmd]
            return subprocess.run(cmd, capture_output=True, timeout=5).returncode == 0
    except (OSError, UnicodeError, subprocess.TimeoutExpired):
        return False


def desktop_confirmed(environment_id):
    try:
        return (ROOT / "desktop-confirmed").read_text(encoding="utf-8").strip() == environment_id
    except FileNotFoundError:
        return False


def setup_t3(provider, name, peer_links=False, cliproxy_models=None):
    ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
    with (ROOT / "install.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        install_runtime()
        label_saved = save_environment_label(name)
        ready = provider_ready(provider, cliproxy_models)
        if peer_links:
            (ROOT / "peer-proxy").touch()
            update_json(DATA / "userdata/settings.json", lambda s: {**s, "linkedEnvironmentsEnabled": True})
        install_hook()
        result = descriptor()
        if result is None:
            boot()
            deadline, delay = time.monotonic() + 60, 0.1
            while (result := descriptor()) is None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise RuntimeError("T3 did not start. See ~/.local/share/t3-agent37/server.log")
                time.sleep(min(delay, remaining))
                delay = min(delay * 2, 1.0)
        return {
            "environmentId": result["environmentId"], "version": VERSION,
            "label": result.get("label", ""), "labelSaved": label_saved,
            "port": PORT, "providerReady": ready,
            "peerProxy": (ROOT / "peer-proxy-active").is_file(),
            "desktopConfirmed": desktop_confirmed(result["environmentId"]),
        }


# ---------------------------------------------------------------------------
# Unprivileged userspace Tailscale client
# ---------------------------------------------------------------------------

@contextmanager
def tailscale_locked():
    TAILSCALE_ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
    with (TAILSCALE_ROOT / "lifecycle.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def _ts_cmd(*args):
    return ["tailscale", "--socket", str(TAILSCALE_ROOT / "tailscaled.sock"), *args]


def tailnet_status():
    try:
        info = json.loads(subprocess.run(_ts_cmd("status", "--json", "--peers=false"),
                                         capture_output=True, text=True, timeout=3).stdout)
    except FileNotFoundError:
        raise TailnetError("Tailscale is missing from the sandbox template") from None
    except (ValueError, subprocess.TimeoutExpired):
        return None
    return info if isinstance(info, dict) and isinstance(info.get("BackendState"), str) else None


def tailnet_start_daemon():
    info, daemon = tailnet_status(), None
    if info is None:
        try:
            with (TAILSCALE_ROOT / "tailscaled.log").open("ab") as log:
                daemon = subprocess.Popen(
                    ["tailscaled", "--tun=userspace-networking", "--statedir", str(TAILSCALE_ROOT),
                     "--socks5-server=127.0.0.1:1055", "--outbound-http-proxy-listen=127.0.0.1:1055",
                     "--socket", str(TAILSCALE_ROOT / "tailscaled.sock")],
                    stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True,
                )
        except FileNotFoundError:
            raise TailnetError("The Tailscale daemon is missing from the sandbox template") from None
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if info is not None and info["BackendState"] != "NoState":
            return info
        if daemon is not None and daemon.poll() is not None:
            break
        time.sleep(0.15)
        info = tailnet_status()
    raise TailnetError("The sandbox Tailscale daemon did not start")


def _ts_run_private(*args, timeout, pass_fds=()):
    try:
        return subprocess.run(_ts_cmd(*args), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL, timeout=timeout, pass_fds=pass_fds).returncode == 0
    except subprocess.TimeoutExpired:
        return False


def tailnet_identity(info):
    peer = (info or {}).get("Self")
    if (not info or info.get("BackendState") != "Running" or not isinstance(peer, dict)
            or peer.get("Online") is not True
            or not all(isinstance(peer.get(k), str) and peer[k] for k in ("ID", "HostName"))):
        return None
    dns = peer.get("DNSName")
    return {"id": peer["ID"], "hostname": peer["HostName"], "dnsName": dns.rstrip(".") if isinstance(dns, str) else ""}


def _check_enrollment(info, auth_key):
    state = (info or {}).get("BackendState")
    if state == "NeedsLogin" and not auth_key:
        raise EnrollmentRequired("The sandbox needs a fresh Tailscale enrollment key")
    if state == "NeedsMachineAuth":
        raise TailnetError("Tailscale is waiting for device approval")


def tailnet_join(hostname, auth_key=None):
    if not isinstance(hostname, str) or not HOSTNAME_RE.fullmatch(hostname):
        raise TailnetError("Invalid sandbox Tailscale hostname")
    with tailscale_locked():
        active = TAILSCALE_ROOT / "joined"
        if active.exists() and active.read_text(encoding="utf-8") != hostname:
            raise TailnetError("The sandbox already uses another Tailscale hostname")
        info = tailnet_start_daemon()
        current = tailnet_identity(info)
        if current and current["hostname"] != hostname:
            raise TailnetError("The running Tailscale client has an unexpected hostname")
        if not current:
            _check_enrollment(info, auth_key)
            args = ("up", "--hostname", hostname, "--timeout=30s", "--accept-dns=false",
                    "--accept-routes=false", "--shields-up=true", "--ssh=false")
            if auth_key:
                with tempfile.TemporaryFile(mode="w+", dir=TAILSCALE_ROOT) as cred:
                    cred.write(auth_key)
                    cred.flush()
                    fd = cred.fileno()
                    ok = _ts_run_private(*args, f"--auth-key=file:/proc/self/fd/{fd}", timeout=35, pass_fds=(fd,))
            else:
                ok = _ts_run_private(*args, timeout=35)
            info = tailnet_status()
            _check_enrollment(info, auth_key)
            if not ok:
                raise TailnetError("Tailscale join failed. Check the sandbox enrollment key and policy.")
            current = tailnet_identity(info)
        if not current or current["hostname"] != hostname:
            raise TailnetError("Tailscale did not report the sandbox online under its assigned hostname")
        active.write_text(hostname, encoding="utf-8")
        return current


def tailnet_serve_peer(port):
    with tailscale_locked():
        peer = tailnet_identity(tailnet_status())
        if peer is None or not peer["dnsName"].endswith(".ts.net"):
            raise TailnetError("Tailscale needs MagicDNS to serve T3 peer connections")
        if not _ts_run_private("set", "--shields-up=false", timeout=10):
            raise TailnetError("Could not enable private Tailscale ingress")
        if not _ts_run_private("serve", "--bg", "--yes", "--https=443", f"http://127.0.0.1:{port}", timeout=45):
            _ts_run_private("set", "--shields-up=true", timeout=10)
            raise TailnetError("Could not publish T3 through private Tailscale Serve")
        return f"https://{peer['dnsName']}"


def tailnet_restore():
    if TAILSCALE_ROOT.is_dir():
        with tailscale_locked():
            if (TAILSCALE_ROOT / "joined").exists() and (TAILSCALE_ROOT / "tailscaled.state").exists():
                tailnet_start_daemon()


def tailnet_leave():
    if not TAILSCALE_ROOT.is_dir():
        return
    with tailscale_locked():
        active = TAILSCALE_ROOT / "joined"
        if not any((TAILSCALE_ROOT / n).exists() for n in ("joined", "tailscaled.state", "tailscaled.sock")):
            return
        info = tailnet_start_daemon()
        if info.get("BackendState") != "NeedsLogin" and not _ts_run_private("logout", timeout=20):
            if (tailnet_status() or {}).get("BackendState") != "NeedsLogin":
                raise TailnetError("Tailscale logout did not complete. Retry before stopping the sandbox.")
        active.unlink(missing_ok=True)


def warm_tailnet_cert(dns_name):
    """Trigger Let's Encrypt cert provisioning in tailscaled without blocking."""
    try:
        with (TAILSCALE_ROOT / "cert.log").open("ab") as log:
            subprocess.Popen(
                _ts_cmd("cert", "--cert-file", "-", "--key-file", "-", dns_name),
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=log,
                start_new_session=True,
            )
    except OSError:
        pass


def tailnet_cert_error():
    """Return a sanitized Let's Encrypt rate-limit hint from tailscaled.log, if any."""
    log = TAILSCALE_ROOT / "tailscaled.log"
    if not log.is_file():
        return {"rateLimited": False}
    try:
        text = log.read_text(encoding="utf-8", errors="replace")[-32768:]
    except OSError:
        return {"rateLimited": False}
    m = re.search(r"too many certificates \(\d+\) already issued.*?retry after ([0-9:-]+ [0-9:]+ UTC)", text)
    if m:
        return {"rateLimited": True, "retryAfter": m.group(1)}
    if "rateLimited" in text or "429 urn:ietf:params:acme:error:rateLimited" in text:
        return {"rateLimited": True}
    return {"rateLimited": False}


def publish_tailnet(hostname, auth_key=None):
    try:
        peer = tailnet_join(hostname, auth_key)
        peer["url"] = tailnet_serve_peer(PORT)
        if peer.get("dnsName"):
            warm_tailnet_cert(peer["dnsName"])
        return peer
    except EnrollmentRequired:
        return {"enrollmentRequired": True}


# ---------------------------------------------------------------------------
# GitHub access handoff
# ---------------------------------------------------------------------------

def _gh_run(args, *, data=None):
    env = {k: v for k, v in os.environ.items() if k not in AUTH_ENV_STRIP}
    env.update(GH_PROMPT_DISABLED="1", GH_NO_UPDATE_NOTIFIER="1")
    return subprocess.run(args, input=data, capture_output=True, text=True, timeout=25, env=env)


def github_binary():
    if installed := shutil.which("gh"):
        return installed
    target = Path.home() / ".local/bin/gh"
    if target.is_file() and os.access(target, os.X_OK):
        return str(target)
    arch, expected = GH_CHECKSUMS[platform.machine()]
    package = f"gh_{GH_VERSION}_linux_{arch}"
    target.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".gh-", dir=target.parent) as tmp:
        stage = Path(tmp)
        archive = stage / "gh.tar.gz"
        download_verified(f"https://github.com/cli/cli/releases/download/v{GH_VERSION}/{package}.tar.gz",
                          archive, "sha256", expected, 50 << 20, timeout=45, label="GitHub CLI archive")
        binary = stage / "gh"
        with tarfile.open(archive) as src:
            member = src.getmember(f"{package}/bin/gh")
            if not member.isfile():
                raise RuntimeError("Invalid GitHub CLI archive")
            with src.extractfile(member) as content, binary.open("xb") as out:
                shutil.copyfileobj(content, out)
        binary.chmod(0o755)
        try:
            os.link(binary, target)
        except FileExistsError:
            if not target.is_file() or not os.access(target, os.X_OK):
                raise
    return str(target)


def github_hosts():
    config = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    return Path(os.environ.get("GH_CONFIG_DIR") or config / "gh") / "hosts.yml"


def _valid_hosts_file(path):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise RuntimeError("GitHub CLI credential file is not private")


def install_app_login(token, path):
    req = Request("https://api.github.com/installation/repositories?per_page=1",
                  headers={"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json"})
    with build_opener(NoRedirect()).open(req, timeout=15) as resp:
        if resp.status != 200:
            raise RuntimeError("GitHub installation access was refused")
    path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, delete=False) as out:
        staged = Path(out.name)
        try:
            out.write(f"github.com:\n    git_protocol: https\n    oauth_token: {token}\n    user: x-access-token\n")
            out.flush()
            os.link(staged, path)
        finally:
            staged.unlink(missing_ok=True)


def git_config(*args, missing_ok=False):
    result = _gh_run(["git", "config", *args])
    if result.returncode != 0 and not (missing_ok and result.returncode == 1):
        raise RuntimeError("Could not configure Git for this sandbox")
    return result.stdout


def configure_git_for_github(gh):
    key = "credential.https://github.com.helper"
    if not any(git_config("--get-all", key, missing_ok=True).splitlines()):
        git_config("--global", "--add", key, "!" + shlex.quote(gh) + " auth git-credential")
    rules = git_config("--null", "--get-regexp", r"^url\..*\.insteadof$", missing_ok=True)
    prefixes = {rule.partition("\n")[2] for rule in rules.split("\0")}
    for prefix in ("git@github.com:", "ssh://git@github.com/"):
        if prefix not in prefixes:
            git_config("--global", "--add", "url.https://github.com/.insteadOf", prefix)


def configure_identity(payload):
    configured = []
    for key, value in (("user.name", payload.get("name", "")), ("user.email", payload.get("email", ""))):
        present = git_config("--global", "--get", key, missing_ok=True).strip()
        if value and not present:
            git_config("--global", "--", key, value)
        configured.append(bool(value or present))
    return all(configured)


def configure_github(token):
    gh = github_binary()
    check = [gh, "auth", "token", "--hostname", "github.com"]
    if _gh_run(check).returncode != 0:
        path = github_hosts()
        _valid_hosts_file(path)
        if token.startswith("ghs_"):
            install_app_login(token, path)
        elif _gh_run([gh, "auth", "login", "--hostname", "github.com", "--with-token", "--git-protocol", "https"],
                     data=token + "\n").returncode != 0:
            raise RuntimeError("GitHub did not accept the workstation login")
        _valid_hosts_file(path)
        if _gh_run(check).returncode != 0:
            raise RuntimeError("GitHub CLI did not retain the sandbox login")
    configure_git_for_github(gh)


def apply_github_handoff(payload):
    if not isinstance(payload, dict) or any(not isinstance(v, str) for v in payload.values()):
        raise RuntimeError("Invalid GitHub handoff data")
    token = payload.get("token", "")
    if token and not TOKEN_RE.fullmatch(token):
        raise RuntimeError("Invalid GitHub handoff token")
    git_identity = configure_identity(payload)
    if token:
        configure_github(token)
    return {"github": bool(token), "gitIdentity": git_identity}


# ---------------------------------------------------------------------------
# Workspace checkout, TerraScale tools, and T3 project registration
# ---------------------------------------------------------------------------

def git_environment():
    env = {k: v for k, v in os.environ.items() if k not in {"GH_DEBUG", "GIT_CURL_VERBOSE"} and not k.startswith("GIT_TRACE")}
    env.update(GIT_TERMINAL_PROMPT="0", GH_PROMPT_DISABLED="1", GCM_INTERACTIVE="Never")
    return env


def _git(repo_dir, *args, env, timeout=5):
    return subprocess.run(["git", "-C", str(repo_dir), *args], capture_output=True, text=True, timeout=timeout, env=env)


def valid_path_part(part):
    return bool(part) and part not in {".", ".."} and "/" not in part and "\\" not in part


def matches_checkout(path, repo, env):
    if path.is_symlink() or not path.is_dir():
        return False
    root = _git(path, "rev-parse", "--show-toplevel", "--verify", "HEAD^{commit}", env=env)
    if root.returncode != 0 or Path(root.stdout.partition("\n")[0]) != path.resolve():
        return False
    origin_res = _git(path, "config", "--local", "--get", "remote.origin.url", env=env)
    origin = origin_res.stdout.strip().lower().removesuffix("/").removesuffix(".git")
    slug = repo.lower()
    return origin_res.returncode == 0 and origin in {
        "https://github.com/" + slug, "git@github.com:" + slug, "ssh://git@github.com/" + slug,
    }


def refresh_checkout(checkout, env, workspace_root, fetch_timeout="120s"):
    branch = _git(checkout, "symbolic-ref", "--quiet", "--short", "HEAD", env=env)
    if branch.returncode != 0:
        return "existing"
    branch_name = branch.stdout.strip()
    remote = _git(checkout, "config", "--get", f"branch.{branch_name}.remote", env=env)
    if remote.returncode != 0 or remote.stdout.strip() != "origin":
        return "existing"
    merge_ref = _git(checkout, "config", "--get", f"branch.{branch_name}.merge", env=env)
    ref = merge_ref.stdout.strip()
    if merge_ref.returncode != 0 or not ref.startswith("refs/heads/"):
        return "pull_conflict"
    tracking = "refs/remotes/origin/" + ref.removeprefix("refs/heads/")
    fetched = subprocess.run(
        ["timeout", "--kill-after=5s", fetch_timeout, "git", "-C", str(checkout), "fetch", "--quiet",
         "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--refmap=", "origin", f"+{ref}:{tracking}"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env,
    )
    if fetched.returncode in {124, 137}:
        return "timeout"
    if fetched.returncode != 0:
        return "fetch_failed"
    if _git(checkout, "symbolic-ref", "--quiet", "--short", "HEAD", env=env).stdout.strip() != branch_name:
        return "pull_conflict"
    upstream = _git(checkout, "rev-parse", "--verify", tracking + "^{commit}", env=env)
    if upstream.returncode != 0:
        return "pull_conflict"
    revision = upstream.stdout.strip()
    if workspace_root:
        nested = {e.name for e in checkout.iterdir() if (e / ".git").exists()}
        changes = _git(checkout, "diff", "--name-only", "-z", "HEAD", revision, env=env, timeout=10)
        if changes.returncode != 0 or any(name.split("/", 1)[0] in nested for name in changes.stdout.split("\0")):
            return "pull_conflict"
    merged = subprocess.run(
        ["git", "-C", str(checkout), "merge", "--quiet", "--ff-only", "--no-autostash", "--no-overwrite-ignore", revision],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30, env=env,
    )
    return "existing" if merged.returncode == 0 else "pull_conflict"


def use_snapshot(snapshot, checkout, repo, env):
    if snapshot.is_symlink() or not snapshot.is_dir():
        return False
    if subprocess.run(["cp", "-R", "--preserve=mode,timestamps", "--reflink=auto", "--", str(snapshot), str(checkout)],
                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20).returncode != 0:
        return False
    if not matches_checkout(checkout, repo, env):
        return False
    remote = _git(checkout, "ls-remote", "--symref", "origin", "HEAD", env=env, timeout=12)
    if remote.returncode != 0:
        return False
    remote_ref, remote_rev = "", ""
    for line in remote.stdout.splitlines():
        val, _, name = line.partition("\t")
        if name == "HEAD":
            if val.startswith("ref: "):
                remote_ref = val.removeprefix("ref: ")
            else:
                remote_rev = val
    if not remote_ref.startswith("refs/heads/") or not re.fullmatch(r"[a-f0-9]{40}", remote_rev):
        return False
    branch = _git(checkout, "symbolic-ref", "--quiet", "HEAD", env=env)
    if branch.returncode != 0 or branch.stdout.strip() != remote_ref:
        return False

    def matches_remote():
        rev = _git(checkout, "rev-parse", "--verify", "HEAD^{commit}", env=env)
        return rev.returncode == 0 and rev.stdout.strip() == remote_rev

    if matches_remote():
        return True
    tracking = "refs/remotes/origin/" + remote_ref.removeprefix("refs/heads/")
    fetched = subprocess.run(
        ["timeout", "--kill-after=5s", "20s", "git", "-C", str(checkout), "fetch", "--quiet",
         "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--refmap=", "origin",
         f"+{remote_ref}:{tracking}"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env,
    )
    if fetched.returncode != 0:
        return False
    merged = subprocess.run(
        ["git", "-C", str(checkout), "merge", "--quiet", "--ff-only", "--no-autostash", "--no-overwrite-ignore", remote_rev],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30, env=env,
    )
    return merged.returncode == 0 and matches_remote()


def can_adopt_root(path):
    git_dir = path / ".git"
    return path.is_dir() and not path.is_symlink() and not git_dir.exists() and not git_dir.is_symlink()


def move_without_overwrite(source, destination):
    res = subprocess.run(["mv", "--no-clobber", "--no-target-directory", "--", str(source), str(destination)],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return res.returncode == 0 and not source.exists()


def clone_repository(home, workspace_path, repo, directory, ref):
    parts = workspace_path.removeprefix("~/").split("/")
    if (not workspace_path.startswith("~/") or not all(map(valid_path_part, parts))
            or not REPO_RE.fullmatch(repo) or not valid_path_part(repo.split("/")[1])
            or (directory and not valid_path_part(directory)) or ref.startswith("-")):
        raise ValueError("Invalid workspace repository")
    parent = home
    for part in (parts if directory else parts[:-1]):
        parent /= part
        parent.mkdir(exist_ok=True)
        if not stat.S_ISDIR(parent.lstat().st_mode):
            return "filesystem_error"

    target, env = parent / (directory or parts[-1]), git_environment()
    if target.exists() or target.is_symlink():
        if matches_checkout(target, repo, env):
            return refresh_checkout(target, env, not directory)
        if directory or not can_adopt_root(target):
            return "conflict"

    with tempfile.TemporaryDirectory(prefix=".t3-agent37-clone-", dir=parent) as stage:
        checkout = Path(stage) / target.name
        try:
            seeded = not ref and use_snapshot(PUBLIC_WORKTREE_SNAPSHOTS / repo.lower(), checkout, repo, env)
        except subprocess.TimeoutExpired:
            seeded = False
        if not seeded:
            if checkout.exists():
                checkout = Path(stage) / "fresh-clone"
            cmd = ["timeout", "--kill-after=5s", "210s", "git", "clone", "--quiet", "--filter=blob:none"]
            cache = PUBLIC_GIT_CACHE / (repo.lower() + ".git")
            if cache.is_dir():
                cmd.extend(["--reference-if-able", str(cache), "--dissociate"])
            if ref:
                cmd.extend(["--branch", ref])
            cmd.extend(["--", f"https://github.com/{repo}.git", str(checkout)])
            res = subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
            if res.returncode in {124, 137}:
                return "timeout"
            if res.returncode != 0:
                return "clone_failed"

        if move_without_overwrite(checkout, target):
            return "seeded" if seeded else "cloned"
        if not directory and can_adopt_root(target):
            if any(e.name != ".git" and (target / e.name / ".git").exists() for e in checkout.iterdir()):
                return "conflict"
            git = ["git", f"--git-dir={checkout / '.git'}", f"--work-tree={target}"]
            subprocess.run([*git, "checkout-index", "--all"], timeout=30, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
            if (subprocess.run([*git, "diff", "--quiet"], timeout=30, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env).returncode == 0
                    and move_without_overwrite(checkout / ".git", target / ".git")):
                return "seeded" if seeded else "cloned"
        if target.exists() or target.is_symlink():
            return refresh_checkout(target, env, not directory) if matches_checkout(target, repo, env) else "conflict"
        return "filesystem_error"


def checkout_status(home, workspace_path, repo, directory, ref):
    try:
        return clone_repository(home, workspace_path, repo, directory, ref)
    except subprocess.TimeoutExpired:
        return "timeout"
    except (OSError, ValueError):
        return "filesystem_error"


def read_terrascale_manifest(root):
    try:
        size = subprocess.check_output(["git", "-C", str(root), "cat-file", "-s", "HEAD:repos.txt"], stderr=subprocess.DEVNULL, timeout=5)
        if int(size) > 16 * 1024:
            return None, "too_large"
        return subprocess.check_output(["git", "-C", str(root), "show", "HEAD:repos.txt"], stderr=subprocess.DEVNULL, text=True, timeout=5), ""
    except (OSError, ValueError, subprocess.SubprocessError):
        return None, "unreadable"


def merge_terrascale_manifest(repositories, manifest):
    merged = list(repositories)
    seen = {item["repo"].lower(): item.get("directory") or item["repo"].rsplit("/", 1)[-1] for item in merged}
    directories = set(seen.values())
    entries = manifest.split()
    if not entries:
        return None, "empty"
    for entry in entries:
        repo = "TerraScale/" + entry
        if not REPO_RE.fullmatch(repo) or not valid_path_part(entry):
            return None, "invalid"
        key = repo.lower()
        if key not in seen:
            if entry in directories:
                return None, "duplicate"
            merged.append({"repo": repo})
            seen[key] = entry
            directories.add(entry)
        elif seen[key] != entry:
            return None, "directory_conflict"
    return merged, ""


def checkout_workspace(home, playbook, on_terrascale_ready=None, on_checkout_ready=None):
    workspace_path, root_repo = playbook["path"], playbook.get("root_repository", "")
    repositories = list(playbook.get("repositories") or [])
    is_terrascale = root_repo.lower() == "terrascale/workspace"
    checkouts = {}

    if root_repo:
        status = checkout_status(home, workspace_path, root_repo, "", "")
        if status not in {"cloned", "existing", "seeded"}:
            return {"error": {"status": status, "repo": root_repo, "folder": ""}}
        checkouts[workspace_path] = root_repo
        if on_checkout_ready is not None:
            on_checkout_ready(workspace_path, root_repo)
        if is_terrascale:
            manifest, err = read_terrascale_manifest(home / workspace_path.removeprefix("~/"))
            if err:
                return {"manifestError": err}
            repositories, err = merge_terrascale_manifest(repositories, manifest)
            if err:
                return {"manifestError": err}

    needed_for_tools = {"terrascale/terrascale", "terrascale/ts-orchestrator"} if is_terrascale and on_terrascale_ready else set()
    ready_for_tools = set()
    tools_started = False

    def run_one(item):
        directory = item.get("directory") or item["repo"].rsplit("/", 1)[-1]
        return item, directory, checkout_status(home, workspace_path, item["repo"], directory, item.get("ref", ""))

    seeded, errors = 0, []
    if repositories:
        with ThreadPoolExecutor(max_workers=min(12, len(repositories))) as pool:
            futs = [pool.submit(run_one, item) for item in repositories]
            for fut in as_completed(futs):
                item, directory, status = fut.result()
                seeded += status == "seeded"
                if status in {"cloned", "existing", "seeded"}:
                    checkout_path = workspace_path.rstrip("/") + "/" + directory
                    checkouts[checkout_path] = item["repo"]
                    if on_checkout_ready is not None:
                        on_checkout_ready(checkout_path, item["repo"])
                    slug = item["repo"].lower()
                    if slug in needed_for_tools:
                        ready_for_tools.add(slug)
                        if not tools_started and ready_for_tools == needed_for_tools:
                            tools_started = True
                            on_terrascale_ready()
                else:
                    errors.append({"status": status, "repo": item["repo"], "folder": directory})
    return ({"errors": errors, "seededWorktrees": seeded} if errors
            else {"checkouts": checkouts, "seededWorktrees": seeded, "isTerraScale": is_terrascale})


def _run_tool(args, cwd=None, timeout=10, env=None):
    return subprocess.run(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                          stderr=subprocess.DEVNULL, timeout=timeout, check=True, text=True).stdout.strip()


def _ensure_symlink(source, target):
    if target.is_symlink():
        if target.resolve() == source:
            return
        target.unlink()
    target.symlink_to(source)


def install_terrascale_links(workspace, home):
    skills_dir = workspace / "skills"
    if not skills_dir.is_dir() or skills_dir.is_symlink():
        raise ValueError("Missing workspace skills")
    skills = [item for item in skills_dir.iterdir() if not item.is_symlink() and (item / "SKILL.md").is_file()]
    if not skills:
        raise ValueError("Empty workspace skills")
    for dest in (".gemini/config/skills", ".agents/skills", ".claude/skills", ".codex/skills", ".config/opencode/skills"):
        folder = home / dest
        if not folder.resolve().is_relative_to(home):
            raise ValueError("Workspace skill links must stay inside the sandbox home")
        folder.mkdir(parents=True, exist_ok=True)
        for src in skills:
            _ensure_symlink(src, folder / src.name)

    helper_links = []
    for dirname, prefix in (("scripts", "ts-"), ("bin", "")):
        source_dir = workspace / dirname
        if source_dir.is_dir() and not source_dir.is_symlink():
            for item in source_dir.iterdir():
                if not item.is_symlink() and item.is_file() and os.access(item, os.X_OK):
                    link_name = item.name if (not prefix or item.name.startswith(prefix)) else prefix + item.name
                    helper_links.append((item, link_name))
    if not helper_links:
        raise ValueError("Missing workspace helper scripts")
    bin_dir = home / ".local/bin"
    if not bin_dir.resolve().is_relative_to(home):
        raise ValueError("Workspace helper links must stay inside the sandbox home")
    bin_dir.mkdir(parents=True, exist_ok=True)
    for src, link_name in helper_links:
        _ensure_symlink(src, bin_dir / link_name)


def prepare_mise(home, orchestrator):
    pin = tomllib.loads((orchestrator / ".mise.toml").read_text(encoding="utf-8"))["tools"]["go"]
    if not isinstance(pin, str) or not re.fullmatch(r"\d+\.\d+\.\d+", pin, re.ASCII):
        raise ValueError("Invalid TerraScale Go version")
    goroot, version = _run_tool(["go", "env", "GOROOT", "GOVERSION"], cwd=home).splitlines()
    if version != "go" + pin:
        raise ValueError("The template Go version does not match the workspace")
    data = Path(os.environ.get("MISE_DATA_DIR", home / ".local/share/mise")).resolve()
    installation = data / "installs" / "go" / pin
    if not installation.exists():
        if data == Path("/opt/terrascale/mise"):
            if Path(goroot).resolve() != Path("/usr/local/go"):
                raise ValueError("Unexpected Go installation")
            _run_tool(["sudo", "-n", "install", "-d", "--", str(installation.parent)])
            try:
                _run_tool(["sudo", "-n", "ln", "-s", "--", "/usr/local/go", str(installation)])
            except subprocess.CalledProcessError:
                if installation.resolve() != Path("/usr/local/go"):
                    raise
        elif data.is_relative_to(home):
            _run_tool(["mise", "link", "go@" + pin, goroot], cwd=home)
        else:
            raise ValueError("Unexpected Mise data directory")
    _run_tool(["mise", "trust", str(orchestrator / ".mise.toml")], cwd=home)
    if _run_tool(["mise", "exec", "--", "go", "env", "GOVERSION"], cwd=orchestrator) != version:
        raise ValueError("Mise selected another Go version")


def build_info(binary, home):
    return dict(line.removeprefix("\tbuild\t").partition("=")[::2]
                for line in _run_tool(["go", "version", "-m", str(binary)], cwd=home).splitlines()
                if line.startswith("\tbuild\t"))


def build_orchestrator(orchestrator, destination, env, timeout=165):
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".ts-orch-", dir=destination.parent) as stage:
        binary = Path(stage) / "ts-orch"
        _run_tool(["go", "build", "-trimpath", "-buildvcs=true", "-ldflags=-s -w", "-o", str(binary),
                   "./cmd/terrascale-orchestrator"], cwd=orchestrator, timeout=timeout, env=env)
        binary.replace(destination)


def install_orchestrator(orchestrator, home):
    destination = home / ".local/bin/ts-orch"
    revision = _run_tool(["git", "rev-parse", "HEAD"], cwd=orchestrator)
    installed = destination if destination.exists() else Path("/usr/local/bin/ts-orch")
    if installed.is_file() and os.access(installed, os.X_OK):
        try:
            info = build_info(installed, home)
            if info.get("vcs.revision") == revision and info.get("vcs.modified") == "false":
                return
        except subprocess.CalledProcessError:
            pass
    env = {**os.environ, "GOTOOLCHAIN": "local", "GIT_TERMINAL_PROMPT": "0"}
    build_cache, modules = ORCHESTRATOR_CACHE / "build", ORCHESTRATOR_CACHE / "modules"
    if build_cache.is_dir() and modules.is_dir():
        try:
            build_orchestrator(orchestrator, destination, {**env, "GOCACHE": str(build_cache),
                                                           "GOMODCACHE": str(modules), "CGO_ENABLED": "0"}, timeout=45)
            return
        except (OSError, ValueError, subprocess.SubprocessError):
            pass
    build_orchestrator(orchestrator, destination, env)


def prepare_terrascale(workspace_path):
    home = Path.home().resolve()
    try:
        workspace = (home / workspace_path.removeprefix("~/")).resolve(strict=True)
        if not workspace_path.startswith("~/") or workspace == home or not workspace.is_relative_to(home):
            raise ValueError("Invalid TerraScale workspace path")
        orchestrator, product = workspace / "ts-orchestrator", workspace / "terrascale"
        install_terrascale_links(workspace, home)
    except (OSError, ValueError, KeyError, subprocess.SubprocessError):
        return "skills"

    if any(shutil.which(t) is None for t in ("git", "gh", "dotnet", "go", "node", "npm", "python3", "rustc", "codex", "claude", "opencode", "ts")):
        return "toolchain"

    def check_dotnet():
        _run_tool(["dotnet", "--version"], cwd=product)
        if (product / ".config/dotnet-tools.json").is_file():
            _run_tool(["dotnet", "tool", "restore", "--disable-parallel"], cwd=product, timeout=45)

    def run_stage(stage_name, fn):
        try:
            fn()
            return ""
        except (OSError, ValueError, KeyError, subprocess.SubprocessError):
            return stage_name

    with ThreadPoolExecutor(max_workers=3) as pool:
        futs = [
            pool.submit(run_stage, "toolchain", check_dotnet),
            pool.submit(run_stage, "mise", lambda: prepare_mise(home, orchestrator)),
            pool.submit(run_stage, "orchestrator", lambda: install_orchestrator(orchestrator, home)),
        ]
        for fut in futs:
            if failed := fut.result():
                return failed
    return ""


class ProjectRegistrar:
    """Streams T3 project registration as checkouts finish, then verifies all identities."""

    def __init__(self):
        self._lock = threading.Lock()
        self._t3_ready = False
        self._started = False
        self._closed = False
        self._queued = []
        self._expected = {}
        self._futs = []
        self._pool = None
        self._opener = None
        self._base_args = ["--base-dir", str(DATA)]
        self._url = f"http://127.0.0.1:{PORT}/api/projects"
        self._auth = None
        self._session_id = None
        self._active = {}
        self._start_error = None
        self.streamed = False

    def _resolve_root(self, checkout_path):
        home = Path.home().resolve()
        root = (home / checkout_path.removeprefix("~/")).resolve(strict=True)
        if not checkout_path.startswith("~/") or not root.is_relative_to(home) or root == home or not root.is_dir():
            raise RuntimeError("Invalid T3 project path")
        return str(root)

    def _active_projects(self):
        with self._opener.open(Request(self._url, headers=self._auth), timeout=5) as resp:
            return {p["workspaceRoot"]: (p.get("repositoryIdentity") or {}).get("canonicalKey")
                    for p in json.load(resp)["projects"] if not p.get("deletedAt") and not p.get("archivedAt")}

    def _create_project(self, project):
        body = json.dumps({"type": "project.create", "commandId": str(uuid.uuid4()),
                           "projectId": str(uuid.uuid4()), "title": Path(project).name, "workspaceRoot": project}).encode()
        try:
            with self._opener.open(Request(self._url + "/mutate", data=body,
                                           headers={**self._auth, "Content-Type": "application/json"}), timeout=5):
                return None
        except HTTPError as err:
            code = err.code
            err.close()
            if code in {400, 404, 409, 422}:
                return project
            raise RuntimeError(f"T3 could not register the project at {project}") from None

    def _submit_locked(self, checkout_path, repo):
        root = self._resolve_root(checkout_path)
        self._expected[root] = "github.com/" + repo.lower()
        if root not in self._active:
            self._futs.append(self._pool.submit(self._create_project, root))

    def _start_locked(self):
        if self._started or self._closed:
            return
        self._started = True
        try:
            self._opener = local_opener()
            issued = json.loads(subprocess.run(
                [str(T3), "auth", "session", "issue", *self._base_args, "--scope", "orchestration:read",
                 "--scope", "orchestration:operate", "--ttl", "5m", "--label", "t3-agent37-project-registration", "--json"],
                capture_output=True, check=True, timeout=15, text=True,
            ).stdout)
            self._auth = {"Authorization": f"Bearer {issued['token']}"}
            self._session_id = issued["sessionId"]
            self._active = self._active_projects()
            self._pool = ThreadPoolExecutor(max_workers=10)
            for checkout_path, repo in self._queued:
                self._submit_locked(checkout_path, repo)
            self._queued.clear()
        except Exception as err:
            self._start_error = err

    def mark_t3_ready(self):
        with self._lock:
            self._t3_ready = True
            if self._queued:
                self._start_locked()

    def enqueue(self, checkout_path, repo):
        with self._lock:
            self.streamed = True
            if self._closed:
                return
            if not self._t3_ready:
                self._queued.append((checkout_path, repo))
                return
            if not self._started:
                self._queued.append((checkout_path, repo))
                self._start_locked()
            elif self._start_error is None:
                self._submit_locked(checkout_path, repo)

    def close(self):
        with self._lock:
            self._closed = True
            pool, session_id = self._pool, self._session_id
            self._pool = self._session_id = None
        if pool is not None:
            pool.shutdown(wait=False)
        if session_id is not None:
            subprocess.run([str(T3), "auth", "session", "revoke", *self._base_args, session_id], capture_output=True, timeout=10)

    def finish(self, extra_checkouts=None):
        try:
            with self._lock:
                self._t3_ready = True
                if not self._started and (self._queued or extra_checkouts):
                    self._start_locked()
                if self._start_error is not None:
                    raise self._start_error
                if extra_checkouts:
                    for checkout_path, repo in extra_checkouts.items():
                        root = self._resolve_root(checkout_path)
                        if root not in self._expected:
                            self._submit_locked(checkout_path, repo)
                futs = list(self._futs)
                expected = dict(self._expected)
            if not expected:
                return
            fallback = [res for fut in futs if (res := fut.result())]
            for project in fallback:
                if subprocess.run([str(T3), "project", "add", *self._base_args, "--", project],
                                  capture_output=True, timeout=20).returncode != 0 and project not in self._active_projects():
                    raise RuntimeError(f"T3 could not register the project at {project}")
            deadline = time.monotonic() + 10
            while not (expected.items() <= self._active_projects().items()):
                if time.monotonic() >= deadline:
                    raise RuntimeError("T3 did not report the expected repository identities")
                time.sleep(0.1)
        finally:
            self.close()


def register_projects(checkouts):
    if not checkouts:
        return
    registrar = ProjectRegistrar()
    registrar.finish(checkouts)


# ---------------------------------------------------------------------------
# Pairing, owner sessions, peer inspection, and T3 environment CLI
# ---------------------------------------------------------------------------

def create_pairing(address, label="t3-agent37", ttl="5m"):
    out = subprocess.run([str(T3), "auth", "pairing", "create", "--base-dir", str(DATA),
                          "--ttl", ttl, "--label", label, "--base-url", address, "--json"],
                         check=True, capture_output=True, text=True, timeout=15).stdout
    return json.loads(out.strip())


def issue_peer_admin():
    out = subprocess.run([str(T3), "auth", "session", "issue", "--base-dir", str(DATA), "--json",
                          "--ttl", "5m", "--label", "t3-agent37-peer-sync",
                          "--scope", "access:read", "--scope", "access:write",
                          "--scope", "orchestration:read", "--scope", "orchestration:operate"],
                         capture_output=True, text=True, check=True, timeout=15).stdout
    return json.loads(out.strip())


def _check_pairing_once(label, environment_id, labels):
    current = descriptor()
    if current is None:
        raise RuntimeError("T3 is temporarily unavailable while confirming desktop pairing")
    if current["environmentId"] != environment_id:
        return {"changed": True}
    out = subprocess.run([str(T3), "auth", "session", "list", "--base-dir", str(DATA), "--json"],
                         check=True, capture_output=True, text=True, timeout=5).stdout
    sessions = [((s.get("client") or {}).get("label"), s.get("lastConnectedAt"))
                for s in json.loads(out) if s.get("subject") == "one-time-token"]
    redeemed = any(sl == label for sl, _ in sessions)
    confirmed = any(sl in labels and conn for sl, conn in sessions)
    if confirmed:
        atomic_write(ROOT / "desktop-confirmed", environment_id + "\n")
    return {"confirmed": confirmed, "redeemed": redeemed}


def pairing_status(label, environment_id, *previous_labels, wait_seconds=0.0, was_redeemed=False):
    labels = {label, *previous_labels}
    if not all(re.fullmatch(r"t3-agent37-desktop-[a-f0-9]{32}", item) for item in labels):
        raise RuntimeError("Invalid desktop confirmation label")
    deadline = time.monotonic() + min(max(wait_seconds, 0.0), 10.0)
    while True:
        state = _check_pairing_once(label, environment_id, labels)
        if state.get("changed") or state.get("confirmed") or (state.get("redeemed") and not was_redeemed):
            return state
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return state
        time.sleep(min(0.2, remaining))


def inspect_peer(workspace_marker=None):
    if workspace_marker and not (ROOT / workspace_marker).is_file():
        return {"workspacePending": True}
    try:
        status = tailnet_status() or {}
    except TailnetError:
        status = {}
    try:
        server = descriptor()
    except RuntimeError:
        server = None
    dns = ((status.get("Self") or {}).get("DNSName") or "").rstrip(".")
    environment = (server or {}).get("environmentId", "")
    return {
        "environmentId": environment, "url": "https://" + dns if dns else "",
        "offline": tailnet_identity(status) is None,
        "peerProxy": (ROOT / "peer-proxy-active").is_file(),
        "desktopConfirmed": bool(environment) and desktop_confirmed(environment),
    }


def run_environment(args):
    env = dict(os.environ, NODE_USE_ENV_PROXY="1", HTTPS_PROXY="http://127.0.0.1:1055", NO_PROXY="127.0.0.1,localhost,::1")
    return subprocess.run([str(T3), "environment", *args, "--base-dir", str(DATA)], stderr=subprocess.DEVNULL, env=env).returncode


# ---------------------------------------------------------------------------
# Private stage consumption and one-shot `up` / `preboot` orchestration
# ---------------------------------------------------------------------------

def _check_private_dir(path, mask):
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & mask:
        raise RuntimeError("Launch stage directory is not private")


def consume_stage(stage_name):
    if not STAGE_RE.fullmatch(stage_name):
        raise RuntimeError("Invalid launch stage name")
    _check_private_dir(ROOT, 0o022)
    stage_dir = ROOT / stage_name
    _check_private_dir(stage_dir, 0o077)
    spec_file = stage_dir / "spec.json"
    try:
        with os.fdopen(os.open(spec_file, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as src:
            info = os.fstat(src.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_nlink != 1 or info.st_size > 512 * 1024:
                raise RuntimeError("Invalid launch specification file")
            spec_file.unlink()
            return json.load(src)
    finally:
        spec_file.unlink(missing_ok=True)
        stage_dir.rmdir()


def cleanup_stage(stage_name):
    if not STAGE_RE.fullmatch(stage_name):
        raise RuntimeError("Invalid launch stage name")
    try:
        _check_private_dir(ROOT, 0o022)
        stage_dir = ROOT / stage_name
        _check_private_dir(stage_dir, 0o077)
    except FileNotFoundError:
        return
    (stage_dir / "spec.json").unlink(missing_ok=True)
    stage_dir.rmdir()


def run_up(spec):
    if spec.get("requireWorkspaceReady") and not (ROOT / "workspace-ready").is_file():
        return {"workspacePending": True}

    timings, tailscale = {}, spec.get("tailscale")
    github_spec, workspace_spec = spec.get("github"), spec.get("workspace")

    def timed(key, fn):
        t0 = time.monotonic()
        val = fn()
        timings[key] = round(time.monotonic() - t0, 3)
        return val

    def do_tailnet():
        try:
            return timed("tailnet", lambda: publish_tailnet(tailscale["hostname"], tailscale.get("authKey")))
        except TailnetError as err:
            return {"tailnetError": str(err)}

    tools_fut = None
    registrar = ProjectRegistrar() if workspace_spec else None
    with ThreadPoolExecutor(max_workers=6) as pool:
        def start_terrascale_tools():
            nonlocal tools_fut
            if tools_fut is None and workspace_spec:
                tools_fut = pool.submit(timed, "tools", lambda: prepare_terrascale(workspace_spec["path"]))

        def do_workspace():
            out = {}
            if github_spec:
                try:
                    out.update(apply_github_handoff(github_spec))
                except (OSError, ValueError, RuntimeError, subprocess.SubprocessError, KeyError):
                    out["githubError"] = "GitHub handoff failed"
            if workspace_spec:
                out["workspace"] = checkout_workspace(
                    Path.home(), workspace_spec,
                    on_terrascale_ready=start_terrascale_tools,
                    on_checkout_ready=registrar.enqueue if registrar is not None else None,
                )
            return out

        t3_fut = pool.submit(timed, "t3", lambda: setup_t3(
            spec["provider"], spec["name"], bool(spec.get("peerLinks")), spec.get("cliproxyModels")))
        ts_fut = pool.submit(do_tailnet) if tailscale else None
        ws_fut = pool.submit(timed, "workspace", do_workspace) if (github_spec or workspace_spec) else None

        result = t3_fut.result()
        if registrar is not None:
            pool.submit(registrar.mark_t3_ready)
        ts_out = ts_fut.result() if ts_fut else {}
        if ts_out.get("enrollmentRequired"):
            result["enrollmentRequired"] = True
        elif ts_out.get("tailnetError"):
            result["tailnetError"] = ts_out["tailnetError"]
        elif ts_out.get("url"):
            result.update(tailnetUrl=ts_out["url"], tailnetHostname=ts_out.get("hostname", ""), tailnetId=ts_out.get("id", ""))

        # Mint pairing and peer-admin tokens immediately once T3 + Tailscale are ready,
        # overlapping with workspace clones and TerraScale setup.
        pairing_fut = owner_fut = None
        if result.get("tailnetUrl"):
            if ps := spec.get("pairing"):
                pairing_fut = pool.submit(create_pairing, result["tailnetUrl"], ps["label"], ps.get("ttl", "6m"))
            if spec.get("issueOwner") and result.get("peerProxy"):
                owner_fut = pool.submit(issue_peer_admin)

        ws_out = ws_fut.result() if ws_fut else {}
        projects_fut, workspace_ok = None, True
        if workspace_spec:
            ws = ws_out.get("workspace") or {}
            if ws.get("seededWorktrees"):
                result["seededWorktrees"] = ws["seededWorktrees"]
            if ws.get("error") or ws.get("errors"):
                result["workspaceErrors"] = [ws["error"]] if ws.get("error") else ws["errors"]
                workspace_ok = False
                registrar.close()
            elif ws.get("manifestError"):
                result["manifestError"] = ws["manifestError"]
                workspace_ok = False
                registrar.close()
            else:
                if registrar.streamed:
                    projects_fut = pool.submit(timed, "projects", lambda: registrar.finish(ws.get("checkouts") or {}))
                else:
                    registrar.close()
                    projects_fut = pool.submit(timed, "projects", lambda: register_projects(ws.get("checkouts") or {}))
                if ws.get("isTerraScale") and tools_fut is None:
                    start_terrascale_tools()

        if github_spec:
            result.update(github=bool(ws_out.get("github")), gitIdentity=bool(ws_out.get("gitIdentity")))
            if ws_out.get("githubError"):
                result["githubError"] = ws_out["githubError"]

        if projects_fut is not None:
            try:
                projects_fut.result()
            except Exception as err:
                result["projectsError"] = str(err)
                workspace_ok = False
        if tools_fut is not None and (stage := tools_fut.result()):
            result["toolsStage"] = stage
            workspace_ok = False
        if (workspace_spec and workspace_ok) or (spec.get("markWorkspaceReady") and not workspace_spec):
            ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
            (ROOT / "workspace-ready").touch()
            result["workspaceReady"] = True

        if pairing_fut is not None and workspace_ok:
            try:
                result["pairingUrl"] = pairing_fut.result().get("pairUrl", "")
            except Exception:
                pass
        if owner_fut is not None:
            try:
                result["ownerToken"] = owner_fut.result().get("token", "")
            except Exception:
                pass

    result["timings"] = timings
    return result


def preboot():
    """Pre-warm T3, provider config, and tailscaled during container startup."""
    ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)

    def warm_tailnet():
        if shutil.which("tailscaled") and shutil.which("tailscale"):
            try:
                with tailscale_locked():
                    tailnet_start_daemon()
            except Exception:
                pass

    def warm_t3():
        if not ((RUNTIME / ".verified").is_file() and T3.is_file()):
            return
        with (ROOT / "install.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            if name := os.environ.get("T3_AGENT37_NAME", ""):
                save_environment_label(name)
            provider = os.environ.get("T3_AGENT37_PROVIDER", "")
            if os.environ.get("T3_AGENT37_PEER_LINKS") == "1":
                (ROOT / "peer-proxy").touch()
                update_json(DATA / "userdata/settings.json", lambda s: {**s, "linkedEnvironmentsEnabled": True})
            if provider in {"cliproxy", "opencode", "codex", "claude"}:
                try:
                    provider_ready(provider, preboot_mode=True)
                except Exception:
                    return
            elif not (DATA / "userdata/settings.json").is_file():
                return
            install_hook()
            config_home = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
            if (provider != "cliproxy" or (config_home / "opencode/opencode.json").is_file()) and descriptor() is None:
                boot()

    with ThreadPoolExecutor(max_workers=2) as pool:
        for fut in (pool.submit(warm_tailnet), pool.submit(warm_t3)):
            try:
                fut.result()
            except Exception:
                pass


def main(argv):
    os.umask(0o077)
    action, args = argv[0], argv[1:]
    if action == "preboot":
        preboot()
    elif action == "up":
        print(json.dumps(run_up(consume_stage(args[0]))))
    elif action == "cleanup":
        cleanup_stage(args[0])
    elif action == "serve":
        serve()
    elif action == "boot":
        boot()
        tailnet_restore()
    elif action == "pair":
        print(json.dumps(create_pairing(*args)))
    elif action == "pair-status":
        wait_seconds, was_redeemed, positional, i = 0.0, False, [], 0
        while i < len(args):
            if args[i] == "--wait" and i + 1 < len(args):
                wait_seconds, i = float(args[i + 1]), i + 2
            elif args[i] == "--redeemed":
                was_redeemed, i = True, i + 1
            else:
                positional.append(args[i])
                i += 1
        print(json.dumps(pairing_status(*positional, wait_seconds=wait_seconds, was_redeemed=was_redeemed)))
    elif action == "peer-admin":
        print(json.dumps(issue_peer_admin()))
    elif action == "inspect":
        print(json.dumps(inspect_peer(args[0] if args else None)))
    elif action == "env":
        raise SystemExit(run_environment(args))
    elif action == "tailnet-publish":
        ts = consume_stage(args[0])["tailscale"]
        print(json.dumps(publish_tailnet(ts["hostname"], ts.get("authKey"))))
    elif action == "tailnet-leave":
        tailnet_leave()
    elif action == "tailnet-cert-error":
        print(json.dumps(tailnet_cert_error()))
    else:
        raise RuntimeError(f"Unknown action: {action}")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except SystemExit:
        raise
    except subprocess.SubprocessError:
        sys.exit("sandbox command failed")
    except Exception as error:
        sys.exit(str(error) or "sandbox startup failed")
