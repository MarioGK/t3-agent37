"""Mint a single-use ephemeral enrollment key on the workstation."""

import json
import os
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, build_opener

from .api import Agent37Error, NoRedirect

API = "https://api.tailscale.com/api/v2"
TAG = "tag:agent37"


def credentials():
    names = ("T3_AGENT37_TAILSCALE_CLIENT_ID", "T3_AGENT37_TAILSCALE_CLIENT_SECRET")
    values = tuple(os.environ.get(name) for name in names)
    if not all(values):
        raise Agent37Error(f"Load {' and '.join(names)} for Tailscale enrollment")
    return values


def enrollment_key(client_id, client_secret):
    opener = build_opener(NoRedirect())

    def post(path, body, headers):
        try:
            with opener.open(Request(API + path, data=body, headers=headers), timeout=15) as response:
                return json.load(response)
        except HTTPError as error:
            error.close()
            raise Agent37Error(f"Tailscale {path} returned HTTP {error.code}") from None
        except (URLError, OSError, ValueError):
            raise Agent37Error(f"Tailscale {path} could not be reached or returned invalid JSON") from None

    token = post("/oauth/token", urlencode({"client_id": client_id, "client_secret": client_secret,
                                           "grant_type": "client_credentials"}).encode(),
                 {"Content-Type": "application/x-www-form-urlencoded"}).get("access_token")
    if not token:
        raise Agent37Error("Tailscale did not issue an OAuth access token")
    policy = {"capabilities": {"devices": {"create": {
        "reusable": False, "ephemeral": True, "preauthorized": True, "tags": [TAG],
    }}}, "expirySeconds": 600, "description": "Agent37 disposable enrollment"}
    key = post("/tailnet/-/keys", json.dumps(policy).encode(),
               {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}).get("key")
    if not isinstance(key, str) or not key.startswith("tskey-auth-"):
        raise Agent37Error("Tailscale did not issue a device enrollment key")
    return key
