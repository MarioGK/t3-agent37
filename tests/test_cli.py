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
    assert output["template"] == "agent37-codex"
    assert output["public_ports"] == []
    assert output["auto_sleep"] is False


def test_external_pair_url_rejected():
    client = Mock()
    client.exec.return_value = '{"pairUrl":"https://example.org/pair#token=abc"}'
    with pytest.raises(Agent37Error, match="unexpected"):
        pair_link(client, "ab12cd34ef", 43737)


@pytest.mark.parametrize("value", [None, "x", "http://abcd", "foo@bar.net", "abcde12345.example"])
def test_invalid_remote_id(value):
    with pytest.raises(Agent37Error):
        instance_id(value)


def test_already_running_environment_uses_no_start_request(monkeypatch, capsys):
    client = Mock()
    client.list.return_value = [INSTANCE]
    monkeypatch.setenv("AGENT37_API_KEY", "test")
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
