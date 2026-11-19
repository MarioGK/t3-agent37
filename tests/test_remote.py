import json
from io import BytesIO
from unittest.mock import Mock

from t3_agent37 import remote


def test_configures_managed_model_without_writing_the_token(monkeypatch, tmp_path):
    monkeypatch.setenv("AGENT37_MANAGED_TOKEN", "fixture-token")
    monkeypatch.setenv("AGENT37_LLM_PROXY_URL", "https://api.agent37.com/llm/v1")
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))
    monkeypatch.setattr(remote, "DATA", tmp_path / "t3")
    monkeypatch.setattr(remote.shutil, "which", lambda executable: "/usr/local/bin/opencode")
    path = remote.DATA / "userdata/settings.json"
    path.parent.mkdir(parents=True)
    path.write_text('{"unrelated": true, "providerInstances": {"codex": {"driver": "codex"}}}')

    remote.configure_managed_opencode()
    remote.configure_managed_opencode()

    settings = json.loads(path.read_text())
    assert settings["defaultModelSelection"] == {"instanceId": "opencode", "model": "agent37/default"}
    assert settings["providerInstances"]["codex"] == {"driver": "codex"}
    assert settings["providerInstances"]["opencode"]["enabled"]
    assert settings["unrelated"] is True

    opencode = tmp_path / "config/opencode/opencode.json"
    config = opencode.read_text()
    assert "fixture-token" not in config
    assert json.loads(config)["provider"]["agent37"]["options"]["apiKey"] == "{env:AGENT37_MANAGED_TOKEN}"


def test_cliproxy_profile_loads_text_models_and_keeps_secret_out_of_config(monkeypatch):
    monkeypatch.setenv("T3_AGENT37_CLIPROXY_URL", "https://example.org/")
    monkeypatch.setenv("T3_AGENT37_CLIPROXY_KEY", "fixture-key")
    monkeypatch.delenv("T3_AGENT37_CLIPROXY_MODEL", raising=False)
    response = BytesIO(json.dumps({"data": [
        {"id": "glm-5.3"}, {"id": "gpt-6.2-sol"}, {"id": "gpt-image-2"}, None,
    ]}).encode())
    opener = Mock()
    opener.open.return_value = response
    monkeypatch.setattr(remote, "build_opener", lambda *handlers: opener)
    model, profile = remote.cli_proxy_profile()
    assert model == "cliproxy/glm-5.3"
    assert set(profile["models"]) == {"glm-5.3", "gpt-6.2-sol"}
    assert profile["options"]["baseURL"] == "https://example.org/v1"
    assert profile["options"]["apiKey"] == "{env:T3_AGENT37_CLIPROXY_KEY}"
    request = opener.open.call_args.args[0]
    assert request.full_url == "https://example.org/v1/models"
    assert request.get_header("Authorization") == "Bearer fixture-key"
