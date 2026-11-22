"""Install and start a checksum-pinned T3 runtime on an Agent37 sandbox."""

from copy import deepcopy
from concurrent.futures import ThreadPoolExecutor
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
import uuid
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener, urlopen

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
    paths = [str(ROOT / "bin"), str(Path.home() / ".local/bin"), *os.get_exec_path()]
    os.environ["PATH"] = os.pathsep.join(dict.fromkeys(paths))
    if (ROOT / "bin/claude").is_file() and os.environ.get("T3_AGENT37_CLIPROXY_URL"):
        from claude_proxy import claude_base_url

        os.environ["ANTHROPIC_BASE_URL"] = claude_base_url(os.environ["T3_AGENT37_CLIPROXY_URL"])
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


def configure_managed_providers(provider):
    profile = "cliproxy" if provider == "cliproxy" else "agent37"
    if profile == "cliproxy":
        from claude_proxy import configure_claude, configure_claude_settings, configure_claude_t3
        from codex_proxy import configure as configure_codex

        model, config = cli_proxy_profile()
        base_url = config["options"]["baseURL"]
        model_id = model.removeprefix("cliproxy/")
        claude = configure_claude(ROOT, base_url, model_id)
        codex = configure_codex(base_url, model_id, config["models"])
        claude_home = Path(os.environ.get("CLAUDE_CONFIG_DIR") or Path.home() / ".claude").expanduser()
        update_json(claude_home / "settings.json", lambda settings: configure_claude_settings(settings, base_url))
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
        if profile == "cliproxy":
            configure_claude_t3(settings, claude, model_id)
            instance = instances.setdefault("codex", {})
            instance.update(driver="codex", enabled=True)
            codex_config = instance.setdefault("config", {})
            custom_models = codex_config.get("customModels", [])
            known = {entry if isinstance(entry, str) else entry.get("slug") for entry in custom_models}
            new_models = [entry for entry in codex["customModels"] if entry not in known]
            codex_config.update(codex, customModels=[*custom_models, *new_models])
        default = settings.get("defaultModelSelection") or {}
        previous = default.get("instanceId", default.get("provider"))
        if (not previous or (previous == "codex" and profile == "agent37")
                or (profile == "cliproxy" and default.get("model") == MANAGED_MODEL)):
            settings["defaultModelSelection"] = {"instanceId": "opencode", "model": model}
        return settings

    config_home = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    update_json(config_home / "opencode/opencode.json", configure_opencode)
    update_json(DATA / "userdata/settings.json", configure_t3)


def provider_ready(provider):
    if provider in {"opencode", "cliproxy"}:
        configure_managed_providers(provider)
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
            deadline = time.monotonic() + 60
            delay = 0.2
            while result is None:
                result = descriptor()
                if result is not None:
                    break
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise RuntimeError("T3 did not start. See ~/.local/share/t3-agent37/server.log")
                time.sleep(min(delay, remaining))
                delay = min(delay * 2, 1)
        print(json.dumps({"environmentId": result["environmentId"], "version": VERSION,
                          "label": result.get("label", ""), "labelSaved": label_saved,
                          "port": PORT, "providerReady": ready,
                          "peerProxy": (ROOT / "peer-proxy-active").is_file(),
                          "desktopConfirmed": desktop_confirmed(result["environmentId"])}))


def register_projects(checkouts):
    home = Path.home().resolve()
    expected = {}
    for path, repo in checkouts.items():
        root = (home / path.removeprefix("~/")).resolve(strict=True)
        if not path.startswith("~/") or not root.is_relative_to(home) or root == home or not root.is_dir():
            raise RuntimeError("Invalid T3 project path")
        expected[str(root)] = "github.com/" + repo.lower()

    class NoRedirect(HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None

    opener = build_opener(ProxyHandler({}), NoRedirect())
    base_args = ["--base-dir", str(DATA)]
    issued = json.loads(subprocess.run(
        [str(T3), "auth", "session", "issue", *base_args, "--scope", "orchestration:read", "--scope", "orchestration:operate",
         "--ttl", "5m", "--label", "t3-agent37-project-registration", "--json"],
        capture_output=True, check=True, timeout=15, text=True,
    ).stdout)
    url = f"http://127.0.0.1:{PORT}/api/projects"
    authorization = {"Authorization": f"Bearer {issued['token']}"}

    def active_projects():
        request = Request(url, headers=authorization)
        with opener.open(request, timeout=5) as response:
            projects = json.load(response)["projects"]
        return {project["workspaceRoot"]: (project.get("repositoryIdentity") or {}).get("canonicalKey")
                for project in projects if not project.get("deletedAt") and not project.get("archivedAt")}

    def create_project(project):
        command = {"type": "project.create", "commandId": str(uuid.uuid4()),
                   "projectId": str(uuid.uuid4()), "title": Path(project).name,
                   "workspaceRoot": project}
        request = Request(url + "/mutate", data=json.dumps(command).encode(),
                          headers={**authorization, "Content-Type": "application/json"})
        try:
            with opener.open(request, timeout=5):
                return None
        except HTTPError as error:
            status = error.code
            error.close()
            if status in {400, 404, 409, 422}:
                return project
            raise RuntimeError(f"T3 could not register the project at {project}") from None

    try:
        active = active_projects()
        missing = sorted(expected.keys() - active.keys())
        if missing:
            with ThreadPoolExecutor(max_workers=min(4, len(missing))) as worker:
                fallback = [project for project in worker.map(create_project, missing) if project]
            for project in fallback:
                # Use the native CLI for older servers and archived projects.
                result = subprocess.run([str(T3), "project", "add", *base_args, "--", project],
                                        capture_output=True, timeout=20)
                if result.returncode != 0 and project not in active_projects():
                    raise RuntimeError(f"T3 could not register the project at {project}")
        deadline = time.monotonic() + 10
        while True:
            active = active_projects()
            if expected.items() <= active.items():
                return
            if time.monotonic() >= deadline:
                raise RuntimeError("T3 did not report the expected repository identities")
            time.sleep(0.2)
    finally:
        subprocess.run(
            [str(T3), "auth", "session", "revoke", *base_args, issued["sessionId"]],
            capture_output=True, timeout=10,
        )


def pair(address, label="t3-agent37", ttl="5m"):
    result = subprocess.run([
        str(T3), "auth", "pairing", "create", "--base-dir", str(DATA),
        "--ttl", ttl, "--label", label, "--base-url", address,
        "--json",
    ], check=True, capture_output=True, text=True)
    print(result.stdout.strip())


def desktop_confirmed(environment_id):
    try:
        return (ROOT / "desktop-confirmed").read_text().strip() == environment_id
    except FileNotFoundError:
        return False


def pairing_status(label, environment_id, *previous_labels):
    labels = {label, *previous_labels}
    if not all(re.fullmatch(r"t3-agent37-desktop-[a-f0-9]{32}", item) for item in labels):
        raise RuntimeError("Invalid desktop confirmation label")
    current = descriptor()
    if current is None:
        raise RuntimeError("T3 is temporarily unavailable while confirming desktop pairing")
    if current["environmentId"] != environment_id:
        print(json.dumps({"changed": True}))
        return
    result = subprocess.run([
        str(T3), "auth", "session", "list", "--base-dir", str(DATA), "--json",
    ], check=True, capture_output=True, text=True, timeout=5)
    # The grant label is carried to the session during token exchange. The
    # first RPC connection follows redemption by the confirmed desktop.
    sessions = [((session.get("client") or {}).get("label"), session.get("lastConnectedAt"))
                for session in json.loads(result.stdout) if session.get("subject") == "one-time-token"]
    redeemed = any(session_label == label for session_label, _ in sessions)
    confirmed = any(session_label in labels and connected_at for session_label, connected_at in sessions)
    if confirmed:
        with tempfile.NamedTemporaryFile(mode="w", dir=ROOT, delete=False) as staged:
            path = Path(staged.name)
            try:
                staged.write(environment_id + "\n")
                staged.flush()
                path.replace(ROOT / "desktop-confirmed")
            finally:
                path.unlink(missing_ok=True)
    print(json.dumps({"confirmed": confirmed, "redeemed": redeemed}))


def manage_tailnet(action, *args):
    import tailnet  # the CLI installs this alongside the standalone bootstrap

    if action == "tailnet-leave":
        tailnet.leave()
        return
    if action == "tailnet-restore":
        tailnet.restore()
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
        peer = tailnet.join(hostname, key)
        peer["url"] = tailnet.serve_peer(PORT)
        print(json.dumps(peer))
    except tailnet.EnrollmentRequired:
        print(json.dumps({"enrollmentRequired": True}))


if __name__ == "__main__":
    action = sys.argv[1]
    if action == "setup":
        setup(sys.argv[2], sys.argv[3], "--peer-links" in sys.argv[4:])
    elif action == "register-projects":
        register_projects(json.loads(sys.argv[2]))
    elif action == "serve":
        serve()
    elif action == "boot":
        boot()
        if (ROOT / "tailnet.py").is_file():
            manage_tailnet("tailnet-restore")
    elif action == "pair":
        pair(*sys.argv[2:])
    elif action == "pair-status":
        pairing_status(*sys.argv[2:])
    elif action == "peer-admin":
        result = subprocess.run([str(T3), "auth", "session", "issue", "--base-dir", str(DATA),
                                 "--json", "--ttl", "5m", "--label", "t3-agent37-peer-sync",
                                 "--scope", "access:read", "--scope", "access:write",
                                 "--scope", "orchestration:read", "--scope", "orchestration:operate"],
                                capture_output=True, text=True, check=True)
        print(result.stdout.strip())
    elif action in {"tailnet-publish", "tailnet-leave"}:
        manage_tailnet(action, *sys.argv[2:])
