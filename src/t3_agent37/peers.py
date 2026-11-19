"""Pair a sandbox and its private T3 peers with short-lived native grants."""

import asyncio
from dataclasses import dataclass
import json
import os
from pathlib import Path
import re
import subprocess
from urllib.parse import urlsplit

import aiohttp

from .api import Agent37Error


ACCESS = "approval-required"
CAPABILITIES = ("peerLinks", "linkFence", "mcpModeLimitHeader", "peerLinkPairingCodes")


@dataclass(frozen=True)
class Peer:
    host: str
    url: str
    environment_id: str


def private_origin(url):
    parsed = urlsplit(url)
    if (parsed.scheme != "https" or not parsed.hostname or not parsed.hostname.endswith(".ts.net")
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path not in ("", "/")):
        raise Agent37Error("T3 peers need a private Tailscale HTTPS origin")
    parsed.port  # reject invalid port syntax before opening SSH or HTTP connections
    return url.rstrip("/")


def configured_peers():
    override = os.environ.get("T3_AGENT37_PEERS_FILE")
    if override == "":
        return ()
    config = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    path = Path(override).expanduser() if override else config / "t3-agent37/peers.json"
    try:
        contents = path.read_text()
    except FileNotFoundError:
        if override is None:
            return ()
        raise Agent37Error("The configured T3 peers file does not exist") from None
    except OSError:
        raise Agent37Error("Could not read the configured T3 peers file") from None
    try:
        entries = json.loads(contents)
        if not isinstance(entries, list):
            raise ValueError
        peers = []
        for item in entries:
            host = item["ssh"]
            environment_id = item["environmentId"]
            if (not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]*", host)
                    or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}", environment_id)):
                raise ValueError
            peers.append(Peer(host, private_origin(item["url"]), environment_id))
        if len({peer.environment_id for peer in peers}) != len(peers):
            raise ValueError
        return tuple(peers)
    except (TypeError, AttributeError, KeyError, ValueError):
        raise Agent37Error("Invalid T3 peer configuration") from None


def issue_over_ssh(peer):
    # The owner-authorized command emits a temporary token through the captured SSH stream.
    script = '''set -eu
for binary in "$HOME/.t3/bin/t3" "$HOME/.local/bin/t3" "$HOME/.local/share/mise/shims/t3"; do
    if test -x "$binary"; then
        exec "$binary" auth session issue --json --ttl 5m --label t3-agent37-peer-sync \\
            --scope access:read --scope access:write --scope orchestration:read --scope orchestration:operate
    fi
done
exit 127
'''
    ssh = ("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=5")
    command = (*ssh, peer.host, "sh", "-s")
    try:
        result = subprocess.run(command, input=script,
                                capture_output=True, text=True, check=True, timeout=20)
        return json.loads(result.stdout)["token"]
    except (OSError, subprocess.SubprocessError, ValueError, KeyError):
        raise Agent37Error(f"Could not authorize T3 peer administration over SSH on {peer.host}") from None


class T3PeerAPI:
    def __init__(self, session, name, origin, environment_id, headers=None):
        self.session = session
        self.name = name
        self.origin = origin
        self.environment_id = environment_id
        self.headers = {"X-T3-Orchestration-Protocol": "2", **(headers or {})}

    async def json(self, method, route, *, token=None, data=None):
        headers = self.headers if token is None else {**self.headers, "Authorization": f"Bearer {token}"}
        try:
            async with self.session.request(method, self.origin + route, headers=headers,
                                            json=data, allow_redirects=False) as response:
                if not 200 <= response.status < 300:
                    raise Agent37Error(f"{self.name} returned HTTP {response.status} for {route}")
                return await response.json()
        except (aiohttp.ClientError, asyncio.TimeoutError, ValueError):
            raise Agent37Error(f"Could not read {route} from {self.name}") from None

    async def verify(self, *, attempts=1):
        for attempt in range(attempts):
            try:
                row = await self.json("GET", "/.well-known/t3/environment")
                break
            except Agent37Error:
                if attempt + 1 == attempts:
                    raise
                await asyncio.sleep(1)
        if row.get("environmentId") != self.environment_id:
            raise Agent37Error(f"T3 environment identity changed for {self.name}")
        if not all(row.get("capabilities", {}).get(capability) for capability in CAPABILITIES):
            raise Agent37Error(f"{self.name} does not support scoped T3 peer links")

    async def rpc(self, token, method, payload):
        ticket = await self.json("POST", "/api/auth/websocket-ticket/rpc", token=token)
        request_id = "1"  # Each RPC has its own WebSocket.
        hint = ""
        try:
            async with asyncio.timeout(40), self.session.ws_connect(
                self.origin + "/ws", headers=self.headers,
                params={"wsTicket": ticket["ticket"], "orchestrationProtocol": "2"},
                timeout=aiohttp.ClientWSTimeout(ws_receive=35, ws_close=3),
            ) as websocket:
                await websocket.send_json({"_tag": "Request", "id": request_id, "tag": method,
                                           "payload": payload, "headers": []})
                async for message in websocket:
                    if message.type != aiohttp.WSMsgType.TEXT:
                        break
                    value = message.json()
                    if value.get("_tag") == "Ping":
                        await websocket.send_json({"_tag": "Pong"})
                    if value.get("_tag") != "Exit" or str(value.get("requestId")) != request_id:
                        continue
                    result = value["exit"]
                    if result.get("_tag") == "Success":
                        return result["value"]
                    for cause in result.get("cause", []):
                        error = cause.get("error")
                        reason = error.get("reason") if isinstance(error, dict) else None
                        if reason in {"disabled", "unreachable", "incompatible", "self", "pairing_rejected", "storage"}:
                            hint = f" ({reason.replace('_', ' ')})"
                            break
                    break
        except (aiohttp.ClientError, asyncio.TimeoutError, ValueError, KeyError):
            pass  # RPC and upgrade errors may contain one-time credentials.
        raise Agent37Error(f"{self.name} did not complete T3 RPC {method}{hint}")


async def ensure_link(source, source_token, target, target_token, target_url):
    result = await source.rpc(source_token, "peerLinks.list", {})
    previous = next((link for link in result["links"] if link["environmentId"] == target.environment_id), None)
    if previous is not None:
        if previous["status"] == "reachable":
            return
        if previous["status"] != "expired" or target_url not in previous["urls"]:
            raise Agent37Error(f"Review the existing T3 link from {source.name} to {target.name}")
    access = previous["access"] if previous is not None else ACCESS
    if access not in (ACCESS, "read-only"):
        raise Agent37Error(f"Renew the existing {access} link from {source.name} to {target.name} in T3")
    grant = await target.json("POST", "/api/auth/pairing-token", token=target_token,
                              data={"label": f"t3-agent37 {source.name}", "purpose": "peer-link", "access": access})
    if (grant.get("purpose") != "peer-link" or grant.get("access") != access
            or not isinstance(grant.get("credential"), str)):
        raise Agent37Error(f"{target.name} did not issue a scoped peer pairing code")
    result = await source.rpc(source_token, "peerLinks.link", {
        "url": target_url, "pairingCode": grant["credential"], "access": access,
        "expectedEnvironmentId": target.environment_id,
    })
    if result.get("environmentId") != target.environment_id or result.get("access") != access:
        raise Agent37Error(f"T3 did not verify the link from {source.name} to {target.name}")


async def sync_peers(peers, name, sandbox_id, private_url, protected_url, agent37_key, cloud_token):
    private_url = private_origin(private_url)
    timeout = aiohttp.ClientTimeout(total=35, connect=5)
    trace = aiohttp.TraceConfig()

    async def reject_redirect(*_):
        raise Agent37Error("T3 peer request tried to redirect")

    trace.on_request_redirect.append(reject_redirect)
    async with aiohttp.ClientSession(timeout=timeout, trace_configs=[trace]) as session:
        cloud = T3PeerAPI(session, name, protected_url, sandbox_id, {"X-Agent37-Key": agent37_key})
        private = T3PeerAPI(session, name, private_url, sandbox_id)
        await cloud.verify()
        await private.verify(attempts=6)
        failures = []
        for peer in peers:
            target = T3PeerAPI(session, peer.host, peer.url, peer.environment_id)
            try:
                await target.verify()
                token = await asyncio.to_thread(issue_over_ssh, peer)
                await ensure_link(cloud, cloud_token, target, token, peer.url)
                await ensure_link(target, token, cloud, cloud_token, private_url)
            except Agent37Error as error:
                failures.append(f"{peer.host}: {error}")
        if failures:
            raise Agent37Error("T3 peer links incomplete. " + ". ".join(failures))
