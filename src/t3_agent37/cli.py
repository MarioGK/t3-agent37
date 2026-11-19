"""Create, pair, and connect to Agent37 environments with the T3 desktop app."""

import argparse
import asyncio
import base64
import fcntl
from importlib.metadata import version
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import socket
import sqlite3
import subprocess
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlsplit
from urllib.request import Request
from uuid import uuid4

from aiohttp import web

from .api import Agent37, Agent37Error, MANAGED_CREDIT_MICROS, instance_id
from .relay import application
from .tailscale_oauth import credentials as tailscale_credentials, enrollment_key


OWNER = "t3-agent37"
DEFAULT_PORT = 43737
T3_PORTS = (3773, 4373)
TEMPLATES = {"codex": "agent37-codex", "claude": "agent37-claude-code",
             "opencode": "agent37-opencode", "cliproxy": "agent37-opencode"}


def label(value):
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,42}", value):
        raise argparse.ArgumentTypeError("Use 1 to 43 lowercase letters, digits, or hyphens")
    return value


def port_number(value):
    port = int(value)
    if not 1024 <= port <= 65535:
        raise argparse.ArgumentTypeError("Port must be between 1024 and 65535")
    return port


def terrascale_profile(template):
    name, _, version = (template or "").partition("@")
    return name == "terrascale-agent37" and (not version or version.isdecimal() and int(version) >= 3)


def cliproxy_env():
    keys = ("T3_AGENT37_CLIPROXY_URL", "T3_AGENT37_CLIPROXY_KEY")
    values = {key: os.environ.get(key) for key in keys}
    if not all(values.values()):
        raise Agent37Error(f"Load {' and '.join(keys)} for CLIProxy")
    model = os.environ.get("T3_AGENT37_CLIPROXY_MODEL")
    if model:
        values["T3_AGENT37_CLIPROXY_MODEL"] = model
    return values


def request_body(name, template, provider="codex", *, tailnet=False, provider_env=None):
    payload = {
        "name": name, "template": template,
        "metadata": {"managed_by": OWNER, "environment": name, "provider": provider},
        "type": "default",
        "resources": {"cpu": 4, "memory": 8},
        "public_ports": [],
        "auto_sleep": True,
        "idle_timeout_seconds": 900,
        "env": {"AGENT37_MANAGED_PLUGIN_COMPOSIO_ENABLED": "false"},
    }
    if provider == "opencode":
        payload["budget"] = {"credit_micros": MANAGED_CREDIT_MICROS}
    if tailnet:
        payload["metadata"]["tailnet"] = "tailscale"
    if provider_env:
        payload["env"].update(provider_env)
    return payload


def managed(client, name=None):
    return [
        instance for instance in client.list()
        if instance.get("status") != "deleted"
        and (instance.get("metadata") or {}).get("managed_by") == OWNER
        and (name is None or instance["metadata"].get("environment") == name)
    ]


def lookup(client, name):
    matches = managed(client, name)
    if len(matches) > 1:
        raise Agent37Error(f"More than one managed instance matches {name}")
    return matches[0] if matches else None


def get_or_create(client, name, template, state, provider="codex", *, tailnet=False):
    existing = lookup(client, name)
    if existing:
        (state / f"{name}.pending").unlink(missing_ok=True)
        return existing
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    marker = state / f"{name}.pending"
    try:
        marker.touch(mode=0o600, exist_ok=False)
    except FileExistsError:
        raise Agent37Error(
            f"A create operation for {name} is pending. Retry to look for the instance. "
            f"If the workspace confirms no instance was created, remove {marker}.",
        ) from None
    try:
        existing = lookup(client, name)
    except BaseException:
        marker.unlink(missing_ok=True)
        raise
    try:
        provider_env = cliproxy_env() if not existing and provider == "cliproxy" else None
        if not existing and tailnet:
            tailscale_credentials()
    except Agent37Error:
        marker.unlink(missing_ok=True)
        raise
    try:
        result = existing or client.create(request_body(name, template, provider, tailnet=tailnet,
                                                       provider_env=provider_env))
        instance_id(result["id"])
    except Agent37Error as error:
        if error.status in {400, 401, 402, 403, 404, 422, 429}:
            marker.unlink(missing_ok=True)
        raise
    marker.unlink(missing_ok=True)
    return result


REMOTE = 'python3 "$HOME/.local/share/t3-agent37/remote.py"'


def setup(client, instance, provider, *, tailnet=False):
    names = ("remote.py", "tailnet.py") if tailnet else ("remote.py",)
    arguments = []
    for name in names:
        source = Path(__file__).with_name(name).read_bytes()
        arguments.extend((name, base64.b64encode(source).decode()))
    install = (
        "import base64, pathlib, sys, os, tempfile; "
        "path = pathlib.Path.home() / '.local/share/t3-agent37'; "
        "path.mkdir(mode=0o700, parents=True, exist_ok=True); "
        "\nfor filename, body in zip(sys.argv[1::2], sys.argv[2::2]):\n"
        " with tempfile.NamedTemporaryFile(dir=path, delete=False) as stage:\n"
        "  stage.write(base64.b64decode(body)); stage.flush()\n"
        " os.replace(stage.name, path / filename)"
    )
    command = (
        f"python3 -c {shlex.quote(install)} {shlex.join(arguments)} "
        f"&& {REMOTE} setup {provider}"
    )
    # Bootstrap is idempotent. Run it again after a restart so an Agent37
    # cached exec response cannot claim that the previous process is ready.
    proof = json.loads(client.exec(instance, command))
    if not proof.get("environmentId"):
        raise Agent37Error("Sandbox setup did not return a T3 environment ID")
    visible, port = wait_for_t3(client, instance, expected=proof["environmentId"], remote_port=proof["port"])
    return {**visible, "providerReady": proof["providerReady"]}, port


def join_tailnet(client, instance, name):
    hostname = f"a37-{label(name)}-{instance_id(instance)}"
    command = f"{REMOTE} tailnet-join {hostname}"
    try:
        proof = json.loads(client.exec(instance, command))
        if proof.get("enrollmentRequired"):
            key = enrollment_key(*tailscale_credentials())
            filename = f"tailscale-enrollment-{uuid4().hex}"
            try:
                client.upload_enrollment_key(instance, filename, key)
                proof = json.loads(client.exec(instance, f"{command} {filename}"))
            except Agent37Error:
                try:
                    client.exec(instance, f'unlink "$HOME/.local/share/t3-agent37/{filename}"')
                except Agent37Error:
                    pass  # the bootstrap may already have consumed the short-lived key
                raise
        if not proof.get("id") or proof.get("hostname") != hostname:
            raise Agent37Error("Tailscale did not confirm the sandbox identity")
    except Agent37Error as error:
        raise Agent37Error(f"Could not enroll {name} in Tailscale. {error}") from None


def wait_for_t3(client, instance, *, expected=None, remote_port=None, timeout=45):
    ports = (remote_port,) if remote_port is not None else T3_PORTS
    deadline = time.monotonic() + timeout
    while True:
        for port in ports:
            origin = protected_origin(instance, port)
            request = Request(origin + "/.well-known/t3/environment", headers={"X-Agent37-Key": client.key})
            try:
                with client.opener.open(request, timeout=5) as response:
                    visible = json.load(response)
            except HTTPError as error:
                status = error.code
                error.close()
                if status not in {404, 409, 425, 429, 502, 503, 504}:
                    raise Agent37Error(f"Protected T3 endpoint returned HTTP {status}", status) from None
            except (URLError, TimeoutError):
                continue
            else:
                identity = visible.get("environmentId")
                if not identity or (expected and identity != expected):
                    raise Agent37Error("Protected T3 endpoint returned a different environment")
                return visible, port
        if time.monotonic() >= deadline:
            raise Agent37Error("Protected T3 endpoint did not become ready. Check instance status and retry.")
        time.sleep(1)


def port_available(port):
    with socket.socket() as listener:
        try:
            listener.bind(("127.0.0.1", port))
        except OSError:
            return False
    return True


def assigned_port(state, identifier, requested=None):
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    with sqlite3.connect(state / "ports.sqlite3", isolation_level="IMMEDIATE") as db:
        db.execute("BEGIN IMMEDIATE")
        db.execute("CREATE TABLE IF NOT EXISTS ports (instance TEXT PRIMARY KEY, port INTEGER UNIQUE)")
        old = db.execute("SELECT port FROM ports WHERE instance = ?", (identifier,)).fetchone()
        if old:
            if requested is not None and old[0] != requested:
                raise Agent37Error(f"This environment is already assigned loopback port {old[0]}")
            return old[0]
        reserved = {row[0] for row in db.execute("SELECT port FROM ports")}
        candidates = [requested] if requested is not None else range(DEFAULT_PORT, DEFAULT_PORT + 100)
        for port in candidates:
            if port not in reserved and port_available(port):
                db.execute("INSERT INTO ports VALUES (?, ?)", (identifier, port))
                return port
    raise Agent37Error("No free relay port in the requested range")


def protected_origin(instance, port=T3_PORTS[0]):
    if port not in T3_PORTS:
        raise Agent37Error("Unexpected T3 service port")
    return f"https://{instance_id(instance)}-{port}.agent37.app"


def pair_link(client, instance, port):
    result = client.exec(instance, f'python3 "$HOME/.local/share/t3-agent37/remote.py" pair {port}')
    pairing = json.loads(result)["pairUrl"]
    parsed = urlsplit(pairing)
    if (parsed.scheme, parsed.netloc, parsed.path) != ("http", f"127.0.0.1:{port}", "/pair"):
        raise Agent37Error("T3 returned an unexpected pairing address")
    if not parsed.fragment.startswith("token="):
        raise Agent37Error("T3 pairing URL does not contain a token")
    return pairing


def open_pairing(link):
    executable = "open" if sys.platform == "darwin" else "xdg-open"
    opener = shutil.which(executable)
    if opener:
        subprocess.Popen([opener, "t3code://pair?pairingUrl=" + quote(link, safe="")],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


async def serve(client, instance, port, remote_port, pairing):
    origin = protected_origin(instance, remote_port)
    runner = web.AppRunner(application(origin, client.key, port), access_log=None)
    await runner.setup()
    try:
        await web.TCPSite(runner, "127.0.0.1", port).start()
        print(f"Relay running at http://127.0.0.1:{port}", flush=True)
        if pairing:
            print(f"Pair in T3 Code (expires in 5 minutes): {pairing}", flush=True)
            open_pairing(pairing)
        await asyncio.Event().wait()
    finally:
        await runner.cleanup()


def summary(instance):
    fields = {field: instance.get(field) for field in ("name", "id", "status", "template")}
    return {**fields, "provider": (instance.get("metadata") or {}).get("provider", "codex")}


def lifecycle(client, instance, name, action):
    target = "running" if action == "start" else "stopped"
    identifier = instance_id(instance["id"])
    provider = summary(instance)["provider"]
    tailnet = (instance.get("metadata") or {}).get("tailnet") == "tailscale"
    if action == "stop" and instance["status"] != target and tailnet:
        installed = 'test -f "$HOME/.local/share/t3-agent37/tailnet.py"'
        try:
            client.exec(identifier, f"if {installed}; then {REMOTE} tailnet-leave; fi")
        except Agent37Error as error:
            raise Agent37Error(f"Tailscale logout failed for {name}. Retry down. {error}") from None
    if instance["status"] != target:
        if action == "start" and tailnet and instance["status"] == "stopped":
            tailscale_credentials()
        instance = {**instance, **client.lifecycle(identifier, action)}
    if action == "start" and tailnet:
        setup(client, identifier, provider, tailnet=True)
        join_tailnet(client, identifier, name)
        instance = client.get(identifier)
    return summary(instance)


def operate(client, args, state):
    instance = (get_or_create(client, args.name, args.template, state, args.provider, tailnet=args.tailscale)
                if args.action == "create" else lookup(client, args.name))
    if not instance:
        raise Agent37Error(f"No managed Agent37 environment named {args.name}")
    identifier = instance_id(instance["id"])
    tailnet = (instance.get("metadata") or {}).get("tailnet") == "tailscale"
    if args.action in {"start", "stop"}:
        print(json.dumps(lifecycle(client, instance, args.name, args.action)))
        return None
    if args.action == "terminal":
        print(client.terminal(identifier))
        return None
    if args.action == "prepare":
        previous = summary(instance)["provider"]
        provider = "cliproxy" if previous == "cliproxy" else "opencode"
        if provider == "opencode":
            client.require_managed_budget(identifier, initialize=previous != "opencode")
        start = instance["status"] in {"stopped", "sleeping"}
        if start:
            lifecycle(client, instance, args.name, "start")
        if not (start and tailnet and previous == provider):
            setup(client, identifier, provider, tailnet=tailnet)
        if provider == "cliproxy":
            print(f"CLIProxy model catalog refreshed on {args.name}.")
        else:
            client.set_provider(instance, provider)
            print(f"Managed OpenCode is ready on {args.name} (agent37/default).")
        return None
    port = assigned_port(state, identifier, args.port)
    if args.action == "create":
        if (instance["template"] != args.template or summary(instance)["provider"] != args.provider
                or tailnet != args.tailscale):
            raise Agent37Error("This name already uses a different template, provider, or Tailscale setting")
        if args.provider == "opencode":
            client.require_managed_budget(identifier)
        if instance["status"] in {"stopped", "sleeping"}:
            if tailnet and instance["status"] == "stopped":
                tailscale_credentials()
            client.lifecycle(identifier, "start")
        proof, remote_port = setup(client, identifier, args.provider, tailnet=tailnet)
        if tailnet:
            join_tailnet(client, identifier, args.name)
        print(f"Cloud T3 ready ({identifier}, {proof['environmentId']}).", flush=True)
        if not proof["providerReady"]:
            print(f"{args.provider} needs sign-in. Run t3-agent37 terminal {args.name}.", flush=True)
    else:
        if instance["status"] == "stopped":
            raise Agent37Error("The sandbox is stopped. Run t3-agent37 up first.")
        _, remote_port = wait_for_t3(client, identifier)
    pairing = pair_link(client, identifier, port) if args.action == "create" or args.pair else None
    return identifier, port, remote_port, pairing


def main(argv=None):
    parser = argparse.ArgumentParser(prog=OWNER, description=__doc__)
    parser.add_argument("--version", action="version", version=version(OWNER))
    sub = parser.add_subparsers(dest="action", required=True)
    sub.add_parser("list", help="List managed Agent37 environments")
    create = sub.add_parser("create", help="Create a sandbox, install T3, and open pairing")
    create.add_argument("name", type=label)
    create.add_argument("--provider", choices=TEMPLATES,
                        help="Agent provider (default: CLIProxy for TerraScale @3+, Agent37 OpenCode otherwise)")
    create.add_argument("--tailscale", action=argparse.BooleanOptionalAction,
                        help="Enroll in Tailscale (default: enabled for TerraScale @3+)")
    create.add_argument("--template", help="Agent37 template override, optionally pinned by revision")
    create.add_argument("--port", type=port_number)
    mode = create.add_mutually_exclusive_group(required=True)
    mode.add_argument("--yes", action="store_true", help="Authorize paid provisioning")
    mode.add_argument("--dry-run", action="store_true", help="Print the request without using the API")
    prepare = sub.add_parser("prepare", help="Configure OpenCode or refresh the CLIProxy model catalog")
    prepare.add_argument("name", type=label)
    connect = sub.add_parser("connect", help="Run the local relay for a paired environment")
    connect.add_argument("name", type=label)
    connect.add_argument("--port", type=port_number)
    connect.add_argument("--pair", action="store_true", help="Open a new one-time T3 pairing link")
    for action in ("start", "stop", "terminal"):
        aliases = {"start": ["up"], "stop": ["down"]}.get(action, [])
        command = sub.add_parser(action, aliases=aliases, help={
            "start": "Start an Agent37 instance",
            "stop": "Remove the instance from Tailscale and stop it, keeping its disk",
            "terminal": "Print the signed Agent37 terminal URL for provider login",
        }[action])
        command.add_argument("name", type=label)
    args = parser.parse_args(argv)
    args.action = {"up": "start", "down": "stop"}.get(args.action, args.action)
    if args.action == "create":
        terra = terrascale_profile(args.template)
        args.provider = args.provider or ("cliproxy" if terra else "opencode")
        args.template = args.template or TEMPLATES[args.provider]
        args.tailscale = terra if args.tailscale is None else args.tailscale
        if args.dry_run:
            print(json.dumps(request_body(args.name, args.template, args.provider, tailnet=args.tailscale), indent=2))
            return 0
    key = os.environ.get("AGENT37_API_KEY")
    if not key:
        parser.error("Load AGENT37_API_KEY into the environment")
    try:
        client = Agent37(key)
        if args.action == "list":
            print(json.dumps([summary(item) for item in managed(client)], indent=2))
            return 0
        state = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state") / OWNER
        state.mkdir(mode=0o700, parents=True, exist_ok=True)
        with (state / f"{args.name}.lifecycle.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            relay = operate(client, args, state)
        if relay is not None:
            asyncio.run(serve(client, *relay))
    except KeyboardInterrupt:
        return 0
    except (Agent37Error, OSError, ValueError, KeyError, sqlite3.Error) as error:
        print(f"t3-agent37: {error}", file=sys.stderr)
        return 1
    return 0
