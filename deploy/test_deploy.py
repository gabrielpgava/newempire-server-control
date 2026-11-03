import base64
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
