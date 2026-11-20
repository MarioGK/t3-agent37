import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import claude_proxy


class ClaudeProxyTest(unittest.TestCase):
    def test_launcher_sets_the_gateway_and_preserves_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            tools = home / "tools with spaces"
            tools.mkdir()
            binary = tools / "claude"
            binary.write_text("""#!/bin/sh
printf '%s\\n' "$ANTHROPIC_BASE_URL" "$ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_MODEL" \\
  "$ANTHROPIC_DEFAULT_HAIKU_MODEL" "${ANTHROPIC_API_KEY:-}" "$1"
""")
            binary.chmod(0o700)
            root = home / "t3-agent37"
            env = {"HOME": directory, "PATH": str(tools), "T3_AGENT37_CLIPROXY_KEY": "fixture-key",
                   "ANTHROPIC_API_KEY": "stale-key"}
            with patch.dict(os.environ, env, clear=True):
                wrapper = claude_proxy.configure_claude(root, "https://proxy.example/anthropic/v1/")
            alias = home / ".local/bin/claude"
            self.assertEqual(alias.resolve(), wrapper)
            env["PATH"] = os.pathsep.join([str(alias.parent), str(tools)])
            with patch.dict(os.environ, env, clear=True):
                self.assertEqual(claude_proxy.configure_claude(root, "https://proxy.example/anthropic"), wrapper)
            self.assertNotIn("fixture-key", wrapper.read_text())
            result = subprocess.run([alias, "test argument"], env=env, check=True, capture_output=True, text=True)
            self.assertEqual(result.stdout.splitlines(), ["https://proxy.example/anthropic", "fixture-key",
                                                         "glm-5.3", "glm-5.3", "", "test argument"])

    def test_rejects_insecure_endpoint(self):
        for endpoint in ("http://proxy.example", "https://secret@proxy.example", "https://proxy.example?q=secret"):
            with self.subTest(endpoint=endpoint), self.assertRaisesRegex(RuntimeError, "HTTPS CLIProxy"):
                claude_proxy.claude_base_url(endpoint)

    def test_native_settings_expose_only_the_proxy_url(self):
        settings = {"env": {"ENABLE_TOOL_SEARCH": "false"}, "model": "other-model"}
        self.assertEqual(claude_proxy.configure_claude_settings(settings, "https://proxy.example/v1"), {
            "env": {"ENABLE_TOOL_SEARCH": "false", "ANTHROPIC_BASE_URL": "https://proxy.example"},
            "model": "other-model",
        })

    def test_t3_config_preserves_previous_models_and_default(self):
        default = {"instanceId": "opencode", "model": "cliproxy/glm-5.3"}
        config = {"customModels": ["private-model", {"slug": "glm-5.3", "name": "GLM 5.3"}]}
        settings = {"defaultModelSelection": default, "providerInstances": {"claudeAgent": {"config": config}}}
        claude_proxy.configure_claude_t3(settings, Path("/app/bin/claude"))
        self.assertEqual(settings["providerInstances"]["claudeAgent"], {
            "driver": "claudeAgent", "enabled": True, "config": {
                "binaryPath": "/app/bin/claude",
                "customModels": ["private-model", {"slug": "glm-5.3", "name": "GLM 5.3"}],
            },
        })
        self.assertEqual(settings["defaultModelSelection"], default)


if __name__ == "__main__":
    unittest.main()
