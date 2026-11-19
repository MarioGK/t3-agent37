import asyncio
import json
from unittest.mock import AsyncMock, Mock

import pytest

from t3_agent37.api import Agent37Error
from t3_agent37.peers import Peer, configured_peers, ensure_link, private_origin


def test_reads_default_peers_and_can_skip_them(monkeypatch, tmp_path):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    monkeypatch.delenv("T3_AGENT37_PEERS_FILE", raising=False)
    assert configured_peers() == ()
    path = tmp_path / "t3-agent37/peers.json"
    path.parent.mkdir()
    path.write_text(json.dumps([{"ssh": "beta", "url": "https://beta.test.ts.net:18456/",
                                 "environmentId": "beta-id"}]))
    assert configured_peers() == (Peer("beta", "https://beta.test.ts.net:18456", "beta-id"),)
    monkeypatch.setenv("T3_AGENT37_PEERS_FILE", "")
    assert configured_peers() == ()


@pytest.mark.parametrize("url", ["http://beta.test.ts.net", "https://example.net", "https://user@beta.test.ts.net"])
def test_peer_configuration_rejects_nonprivate_origins(url):
    with pytest.raises(Agent37Error, match="private Tailscale"):
        private_origin(url)


def test_creates_supervised_link_to_the_pinned_peer():
    source = Mock(name="sandbox", rpc=AsyncMock(side_effect=[{"links": []}, {
        "environmentId": "beta-id", "access": "approval-required",
    }]))
    source.name = "sandbox"
    target = Mock(environment_id="beta-id", json=AsyncMock(return_value={
        "credential": "fixture-one-time", "purpose": "peer-link", "access": "approval-required",
    }))
    asyncio.run(ensure_link(source, "local", target, "remote", "https://beta.test.ts.net"))
    target.json.assert_awaited_once_with("POST", "/api/auth/pairing-token", token="remote", data={
        "label": "t3-agent37 sandbox", "purpose": "peer-link", "access": "approval-required",
    })
    source.rpc.assert_awaited_with("local", "peerLinks.link", {
        "url": "https://beta.test.ts.net", "pairingCode": "fixture-one-time",
        "expectedEnvironmentId": "beta-id", "access": "approval-required",
    })


def test_retries_reuse_existing_reachable_link():
    source = Mock(rpc=AsyncMock(return_value={"links": [
        {"environmentId": "beta-id", "status": "reachable", "access": "full-access"},
    ]}))
    target = Mock(environment_id="beta-id", json=AsyncMock())
    asyncio.run(ensure_link(source, "local", target, "remote", "https://beta.test.ts.net"))
    source.rpc.assert_awaited_once_with("local", "peerLinks.list", {})
    target.json.assert_not_awaited()
