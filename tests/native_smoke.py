"""Exercise the pinned T3 binary, restart hook, and native pairing through the relay."""

import asyncio
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from urllib.request import urlopen

import aiohttp
from aiohttp.test_utils import TestServer

from t3_agent37 import remote
from t3_agent37.relay import application


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


async def pair_through_relay(origin, identifier, credential):
    port = free_port()
    relay = application(origin, "fixture-only", port)
    async with TestServer(relay) as server, aiohttp.ClientSession() as client:
        headers = {"Host": f"127.0.0.1:{port}", "Origin": "t3code://app"}
        async with client.get(server.make_url("/.well-known/t3/environment"), headers=headers) as result:
            assert result.status == 200
            assert (await result.json())["environmentId"] == identifier
        data = {
            "grant_type": "urn:ietf:params:oauth:grant-type:token-exchange",
            "subject_token": credential,
            "subject_token_type": "urn:t3:params:oauth:token-type:environment-bootstrap",
            "requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
        }
        async with client.post(server.make_url("/oauth/token"), headers=headers, data=data) as result:
            assert result.status == 200, result.status
            assert (await result.json())["access_token"]


def main():
    with tempfile.TemporaryDirectory() as directory:
        home = Path(directory)
        os.environ["HOME"] = directory
        os.environ["T3_AGENT37_BIND"] = "127.0.0.1"
        bin_path = home / "bin"
        bin_path.mkdir()
        if shutil.which("codex") is None:
            (bin_path / "codex").symlink_to(shutil.which("true"))
        os.environ["PATH"] = f"{bin_path}{os.pathsep}{os.environ['PATH']}"

        remote.ROOT = home / ".local/share/t3-agent37"
        remote.ROOT.mkdir(parents=True)
        remote.RUNTIME = remote.ROOT / "versions" / remote.VERSION
        remote.T3 = remote.RUNTIME / "t3"
        remote.SCRIPT = remote.ROOT / "remote.py"
        shutil.copyfile(remote.__file__, remote.SCRIPT)
        remote.install_runtime()
        remote.install_hook()
        remote.install_hook()
        hook = home / ".agent37/hooks/post-restart.sh"
        assert hook.read_text().count("remote.py") == 1

        # Use a temporary port in smoke tests to stay clear of workstation T3.
        port = free_port()
        origin = f"http://127.0.0.1:{port}"
        cmd = [str(remote.T3), "serve", "--host", "127.0.0.1", "--port", str(port),
               "--base-dir", str(remote.ROOT / "data"), "--mode", "web", "--no-browser"]

        def check_server(expected=None):
            with subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) as process:
                try:
                    for _ in range(60):
                        try:
                            with urlopen(origin + "/.well-known/t3/environment", timeout=1) as result:
                                info = json.load(result)
                                break
                        except OSError:
                            if process.poll() is not None:
                                raise RuntimeError("T3 exited during startup")
                            time.sleep(0.25)
                    else:
                        raise RuntimeError("T3 did not start")
                    identifier = info["environmentId"]
                    assert info["serverVersion"].lstrip("v") == remote.VERSION
                    if expected is not None:
                        assert identifier == expected
                    else:
                        pairing = subprocess.run([
                            str(remote.T3), "auth", "pairing", "create", "--base-dir", str(remote.ROOT / "data"),
                            "--ttl", "5m", "--label", "smoke", "--json",
                        ], check=True, capture_output=True, text=True)
                        credential = json.loads(pairing.stdout)["credential"]
                        asyncio.run(pair_through_relay(origin, identifier, credential))
                    return identifier
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()

        check_server(check_server())
        print("Pinned T3 started, paired through the loopback relay, and retained its environment ID after restart")


if __name__ == "__main__":
    sys.exit(main())
