import gzip
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from terrascale import install_links, install_shared


class WorkspaceToolsTest(unittest.TestCase):
    def test_links_committed_tools_for_each_provider_and_preserves_local_files(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            workspace = home / "work"
            skill = workspace / "skills/setup"
            skill.mkdir(parents=True)
            (skill / "SKILL.md").write_text("A test skill")
            helper = workspace / "bin/ts-helper"
            helper.parent.mkdir()
            helper.touch(mode=0o755)
            for _ in range(2):
                install_links(workspace, home)
            for folder in (".agents", ".claude", ".codex", ".config/opencode"):
                self.assertEqual((home / folder / "skills/setup").resolve(), skill)
            target = home / ".local/bin/ts-helper"
            self.assertEqual(target.resolve(), helper)
            target.unlink()
            target.write_text("existing local file")
            with self.assertRaises(FileExistsError):
                install_links(workspace, home)
            self.assertEqual(target.read_text(), "existing local file")

    def test_shared_build_verifies_digest_and_identity_before_installing(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            (home / "work/ts-orchestrator").mkdir(parents=True)
            (home / ".local/bin").mkdir(parents=True)
            stage = home / ".local/share/t3-agent37"
            stage.mkdir(parents=True)
            name = "ts-orch-" + "a" * 32 + ".gz"
            destination = home / ".local/bin/ts-orch"
            destination.write_bytes(b"keep old build")
            payload = b"the workstation build"
            target = {"arch": "amd64", "revision": "b" * 40}
            with (patch("terrascale.Path.home", return_value=home),
                  patch("terrascale.orchestrator_target", return_value=target),
                  patch("terrascale.build_info") as info):
                (stage / name).write_bytes(gzip.compress(payload))
                with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                    install_shared("~/work", name, "c" * 64)
                self.assertFalse((stage / name).exists())
                self.assertEqual(destination.read_bytes(), b"keep old build")
                info.assert_not_called()

                digest = hashlib.sha256(payload).hexdigest()
                info.return_value = {"GOOS": "linux", "GOARCH": "arm64",
                                     "vcs.revision": target["revision"], "vcs.modified": "false"}
                (stage / name).write_bytes(gzip.compress(payload))
                with self.assertRaisesRegex(ValueError, "revision or platform"):
                    install_shared("~/work", name, digest)
                self.assertEqual(destination.read_bytes(), b"keep old build")

                info.return_value["GOARCH"] = "amd64"
                (stage / name).write_bytes(gzip.compress(payload))
                install_shared("~/work", name, digest)
                self.assertEqual(destination.read_bytes(), payload)
                self.assertTrue(destination.stat().st_mode & 0o111)
                self.assertFalse((stage / name).exists())
