import contextlib
import hashlib
import importlib.util
import io
import os
import pathlib
import socket
import tempfile
import unittest
from unittest import mock

HERE = pathlib.Path(__file__).resolve().parent
NAMES = (
    "install.py", "install.sh", "verify_release.py", "release-public-key.pem",
    "control", "new-empire-runtime", "systemd/new-empire-control.service",
    "systemd/new-empire-update.service", "sudoers/new-empire-control",
)
spec = importlib.util.spec_from_file_location("bootstrap", HERE / "bootstrap.py")
bootstrap = None
if (HERE / "bootstrap.py").is_file():
    bootstrap = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(bootstrap)


class Response(io.BytesIO):
    def __init__(self, data, status=200, headers=None):
        super().__init__(data)
        self.status = status
        self.headers = {"Content-Length": str(len(data))} if headers is None else headers

    def getheader(self, name):
        return self.headers.get(name)


class BootstrapTests(unittest.TestCase):
    def require_bootstrap(self):
        self.assertIsNotNone(bootstrap, "verified bootstrap is missing")

    @contextlib.contextmanager
    def transport(self, transform=None):
        def connection(host, timeout):
            self.assertEqual(host, "raw.githubusercontent.com")
            self.assertLessEqual(timeout, 30)
            client = mock.Mock()

            def request(method, path, headers):
                self.assertEqual(method, "GET")
                prefix = "/gabrielpgava/newempire-server-control/" + bootstrap.SOURCE_COMMIT + "/deploy/"
                self.assertTrue(path.startswith(prefix))
                name = path[len(prefix):]
                self.assertIn(name, NAMES)
                self.assertEqual(headers["Accept-Encoding"], "identity")
                data = (HERE / name).read_bytes()
                client.getresponse.return_value = transform(name, data) if transform else Response(data)

            client.request.side_effect = request
            return client

        with mock.patch.object(bootstrap.http.client, "HTTPSConnection", side_effect=connection):
            yield

    def test_stage_matches_all_nine_pinned_source_files(self):
        self.require_bootstrap()
        self.assertEqual(bootstrap.SOURCE_COMMIT, "646bb0deab8e0128c051df7028f8cc41caa98b5b")
        self.assertEqual(set(bootstrap.FILES), set(NAMES))
        with tempfile.TemporaryDirectory() as temporary, self.transport():
            stage = pathlib.Path(temporary)
            bootstrap.stage_deployment(stage)
            for name in NAMES:
                expected = (HERE / name).read_bytes()
                self.assertEqual(bootstrap.FILES[name], hashlib.sha256(expected).hexdigest())
                self.assertEqual((stage / name).read_bytes(), expected)

    def test_tampered_or_truncated_file_is_not_staged(self):
        self.require_bootstrap()
        for change in (lambda data: b"!" + data[1:], lambda data: data[:-1]):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as temporary:
                stage = pathlib.Path(temporary)
                with self.transport(lambda name, data: Response(change(data))), self.assertRaises(ValueError):
                    bootstrap.stage_deployment(stage)
                self.assertFalse((stage / "install.py").exists())

    def test_oversized_declared_or_streamed_input_fails(self):
        self.require_bootstrap()
        for headers in ({"Content-Length": "65537"}, {}):
            with self.subTest(headers=headers), self.transport(lambda name, data: Response(b"x" * 65537, headers=headers)), self.assertRaises(ValueError):
                bootstrap.download("install.py")

    def test_redirects_and_http_errors_are_not_followed(self):
        self.require_bootstrap()
        for status in (301, 302, 303, 307, 308, 403, 404, 500):
            with self.subTest(status=status), self.transport(lambda name, data: Response(data, status, {"Location": "https://127.0.0.1/secret"})), self.assertRaises(ValueError):
                bootstrap.download("install.py")

    def test_invalid_length_and_timeout_fail_closed(self):
        self.require_bootstrap()
        for length in ("bad", "-1", "1"):
            with self.subTest(length=length), self.transport(lambda name, data: Response(data, headers={"Content-Length": length})), self.assertRaises(ValueError):
                bootstrap.download("install.py")
        with self.transport(), mock.patch.object(bootstrap.time, "monotonic", side_effect=[0, 31]), self.assertRaises(ValueError):
            bootstrap.download("install.py")

    def test_unknown_path_is_rejected_before_network(self):
        self.require_bootstrap()
        for name in ("../install.py", "https://evil.invalid/install.py", "release-public-key.txt"):
            with self.subTest(name=name), mock.patch.object(bootstrap.http.client, "HTTPSConnection") as connection, self.assertRaises(ValueError):
                bootstrap.download(name)
            connection.assert_not_called()

    def test_private_mixed_or_empty_dns_is_rejected_before_connect(self):
        self.require_bootstrap()
        for values in ([], ["127.0.0.1"], ["10.1.2.3"], ["169.254.169.254"], ["::1"], ["fd12::1"], ["93.184.216.34", "192.168.1.2"]):
            addresses = [(socket.AF_INET6 if ":" in value else socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", (value, 443)) for value in values]
            with self.subTest(values=values), mock.patch.object(socket, "getaddrinfo", return_value=addresses), mock.patch.object(socket, "socket") as connection, self.assertRaises(ValueError):
                bootstrap.public_socket(("raw.githubusercontent.com", 443))
            connection.assert_not_called()

    def test_only_fixed_https_destination_can_connect(self):
        self.require_bootstrap()
        for address in (("localhost", 443), ("github.com", 443), ("raw.githubusercontent.com", 80)):
            with self.subTest(address=address), mock.patch.object(socket, "getaddrinfo") as resolve, self.assertRaises(ValueError):
                bootstrap.public_socket(address)
            resolve.assert_not_called()
        with self.assertRaises(ValueError):
            bootstrap.public_socket(("raw.githubusercontent.com", 443), source_address=("127.0.0.1", 0))

    def test_connection_uses_validated_address_without_second_resolution(self):
        self.require_bootstrap()
        target = ("93.184.216.34", 443)
        addresses = [(socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", target)]
        client = mock.Mock()
        with mock.patch.object(socket, "getaddrinfo", return_value=addresses) as resolve, mock.patch.object(socket, "socket", return_value=client):
            result = bootstrap.public_socket(("raw.githubusercontent.com", 443))
        self.assertIs(result, client)
        self.assertEqual(resolve.call_count, 1)
        client.connect.assert_called_once_with(target)

    def run_main(self, transform=None):
        temporary = tempfile.TemporaryDirectory()
        stage = pathlib.Path(temporary.name)
        output = io.StringIO()
        calls = []
        arguments = ["--version", "1.2.3", "--java", "/usr/lib/jvm/example/bin/java"]

        def installed(command, **kwargs):
            calls.append(command)
            self.assertEqual(command, ["/bin/sh", str(stage / "install.sh"), *arguments])
            self.assertEqual(kwargs["env"], {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "LC_ALL": "C"})
            self.assertNotIn("stdout", kwargs, "installer must inherit the terminal")
            self.assertEqual(stage.stat().st_mode & 0o777, 0o700)
            for name in NAMES:
                self.assertEqual((stage / name).read_bytes(), (HERE / name).read_bytes())

        with mock.patch.object(bootstrap.tempfile, "TemporaryDirectory", return_value=temporary), mock.patch.object(bootstrap.os, "getuid", return_value=0), mock.patch.object(bootstrap.os, "geteuid", return_value=0), mock.patch.object(bootstrap.sys, "platform", "linux"), mock.patch.object(bootstrap.sys, "argv", ["bootstrap.py", *arguments]), mock.patch.object(bootstrap.sys, "stderr", output), mock.patch.object(bootstrap.subprocess, "run", side_effect=installed), self.transport(transform):
            result = bootstrap.main()
        self.assertFalse(stage.exists(), "private staging must be removed on success and failure")
        return result, calls, output.getvalue()

    def test_installer_runs_only_after_all_files_verify(self):
        self.require_bootstrap()
        result, calls, output = self.run_main()
        self.assertEqual(result, 0)
        self.assertEqual(len(calls), 1)
        self.assertEqual(output, "")

    def test_any_failed_download_prevents_installer_execution(self):
        self.require_bootstrap()
        for failure in ("tamper", "truncate", "oversize", "redirect", "error"):
            def changed(name, data):
                if name != NAMES[-1]:
                    return Response(data)
                if failure == "tamper":
                    return Response(b"!" + data[1:])
                if failure == "truncate":
                    return Response(data[:-1])
                if failure == "oversize":
                    return Response(b"x" * 65537)
                return Response(data, 302 if failure == "redirect" else 404)

            with self.subTest(failure=failure):
                result, calls, output = self.run_main(changed)
                self.assertEqual(result, 1)
                self.assertEqual(calls, [])
                self.assertIn("Bootstrap stopped", output)
                self.assertNotIn("first-owner", output)

    def test_nonroot_bootstrap_refuses_before_download(self):
        self.require_bootstrap()
        with mock.patch.object(bootstrap.os, "geteuid", return_value=1000), mock.patch.object(bootstrap.sys, "stderr", io.StringIO()), mock.patch.object(bootstrap, "stage_deployment") as stage:
            self.assertEqual(bootstrap.main(), 1)
        stage.assert_not_called()

    def test_readme_pins_bootstrap_checksum_before_execution(self):
        self.require_bootstrap()
        readme = (HERE.parent / "README.md").read_text()
        commands = [line for line in readme.splitlines() if "curl " in line and "bootstrap.py" in line]
        self.assertEqual(len(commands), 1, "document exactly one physical installation command")
        command = commands[0]
        digest = hashlib.sha256((HERE / "bootstrap.py").read_bytes()).hexdigest()
        self.assertIn(digest, command)
        self.assertLess(command.index("sha256sum"), command.index("/usr/bin/python3 -I"))
        self.assertIn('"${VERSION:?', command)
        self.assertIn('"${JAVA_EXECUTABLE:?', command)
        self.assertNotRegex(command, r"curl[^;]*\|\s*(?:sudo\s+)?(?:sh|bash|python)")
        self.assertIn("--proto '=https'", command)
        self.assertIn("--max-time", command)


if __name__ == "__main__":
    unittest.main()
