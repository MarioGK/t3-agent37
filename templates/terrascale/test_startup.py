import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
from threading import Barrier
import tomllib
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

import startup


class TomlAndCodexTest(unittest.TestCase):
    def test_pure_python_toml_preserves_existing_settings_and_adds_cliproxy(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            config = home / "config.toml"
            config.write_text(
                'approval_policy = "on-request"\n'
                'features = ["a", "b"]\n\n'
                '[model_providers.custom]\n'
                'name = "Existing"\n'
                'requires_openai_auth = true\n',
                encoding="utf-8",
            )
            with (patch.dict(os.environ, {"CODEX_HOME": str(home)}),
                  patch.object(startup.shutil, "which", return_value="/tools/codex")):
                result = startup.configure_codex("https://proxy.example/v1", "glm-5.3", ("glm-5.3", "gpt-6.2-sol"))
            self.assertEqual(result, {
                "setupMode": "existing",
                "binaryPath": "/tools/codex",
                "homePath": str(home.resolve()),
                "customModels": ["glm-5.3", "gpt-6.2-sol"],
            })
            parsed = tomllib.loads(config.read_text(encoding="utf-8"))
            self.assertEqual(parsed["approval_policy"], "on-request")
            self.assertEqual(parsed["features"], ["a", "b"])
            self.assertEqual(parsed["model"], "glm-5.3")
            self.assertEqual(parsed["model_provider"], startup.CODEX_PROVIDER)
            self.assertEqual(parsed["model_providers"]["custom"]["name"], "Existing")
            self.assertEqual(parsed["model_providers"][startup.CODEX_PROVIDER], {
                "name": "CLIProxyAPI",
                "base_url": "https://proxy.example/v1",
                "env_key": "T3_AGENT37_CLIPROXY_KEY",
                "wire_api": "responses",
                "requires_openai_auth": False,
            })

    def test_fresh_codex_config_uses_owner_only_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory) / ".codex"
            with (patch.dict(os.environ, {"CODEX_HOME": str(home)}),
                  patch.object(startup.shutil, "which", return_value="/tools/codex")):
                startup.configure_codex("https://proxy.example/v1", "gpt-6.2-sol")
            config = home / "config.toml"
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)
            self.assertEqual(tomllib.loads(config.read_text(encoding="utf-8"))["model"], "gpt-6.2-sol")


class ClaudeProxyTest(unittest.TestCase):
    def test_launcher_sets_gateway_and_preserves_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            tools = home / "tools with spaces"
            tools.mkdir()
            binary = tools / "claude"
            binary.write_text(
                '#!/bin/sh\nprintf \'%s\\n\' "$ANTHROPIC_BASE_URL" "$ANTHROPIC_AUTH_TOKEN" '
                '"$ANTHROPIC_MODEL" "$ANTHROPIC_DEFAULT_HAIKU_MODEL" "${ANTHROPIC_API_KEY:-}" "$1"\n'
            )
            binary.chmod(0o700)
            root = home / "t3-agent37"
            env = {"HOME": directory, "PATH": str(tools), "T3_AGENT37_CLIPROXY_KEY": "fixture-key",
                   "ANTHROPIC_API_KEY": "stale-key"}
            with patch.dict(os.environ, env, clear=True):
                wrapper = startup.configure_claude(root, "https://proxy.example/anthropic/v1/")
            alias = home / ".local/bin/claude"
            self.assertEqual(alias.resolve(), wrapper)
            env["PATH"] = os.pathsep.join([str(alias.parent), str(tools)])
            with patch.dict(os.environ, env, clear=True):
                self.assertEqual(startup.configure_claude(root, "https://proxy.example/anthropic"), wrapper)
            self.assertNotIn("fixture-key", wrapper.read_text())
            result = subprocess.run([alias, "test argument"], env=env, check=True, capture_output=True, text=True)
            self.assertEqual(result.stdout.splitlines(), [
                "https://proxy.example/anthropic", "fixture-key", "glm-5.3", "glm-5.3", "", "test argument",
            ])

    def test_rejects_insecure_endpoint(self):
        for endpoint in ("http://proxy.example", "https://secret@proxy.example", "https://proxy.example?q=secret"):
            with self.subTest(endpoint=endpoint), self.assertRaisesRegex(RuntimeError, "HTTPS CLIProxy"):
                startup.claude_base_url(endpoint)


class ProvidersAndRuntimeTest(unittest.TestCase):
    def test_cliproxy_uses_prefetched_models_without_network_call(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            root = home / ".local/share/t3-agent37"
            data = root / "data"
            t3_path = data / "userdata/settings.json"
            t3_path.parent.mkdir(parents=True)
            t3_path.write_text(json.dumps({"linkedEnvironmentsEnabled": True, "providerInstances": {
                "codex": {"config": {"customModels": [{"slug": "glm-5.3", "name": "GLM 5.3"}], "extra": True}},
            }}))
            with (patch("pathlib.Path.home", return_value=home),
                  patch.dict(os.environ, {
                      "XDG_CONFIG_HOME": str(home / ".config"), "CLAUDE_CONFIG_DIR": "",
                      "CODEX_HOME": str(home / ".codex"),
                      "T3_AGENT37_CLIPROXY_URL": "https://proxy.example",
                      "T3_AGENT37_CLIPROXY_KEY": "fixture-key",
                  }),
                  patch.object(startup, "ROOT", root), patch.object(startup, "DATA", data),
                  patch.object(startup, "build_opener", side_effect=AssertionError("should not fetch network catalog")),
                  patch.object(startup.shutil, "which", side_effect=lambda name: f"/usr/local/bin/{name}"),
                  patch.object(startup, "configure_claude", return_value=root / "bin/claude")):
                self.assertTrue(startup.provider_ready("cliproxy", cliproxy_models=["glm-5.3", "gpt-6.2-sol", "gpt-image-1"]))
            settings = json.loads(t3_path.read_text())
            self.assertEqual(settings["defaultModelSelection"], {"instanceId": "opencode", "model": "cliproxy/glm-5.3"})
            self.assertEqual(settings["providerInstances"]["codex"]["config"]["customModels"],
                             [{"slug": "glm-5.3", "name": "GLM 5.3"}, "gpt-6.2-sol"])
            opencode = json.loads((home / ".config/opencode/opencode.json").read_text())
            self.assertIn("glm-5.3", opencode["provider"]["cliproxy"]["models"])
            self.assertNotIn("gpt-image-1", opencode["provider"]["cliproxy"]["models"])

            # Falls back cleanly when glm-5.3 is absent from the catalog.
            with patch.dict(os.environ, {
                "T3_AGENT37_CLIPROXY_URL": "https://proxy.example",
                "T3_AGENT37_CLIPROXY_KEY": "fixture-key",
                "T3_AGENT37_CLIPROXY_MODEL": "",
            }):
                model, cfg = startup.cli_proxy_profile(["claude-opus-5-5", "gpt-6.2-sol", "gpt-image-2"])
                self.assertEqual(model, "cliproxy/gpt-6.2-sol")
                self.assertNotIn("gpt-image-2", cfg["models"])

    def test_machine_info_and_runtime_selection_and_unpack(self):
        self.assertEqual(
            startup.machine_info_with_label('PRETTY_HOSTNAME="old"\nICON_NAME=computer\n', "ts-1"),
            'PRETTY_HOSTNAME="ts-1"\nICON_NAME=computer\n',
        )
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            versions, fallback = root / "image/versions", root / "fallback"
            runtime = versions / "v1"
            runtime.mkdir(parents=True)
            binary = runtime / "t3"
            binary.touch()
            entry = root / "t3"
            entry.symlink_to(binary)
            self.assertEqual(startup.select_runtime(entry, versions, fallback), fallback)
            (runtime / ".verified").touch()
            self.assertEqual(startup.select_runtime(entry, versions, fallback), runtime)

            dest = root / "unpacked"
            dest.mkdir()
            stream = io.BytesIO()
            with tarfile.open(fileobj=stream, mode="w") as archive:
                member = tarfile.TarInfo("runtime/t3")
                member.size = 7
                member.mode = 0o4755
                archive.addfile(member, io.BytesIO(b"fixture"))
            stream.seek(0)
            with tarfile.open(fileobj=stream, mode="r") as archive:
                startup.unpack_runtime(archive, dest)
            self.assertEqual((dest / "runtime/t3").stat().st_mode & 0o7777, 0o755)

            bad = io.BytesIO()
            with tarfile.open(fileobj=bad, mode="w") as archive:
                member = tarfile.TarInfo("../outside")
                archive.addfile(member, io.BytesIO(b"x"))
            bad.seek(0)
            with tarfile.open(fileobj=bad, mode="r") as archive, self.assertRaisesRegex(RuntimeError, "Unexpected file"):
                startup.unpack_runtime(archive, dest)


class StageAndOneShotUpTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.home = Path(tmp.name)
        self.root = self.home / ".local/share/t3-agent37"
        self.root.mkdir(parents=True, mode=0o700)
        self.stage = "launch-" + "a" * 32
        self.addCleanup(os.umask, os.umask(0o077))
        self.enterContext(patch.object(startup, "ROOT", self.root))

    def test_stage_consumption_is_private_and_deletes_on_read_and_refuses_symlinks(self):
        stage_dir = self.root / self.stage
        stage_dir.mkdir(mode=0o700)
        spec_file = stage_dir / "spec.json"
        spec_file.write_text(json.dumps({"provider": "codex", "name": "ts-1"}))
        consumed = startup.consume_stage(self.stage)
        self.assertEqual(consumed["name"], "ts-1")
        self.assertFalse(stage_dir.exists())

        outside = self.home / "outside"
        outside.mkdir()
        (outside / "spec.json").write_text("{}")
        (self.root / self.stage).symlink_to(outside)
        with self.assertRaises(RuntimeError):
            startup.cleanup_stage(self.stage)
        self.assertTrue((outside / "spec.json").is_file())

    def test_one_shot_up_runs_all_stages_and_returns_proof_pairing_and_owner(self):
        t3_proof = {"environmentId": "env-1", "version": "1", "label": "ts-1", "labelSaved": True,
                    "port": 3773, "providerReady": True, "peerProxy": True, "desktopConfirmed": False}
        ts_proof = {"id": "node-1", "hostname": "a37-ts-1-abcdefghij", "url": "https://a37-ts-1-abcdefghij.example.ts.net"}
        ws_proof = {"checkouts": {"~/Projects/Ex": "org/root", "~/Projects/Ex/app": "org/app"},
                    "seededWorktrees": 1, "isTerraScale": False}
        with (patch.object(startup, "setup_t3", return_value=dict(t3_proof)),
              patch.object(startup, "publish_tailnet", return_value=dict(ts_proof)),
              patch.object(startup, "apply_github_handoff", return_value={"github": True, "gitIdentity": True}),
              patch.object(startup, "checkout_workspace", return_value=ws_proof),
              patch.object(startup, "register_projects") as reg,
              patch.object(startup, "create_pairing", return_value={"pairUrl": ts_proof["url"] + "/pair#token=abc"}),
              patch.object(startup, "issue_peer_admin", return_value={"token": "owner-tok"})):
            result = startup.run_up({
                "provider": "cliproxy", "name": "ts-1", "peerLinks": True,
                "tailscale": {"hostname": ts_proof["hostname"], "authKey": "tskey-auth-fixture"},
                "github": {"token": "gho_fixture", "name": "Dev", "email": "dev@example.test"},
                "workspace": {"path": "~/Projects/Ex", "root_repository": "org/root", "repositories": [{"repo": "org/app"}]},
                "pairing": {"label": "t3-agent37-desktop-" + "1" * 32, "ttl": "360s"},
                "issueOwner": True,
            })
        reg.assert_called_once_with(ws_proof["checkouts"])
        self.assertTrue((self.root / "workspace-ready").is_file())
        self.assertEqual(result["environmentId"], "env-1")
        self.assertEqual(result["tailnetUrl"], ts_proof["url"])
        self.assertEqual(result["pairingUrl"], ts_proof["url"] + "/pair#token=abc")
        self.assertEqual(result["ownerToken"], "owner-tok")
        self.assertTrue(result["github"])
        self.assertTrue(result["workspaceReady"])
        self.assertEqual(result["seededWorktrees"], 1)

    def test_require_workspace_ready_short_circuits_before_setup(self):
        with patch.object(startup, "setup_t3", side_effect=AssertionError("should not run setup")):
            self.assertEqual(startup.run_up({"requireWorkspaceReady": True}), {"workspacePending": True})

    def test_preboot_warms_tailnet_and_providers_and_defers_fresh_cliproxy_boot(self):
        runtime = self.home / "runtime"
        runtime.mkdir()
        (runtime / ".verified").touch()
        t3_bin = runtime / "t3"
        t3_bin.touch()
        data = self.root / "data"
        env = {
            "HOME": str(self.home),
            "XDG_CONFIG_HOME": str(self.home / ".config"),
            "CODEX_HOME": str(self.home / ".codex"),
            "CLAUDE_CONFIG_DIR": str(self.home / ".claude"),
            "T3_AGENT37_NAME": "ts-1",
            "T3_AGENT37_PROVIDER": "cliproxy",
            "T3_AGENT37_PEER_LINKS": "1",
            "T3_AGENT37_CLIPROXY_URL": "https://proxy.example",
            "T3_AGENT37_CLIPROXY_KEY": "fixture-key",
            "T3_AGENT37_CLIPROXY_HAS_SOL": "1",
        }
        with (patch("pathlib.Path.home", return_value=self.home),
              patch.dict(os.environ, env),
              patch.object(startup, "DATA", data),
              patch.object(startup, "RUNTIME", runtime),
              patch.object(startup, "T3", t3_bin),
              patch.object(startup, "save_environment_label", return_value=True) as label,
              patch.object(startup, "tailnet_start_daemon") as ts_daemon,
              patch.object(startup, "boot") as boot_fn,
              patch.object(startup, "descriptor", return_value=None),
              patch.object(startup, "build_opener", side_effect=AssertionError("preboot must not call network")),
              patch.object(startup.shutil, "which", side_effect=lambda name: f"/usr/local/bin/{name}"),
              patch.object(startup, "configure_claude", return_value=self.root / "bin/claude")):
            startup.preboot()
            label.assert_called_once_with("ts-1")
            ts_daemon.assert_called_once()
            boot_fn.assert_not_called()
            self.assertTrue((self.root / "peer-proxy").is_file())
            self.assertTrue((self.home / ".agent37/hooks/post-restart.sh").is_file())
            settings = json.loads((data / "userdata/settings.json").read_text())
            self.assertTrue(settings["linkedEnvironmentsEnabled"])
            self.assertEqual(settings["providerInstances"]["codex"]["config"]["customModels"], ["glm-5.3", "gpt-6.2-sol"])

            # On restart, opencode.json already exists so preboot starts T3 immediately.
            opencode = self.home / ".config/opencode/opencode.json"
            opencode.parent.mkdir(parents=True, exist_ok=True)
            opencode.write_text("{}")
            startup.preboot()
            boot_fn.assert_called_once()

    def test_tailnet_cert_error_detects_letsencrypt_rate_limit(self):
        ts_root = self.root / "tailscale"
        ts_root.mkdir(mode=0o700)
        with patch.object(startup, "TAILSCALE_ROOT", ts_root):
            self.assertEqual(startup.tailnet_cert_error(), {"rateLimited": False})
            (ts_root / "tailscaled.log").write_text(
                '2026/11/23 14:09:46 cert("a37-ts-1.tail.example.ts.net"): getCertPEM: '
                '429 urn:ietf:params:acme:error:rateLimited: too many certificates (50) '
                'already issued for "tail.example.ts.net" in the last 168h0m0s, '
                'retry after 2026-11-23 17:22:50 UTC\n'
            )
            self.assertEqual(startup.tailnet_cert_error(), {"rateLimited": True, "retryAfter": "2026-11-23 17:22:50 UTC"})


class PairingAndRegistrationTest(unittest.TestCase):
    def test_pairing_status_long_poll_returns_immediately_on_redeem_and_confirm(self):
        label = "t3-agent37-desktop-" + "a" * 32
        previous = "t3-agent37-desktop-" + "b" * 32
        redeemed = [{"subject": "one-time-token", "client": {"label": label}, "lastConnectedAt": None}]
        confirmed = [{"subject": "one-time-token", "client": {"label": label}, "lastConnectedAt": "2026-11-23T00:00:00Z"}]
        responses = iter([[], redeemed, confirmed])
        with (tempfile.TemporaryDirectory() as directory,
              patch.object(startup, "ROOT", Path(directory)),
              patch.object(startup, "descriptor", return_value={"environmentId": "sandbox-id"}),
              patch.object(startup.time, "sleep"),
              patch.object(startup.subprocess, "run", side_effect=lambda *a, **kw: SimpleNamespace(stdout=json.dumps(next(responses))))):
            first = startup.pairing_status(label, "sandbox-id", previous, wait_seconds=5.0, was_redeemed=False)
            self.assertEqual(first, {"confirmed": False, "redeemed": True})
            second = startup.pairing_status(label, "sandbox-id", previous, wait_seconds=5.0, was_redeemed=True)
            self.assertEqual(second, {"confirmed": True, "redeemed": True})
            self.assertTrue(startup.desktop_confirmed("sandbox-id"))

    def test_parallel_project_registration_with_native_fallback(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            current, archived = home / "current", home / "archived"
            current.mkdir()
            archived.mkdir()
            repos = {str(current): "github.com/fixture/current", str(archived): "github.com/fixture/archived"}
            active = {}
            barrier = Barrier(2, timeout=3)

            def open_req(req, timeout):
                if req.get_method() == "GET":
                    return io.BytesIO(json.dumps({"projects": [
                        {"workspaceRoot": k, "repositoryIdentity": {"canonicalKey": v}} for k, v in active.items()
                    ]}).encode())
                name = json.loads(req.data)["workspaceRoot"]
                barrier.wait()
                if name == str(archived):
                    raise HTTPError(req.full_url, 409, "archived", {}, None)
                active[name] = repos[name]
                return io.BytesIO(b"{}")

            def run(args, **kwargs):
                if args[1:4] == ["auth", "session", "issue"]:
                    return SimpleNamespace(stdout='{"token":"fixture","sessionId":"reg"}')
                if args[1:3] == ["project", "add"]:
                    active[args[-1]] = repos[args[-1]]
                return SimpleNamespace(returncode=0)

            with (patch("pathlib.Path.home", return_value=home),
                  patch.object(startup, "local_opener", return_value=SimpleNamespace(open=open_req)),
                  patch.object(startup.subprocess, "run", side_effect=run) as commands):
                startup.register_projects({"~/current": "fixture/current", "~/archived": "fixture/archived"})
            self.assertEqual(active, repos)
            self.assertEqual(sum(c.args[0][1:3] == ["project", "add"] for c in commands.call_args_list), 1)
            self.assertEqual(sum(c.args[0][1:4] == ["auth", "session", "revoke"] for c in commands.call_args_list), 1)


@unittest.skipUnless(shutil.which("git") and shutil.which("timeout"), "Git and GNU timeout are required")
class WorkspaceAndGitHubTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.home = Path(tmp.name)
        self.remotes = self.home / "github"
        self.remotes.mkdir()
        self.workspace_path = "~/Projects/Example Workspace"
        self.root = self.home / "Projects/Example Workspace"
        self.source = self.home / "source"
        self.enterContext(patch.dict(os.environ, {
            "HOME": str(self.home),
            "GH_CONFIG_DIR": str(self.home / ".config/gh"),
            "GH_TOKEN": "", "GITHUB_TOKEN": "",
            "GIT_CONFIG_GLOBAL": str(self.home / ".gitconfig"),
            "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_COUNT": "0", "GIT_ALLOW_PROTOCOL": "file",
        }))
        self.git("config", "--global", "url." + self.remotes.as_uri() + "/.insteadOf", "https://github.com/example-org/")
        self.git("init", "--initial-branch=main", str(self.source))
        (self.source / "README").write_text("original")
        self.git("-C", str(self.source), "add", "README")
        self.git("-C", str(self.source), "-c", "user.name=Fix", "-c", "user.email=g@e.test", "commit", "--no-gpg-sign", "-m", "Init")

    def git(self, *args):
        return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout.strip()

    def publish(self, name):
        self.git("clone", "--bare", str(self.source), str(self.remotes / f"{name}.git"))

    def advance(self, name, filename, text="updated upstream"):
        path = self.source / filename
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        self.git("-C", str(self.source), "add", filename)
        self.git("-C", str(self.source), "-c", "user.name=Fix", "-c", "user.email=g@e.test", "commit", "--no-gpg-sign", "-m", "Up")
        self.git("-C", str(self.source), "push", str(self.remotes / f"{name}.git"), "main")

    def test_workspace_clones_refreshes_adopts_root_and_uses_snapshots(self):
        self.publish("example-app")
        snapshots = self.home / "worktree-snapshots"
        snap = snapshots / "example-org/example-app"
        snap.parent.mkdir(parents=True)
        self.git("clone", "https://github.com/example-org/example-app.git", str(snap))
        self.advance("example-app", "new-file")

        with patch.object(startup, "PUBLIC_WORKTREE_SNAPSHOTS", snapshots):
            self.assertEqual(startup.clone_repository(self.home, self.workspace_path, "example-org/example-app", "example-app", ""), "seeded")
        self.assertEqual((self.root / "example-app/new-file").read_text(), "updated upstream")

        # Root adoption around existing child repo + conflict protection.
        self.publish("example-workspace")
        self.assertEqual(startup.clone_repository(self.home, self.workspace_path, "example-org/example-workspace", "", ""), "cloned")
        self.advance("example-workspace", "example-app/conflict")
        self.assertEqual(startup.clone_repository(self.home, self.workspace_path, "example-org/example-workspace", "", ""), "pull_conflict")
        self.assertFalse((self.root / "example-app/conflict").exists())

    def test_terrascale_manifest_merged_in_guest(self):
        self.git("config", "--global", "--add", "url." + self.remotes.as_uri() + "/.insteadOf", "https://github.com/TerraScale/")
        self.publish("workspace")
        self.publish("terrascale")
        self.publish("ts-orchestrator")
        self.advance("workspace", "repos.txt", "terrascale\nts-orchestrator\n")
        out = startup.checkout_workspace(self.home, {
            "path": self.workspace_path, "root_repository": "TerraScale/workspace",
            "repositories": [{"repo": "TerraScale/terrascale", "ref": "main"}],
        })
        self.assertTrue(out["isTerraScale"])
        self.assertEqual(sorted(out["checkouts"].values()), ["TerraScale/terrascale", "TerraScale/ts-orchestrator", "TerraScale/workspace"])
        self.assertEqual(startup.merge_terrascale_manifest([], "../outside")[1], "invalid")

    def test_host_only_git_config_preserves_existing_settings(self):
        startup.git_config("--global", "user.name", "Existing Name")
        startup.git_config("--global", "--add", "url.https://mirror.example/.insteadOf", "ssh://git@github.com/")
        self.assertTrue(startup.configure_identity({"name": "Other", "email": "dev@example.test"}))
        startup.configure_git_for_github("/fixture/gh")
        self.assertEqual(startup.git_config("--global", "--get", "user.name").strip(), "Existing Name")
        self.assertEqual(startup.git_config("--global", "--get", "user.email").strip(), "dev@example.test")
        self.assertEqual(startup.git_config("--global", "--get-all", "url.https://github.com/.insteadOf").splitlines(), ["git@github.com:"])


class TerraScaleToolsTest(unittest.TestCase):
    def test_links_and_cached_orchestrator_build(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            workspace = home / "work"
            skill = workspace / "skills/setup"
            skill.mkdir(parents=True)
            (skill / "SKILL.md").write_text("skill")
            helper = workspace / "scripts/sync-skills"
            helper.parent.mkdir()
            helper.touch(mode=0o755)
            startup.install_terrascale_links(workspace, home)
            for folder in (".gemini/config/skills", ".agents", ".claude", ".codex", ".config/opencode"):
                target = home / (folder if folder.endswith("skills") else folder + "/skills") / "setup"
                self.assertEqual(target.resolve(), skill)
            self.assertEqual((home / ".local/bin/ts-sync-skills").resolve(), helper)

            orch = workspace / "ts-orchestrator"
            orch.mkdir(parents=True)
            cache = home / "orch-go-cache"
            (cache / "build").mkdir(parents=True)
            (cache / "modules").mkdir()
            dest = home / ".local/bin/ts-orch"
            with (patch.object(startup, "ORCHESTRATOR_CACHE", cache),
                  patch.object(startup, "_run_tool", return_value="b" * 40),
                  patch.object(startup, "build_orchestrator") as build):
                startup.install_orchestrator(orch, home)
            build.assert_called_once()
            self.assertEqual(build.call_args.args[2]["GOCACHE"], str(cache / "build"))


if __name__ == "__main__":
    unittest.main()
