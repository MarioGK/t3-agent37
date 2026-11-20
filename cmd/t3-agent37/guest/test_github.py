import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import github


class GitHubHandoffTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.home = Path(temporary.name)
        self.stage = "github-handoff-" + "a" * 32
        self.addCleanup(os.umask, os.umask(0o077))
        self.enterContext(patch.object(github, "ROOT", self.home / ".local/share/t3-agent37"))
        self.enterContext(patch.dict(os.environ, {
            "HOME": str(self.home),
            "GH_CONFIG_DIR": str(self.home / ".config/gh"),
            "GH_TOKEN": "",
            "GITHUB_TOKEN": "",
            "GIT_CONFIG_GLOBAL": str(self.home / ".gitconfig"),
            "GIT_CONFIG_COUNT": "0",
            "GIT_CONFIG_NOSYSTEM": "1",
        }))

    def test_credential_staging_is_private_and_consumed_on_failure(self):
        github.main(self.stage, "prepare")
        directory = github.ROOT / self.stage
        self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
        access = directory / "access.json"
        access.write_text(json.dumps({"token": "ghs_fixture"}))
        with patch.object(github, "configure_identity", return_value=False):
            with patch.object(github, "configure_github", side_effect=RuntimeError("fixture error")):
                with self.assertRaises(RuntimeError):
                    github.main(self.stage, "apply")
        self.assertFalse(directory.exists())
        github.main(self.stage, "cleanup")

    def test_staging_refuses_symlink(self):
        outside = self.home / "outside"
        outside.mkdir()
        (outside / "access.json").touch()
        github.ROOT.mkdir(parents=True)
        (github.ROOT / self.stage).symlink_to(outside)
        with self.assertRaises(RuntimeError):
            github.main(self.stage, "cleanup")
        self.assertTrue((outside / "access.json").is_file())

    @unittest.skipUnless(shutil.which("git"), "git must be installed")
    def test_host_only_git_configuration_preserves_existing_values(self):
        github.git_config("--global", "user.name", "Existing Name")
        github.git_config("--global", "credential.https://git.example.com.helper", "fixture-manager")
        github.git_config("--global", "--add", "url.https://github-mirror.example.com/.insteadOf",
                          "ssh://git@github.com/")
        self.assertTrue(github.configure_identity({"name": "Other Name", "email": "fixture@example.test"}))
        for _ in range(2):
            github.configure_git_for_github("/fixture/gh")
        self.assertEqual(github.git_config("--global", "--get", "user.name").strip(), "Existing Name")
        self.assertEqual(github.git_config("--global", "--get", "user.email").strip(), "fixture@example.test")
        self.assertEqual(github.git_config("--global", "--get", "credential.https://git.example.com.helper").strip(),
                         "fixture-manager")
        self.assertEqual(github.git_config("--global", "--get-all", "credential.https://github.com.helper").splitlines(),
                         ["!/fixture/gh auth git-credential"])
        self.assertEqual(github.git_config("--global", "--get-all", "url.https://github.com/.insteadOf").splitlines(),
                         ["git@github.com:"])
        self.assertEqual(github.git_config("--global", "--get-all", "url.https://github-mirror.example.com/.insteadOf")
                         .splitlines(), ["ssh://git@github.com/"])

    @unittest.skipUnless(shutil.which("gh"), "gh must be installed")
    def test_app_token_native_configuration_uses_installed_gh(self):
        class Accepted:
            status = 200

        class Opener:
            def open(self, request, timeout):
                return contextlib.nullcontext(Accepted())

        path = github.github_hosts()
        with patch.object(github, "build_opener", return_value=Opener()):
            github.install_app_login("ghs_fixture", path)
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        command = ["gh", "auth", "git-credential", "get"]
        credential = subprocess.run(command, input=b"protocol=https\nhost=github.com\n\n",
                                    capture_output=True, check=True, timeout=5)
        self.assertIn(b"username=x-access-token", credential.stdout)
        self.assertIn(b"password=ghs_fixture", credential.stdout)
        if shutil.which("git"):
            github.configure_git_for_github(shutil.which("gh"))
            rewrite = subprocess.run(["git", "ls-remote", "--get-url", "git@github.com:example/repo.git"],
                                     capture_output=True, text=True, check=True, timeout=5)
            self.assertEqual(rewrite.stdout.strip(), "https://github.com/example/repo.git")
            filled = subprocess.run(["git", "credential", "fill"], input=b"protocol=https\nhost=github.com\n\n",
                                    capture_output=True, check=True, timeout=5)
            self.assertIn(b"password=ghs_fixture", filled.stdout)

    def test_apply_output_reports_status_only(self):
        github.main(self.stage, "prepare")
        (github.ROOT / self.stage / "access.json").write_text(json.dumps({"token": "fixture-key"}))
        output = io.StringIO()
        with patch.object(github, "configure_identity", return_value=True):
            with patch.object(github, "configure_github", return_value=None):
                with contextlib.redirect_stdout(output):
                    github.main(self.stage, "apply")
        self.assertEqual(json.loads(output.getvalue()), {"github": True, "gitIdentity": True})
