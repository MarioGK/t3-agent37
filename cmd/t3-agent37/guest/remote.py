"""Install and start a checksum-pinned T3 runtime on an Agent37 sandbox."""

from copy import deepcopy
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener, urlopen

# Images without a verified T3 installation use this download.
FALLBACK_VERSION = "0.1.11-nightly.20261118.4903"
CHECKSUMS = {
    "x86_64": ("x64", "d7450450c9f71c6daac7fb1ec0d5b22fd7e2365f9e124c730fd5d3dc3e3e1d2a"),
    "aarch64": ("arm64", "3b35b94c96098bdd4282536c7f8ca18447593b8c00af6cf7bfcc2e16be155ceb"),
}
ROOT = Path.home() / ".local/share/t3-agent37"
DATA = ROOT / "data"
SCRIPT = ROOT / "remote.py"
OPENCODE_VERSION = "1.19.5"
MANAGED_MODEL = "agent37/default"
# Agent37 may keep the gateway port assigned at original creation even
# after the template's default changes. Keep the two listeners distinct.
PORT = 4373 if os.environ.get("AGENT37_GATEWAY_PORT") == "3773" else 3773
HOST = os.environ.get("T3_AGENT37_BIND", "0.0.0.0")
DESCRIPTOR = f"http://127.0.0.1:{PORT}/.well-known/t3/environment"


def select_runtime(entrypoint, image_versions, fallback):
    if entrypoint.is_symlink():
        binary = entrypoint.readlink()
        runtime = binary.parent
        if (binary.name == "t3" and runtime.parent == image_versions
                and (runtime / ".verified").is_file() and binary.is_file()):
            return runtime
    return fallback


RUNTIME = select_runtime(Path("/usr/local/bin/t3"), Path("/opt/t3-agent37/versions"),
                         ROOT / "versions" / FALLBACK_VERSION)
VERSION = RUNTIME.name
T3 = RUNTIME / "t3"


def unpack_runtime(package, destination):
    # Some Agent37 images run Python 3.11 without tarfile extraction filters.
    # The pinned archive only needs regular files and directories.
    for member in package:
        relative = Path(member.name)
        if relative.is_absolute() or ".." in relative.parts or not (member.isdir() or member.isfile()):
            raise RuntimeError("Unexpected file in T3 release archive")
        target = destination / relative
        if member.isdir():
            target.mkdir(parents=True, exist_ok=True)
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        with package.extractfile(member) as source, target.open("xb") as output:
            shutil.copyfileobj(source, output)
        target.chmod(0o755 if member.mode & 0o111 else 0o644)


def install_runtime():
    if (RUNTIME / ".verified").is_file() and T3.is_file():
        return
    arch, checksum = CHECKSUMS[platform.machine()]
    archive_name = f"t3-{VERSION}-linux-{arch}.tar.gz"
    source = f"https://github.com/pingdotgg/t3code/releases/download/v{VERSION}/{archive_name}"
    RUNTIME.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".install-", dir=RUNTIME.parent) as directory:
        stage = Path(directory)
        archive = stage / archive_name
        digest = hashlib.sha256()
        with urlopen(source, timeout=90) as response, archive.open("wb") as output:
            while chunk := response.read(1024 * 1024):
                digest.update(chunk)
                output.write(chunk)
                if output.tell() > 512 * 1024 * 1024:
                    raise RuntimeError("T3 release archive exceeds the size limit")
        if digest.hexdigest() != checksum:
            raise RuntimeError("T3 release checksum mismatch")
        unpacked = stage / "unpacked"
        unpacked.mkdir()
        with tarfile.open(archive) as package:
            unpack_runtime(package, unpacked)
        children = list(unpacked.iterdir())
        if len(children) != 1 or not (children[0] / "t3").is_file():
            raise RuntimeError("Unexpected T3 release layout")
        (children[0] / ".verified").touch()
        children[0].rename(RUNTIME)


def install_hook():
    hook = Path.home() / ".agent37/hooks/post-restart.sh"
    hook.parent.mkdir(parents=True, exist_ok=True)
    before = hook.read_text() if hook.exists() else "#!/bin/sh\n"
    line = f"python3 {shlex.quote(str(SCRIPT))} boot"
    if line not in before.splitlines():
        with hook.open("w") as output:
            output.write(before.rstrip("\n") + "\n" + line + "\n")
    hook.chmod(0o700)


def descriptor():
    try:
        with urlopen(DESCRIPTOR, timeout=2) as response:
            result = json.load(response)
    except (URLError, ValueError, OSError):
        return None
    if result.get("serverVersion", "").lstrip("v") != VERSION:
        raise RuntimeError(f"A different T3 server is already listening on port {PORT}")
    return result


def boot():
    with (ROOT / "server.log").open("ab") as log:
        subprocess.Popen([sys.executable, str(SCRIPT), "serve"], stdin=subprocess.DEVNULL,
                         stdout=log, stderr=log, start_new_session=True)


def serve():
    lock = (ROOT / "server.lock").open("a")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return
    active_proxy = ROOT / "peer-proxy-active"
    if (ROOT / "peer-proxy").is_file():
        os.environ["NODE_USE_ENV_PROXY"] = "1"
        os.environ["HTTPS_PROXY"] = "http://127.0.0.1:1055"
        os.environ["NO_PROXY"] = "127.0.0.1,localhost,::1"
        active_proxy.touch()
    else:
        active_proxy.unlink(missing_ok=True)
    os.set_inheritable(lock.fileno(), True)
    os.execv(T3, [str(T3), "serve", "--host", HOST, "--port", str(PORT),
                  "--base-dir", str(DATA), "--mode", "web", "--no-browser"])



def update_json(path, update):
    path.parent.mkdir(parents=True, exist_ok=True)
    before = json.loads(path.read_text()) if path.exists() else {}
    after = update(deepcopy(before))
    if before == after:
        return
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as output:
        temporary = Path(output.name)
        try:
            json.dump(after, output, indent=2)
            output.write("\n")
            output.flush()
            temporary.replace(path)
        finally:
            temporary.unlink(missing_ok=True)


def cli_proxy_profile():
    base = os.environ.get("T3_AGENT37_CLIPROXY_URL", "").rstrip("/")
    key = os.environ.get("T3_AGENT37_CLIPROXY_KEY")
    model = os.environ.get("T3_AGENT37_CLIPROXY_MODEL", "glm-5.3")
    url = urlsplit(base)
    if url.scheme != "https" or not url.hostname or url.username or url.password or url.query or url.fragment or not key:
        raise RuntimeError("CLIProxy needs an HTTPS base URL and client key")
    if not base.endswith("/v1"):
        base += "/v1"

    class NoRedirect(HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None

    try:
        request = Request(base + "/models", headers={"Authorization": f"Bearer {key}"})
        with build_opener(NoRedirect()).open(request, timeout=15) as response:
            catalog = json.load(response)
    except HTTPError as error:
        error.close()
        raise RuntimeError(f"CLIProxy model catalog returned HTTP {error.code}") from None
    except (URLError, OSError, ValueError):
        raise RuntimeError("CLIProxy model catalog is unavailable") from None
    if not isinstance(catalog, dict) or not isinstance(catalog.get("data"), list):
        raise RuntimeError("CLIProxy model catalog has an unexpected format")
    media = ("gpt-image-", "grok-imagine-", "grok-tts", "grok-voice-")
    models = {entry["id"]: {"name": entry["id"]} for entry in catalog["data"]
              if isinstance(entry, dict) and isinstance(entry.get("id"), str)
              and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.:/()-]{0,159}", entry["id"])
              and not entry["id"].startswith(media)}
    if model not in models:
        raise RuntimeError("The requested default model is absent from the CLIProxy catalog")
    return f"cliproxy/{model}", {
        "name": "CLIProxyAPI",
        "options": {
            "baseURL": base, "apiKey": "{env:T3_AGENT37_CLIPROXY_KEY}",
            "timeout": 300000, "headerTimeout": 300000, "chunkTimeout": 300000,
        },
        "models": models,
    }


def configure_managed_opencode(provider="opencode"):
    profile = "cliproxy" if provider == "cliproxy" else "agent37"
    if profile == "cliproxy":
        model, config = cli_proxy_profile()
    else:
        if not os.environ.get("AGENT37_MANAGED_TOKEN") or not os.environ.get("AGENT37_LLM_PROXY_URL"):
            raise RuntimeError("The sandbox has no Agent37 managed model credentials")
        model = MANAGED_MODEL
        config = {
            "name": "Agent37",
            "options": {
                "baseURL": "{env:AGENT37_LLM_PROXY_URL}",
                "apiKey": "{env:AGENT37_MANAGED_TOKEN}",
            },
            "models": {"default": {"name": "Agent37 Default"}},
        }
    config["npm"] = "@ai-sdk/openai-compatible"

    binary = shutil.which("opencode")
    if binary is None:
        prefix = ROOT / "providers" / OPENCODE_VERSION
        binary = prefix / "node_modules/.bin/opencode"
        if not binary.is_file():
            prefix.mkdir(parents=True, exist_ok=True)
            subprocess.run(
                ["npm", "install", "--prefix", str(prefix), "--no-audit", "--no-fund",
                 f"opencode-ai@{OPENCODE_VERSION}"],
                check=True, stdout=subprocess.DEVNULL, timeout=120,
            )

    def configure_opencode(settings):
        settings.setdefault("provider", {})[profile] = config
        if not settings.get("model") or (profile == "cliproxy" and settings["model"] == MANAGED_MODEL):
            settings["model"] = model
        return settings

    def configure_t3(settings):
        instances = settings.setdefault("providerInstances", {})
        opencode = instances.setdefault("opencode", {"driver": "opencode"})
        opencode["enabled"] = True
        opencode.setdefault("config", {})["binaryPath"] = str(binary)
        default = settings.get("defaultModelSelection") or {}
        previous = default.get("instanceId", default.get("provider"))
        if (not previous or previous == "codex"
                or (profile == "cliproxy" and default.get("model") == MANAGED_MODEL)):
            settings["defaultModelSelection"] = {"instanceId": "opencode", "model": model}
        return settings

    config_home = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    update_json(config_home / "opencode/opencode.json", configure_opencode)
    update_json(DATA / "userdata/settings.json", configure_t3)


def provider_ready(provider):
    if provider in {"opencode", "cliproxy"}:
        configure_managed_opencode(provider)
        return True
    command = {"codex": ["codex", "login", "status"], "claude": ["claude", "auth", "status"]}
    if provider not in command or shutil.which(provider) is None:
        raise RuntimeError("The selected provider CLI must be installed on PATH")
    return subprocess.run(
        command[provider], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15,
    ).returncode == 0


def machine_info_with_label(contents, name):
    lines = [line for line in contents.splitlines()
             if not line.strip().startswith("PRETTY_HOSTNAME=")]
    return "\n".join([f'PRETTY_HOSTNAME="{name}"', *lines]) + "\n"


def save_environment_label(name):
    # T3 reads PRETTY_HOSTNAME at startup before it falls back to the machine ID.
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,42}", name):
        return False
    path = Path("/etc/machine-info")
    try:
        try:
            previous = path.read_text()
        except FileNotFoundError:
            previous = ""
        desired = machine_info_with_label(previous, name)
        if desired == previous:
            return True
        with tempfile.NamedTemporaryFile(mode="w", dir=ROOT) as staged:
            staged.write(desired)
            staged.flush()
            command = ["/usr/bin/install", "-m", "0644", "--", staged.name, str(path)]
            if os.geteuid() != 0:
                command = ["sudo", "-n", *command]
            return subprocess.run(command, capture_output=True, timeout=5).returncode == 0
    except (OSError, UnicodeError, subprocess.TimeoutExpired):
        return False


def setup(provider, name, peer_links=False):
    ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
    with (ROOT / "install.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        install_runtime()
        label_saved = save_environment_label(name)
        ready = provider_ready(provider)
        if peer_links:
            (ROOT / "peer-proxy").touch()
            update_json(DATA / "userdata/settings.json", lambda settings: {**settings, "linkedEnvironmentsEnabled": True})
        install_hook()
        result = descriptor()
        if result is None:
            boot()
        for _ in range(60):
            result = descriptor()
            if result is not None:
                print(json.dumps({"environmentId": result["environmentId"], "version": VERSION,
                                  "label": result.get("label", ""), "labelSaved": label_saved,
                                  "port": PORT, "providerReady": ready,
                                  "peerProxy": (ROOT / "peer-proxy-active").is_file()}))
                return
            time.sleep(1)
        raise RuntimeError("T3 did not start. See ~/.local/share/t3-agent37/server.log")


def pair(address):
    result = subprocess.run([
        str(T3), "auth", "pairing", "create", "--base-dir", str(DATA),
        "--ttl", "5m", "--label", "t3-agent37", "--base-url", address,
        "--json",
    ], check=True, capture_output=True, text=True)
    print(result.stdout.strip())


def manage_tailnet(action, *args):
    import tailnet  # the CLI installs this alongside the standalone bootstrap

    if action == "tailnet-leave":
        tailnet.leave()
        return
    if action == "tailnet-restore":
        tailnet.restore()
        return
    if action == "tailnet-serve-peer":
        print(json.dumps({"url": tailnet.serve_peer(PORT)}))
        return
    hostname, *files = args
    key = None
    if files:
        filename, = files
        if not re.fullmatch(r"tailscale-enrollment-[a-f0-9]{32}", filename):
            raise ValueError("Invalid enrollment file name")
        path = ROOT / filename
        with path.open() as source:
            path.unlink()
            key = source.read().strip()
    try:
        print(json.dumps(tailnet.join(hostname, key)))
    except tailnet.EnrollmentRequired:
        print(json.dumps({"enrollmentRequired": True}))


if __name__ == "__main__":
    action = sys.argv[1]
    if action == "setup":
        setup(sys.argv[2], sys.argv[3], "--peer-links" in sys.argv[4:])
    elif action == "serve":
        serve()
    elif action == "boot":
        boot()
        if (ROOT / "tailnet.py").is_file():
            manage_tailnet("tailnet-restore")
    elif action == "pair":
        pair(sys.argv[2])
    elif action == "peer-admin":
        result = subprocess.run([str(T3), "auth", "session", "issue", "--base-dir", str(DATA),
                                 "--json", "--ttl", "5m", "--label", "t3-agent37-peer-sync",
                                 "--scope", "access:read", "--scope", "access:write",
                                 "--scope", "orchestration:read", "--scope", "orchestration:operate"],
                                capture_output=True, text=True, check=True)
        print(result.stdout.strip())
    elif action in {"tailnet-join", "tailnet-leave", "tailnet-serve-peer"}:
        manage_tailnet(action, *sys.argv[2:])
