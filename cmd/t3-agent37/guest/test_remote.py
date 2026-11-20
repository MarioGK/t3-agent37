import io
from pathlib import Path
import tarfile
import tempfile
import unittest

from remote import machine_info_with_label, select_runtime, unpack_runtime


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
