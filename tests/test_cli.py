import json
from pathlib import Path
from unittest.mock import Mock

import pytest

from t3_agent37.api import Agent37Error, instance_id
from t3_agent37.cli import get_or_create, main, pair_link, request_body


INSTANCE = {
    "id": "ab12cd34ef", "status": "running",
    "metadata": {"managed_by": "t3-agent37", "environment": "pilot"},
}


def test_duplicate_retry_reuses_cloud_instance(tmp_path):
    client = Mock()
    client.list.side_effect = [[], [], [INSTANCE]]
    client.create.return_value = INSTANCE
    assert get_or_create(client, "pilot", "agent37-codex", tmp_path) == INSTANCE
    assert get_or_create(client, "pilot", "agent37-codex", tmp_path) == INSTANCE
    client.create.assert_called_once_with(request_body("pilot", "agent37-codex"))


def test_uncertain_create_checks_cloud_before_reissuing(tmp_path):
    client = Mock()
    client.list.side_effect = [[], [], [], [INSTANCE]]
    client.create.side_effect = Agent37Error("request lost")
    with pytest.raises(Agent37Error, match="request lost"):
        get_or_create(client, "pilot", "agent37-codex", tmp_path)
    with pytest.raises(Agent37Error, match="pending"):
        get_or_create(client, "pilot", "agent37-codex", tmp_path)
    assert get_or_create(client, "pilot", "agent37-codex", tmp_path) == INSTANCE
    assert client.create.call_count == 1
    assert not list(Path(tmp_path).glob("*.pending"))


def test_dry_run_ignores_missing_key(monkeypatch, capsys):
    monkeypatch.delenv("AGENT37_API_KEY", raising=False)
    assert main(["create", "pilot", "--dry-run"]) == 0
    output = json.loads(capsys.readouterr().out)
    assert output["template"] == "agent37-opencode"
    assert output["budget"] == {"credit_micros": 1_000_000}
    assert output["public_ports"] == []
    assert output["type"] == "default"
    assert output["resources"] == {"cpu": 4, "memory": 8}
    assert output["auto_sleep"] is True
    assert output["idle_timeout_seconds"] == 900


def test_external_pair_url_rejected():
    client = Mock()
    client.exec.return_value = '{"pairUrl":"https://example.org/pair#token=abc"}'
    with pytest.raises(Agent37Error, match="unexpected"):
        pair_link(client, "ab12cd34ef", 43737)


@pytest.mark.parametrize("value", [None, "x", "http://abcd", "foo@bar.net", "abcde12345.example"])
def test_invalid_remote_id(value):
    with pytest.raises(Agent37Error):
        instance_id(value)


def test_already_running_environment_uses_no_start_request(monkeypatch, capsys, tmp_path):
    client = Mock()
    client.list.return_value = [INSTANCE]
    monkeypatch.setenv("AGENT37_API_KEY", "test")
    monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path))
    monkeypatch.setattr("t3_agent37.cli.Agent37", lambda key: client)
    assert main(["up", "pilot"]) == 0
    assert json.loads(capsys.readouterr().out)["status"] == "running"
    client.lifecycle.assert_not_called()


def test_two_sandboxes_keep_distinct_ports(monkeypatch, tmp_path):
    from t3_agent37.cli import assigned_port
    monkeypatch.setattr("t3_agent37.cli.port_available", lambda port: True)
    first = assigned_port(tmp_path, "ab12cd34ef")
    second = assigned_port(tmp_path, "bc23de45fg")
    assert first != second
    assert first == assigned_port(tmp_path, "ab12cd34ef")
    assert second == assigned_port(tmp_path, "bc23de45fg")
    with pytest.raises(Agent37Error, match="No free relay port"):
        assigned_port(tmp_path, "zz88yy77xx", first)


def test_provider_chooses_matching_agent_template(monkeypatch, capsys):
    monkeypatch.delenv("AGENT37_API_KEY", raising=False)
    assert main(["create", "pilot", "--provider", "opencode", "--dry-run"]) == 0
    output = json.loads(capsys.readouterr().out)
    assert output["template"] == "agent37-opencode"
    assert output["metadata"]["provider"] == "opencode"


def test_terrascale_dry_run_uses_private_profile_without_credentials(monkeypatch, capsys):
    monkeypatch.setenv("T3_AGENT37_CLIPROXY_KEY", "fixture-key")
    monkeypatch.setenv("T3_AGENT37_TAILSCALE_CLIENT_SECRET", "fixture-secret")
    assert main(["create", "pilot", "--template", "terrascale-agent37@3", "--dry-run"]) == 0
    raw = capsys.readouterr().out
    assert "fixture" not in raw
    output = json.loads(raw)
    assert output["metadata"]["provider"] == "cliproxy"
    assert output["metadata"]["tailnet"] == "tailscale"
    assert "budget" not in output


def test_down_logs_out_first_and_preserves_sandbox_on_logout_failure():
    from t3_agent37.cli import lifecycle

    instance = {**INSTANCE, "metadata": {**INSTANCE["metadata"], "tailnet": "tailscale"}}
    client = Mock()
    client.exec.side_effect = Agent37Error("fixture failed")
    with pytest.raises(Agent37Error, match="Tailscale logout failed"):
        lifecycle(client, instance, "pilot", "stop")
    client.lifecycle.assert_not_called()
    client.exec.side_effect = None
    client.lifecycle.return_value = {"status": "stopped"}
    assert lifecycle(client, instance, "pilot", "stop")["status"] == "stopped"
    assert [method[0] for method in client.mock_calls][-2:] == ["exec", "lifecycle"]


def test_up_syncs_t3_peers_after_start_and_tailnet_join(monkeypatch):
    from t3_agent37 import cli
    from t3_agent37.peers import Peer

    client = Mock()
    instance = {**INSTANCE, "status": "stopped", "metadata": {
        **INSTANCE["metadata"], "tailnet": "tailscale",
    }}
    client.lifecycle.return_value = {"status": "running"}
    client.get.return_value = {**instance, "status": "running"}
    proof = {"environmentId": "cloud-id", "peerProxy": True}
    peers = (Peer("beta", "https://beta.test.ts.net", "beta-id"),)
    steps = Mock()
    steps.attach_mock(client.lifecycle, "start")
    steps.setup.return_value = (proof, 3773)
    for function in ("tailscale_credentials", "setup", "join_tailnet", "link_fleet"):
        monkeypatch.setattr(cli, function, getattr(steps, function))
    assert cli.lifecycle(client, instance, "pilot", "start", peers=peers)["status"] == "running"
    assert [call[0] for call in steps.mock_calls] == [
        "tailscale_credentials", "start", "setup", "join_tailnet", "link_fleet",
    ]
    steps.link_fleet.assert_called_once_with(client, "ab12cd34ef", "pilot", peers, proof, 3773)


def test_join_only_mints_a_single_use_key_when_needed(monkeypatch):
    from t3_agent37 import cli

    client = Mock()
    proof = '{"id":"tailnet-node-id","hostname":"a37-pilot-ab12cd34ef"}'
    mint = Mock(return_value="tskey-auth-fixture")
    monkeypatch.setattr(cli, "tailscale_credentials", lambda: ("client", "secret"))
    monkeypatch.setattr(cli, "enrollment_key", mint)
    client.exec.return_value = proof
    cli.join_tailnet(client, "ab12cd34ef", "pilot")
    mint.assert_not_called()
    client.exec.side_effect = ['{"enrollmentRequired":true}', proof]
    cli.join_tailnet(client, "ab12cd34ef", "pilot")
    mint.assert_called_once_with("client", "secret")
    client.upload_enrollment_key.assert_called_once()
    assert "tskey-auth-fixture" not in repr(client.exec.call_args_list)


def test_tailscale_key_is_short_lived_ephemeral_and_tagged(monkeypatch):
    from io import BytesIO

    from t3_agent37 import tailscale_oauth

    opener = Mock()
    opener.open.side_effect = [BytesIO(b'{"access_token":"fixture-access"}'),
                               BytesIO(b'{"key":"tskey-auth-fixture"}')]
    monkeypatch.setattr(tailscale_oauth, "build_opener", lambda *handlers: opener)
    assert tailscale_oauth.enrollment_key("fixture-client", "fixture-secret") == "tskey-auth-fixture"
    request = opener.open.call_args.args[0]
    assert request.full_url == "https://api.tailscale.com/api/v2/tailnet/-/keys"
    policy = json.loads(request.data)
    assert policy["expirySeconds"] == 600
    assert policy["capabilities"]["devices"]["create"] == {
        "reusable": False, "ephemeral": True, "preauthorized": True, "tags": ["tag:agent37"],
    }


def test_protected_endpoint_retries_after_start(monkeypatch):
    from io import BytesIO
    from urllib.error import HTTPError

    from t3_agent37.cli import wait_for_t3

    client = Mock(key="placeholder")
    client.opener.open.side_effect = [
        HTTPError("", 404, "Starting", None, BytesIO()),
        HTTPError("", 502, "Not ready", None, BytesIO()),
        BytesIO(b'{"environmentId":"stable"}'),
    ]
    monkeypatch.setattr("t3_agent37.cli.time.sleep", lambda seconds: None)
    descriptor, port = wait_for_t3(client, "ab12cd34ef", expected="stable", remote_port=3773)
    assert descriptor["environmentId"] == "stable" and port == 3773
    assert client.opener.open.call_count == 3


def test_protected_endpoint_rejects_mismatched_identity():
    from io import BytesIO

    from t3_agent37.cli import wait_for_t3

    client = Mock(key="placeholder")
    client.opener.open.return_value = BytesIO(b'{"environmentId":"other"}')
    with pytest.raises(Agent37Error, match="different environment"):
        wait_for_t3(client, "ab12cd34ef", expected="stable")


def test_reconnect_uses_secondary_port_when_gateway_owns_default():
    from io import BytesIO
    from urllib.error import HTTPError

    from t3_agent37.cli import wait_for_t3

    client = Mock(key="placeholder")
    client.opener.open.side_effect = [
        HTTPError("", 404, "Gateway", None, BytesIO()),
        BytesIO(b'{"environmentId":"secondary"}'),
    ]
    descriptor, port = wait_for_t3(client, "ab12cd34ef")
    assert descriptor["environmentId"] == "secondary"
    assert port == 4373
    assert client.opener.open.call_args.args[0].host == "ab12cd34ef-4373.agent37.app"
