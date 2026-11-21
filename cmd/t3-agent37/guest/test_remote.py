import io
from contextlib import redirect_stdout
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

from remote import machine_info_with_label, select_runtime, unpack_runtime
from remote import desktop_confirmed, pairing_status, provider_ready


class DesktopPairingTest(unittest.TestCase):
    def test_only_matching_connected_pairing_session_confirms(self):
        label = "t3-agent37-desktop-" + "a" * 32
        previous = "t3-agent37-desktop-" + "b" * 32
        session = {"subject": "one-time-token", "client": {"label": label}, "lastConnectedAt": "2026-11-20T18:00:00Z"}
        earlier = dict(session, client={"label": previous})
        with tempfile.TemporaryDirectory() as directory, patch("remote.ROOT", Path(directory)), \
                patch("remote.descriptor", return_value={"environmentId": "sandbox-id"}):
            for records, redeemed, confirmed in (
                    ([], False, False), ([dict(session, client={"label": "other"})], False, False),
                    ([dict(session, lastConnectedAt=None)], True, False),
                    ([earlier], False, True), ([session], True, True)):
                with patch("remote.subprocess.run", return_value=SimpleNamespace(stdout=json.dumps(records))), \
                        redirect_stdout(io.StringIO()) as output:
                    pairing_status(label, "sandbox-id", previous)
                self.assertEqual(json.loads(output.getvalue()), {"confirmed": confirmed, "redeemed": redeemed})
                self.assertEqual(desktop_confirmed("sandbox-id"), confirmed)
                self.assertFalse(desktop_confirmed("other-environment"))

    def test_changed_identity_stops_before_checking_sessions(self):
        with patch("remote.descriptor", return_value={"environmentId": "other"}), \
                patch("remote.subprocess.run") as command, redirect_stdout(io.StringIO()) as output:
            pairing_status("t3-agent37-desktop-" + "a" * 32, "sandbox-id")
        self.assertEqual(json.loads(output.getvalue()), {"changed": True})
        command.assert_not_called()


class ProvidersTest(unittest.TestCase):
    def test_cliproxy_enables_all_harnesses_and_preserves_custom_settings(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            root = home / ".local/share/t3-agent37"
            data = root / "data"
            t3_path = data / "userdata/settings.json"
            t3_path.parent.mkdir(parents=True)
            t3_path.write_text(json.dumps({"linkedEnvironmentsEnabled": True, "providerInstances": {
                "codex": {"config": {"customModels": [{"slug": "glm-5.3", "name": "GLM 5.3"}], "extra": True}},
            }}))
            profile = {"options": {"baseURL": "https://proxy.example/v1", "apiKey": "{env:T3_AGENT37_CLIPROXY_KEY}"},
                       "models": {"glm-5.3": {}, "gpt-6.2-sol": {}}}
            codex_config = {"binaryPath": "/usr/local/bin/codex", "customModels": ["glm-5.3", "gpt-6.2-sol"]}
            with (patch("pathlib.Path.home", return_value=home),
                  patch.dict(os.environ, {"XDG_CONFIG_HOME": str(home / ".config"), "CLAUDE_CONFIG_DIR": ""}),
                  patch("remote.ROOT", root), patch("remote.DATA", data),
                  patch("remote.cli_proxy_profile", return_value=("cliproxy/glm-5.3", profile)) as catalog,
                  patch("remote.shutil.which", return_value="/usr/local/bin/opencode"),
                  patch("codex_proxy.configure", return_value=codex_config),
                  patch("claude_proxy.configure_claude", return_value=root / "bin/claude")):
                self.assertTrue(provider_ready("cliproxy"))
                catalog.assert_called_once()
            settings = json.loads(t3_path.read_text())
            self.assertTrue(settings["linkedEnvironmentsEnabled"])
            self.assertEqual(settings["defaultModelSelection"], {"instanceId": "opencode", "model": "cliproxy/glm-5.3"})
            instances = settings["providerInstances"]
            self.assertTrue(all(instances[name]["enabled"] for name in ("opencode", "codex", "claudeAgent")))
            self.assertEqual(instances["claudeAgent"]["config"]["customModels"], ["glm-5.3"])
            self.assertEqual(instances["codex"]["config"]["customModels"],
                             [{"slug": "glm-5.3", "name": "GLM 5.3"}, "gpt-6.2-sol"])
            self.assertTrue(instances["codex"]["config"]["extra"])
            opencode = json.loads((home / ".config/opencode/opencode.json").read_text())
            self.assertEqual(opencode["provider"]["cliproxy"]["options"]["apiKey"], "{env:T3_AGENT37_CLIPROXY_KEY}")


class MachineInfoTest(unittest.TestCase):
    def test_sandbox_label_replaces_old_pretty_hostname(self):
        previous = 'PRETTY_HOSTNAME="sandbox-id"\nICON_NAME=computer\n'
        expected = 'PRETTY_HOSTNAME="ts-1"\nICON_NAME=computer\n'
        self.assertEqual(machine_info_with_label(previous, "ts-1"), expected)
        self.assertEqual(machine_info_with_label(expected, "ts-1"), expected)


class SelectRuntimeTest(unittest.TestCase):
    def test_prefers_the_verified_template_version(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            versions = root / "image" / "versions"
            fallback = root / "fallback"
            runtime = versions / "template-version"
            binary = runtime / "t3"
            entrypoint = root / "t3"
            runtime.mkdir(parents=True)
            binary.touch()
            entrypoint.symlink_to(binary)
            self.assertEqual(select_runtime(entrypoint, versions, fallback), fallback)
            (runtime / ".verified").touch()
            self.assertEqual(select_runtime(entrypoint, versions, fallback), runtime)
            self.assertEqual(select_runtime(entrypoint, root / "other", fallback), fallback)
            binary.unlink()
            self.assertEqual(select_runtime(entrypoint, versions, fallback), fallback)


class UnpackRuntimeTest(unittest.TestCase):
    def unpack(self, member, destination, content=None):
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            archive.addfile(member, io.BytesIO(content) if content is not None else None)
        stream.seek(0)
        with tarfile.open(fileobj=stream, mode="r") as archive:
            unpack_runtime(archive, destination)

    def test_extracts_executable_and_preserves_its_execute_bit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            member = tarfile.TarInfo("runtime/t3")
            member.size = len(b"fixture")
            member.mode = 0o4755
            self.unpack(member, root, b"fixture")
            executable = root / "runtime/t3"
            self.assertEqual(executable.read_bytes(), b"fixture")
            self.assertEqual(executable.stat().st_mode & 0o7777, 0o755)

    def test_rejects_outside_paths_and_archive_links(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name, kind in (("../outside", tarfile.REGTYPE), ("/outside", tarfile.REGTYPE),
                               ("runtime/link", tarfile.SYMTYPE), ("runtime/link", tarfile.LNKTYPE)):
                with self.subTest(name=name, kind=kind):
                    member = tarfile.TarInfo(name)
                    member.type = kind
                    member.linkname = "../outside"
                    with self.assertRaisesRegex(RuntimeError, "Unexpected file"):
                        self.unpack(member, root)
            self.assertEqual(list(root.iterdir()), [])
