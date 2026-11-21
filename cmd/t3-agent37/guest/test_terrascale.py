from pathlib import Path
import tempfile
import unittest

from terrascale import install_links


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
