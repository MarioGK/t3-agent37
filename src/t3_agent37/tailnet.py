"""Run the TerraScale sandbox's own unprivileged Tailscale client."""

from contextlib import contextmanager
import fcntl
import json
from pathlib import Path
import re
import subprocess
import tempfile
import time


ROOT = Path.home() / ".local/share/t3-agent37/tailscale"
HOSTNAME = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?")


class TailnetError(RuntimeError):
    pass


class EnrollmentRequired(TailnetError):
    pass


@contextmanager
def locked():
    ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
    with (ROOT / "lifecycle.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def command(*args):
    return ["tailscale", "--socket", str(ROOT / "tailscaled.sock"), *args]


def status():
    try:
        result = subprocess.run(command("status", "--json", "--peers=false"),
                                capture_output=True, text=True, timeout=3)
        info = json.loads(result.stdout)
    except FileNotFoundError:
        raise TailnetError("Tailscale is missing from the sandbox template") from None
    except (ValueError, subprocess.TimeoutExpired):
        return None
    return info if isinstance(info, dict) and isinstance(info.get("BackendState"), str) else None


def start_daemon():
    info = status()
    daemon = None
    if info is None:
        try:
            daemon = subprocess.Popen(
                ["tailscaled", "--tun=userspace-networking", "--statedir", str(ROOT),
                 "--socks5-server=127.0.0.1:1055", "--socket", str(ROOT / "tailscaled.sock")],
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                start_new_session=True,
            )
        except FileNotFoundError:
            raise TailnetError("The Tailscale daemon is missing from the sandbox template") from None
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if info is not None and info["BackendState"] != "NoState":
            return info
        if daemon is not None and daemon.poll() is not None:
            break
        time.sleep(0.2)
        info = status()
    raise TailnetError("The sandbox Tailscale daemon did not start")


def run_private(*args, timeout, pass_fds=()):
    try:
        result = subprocess.run(command(*args), stdin=subprocess.DEVNULL,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                timeout=timeout, pass_fds=pass_fds)
    except subprocess.TimeoutExpired:
        return False
    return result.returncode == 0


def identity(info):
    if not info or info.get("BackendState") != "Running":
        return None
    peer = info.get("Self")
    if not isinstance(peer, dict) or peer.get("Online") is not True:
        return None
    if not all(isinstance(peer.get(field), str) and peer[field] for field in ("ID", "HostName")):
        return None
    dns_name = peer.get("DNSName")
    return {"id": peer["ID"], "hostname": peer["HostName"],
            "dnsName": dns_name.rstrip(".") if isinstance(dns_name, str) else ""}


def check_enrollment(info, auth_key):
    state = (info or {}).get("BackendState")
    if state == "NeedsLogin" and not auth_key:
        raise EnrollmentRequired("The sandbox needs a fresh Tailscale enrollment key")
    if state == "NeedsMachineAuth":
        raise TailnetError("Tailscale is waiting for device approval")


def join(hostname, auth_key=None):
    if not isinstance(hostname, str) or not HOSTNAME.fullmatch(hostname):
        raise TailnetError("Invalid sandbox Tailscale hostname")
    with locked():
        active = ROOT / "joined"
        if active.exists() and active.read_text() != hostname:
            raise TailnetError("The sandbox already uses another Tailscale hostname")
        info = start_daemon()
        current = identity(info)
        if current and current["hostname"] != hostname:
            raise TailnetError("The running Tailscale client has an unexpected hostname")
        if not current:
            check_enrollment(info, auth_key)
            args = (
                "up", "--hostname", hostname, "--timeout=30s", "--accept-dns=false",
                "--accept-routes=false", "--shields-up=true", "--ssh=false",
            )
            if auth_key:
                with tempfile.TemporaryFile(mode="w+", dir=ROOT) as credential:
                    credential.write(auth_key)
                    credential.flush()
                    fd = credential.fileno()
                    success = run_private(*args, f"--auth-key=file:/proc/self/fd/{fd}",
                                          timeout=35, pass_fds=(fd,))
            else:
                success = run_private(*args, timeout=35)
            info = status()
            check_enrollment(info, auth_key)
            if not success:
                raise TailnetError("Tailscale join failed. Check the sandbox enrollment key and policy.")
            current = identity(info)
        if not current or current["hostname"] != hostname:
            raise TailnetError("Tailscale did not report the sandbox online under its assigned hostname")
        active.write_text(hostname)
        return current


def restore():
    """Resume the same device after a sandbox restart when its state remains valid."""
    if not ROOT.is_dir():
        return
    with locked():
        if (ROOT / "joined").exists() and (ROOT / "tailscaled.state").exists():
            start_daemon()


def leave():
    if not ROOT.is_dir():
        return
    with locked():
        active = ROOT / "joined"
        if not any((ROOT / name).exists() for name in ("joined", "tailscaled.state", "tailscaled.sock")):
            return
        start_daemon()
        if not run_private("logout", timeout=20):
            raise TailnetError("Tailscale logout did not complete. Retry before stopping the sandbox.")
        active.unlink(missing_ok=True)
