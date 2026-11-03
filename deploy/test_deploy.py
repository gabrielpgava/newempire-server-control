import base64
import contextlib
import gzip
import hashlib
import importlib.util
import io
import json
import pathlib
import os
import socket
import struct
import tarfile
import tempfile
import unittest
from unittest import mock
from types import SimpleNamespace

HERE = pathlib.Path(__file__).resolve().parent


def load(name):
    spec = importlib.util.spec_from_file_location(name, HERE / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


vr = load("verify_release")
installer = load("install")


def binary():
    value = bytearray(64)
    value[:7] = b"\x7fELF\x02\x01\x01"
    struct.pack_into("<HH", value, 16, 2, 62)
    return bytes(value)


def archive(extra=False, link=False):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz", format=tarfile.USTAR_FORMAT) as package:
        info = tarfile.TarInfo("new-empire-control")
        info.size = 64
        if link:
            info.type = tarfile.SYMTYPE
            info.linkname = "/etc/passwd"
        package.addfile(info, io.BytesIO(binary()))
        if extra:
            info = tarfile.TarInfo("unexpected")
            info.size = 1
            package.addfile(info, io.BytesIO(b"x"))
    return output.getvalue()


def manifest(data):
    return {"version": "1.2.0", "repository": vr.REPOSITORY, "schema": 1, "assets": [
        {"name": f"new-empire-control_1.2.0_linux_{arch}.tar.gz", "os": "linux", "arch": arch, "size": len(data), "sha256": hashlib.sha256(data).hexdigest()}
        for arch in ("amd64", "arm64")
    ]}


class InstallerDefaultsTests(unittest.TestCase):
    def test_discovers_and_resolves_installed_java(self):
        self.assertTrue(callable(getattr(installer, "approved_java", None)), "Java discovery is missing")
        with tempfile.TemporaryDirectory() as temporary:
            executable = pathlib.Path(temporary) / "java-real"
            executable.write_text("runtime fixture")
            alias = pathlib.Path(temporary) / "java"
            alias.symlink_to(executable)
            with mock.patch.object(installer.shutil, "which", return_value=str(alias)), mock.patch.object(installer, "program") as checked:
                self.assertEqual(installer.approved_java(None), [str(executable.resolve())])
                checked.assert_called_once_with(str(executable.resolve()))

    def test_missing_or_untrusted_java_stops(self):
        self.assertTrue(callable(getattr(installer, "approved_java", None)), "Java discovery is missing")
        with mock.patch.object(installer.shutil, "which", return_value=None), self.assertRaisesRegex(ValueError, "Java"):
            installer.approved_java(None)
        with mock.patch.object(installer.shutil, "which", return_value="/usr/bin/python3"), mock.patch.object(installer, "program", side_effect=ValueError("untrusted executable")), self.assertRaisesRegex(ValueError, "untrusted"):
            installer.approved_java(None)

    def test_latest_selects_stable_version_for_fixed_version_downloads(self):
        self.assertTrue(callable(getattr(installer, "latest_version", None)), "latest stable selection is missing")
        with tempfile.TemporaryDirectory() as temporary:
            stage = pathlib.Path(temporary)

            def downloaded(url, destination, limit):
                self.assertEqual(url, "https://github.com/gabrielpgava/newempire-server-control/releases/latest/download/release.json")
                self.assertEqual(limit, 65536)
                destination.write_bytes(json.dumps(manifest(archive()), separators=(",", ":")).encode())

            with mock.patch.object(installer, "download", side_effect=downloaded):
                self.assertEqual(installer.latest_version(stage), "1.2.0")

    def test_latest_rejects_prerelease_traversal_duplicate_and_wrong_repository(self):
        self.assertTrue(callable(getattr(installer, "latest_version", None)), "latest stable selection is missing")
        valid = manifest(archive())
        invalid = [dict(valid, version=value) for value in ("1.2.0-beta.1", "../evil", "01.2.0", 123)]
        invalid += [dict(valid, repository="attacker/fork")]
        payloads = [json.dumps(value, separators=(",", ":")).encode() for value in invalid]
        payloads += [b'{"version":"1.2.0","version":"9.9.9"}', b'[]', b'null']
        for raw in payloads:
            with self.subTest(raw=raw), tempfile.TemporaryDirectory() as temporary:
                with mock.patch.object(installer, "download", side_effect=lambda url, path, limit: path.write_bytes(raw)), self.assertRaises((ValueError, TypeError, KeyError)):
                    installer.latest_version(pathlib.Path(temporary))


class InstallationAccessTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.etc = self.root / "etc"
        self.data = self.root / "data"
        self.etc.mkdir()
        self.data.mkdir(mode=0o700)
        self.config = self.etc / "config.json"
        self.config.write_text(json.dumps({"public_url": "https://panel.example.test", "data_dir": str(self.data)}))
        self.token = self.data / "setup-token"
        self.code = "D" * 43

    def report(self, interactive=True):
        self.assertTrue(callable(getattr(installer, "print_installation_access", None)), "post-install access reporting is missing")
        output = io.StringIO()
        output.isatty = lambda: interactive

        @contextlib.contextmanager
        def directory(path, **kwargs):
            fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                yield fd
            finally:
                os.close(fd)

        account = SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid())
        with mock.patch.object(installer, "ETC", str(self.etc)), mock.patch.object(installer, "DATA", str(self.data)), mock.patch.object(installer, "directory", directory), mock.patch.object(installer.pwd, "getpwnam", return_value=account), mock.patch.object(installer, "local_addresses", return_value=["192.168.1.20", "fd12::20"]), mock.patch.object(installer.sys, "stdout", output):
            installer.print_installation_access()
        return output.getvalue()

    def write_token(self, value=None):
        self.token.write_text((self.code if value is None else value) + "\n")
        self.token.chmod(0o600)

    def test_private_interface_addresses_exclude_invalid_and_nonlocal_values(self):
        self.assertTrue(callable(getattr(installer, "local_addresses", None)), "local address discovery is missing")
        value = b"192.168.1.20 10.2.3.4 172.16.0.1 fd12::20 192.168.1.20 127.0.0.1 ::1 fe80::1 fd12::1%eth0 8.8.8.8 2001:4860::1 0.0.0.0 224.0.0.1 192.0.2.1 bad\x1b\n"
        with mock.patch.object(installer, "program"), mock.patch.object(installer.subprocess, "check_output", return_value=value):
            self.assertEqual(installer.local_addresses(), ["192.168.1.20", "10.2.3.4", "172.16.0.1", "fd12::20"])

    def test_unavailable_or_oversized_address_discovery_is_nonfatal(self):
        self.assertTrue(callable(getattr(installer, "local_addresses", None)), "local address discovery is missing")
        for failure in (FileNotFoundError(), installer.subprocess.TimeoutExpired("hostname", 3)):
            with self.subTest(failure=type(failure).__name__), mock.patch.object(installer, "program"), mock.patch.object(installer.subprocess, "check_output", side_effect=failure):
                self.assertEqual(installer.local_addresses(), [])
        with mock.patch.object(installer, "program"), mock.patch.object(installer.subprocess, "check_output", return_value=b" " * 65537):
            self.assertEqual(installer.local_addresses(), [])

    def test_interactive_report_preserves_custom_url_and_displays_existing_code(self):
        self.write_token()
        before = self.token.read_bytes()
        output = self.report()
        self.assertIn("Dashboard URL: https://panel.example.test", output)
        self.assertIn("127.0.0.1:8080", output)
        self.assertIn("192.168.1.20", output)
        self.assertIn("fd12::20", output)
        self.assertIn("not dashboard listeners", output)
        self.assertIn("First-owner setup code: " + self.code, output)
        self.assertEqual(self.token.read_bytes(), before)
        self.assertNotIn("http://192.168.1.20", output)

    def test_redirected_output_never_opens_or_prints_token(self):
        self.write_token()
        original_open = os.open

        def opened(path, *args, **kwargs):
            self.assertNotEqual(pathlib.Path(path).name, "setup-token", "redirected reporting opened the secret")
            return original_open(path, *args, **kwargs)

        with mock.patch.object(installer.os, "open", side_effect=opened):
            output = self.report(interactive=False)
        self.assertNotIn(self.code, output)
        self.assertIn("sudo cat --", output)
        self.assertIn(str(self.token), output)

    def test_delayed_token_is_read_without_regenerating_it(self):
        def arrive(_):
            self.write_token()

        with mock.patch.object(installer.time, "sleep", side_effect=arrive):
            output = self.report()
        self.assertIn("First-owner setup code: " + self.code, output)
        self.assertEqual(self.token.read_text(), self.code + "\n")

    def test_missing_token_has_bounded_wait_and_no_false_completion_claim(self):
        with mock.patch.object(installer.time, "sleep") as sleep:
            output = self.report()
        self.assertLessEqual(sleep.call_count, 20)
        self.assertGreater(sleep.call_count, 0)
        self.assertFalse(self.token.exists())
        self.assertIn("sudo cat --", output)
        self.assertNotIn("already configured", output)
        self.assertNotIn("First-owner setup code:", output)

    def test_invalid_url_types_are_nonfatal_and_do_not_display_code(self):
        self.write_token()
        for value in (123, True, [], {"url": "https://panel.example.test"}):
            with self.subTest(value=value):
                self.config.write_text(json.dumps({"public_url": value, "data_dir": str(self.data)}))
                try:
                    output = self.report()
                except Exception as error:
                    self.fail("reporting raised instead of failing safely: " + type(error).__name__)
                self.assertIn("reporting unavailable", output)
                self.assertNotIn(self.code, output)

    def test_unsafe_tokens_are_not_displayed(self):
        for kind in ("symlink", "hardlink", "permissions", "oversized", "control", "fifo", "owner"):
            with self.subTest(kind=kind):
                self.token.unlink(missing_ok=True)
                self.write_token()
                if kind == "symlink":
                    saved = self.root / "other-token"
                    self.token.rename(saved)
                    self.token.symlink_to(saved)
                elif kind == "hardlink":
                    os.link(self.token, self.root / "linked-token")
                elif kind == "permissions":
                    self.token.chmod(0o644)
                elif kind == "oversized":
                    self.token.write_text(self.code + "X" * 1024)
                elif kind == "control":
                    self.token.write_text(self.code + "\x1b[2J\n")
                elif kind == "fifo":
                    self.token.unlink()
                    os.mkfifo(self.token)
                info = self.token.lstat()
                if kind == "owner":
                    values = list(info)
                    values[4] = os.getuid() + 1
                    with mock.patch.object(installer.os, "fstat", return_value=os.stat_result(values)):
                        output = self.report()
                else:
                    output = self.report()
                self.assertNotIn(self.code, output)
                self.assertNotIn("First-owner setup code:", output)
                self.token.unlink()


@unittest.skipUnless(os.geteuid() == 0, "isolated root ownership fixture required")
class InstallationAccessPathTests(unittest.TestCase):
    def test_service_relative_default_and_nested_paths_use_real_traversal(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            etc, working = root / "etc", root / "panel"
            etc.mkdir(); working.mkdir(mode=0o700)
            os.chown(working, 1000, 1000)
            values = (None, "state", str(working / "nested" / "state"), "")
            for value in values:
                with self.subTest(value=value):
                    data = working / ("data" if value is None else value)
                    data.mkdir(parents=True, exist_ok=True)
                    for parent in (data, *data.parents):
                        if parent == root:
                            break
                        os.chown(parent, 1000, 1000); parent.chmod(0o700)
                    token = data / "setup-token"
                    token.write_text("R" * 43 + "\n"); token.chmod(0o600); os.chown(token, 1000, 1000)
                    config = {"public_url": "https://panel.example.test"}
                    if value is not None:
                        config["data_dir"] = value
                    (etc / "config.json").write_text(json.dumps(config))
                    for interactive in (True, False):
                        output = io.StringIO(); output.isatty = lambda: interactive
                        with mock.patch.object(installer, "ETC", str(etc)), mock.patch.object(installer, "DATA", str(working)), mock.patch.object(installer.pwd, "getpwnam", return_value=SimpleNamespace(pw_uid=1000, pw_gid=1000)), mock.patch.object(installer, "local_addresses", return_value=[]), mock.patch.object(installer.sys, "stdout", output):
                            installer.print_installation_access()
                        if interactive:
                            self.assertIn("First-owner setup code: " + "R" * 43, output.getvalue())
                        else:
                            self.assertIn("sudo cat -- " + str(token), output.getvalue())
                            self.assertNotIn("R" * 43, output.getvalue())
                    self.assertEqual(token.read_text(), "R" * 43 + "\n")
            with self.assertRaisesRegex(ValueError, "unsafe ownership"):
                with installer.directory(str(working / "nested" / "state"), owner=(1000, 1000)):
                    self.fail("installation traversal accepted panel-owned ancestors")

    def test_unsafe_data_ancestors_do_not_display_code(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary); etc = root / "etc"; etc.mkdir()
            for kind in ("symlink", "permissions", "foreign"):
                with self.subTest(kind=kind):
                    parent = root / kind; parent.mkdir(mode=0o700); os.chown(parent, 1000, 1000)
                    data = parent / "state"; data.mkdir(mode=0o700); os.chown(data, 1000, 1000)
                    token = data / "setup-token"; token.write_text("R" * 43 + "\n"); token.chmod(0o600); os.chown(token, 1000, 1000)
                    path = data
                    if kind == "symlink":
                        link = root / "link"; link.symlink_to(parent); path = link / "state"
                    elif kind == "permissions":
                        parent.chmod(0o770)
                    else:
                        os.chown(parent, 2000, 2000)
                    (etc / "config.json").write_text(json.dumps({"public_url": "https://panel.example.test", "data_dir": str(path)}))
                    output = io.StringIO(); output.isatty = lambda: True
                    with mock.patch.object(installer, "ETC", str(etc)), mock.patch.object(installer.pwd, "getpwnam", return_value=SimpleNamespace(pw_uid=1000, pw_gid=1000)), mock.patch.object(installer, "local_addresses", return_value=[]), mock.patch.object(installer.sys, "stdout", output):
                        installer.print_installation_access()
                    self.assertNotIn("R" * 43, output.getvalue())
                    self.assertIn("reporting unavailable", output.getvalue())


class ReleaseVerificationTests(unittest.TestCase):
    def test_installer_keeps_updater_state_root_owned(self):
        calls = []

        @installer.contextlib.contextmanager
        def directory(path, create=False, owner=(0, 0), mode=0o755):
            calls.append((path, owner, mode))
            yield 0

        with tempfile.TemporaryDirectory() as temporary:
            stage = pathlib.Path(temporary)
            (stage / "new-empire-control").write_bytes(binary())
            (stage / "pinned.pem").write_text("dummy public fixture")
            with mock.patch.object(installer, "directory", directory), mock.patch.object(installer, "ensure_panel", return_value=(1000, 1000)), mock.patch.object(installer, "write_once"), mock.patch.object(installer, "current"), mock.patch.object(installer, "command"), mock.patch.object(installer.os, "readlink", side_effect=FileNotFoundError):
                installer.install(installer.argparse.Namespace(version="1.2.0"), stage, ["/usr/bin/java"])
        self.assertIn((installer.INSTALL + "/updates", (0, 0), 0o700), calls)
        self.assertIn((installer.DATA, (1000, 1000), 0o700), calls)

    @unittest.skipUnless(os.geteuid() == 0, "isolated root ownership fixture required")
    def test_installer_rejects_legacy_panel_owned_updater_state(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary) / "updates"
            path.mkdir(mode=0o700)
            with installer.directory(str(path), mode=0o700):
                pass
            os.chown(path, 1000, 1000)
            with self.assertRaisesRegex(ValueError, "unsafe ownership"):
                with installer.directory(str(path), mode=0o700):
                    self.fail("panel-owned updater state was accepted")

    def test_pinned_key_files_and_fingerprint(self):
        raw = base64.b64decode((HERE / "release-public-key.txt").read_text().strip(), validate=True)
        self.assertEqual(base64.b64encode(raw).decode(), "5jtZuBvFVWvMMHZo5VCuP98GEOt1uxmzvSF+rBzzQlM=")
        pem = (HERE / "release-public-key.pem").read_text().splitlines()
        der = base64.b64decode("".join(pem[1:-1]), validate=True)
        self.assertEqual(der, vr.ED25519_DER_PREFIX + raw)
        self.assertEqual(base64.b64encode(raw).decode(), vr.PINNED_KEY)
        self.assertEqual(vr.PINNED_KEY, installer.PINNED_KEY)
        self.assertEqual(hashlib.sha256(raw).hexdigest(), vr.PINNED_KEY_SHA256)
        self.assertEqual(vr.PINNED_KEY_SHA256, installer.PINNED_KEY_SHA256)

    def test_canonical_manifest(self):
        value = manifest(archive())
        raw = json.dumps(value, separators=(",", ":")).encode()
        self.assertEqual(vr.parse_manifest(raw, "1.2.0", "amd64"), value["assets"][0])
        for bad in (raw + b"\n", raw.replace(b'"schema":1', b'"schema":1,"schema":1'), raw.replace(b'"schema":1', b'"schema":2'), raw.replace(vr.REPOSITORY.encode(), b"attacker/fork"), raw.replace(b'"size":', b'"extra":')):
            with self.assertRaises((ValueError, KeyError)):
                vr.parse_manifest(bad, "1.2.0", "amd64")
        with self.assertRaises(ValueError):
            vr.parse_manifest(raw, "1.2.1", "amd64")

    def test_single_binary_archive(self):
        data = archive()
        asset = manifest(data)["assets"][0]
        with tempfile.TemporaryDirectory() as temporary:
            destination = pathlib.Path(temporary) / "binary"
            vr.extract_binary(data, asset, destination)
            self.assertEqual(destination.read_bytes(), binary())

    def test_bad_archives_fail(self):
        concatenated = gzip.compress(gzip.decompress(archive()) + gzip.decompress(archive()))
        for data in (archive(extra=True), archive(link=True), concatenated):
            with tempfile.TemporaryDirectory() as temporary, self.assertRaises(ValueError):
                vr.extract_binary(data, manifest(data)["assets"][0], pathlib.Path(temporary) / "binary")
        data = archive()
        asset = manifest(data)["assets"][0]
        asset["arch"] = "arm64"
        with tempfile.TemporaryDirectory() as temporary, self.assertRaises(ValueError):
            vr.extract_binary(data, asset, pathlib.Path(temporary) / "binary")
        asset["sha256"] = "0" * 64
        with tempfile.TemporaryDirectory() as temporary, self.assertRaises(ValueError):
            vr.extract_binary(data, asset, pathlib.Path(temporary) / "binary")

    def test_gzip_bomb_bounded_before_tar_parsing(self):
        data = gzip.compress(b"x" * 20000)
        with mock.patch.object(vr, "LIMIT", 1024), tempfile.TemporaryDirectory() as temporary, self.assertRaises(ValueError):
            vr.extract_binary(data, manifest(data)["assets"][0], pathlib.Path(temporary) / "binary")

    def test_download_rejects_private_dns_before_connect(self):
        addresses = [(socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", ("127.0.0.1", 443))]
        with mock.patch.object(socket, "getaddrinfo", return_value=addresses), mock.patch.object(socket, "socket") as create, self.assertRaises(ValueError):
            installer.public_socket(("github.com", 443))
        create.assert_not_called()
        for host in ("localhost", "evil.invalid", "169.254.169.254"):
            with self.assertRaises(ValueError):
                installer.public_socket((host, 443))

    def test_download_redirect_allowlist(self):
        for url in ("http://github.com/release", "https://evil.invalid/release", "https://github.com:444/release", "https://user@github.com/release"):
            with self.assertRaises(ValueError):
                installer.download(url, pathlib.Path("never-written"), 1024)

    def test_local_release_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary)
            (directory / "source").write_text("test")
            (directory / "link").symlink_to("source")
            with self.assertRaises(OSError):
                vr.read_regular(directory / "link", 1024)

    def test_required_security_tool_versions(self):
        for sudo, systemd, valid in ((b"Sudo version 1.9.13p3\n", b"systemd 252 (252)\n", True), (b"Sudo version 1.8.31\n", b"systemd 252\n", False), (b"Sudo version 1.9.13\n", b"systemd 246\n", False)):
            with mock.patch.object(installer.subprocess, "check_output", side_effect=[sudo, systemd]):
                if valid:
                    installer.required_versions()
                else:
                    with self.assertRaises(ValueError):
                        installer.required_versions()


if __name__ == "__main__":
    unittest.main()
