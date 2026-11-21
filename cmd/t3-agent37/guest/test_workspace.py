import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import workspace


@unittest.skipUnless(shutil.which("git") and shutil.which("timeout"), "Git and GNU timeout are required")
class WorkspaceTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.home = Path(temporary.name)
        self.remotes = self.home / "github"
        self.remotes.mkdir()
        self.workspace_path = "~/Projects/Example Workspace"
        self.root = self.home / "Projects" / "Example Workspace"
        self.source = self.home / "source"
        self.enterContext(patch.dict(os.environ, {
            "GIT_CONFIG_GLOBAL": str(self.home / "gitconfig"),
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_COUNT": "0",
            "GIT_ALLOW_PROTOCOL": "file",
        }))
        self.git("config", "--global", "url." + self.remotes.as_uri() + "/.insteadOf",
                 "https://github.com/example-org/")
        self.git("init", "--initial-branch=main", str(self.source))
        (self.source / "README").write_text("original")
        self.git("-C", str(self.source), "add", "README")
        self.git("-C", str(self.source), "-c", "user.name=Fixture", "-c",
                 "user.email=git@example.test", "commit", "--no-gpg-sign", "-m", "Fixture")

    def git(self, *args):
        return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout.strip()

    def publish(self, name):
        self.git("clone", "--bare", str(self.source), str(self.remotes / f"{name}.git"))

    def advance(self, name, filename):
        path = self.source / filename
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("updated upstream")
        self.git("-C", str(self.source), "add", filename)
        self.git("-C", str(self.source), "-c", "user.name=Fixture", "-c",
                 "user.email=git@example.test", "commit", "--no-gpg-sign", "-m", "Update")
        self.git("-C", str(self.source), "push", str(self.remotes / f"{name}.git"), "main")

    def clone(self, name, ref="", directory=None):
        return workspace.clone_repository(
            self.home, self.workspace_path, f"example-org/{name}", directory or name, ref,
        )

    def test_retry_keeps_local_edits_and_only_clones_missing_repository(self):
        self.publish("example-app")
        self.assertEqual(self.clone("example-app"), "cloned")
        checkout = self.root / "example-app"
        self.git("-C", str(checkout), "switch", "-c", "local-work")
        (checkout / "README").write_text("edited")

        self.assertEqual(self.clone("example-tools"), "clone_failed")
        self.assertFalse((self.root / "example-tools").exists())
        self.assertFalse(list(self.root.glob(".t3-agent37-clone-*")))

        self.publish("example-tools")
        self.assertEqual(self.clone("example-app"), "existing")
        self.assertEqual(self.clone("example-tools", "main", "tools"), "cloned")
        self.assertTrue((self.root / "tools" / "README").is_file())
        self.assertEqual((checkout / "README").read_text(), "edited")
        self.assertEqual(self.git("-C", str(checkout), "branch", "--show-current"), "local-work")

    def test_conflicting_destination_is_preserved(self):
        target = self.root / "example-app"
        target.mkdir(parents=True)
        marker = target / "marker"
        marker.touch()
        self.assertEqual(self.clone("example-app"), "conflict")
        self.assertTrue(marker.is_file())

        self.git("init", str(target))
        self.git("-C", str(target), "remote", "add", "origin", "https://github.com/example-org/example-app.git")
        self.assertEqual(self.clone("example-app"), "conflict")
        self.git("-C", str(target), "add", "marker")
        self.git("-C", str(target), "-c", "user.name=Fixture", "-c",
                 "user.email=git@example.test", "commit", "--no-gpg-sign", "-m", "Other")
        self.git("-C", str(target), "remote", "set-url", "origin", "https://github.com/Other/example-app.git")
        self.assertEqual(self.clone("example-app"), "conflict")
        self.assertEqual(self.git("-C", str(target), "config", "remote.origin.url"),
                         "https://github.com/Other/example-app.git")

    def test_refresh_fast_forwards_and_keeps_unrelated_local_work(self):
        self.publish("example-app")
        self.assertEqual(self.clone("example-app"), "cloned")
        checkout = self.root / "example-app"
        (checkout / "README").write_text("local work")
        self.advance("example-app", "new-file")
        self.assertEqual(self.clone("example-app"), "existing")
        self.assertEqual((checkout / "README").read_text(), "local work")
        self.assertEqual((checkout / "new-file").read_text(), "updated upstream")
        self.assertEqual(self.git("-C", str(checkout), "rev-parse", "HEAD"),
                         self.git("-C", str(self.source), "rev-parse", "HEAD"))

    def test_refresh_refuses_to_overwrite_local_edits(self):
        self.publish("example-app")
        self.assertEqual(self.clone("example-app"), "cloned")
        checkout = self.root / "example-app"
        (checkout / "README").write_text("local work")
        self.advance("example-app", "README")
        self.assertEqual(self.clone("example-app"), "pull_conflict")
        self.assertEqual((checkout / "README").read_text(), "local work")

    def test_root_refresh_cannot_write_into_a_child_checkout(self):
        self.publish("example-workspace")
        self.publish("example-app")
        args = (self.home, self.workspace_path, "example-org/example-workspace", "", "")
        self.assertEqual(workspace.clone_repository(*args), "cloned")
        self.assertEqual(self.clone("example-app"), "cloned")
        self.advance("example-workspace", "example-app/new-file")
        self.assertEqual(workspace.clone_repository(*args), "pull_conflict")
        self.assertFalse((self.root / "example-app" / "new-file").exists())

    def test_publish_preserves_a_folder_created_during_clone(self):
        self.publish("example-app")
        target = self.root / "example-app"
        real_run = subprocess.run

        def create_conflict_before_move(args, **kwargs):
            if args[0] == "mv":
                target.mkdir()
            return real_run(args, **kwargs)

        with patch.object(workspace.subprocess, "run", side_effect=create_conflict_before_move):
            self.assertEqual(self.clone("example-app"), "conflict")
        self.assertEqual(list(target.iterdir()), [])
        self.assertFalse(list(self.root.glob(".t3-agent37-clone-*")))

    def test_root_checkout_can_be_added_around_existing_child_repositories(self):
        self.publish("example-app")
        self.publish("example-workspace")
        self.assertEqual(self.clone("example-app"), "cloned")
        local_file = self.root / "example-app" / "README"
        local_file.write_text("local work")
        args = (self.home, self.workspace_path, "example-org/example-workspace", "", "")
        self.assertEqual(workspace.clone_repository(*args), "cloned")
        self.assertEqual(workspace.clone_repository(*args), "existing")
        self.assertEqual(self.clone("example-app"), "existing")
        self.assertEqual(local_file.read_text(), "local work")
        self.assertEqual((self.root / "README").read_text(), "original")

    def test_root_checkout_preserves_conflicting_user_file(self):
        self.publish("example-workspace")
        self.root.mkdir(parents=True)
        conflict = self.root / "README"
        conflict.write_text("local work")
        args = (self.home, self.workspace_path, "example-org/example-workspace", "", "")
        self.assertEqual(workspace.clone_repository(*args), "conflict")
        self.assertEqual(conflict.read_text(), "local work")
        self.assertFalse((self.root / ".git").exists())

    def test_root_checkout_refuses_to_write_into_existing_child_checkout(self):
        self.publish("example-app")
        self.assertEqual(self.clone("example-app"), "cloned")
        (self.source / "example-app").mkdir()
        (self.source / "example-app" / "extra").write_text("from the root")
        self.git("-C", str(self.source), "add", "example-app")
        self.git("-C", str(self.source), "-c", "user.name=Fixture", "-c",
                 "user.email=git@example.test", "commit", "--no-gpg-sign", "-m", "Overlapping folder")
        self.publish("example-workspace")
        args = (self.home, self.workspace_path, "example-org/example-workspace", "", "")
        self.assertEqual(workspace.clone_repository(*args), "conflict")
        self.assertFalse((self.root / "example-app" / "extra").exists())
