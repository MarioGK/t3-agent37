"""Install and start a checksum-pinned T3 runtime on an Agent37 sandbox."""

import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
from urllib.error import URLError
from urllib.request import urlopen

VERSION = "0.1.11-nightly.20261118.4903"
CHECKSUMS = {
    "x86_64": ("x64", "d7450450c9f71c6daac7fb1ec0d5b22fd7e2365f9e124c730fd5d3dc3e3e1d2a"),
    "aarch64": ("arm64", "3b35b94c96098bdd4282536c7f8ca18447593b8c00af6cf7bfcc2e16be155ceb"),
}
ROOT = Path.home() / ".local/share/t3-agent37"
DATA = ROOT / "data"
SCRIPT = ROOT / "remote.py"
PREBUILT = Path("/opt/t3-agent37/versions") / VERSION
RUNTIME = PREBUILT if (PREBUILT / ".verified").is_file() else ROOT / "versions" / VERSION
T3 = RUNTIME / "t3"
# Agent37 may keep the gateway port assigned at original creation even
# after the template's default changes. Keep the two listeners distinct.
PORT = 4373 if os.environ.get("AGENT37_GATEWAY_PORT") == "3773" else 3773
HOST = os.environ.get("T3_AGENT37_BIND", "0.0.0.0")
DESCRIPTOR = f"http://127.0.0.1:{PORT}/.well-known/t3/environment"


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
            package.extractall(unpacked, filter="data")
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
    os.set_inheritable(lock.fileno(), True)
    os.execv(T3, [str(T3), "serve", "--host", HOST, "--port", str(PORT),
                  "--base-dir", str(DATA), "--mode", "web", "--no-browser"])


def setup(provider):
    ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
    with (ROOT / "install.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        install_runtime()
        if provider not in {"codex", "claude", "opencode"} or shutil.which(provider) is None:
            raise RuntimeError("The selected provider CLI must be installed on PATH")
        install_hook()
        result = descriptor()
        if result is None:
            boot()
        for _ in range(60):
            result = descriptor()
            if result is not None:
                print(json.dumps({"environmentId": result["environmentId"], "version": VERSION, "port": PORT}))
                return
            time.sleep(1)
        raise RuntimeError("T3 did not start. See ~/.local/share/t3-agent37/server.log")


def pair(port):
    result = subprocess.run([
        str(T3), "auth", "pairing", "create", "--base-dir", str(DATA),
        "--ttl", "5m", "--label", "t3-agent37", "--base-url", f"http://127.0.0.1:{int(port)}",
        "--json",
    ], check=True, capture_output=True, text=True)
    print(result.stdout.strip())


if __name__ == "__main__":
    action = sys.argv[1]
    if action == "setup":
        setup(sys.argv[2])
    elif action == "serve":
        serve()
    elif action == "boot":
        boot()
    elif action == "pair":
        pair(sys.argv[2])
