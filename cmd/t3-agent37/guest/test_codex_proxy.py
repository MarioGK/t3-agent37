import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import codex_proxy


class CodexProxyTests(unittest.TestCase):
    def test_t3_uses_native_responses_with_an_environment_reference(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            env = {"CODEX_HOME": directory, "T3_AGENT37_CLIPROXY_KEY": "test-only-key"}
            with (patch.dict(os.environ, env),
                  patch.object(codex_proxy.shutil, "which", return_value="/tools/codex"),
                  patch.object(codex_proxy, "replace_config") as replace):
                settings = codex_proxy.configure(
                    "https://proxy.example/v1", "glm-5.3", ["gpt-6.2-sol", "other-model"],
                )

            self.assertEqual(settings, {
                "setupMode": "existing",
                "binaryPath": "/tools/codex",
                "homePath": str(home),
                "customModels": ["glm-5.3", "gpt-6.2-sol"],
            })
            replace.assert_called_once()
            binary, path, original, values, _ = replace.call_args.args
            self.assertEqual((binary, path, original), ("/tools/codex", home / "config.toml", None))
            self.assertEqual(values, {
                "model": "glm-5.3",
                "model_provider": codex_proxy.PROVIDER,
                f"model_providers.{codex_proxy.PROVIDER}": {
                    "name": "CLIProxyAPI",
                    "base_url": "https://proxy.example/v1",
                    "env_key": "T3_AGENT37_CLIPROXY_KEY",
                    "wire_api": "responses",
                    "requires_openai_auth": False,
                },
            })

    def test_configure_skips_rewriting_an_existing_matching_config(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config.toml"
            original = f'''# Keep operator comments and unrelated settings.
model = "gpt-6.2-sol"
model_provider = "{codex_proxy.PROVIDER}"

[model_providers.{codex_proxy.PROVIDER}]
name = "CLIProxyAPI"
base_url = "https://proxy.example/v1"
env_key = "T3_AGENT37_CLIPROXY_KEY"
wire_api = "responses"
requires_openai_auth = false

[projects."/work"]
trust_level = "trusted"
'''
            config.write_text(original, encoding="utf-8")
            with (patch.dict(os.environ, {"CODEX_HOME": directory}),
                  patch.object(codex_proxy.shutil, "which", return_value="/tools/codex"),
                  patch.object(codex_proxy, "write_config") as write):
                settings = codex_proxy.configure("https://proxy.example/v1", "gpt-6.2-sol")

            self.assertEqual(settings["customModels"], ["gpt-6.2-sol"])
            write.assert_not_called()
            self.assertEqual(config.read_text(encoding="utf-8"), original)

    def test_repairs_the_native_writer_inline_header_without_losing_other_providers(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.toml"
            original = '# Keep the comment.\nmodel_providers = {other = {name = "Other"}}\n'
            path.write_text(original, encoding="utf-8")
            path.chmod(0o600)
            native_output = '''[# Keep the comment.
model_providers ]
other = {name = "Other"}

[model_providers.t3_agent37_cliproxy]
name = "CLIProxyAPI"
'''
            expected = {"model_providers": {"other": {"name": "Other"},
                                            codex_proxy.PROVIDER: {"name": "CLIProxyAPI"}}}

            def native_write(_binary, home, _values):
                (home / "config.toml").write_text(native_output, encoding="utf-8")

            with patch.object(codex_proxy, "write_config", side_effect=native_write):
                codex_proxy.replace_config("/tools/codex", path, original, {}, expected)

            self.assertTrue(path.read_text(encoding="utf-8").startswith("# Keep the comment.\n"))
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
