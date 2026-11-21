"""Clone or fast-forward a GitHub checkout while preserving local work."""

import json
import os
import re
import stat
import subprocess
import sys
import tempfile
from pathlib import Path


def git_environment():
    env = {
        key: value for key, value in os.environ.items()
        if key not in {"GH_DEBUG", "GIT_CURL_VERBOSE"} and not key.startswith("GIT_TRACE")
    }
    env.update(GIT_TERMINAL_PROMPT="0", GH_PROMPT_DISABLED="1", GCM_INTERACTIVE="Never")
    return env


def matches_checkout(path, repo, env):
    if path.is_symlink() or not path.is_dir():
        return False
    root = subprocess.run(
        ["git", "-C", str(path), "rev-parse", "--show-toplevel", "--verify", "HEAD^{commit}"],
        capture_output=True, text=True, timeout=5, env=env,
    )
    if root.returncode != 0 or Path(root.stdout.partition("\n")[0]) != path.resolve():
        return False
    result = subprocess.run(
        ["git", "-C", str(path), "config", "--local", "--get", "remote.origin.url"],
        capture_output=True, text=True, timeout=5, env=env,
    )
    origin = result.stdout.strip().lower().removesuffix("/").removesuffix(".git")
    slug = repo.lower()
    return result.returncode == 0 and origin in {
        "https://github.com/" + slug,
        "git@github.com:" + slug,
        "ssh://git@github.com/" + slug,
    }


def valid_path_part(part):
    return bool(part) and part not in {".", ".."} and "/" not in part and "\\" not in part


def refresh_checkout(checkout, env, workspace_root):
    git = ["git", "-C", str(checkout)]
    branch = subprocess.run([*git, "symbolic-ref", "--quiet", "--short", "HEAD"],
                            capture_output=True, text=True, timeout=5, env=env)
    if branch.returncode != 0:
        return "existing"
    branch_name = branch.stdout.strip()
    remote = subprocess.run([*git, "config", "--get", f"branch.{branch_name}.remote"],
                            capture_output=True, text=True, timeout=5, env=env)
    if remote.returncode != 0 or remote.stdout.strip() != "origin":
        return "existing"
    merge_ref = subprocess.run([*git, "config", "--get", f"branch.{branch_name}.merge"],
                               capture_output=True, text=True, timeout=5, env=env)
    ref = merge_ref.stdout.strip()
    if merge_ref.returncode != 0 or not ref.startswith("refs/heads/"):
        return "pull_conflict"
    fetched = subprocess.run(["timeout", "--kill-after=5s", "120s", *git, "fetch", "--quiet",
                              "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
                              "origin", ref], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
    if fetched.returncode in {124, 137}:
        return "timeout"
    if fetched.returncode != 0:
        return "fetch_failed"
    current = subprocess.run([*git, "symbolic-ref", "--quiet", "--short", "HEAD"],
                             capture_output=True, text=True, timeout=5, env=env)
    if current.stdout.strip() != branch_name:
        return "pull_conflict"
    upstream = subprocess.run([*git, "rev-parse", "--verify", "@{upstream}^{commit}"],
                              capture_output=True, text=True, timeout=5, env=env)
    if upstream.returncode != 0:
        return "pull_conflict"
    revision = upstream.stdout.strip()
    if workspace_root:
        nested = {entry.name for entry in checkout.iterdir() if (entry / ".git").exists()}
        changes = subprocess.run([*git, "diff", "--name-only", "-z", "HEAD", revision],
                                 capture_output=True, text=True, timeout=10, env=env)
        if changes.returncode != 0 or any(name.split("/", 1)[0] in nested for name in changes.stdout.split("\0")):
            return "pull_conflict"
    # Like a fast-forward pull, with Git's additional guard against replacing ignored files.
    merged = subprocess.run([*git, "merge", "--quiet", "--ff-only", "--no-autostash", "--no-overwrite-ignore", revision],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30, env=env)
    return "existing" if merged.returncode == 0 else "pull_conflict"


def clone_repository(home, workspace_path, repo, directory, ref):
    parts = workspace_path.removeprefix("~/").split("/")
    if (not workspace_path.startswith("~/") or not all(map(valid_path_part, parts))
            or not re.fullmatch(r"[A-Za-z0-9-]+/[A-Za-z0-9_.-]+", repo)
            or not valid_path_part(repo.split("/")[1])
            or (directory and not valid_path_part(directory)) or ref.startswith("-")):
        raise ValueError("Invalid workspace repository")
    parent = home
    for part in (parts if directory else parts[:-1]):
        parent /= part
        parent.mkdir(exist_ok=True)
        if not stat.S_ISDIR(parent.lstat().st_mode):
            return "filesystem_error"

    target = parent / (directory or parts[-1])
    env = git_environment()
    if target.exists() or target.is_symlink():
        if matches_checkout(target, repo, env):
            return refresh_checkout(target, env, not directory)
        if directory or not can_adopt_root(target):
            return "conflict"

    with tempfile.TemporaryDirectory(prefix=".t3-agent37-clone-", dir=parent) as stage:
        checkout = Path(stage) / target.name
        command = ["timeout", "--kill-after=5s", "210s", "git", "clone", "--quiet"]
        if ref:
            command.extend(["--branch", ref])
        command.extend(["--", f"https://github.com/{repo}.git", str(checkout)])
        result = subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
        if result.returncode in {124, 137}:
            return "timeout"
        if result.returncode != 0:
            return "clone_failed"

        if move_without_overwrite(checkout, target):
            return "cloned"
        if not directory and can_adopt_root(target):
            for entry in checkout.iterdir():
                if entry.name != ".git" and (target / entry.name / ".git").exists():
                    return "conflict"
            # Git keeps existing files without --force. Verify content before installing
            # the index and config, including on a retry after an interrupted checkout.
            git = ["git", f"--git-dir={checkout / '.git'}", f"--work-tree={target}"]
            subprocess.run([*git, "checkout-index", "--all"], timeout=30, stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, env=env)
            result = subprocess.run([*git, "diff", "--quiet"], timeout=30, stdout=subprocess.DEVNULL,
                                    stderr=subprocess.DEVNULL, env=env)
            if result.returncode == 0 and move_without_overwrite(checkout / ".git", target / ".git"):
                return "cloned"
        if target.exists() or target.is_symlink():
            return refresh_checkout(target, env, not directory) if matches_checkout(target, repo, env) else "conflict"
        return "filesystem_error"


def can_adopt_root(path):
    git_dir = path / ".git"
    return path.is_dir() and not path.is_symlink() and not git_dir.exists() and not git_dir.is_symlink()


def move_without_overwrite(source, destination):
    result = subprocess.run(
        ["mv", "--no-clobber", "--no-target-directory", "--", str(source), str(destination)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    return result.returncode == 0 and not source.exists()


if __name__ == "__main__":
    try:
        status = clone_repository(Path.home(), *sys.argv[1:])
    except subprocess.TimeoutExpired:
        status = "timeout"
    except (OSError, ValueError):
        status = "filesystem_error"
    print(json.dumps({"status": status}))
