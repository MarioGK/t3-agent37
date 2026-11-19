import json

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
