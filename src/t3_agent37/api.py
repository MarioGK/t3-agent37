"""Agent37 Cloud API and protected sandbox exec."""

import json
import re
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import HTTPRedirectHandler, Request, build_opener

API = "https://api.agent37.com/v1/instances"
SANDBOX = "https://sandbox.agent37.app/v1/instances"
MANAGED_CREDIT_MICROS = 1_000_000


class Agent37Error(RuntimeError):
    def __init__(self, message, status=None):
        super().__init__(message)
        self.status = status


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def instance_id(value):
    if not isinstance(value, str) or not re.fullmatch(r"[a-z0-9]{10}", value):
        raise Agent37Error("Invalid Agent37 instance ID")
    return value


class Agent37:
    def __init__(self, key):
        self.key = key
        self.opener = build_opener(NoRedirect())

    def _request(self, url, *, method="GET", payload=None, sandbox=False):
        header = {"X-Agent37-Key": self.key} if sandbox else {
            "Authorization": f"Bearer {self.key}",
        }
        body = payload if isinstance(payload, bytes) else None if payload is None else json.dumps(payload).encode()
        if body is not None:
            header["Content-Type"] = "application/octet-stream" if isinstance(payload, bytes) else "application/json"
        request = Request(url, data=body, headers=header, method=method)
        try:
            with self.opener.open(request, timeout=240) as response:
                if response.status == 204:
                    return {}
                return json.load(response)
        except HTTPError as error:
            with error:
                try:
                    detail = json.load(error)
                except (ValueError, OSError):
                    detail = {}
            issue = detail.get("error") if isinstance(detail, dict) else None
            code = issue.get("code") if isinstance(issue, dict) else issue
            suffix = f" ({code})" if isinstance(code, str) and re.fullmatch(r"[a-z_]{1,80}", code) else ""
            raise Agent37Error(f"Agent37 returned HTTP {error.code}{suffix}", error.code) from None
        except (URLError, OSError, ValueError):
            raise Agent37Error("Agent37 request failed or returned an invalid response") from None

    def list(self):
        return self._request(API)["data"]

    def create(self, payload):
        return self._request(API, method="POST", payload=payload)

    def get(self, instance):
        return self._request(f"{API}/{instance_id(instance)}")

    def lifecycle(self, instance, action):
        if action not in {"start", "stop"}:
            raise ValueError("Unknown lifecycle action")
        return self._request(f"{API}/{instance_id(instance)}/{action}", method="POST")

    def exec(self, instance, command, *, name=None):
        payload = {"command": command}
        if name is not None:
            payload["name"] = name
        response = self._request(
            f"{SANDBOX}/{instance_id(instance)}/exec", method="POST", payload=payload, sandbox=True,
        )
        if response["exit_code"] != 0 or response.get("truncated"):
            raise Agent37Error(f"Sandbox setup command failed (exit {response['exit_code']})")
        return response["stdout"]

    def upload_enrollment_key(self, instance, filename, key):
        if not re.fullmatch(r"tailscale-enrollment-[a-f0-9]{32}", filename):
            raise ValueError("Invalid enrollment file name")
        query = urlencode({"path": f"~/.local/share/t3-agent37/{filename}", "overwrite": "false"})
        url = f"https://{instance_id(instance)}.agent37.app/v1/files/content?{query}"
        self._request(url, method="PUT", payload=key.encode(), sandbox=True)


    def require_managed_budget(self, instance, *, initialize=False):
        url = f"{API}/{instance_id(instance)}/budget"
        budget = self._request(url)
        if initialize and not (budget["monthly_remaining_micros"] or budget["credit_remaining_micros"]):
            budget = self._request(url + "/top-up", method="POST", payload={
                "amount_micros": MANAGED_CREDIT_MICROS,
                "idempotency_key": "t3-agent37-ready-v1",
            })
        if not (budget["monthly_remaining_micros"] or budget["credit_remaining_micros"]):
            raise Agent37Error("Agent37 model budget is exhausted. Add headroom in Agent37 before starting a turn.")

    def set_provider(self, instance, provider):
        metadata = {**instance["metadata"], "provider": provider}
        return self._request(f"{API}/{instance_id(instance['id'])}", method="PATCH",
                             payload={"metadata": metadata})

    def terminal(self, instance):
        return self._request(f"{API}/{instance_id(instance)}/signed-url", method="POST",
                             payload={"port": 7681})["url"]
