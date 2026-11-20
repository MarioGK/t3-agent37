"""Set up GitHub.com access from a private, one-use workstation handoff."""

import hashlib
import json
import os
import platform
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path
from urllib.request import HTTPRedirectHandler, Request, build_opener, urlopen

ROOT = Path.home() / ".local/share/t3-agent37"
VERSION = "2.105.0"
CHECKSUMS = {
    "x86_64": ("amd64", "79e7ab5db738c48777b696e374f8b428f0647debfb424f44d2f426370407fe2f"),
    "aarch64": ("arm64", "b1278c75107423d818f1937fd92cafc9e5e9b606631568f68d7fb816f5885a02"),
}
TOKEN = re.compile(r"[A-Za-z0-9_.-]{1,4096}")
AUTH_ENV = {"GH_DEBUG", "DEBUG", "GIT_TRACE", "GIT_TRACE_CURL"}


def command(args, *, data=None):
    env = os.environ.copy()
    for key in AUTH_ENV:
        env.pop(key, None)
    env["GH_PROMPT_DISABLED"] = "1"
    env["GH_NO_UPDATE_NOTIFIER"] = "1"
    return subprocess.run(args, input=data, capture_output=True, text=True, timeout=25, env=env)


def check_directory(path, mask):
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & mask:
        raise RuntimeError("GitHub handoff directory is not private")


def stage_directory(name, action):
    if not re.fullmatch(r"github-handoff-[a-f0-9]{32}", name):
        raise RuntimeError("Invalid GitHub handoff name")
    ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
    check_directory(ROOT, 0o022)
    path = ROOT / name
    if action == "prepare":
        path.mkdir(mode=0o700)
    else:
        check_directory(path, 0o077)
    return path


def read_once(path):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as source:
        info = os.fstat(source.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_nlink != 1
                or info.st_size > 8192):
            raise RuntimeError("Invalid private GitHub handoff")
        path.unlink()
        return json.load(source)


def github_binary():
    installed = shutil.which("gh")
    if installed:
        return installed
    target = Path.home() / ".local/bin/gh"
    if target.is_file() and os.access(target, os.X_OK):
        return str(target)
    arch, expected = CHECKSUMS[platform.machine()]
    package = f"gh_{VERSION}_linux_{arch}"
    url = f"https://github.com/cli/cli/releases/download/v{VERSION}/{package}.tar.gz"
    target.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".gh-", dir=target.parent) as subdir:
        stage = Path(subdir)
        archive = stage / "gh.tar.gz"
        digest = hashlib.sha256()
        with urlopen(url, timeout=45) as response, archive.open("wb") as output:
            while chunk := response.read(1024 * 1024):
                digest.update(chunk)
                output.write(chunk)
                if output.tell() > 50 * 1024 * 1024:
                    raise RuntimeError("GitHub CLI archive exceeds the size limit")
        if digest.hexdigest() != expected:
            raise RuntimeError("GitHub CLI archive checksum mismatch")
        binary = stage / "gh"
        with tarfile.open(archive) as source:
            member = source.getmember(f"{package}/bin/gh")
            if not member.isfile():
                raise RuntimeError("Invalid GitHub CLI archive")
            with source.extractfile(member) as content, binary.open("xb") as output:
                shutil.copyfileobj(content, output)
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


def valid_hosts_file(path):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise RuntimeError("GitHub CLI credential file is not private")


def install_app_login(token, path):
    # Installation tokens use GitHub's x-access-token identity and cannot use
    # the user lookup in gh auth login. Validate before creating a fresh store.
    class NoRedirect(HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None

    request = Request(
        "https://api.github.com/installation/repositories?per_page=1",
        headers={"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json"},
    )
    with build_opener(NoRedirect()).open(request, timeout=15) as response:
        if response.status != 200:
            raise RuntimeError("GitHub installation access was refused")
    path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, delete=False) as output:
        staged = Path(output.name)
        try:
            output.write(
                "github.com:\n"
                "    git_protocol: https\n"
                f"    oauth_token: {token}\n"
                "    user: x-access-token\n"
            )
            output.flush()
            os.link(staged, path)
        finally:
            staged.unlink(missing_ok=True)


def git_config(*args, missing_ok=False):
    result = command(["git", "config", *args])
    if result.returncode != 0 and not (missing_ok and result.returncode == 1):
        raise RuntimeError("Could not configure Git for this sandbox")
    return result.stdout


def configure_git_for_github(gh):
    key = "credential.https://github.com.helper"
    helper = "!" + shlex.quote(gh) + " auth git-credential"
    helpers = git_config("--get-all", key, missing_ok=True).splitlines()
    if not any(helpers):
        # Add a fallback for GitHub.com alone. Leave configured helpers in place.
        git_config("--global", "--add", key, helper)
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
    if command(check).returncode != 0:
        path = github_hosts()
        valid_hosts_file(path)
        if token.startswith("ghs_"):
            install_app_login(token, path)
        else:
            login = command(
                [gh, "auth", "login", "--hostname", "github.com", "--with-token", "--git-protocol", "https"],
                data=token + "\n",
            )
            if login.returncode != 0:
                raise RuntimeError("GitHub did not accept the workstation login")
        valid_hosts_file(path)
        if command(check).returncode != 0:
            raise RuntimeError("GitHub CLI did not retain the sandbox login")
    configure_git_for_github(gh)


def main(name, action):
    os.umask(0o077)
    try:
        path = stage_directory(name, action)
    except FileNotFoundError:
        if action == "cleanup":
            return
        raise
    if action == "prepare":
        return
    file = path / "access.json"
    try:
        if action == "apply":
            payload = read_once(file)
            if not isinstance(payload, dict) or any(not isinstance(value, str) for value in payload.values()):
                raise RuntimeError("Invalid GitHub handoff data")
            token = payload.get("token", "")
            if token and not TOKEN.fullmatch(token):
                raise RuntimeError("Invalid GitHub handoff token")
            git_identity = configure_identity(payload)
            if token:
                configure_github(token)
            print(json.dumps({"github": bool(token), "gitIdentity": git_identity}))
        elif action != "cleanup":
            raise RuntimeError("Unknown GitHub handoff action")
    finally:
        file.unlink(missing_ok=True)
        path.rmdir()


if __name__ == "__main__":
    try:
        main(*sys.argv[1:])
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError, KeyError):
        # Captured subprocess errors can contain tokens. Exec only needs the exit status.
        sys.exit("GitHub handoff failed")
