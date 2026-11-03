import contextlib
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import re
import shlex
import socket
import threading
import time
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
        self.assertEqual(bootstrap.SOURCE_COMMIT, "0bc59f9cb537982048c4d1cbef355ccf982d072c")
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

    def run_main(self, transform=None, transport=None):
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

        with mock.patch.object(bootstrap.tempfile, "TemporaryDirectory", return_value=temporary), mock.patch.object(bootstrap.os, "getuid", return_value=0), mock.patch.object(bootstrap.os, "geteuid", return_value=0), mock.patch.object(bootstrap.sys, "platform", "linux"), mock.patch.object(bootstrap.sys, "argv", ["bootstrap.py", *arguments]), mock.patch.object(bootstrap.sys, "stderr", output), mock.patch.object(bootstrap.subprocess, "run", side_effect=installed), (transport if transport is not None else self.transport(transform)):
            result = bootstrap.main()
        self.assertFalse(stage.exists(), "private staging must be removed on success and failure")
        return result, calls, output.getvalue()

    @contextlib.contextmanager
    def slow_transport(self, phase):
        def connected(*args, **kwargs):
            client, server = socket.socketpair()
            class LocalSocket(socket.socket):
                def setsockopt(self, level, option, value):
                    if level != socket.IPPROTO_TCP or option != socket.TCP_NODELAY:
                        super().setsockopt(level, option, value)
            client = LocalSocket(fileno=client.detach())
            client.settimeout(10)

            def serve():
                with server:
                    try:
                        request = b""
                        while b"\r\n\r\n" not in request:
                            request += server.recv(4096)
                        name = request.split(b" ")[1].decode().split("/deploy/", 1)[1]
                        data = (HERE / name).read_bytes()
                        if name == "install.py" and phase == "headers":
                            for value in b"HTTP/1.1 200 OK\r\n":
                                server.sendall(bytes([value])); time.sleep(0.01)
                            server.sendall(b"Content-Length: " + str(len(data)).encode() + b"\r\n\r\n" + data)
                        elif name == "install.py":
                            server.sendall(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
                            for value in b"0000000000000001\r\n":
                                server.sendall(bytes([value])); time.sleep(0.01)
                            server.sendall(data[:1] + b"\r\n" + format(len(data) - 1, "x").encode() + b"\r\n" + data[1:] + b"\r\n0\r\n\r\n")
                        else:
                            server.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: " + str(len(data)).encode() + b"\r\n\r\n" + data)
                    except OSError:
                        pass

            threading.Thread(target=serve, daemon=True).start()
            return client

        with mock.patch.object(bootstrap.http.client, "HTTPSConnection", bootstrap.http.client.HTTPConnection), mock.patch.object(bootstrap, "public_socket", side_effect=connected):
            yield

    def test_wall_clock_bounds_slow_headers_and_chunk_framing_before_install(self):
        for phase in ("headers", "chunk framing"):
            with self.subTest(phase=phase), mock.patch.object(bootstrap, "TRANSFER_TIMEOUT", 0.05, create=True):
                started = time.monotonic()
                result, calls, output = self.run_main(transport=self.slow_transport(phase))
                self.assertEqual(result, 1)
                self.assertEqual(calls, [])
                self.assertLess(time.monotonic() - started, 1)
                self.assertRegex(output, r"time limit|timed out")

    def test_wall_clock_bounds_blocking_dns_before_install(self):
        def resolve(*args, **kwargs):
            time.sleep(0.2)
            return [(socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", ("127.0.0.1", 443))]

        with mock.patch.object(bootstrap, "TRANSFER_TIMEOUT", 0.05, create=True), mock.patch.object(socket, "getaddrinfo", side_effect=resolve):
            started = time.monotonic()
            with self.assertRaises(ValueError):
                bootstrap.download("install.py")
            self.assertLess(time.monotonic() - started, 0.18)

    def test_installer_runs_only_after_all_files_verify(self):
        self.require_bootstrap()
        result, calls, output = self.run_main()
        self.assertEqual(result, 0)
        self.assertEqual(len(calls), 1)
        self.assertEqual(output, "")

    def test_interrupted_worker_stops_without_installer_or_traceback(self):
        def interrupted(name, data):
            os._exit(1)

        try:
            result, calls, output = self.run_main(interrupted)
        except EOFError:
            self.fail("interrupted download escaped the nonfatal bootstrap error handler")
        self.assertEqual(result, 1)
        self.assertEqual(calls, [])
        self.assertIn("Bootstrap stopped", output)
        self.assertNotIn("Traceback", output)

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
        self.assertLess(command.index("sha256sum"), command.index('/usr/bin/python3 -I "$work/bootstrap.py"'))
        self.assertIn('"${VERSION:?', command)
        self.assertIn('"${JAVA_EXECUTABLE:?', command)
        self.assertNotRegex(command, r"curl[^;]*\|\s*(?:sudo\s+)?(?:sh|bash|python)")
        self.assertRegex(command, r'--proto [\x22\x27]=https[\x22\x27]')
        self.assertIn("--max-time", command)

    def test_readme_initial_fetch_rejects_private_dns_and_checksum_correct_redirects(self):
        line = next(line for line in (HERE.parent / "README.md").read_text().splitlines() if "curl " in line and "bootstrap.py" in line)
        arguments = shlex.split(line)
        original = arguments[arguments.index("-c") + 1]
        digest = hashlib.sha256((HERE / "bootstrap.py").read_bytes()).hexdigest()
        for addresses, status, accepted in ((["127.0.0.1"], "200", False), (["93.184.216.34", "10.0.0.1"], "200", False), (["93.184.216.34"], "302", False), (["93.184.216.34"], "200", True), (["2606:4700::1111"], "200", True)):
            with self.subTest(addresses=addresses, status=status), tempfile.TemporaryDirectory() as temporary:
                root = pathlib.Path(temporary)
                (root / "timeout.py").write_text('''import json, os, socket, sys
from unittest.mock import patch
assert sys.argv[1:5] == ["10", "/usr/bin/python3", "-I", "-c"]
values = [(socket.AF_INET6 if ":" in value else socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", (value, 443)) for value in json.loads(os.environ["DNS_VALUES"])]
with patch("socket.getaddrinfo", return_value=values):
    exec(sys.argv[5], {"__name__": "__main__"})
''')
                (root / "curl.py").write_text('''import json, os, pathlib, sys
pathlib.Path(os.environ["TRACE"]).write_text(json.dumps(sys.argv[1:]))
pathlib.Path(sys.argv[sys.argv.index("-o") + 1]).write_bytes(pathlib.Path(os.environ["PAYLOAD"]).read_bytes())
print(os.environ["HTTP_STATUS"], end="")
''')
                (root / "executed.py").write_text('import os, pathlib; pathlib.Path(os.environ["EXECUTED"]).write_text("called")\n')
                python = shlex.quote(bootstrap.sys.executable)
                body = original.replace("PATH=/usr/sbin:/usr/bin:/sbin:/bin; export PATH;", "")
                body = body.replace("-d /root/new-empire-bootstrap.XXXXXXXX", "-d " + shlex.quote(str(root / "work.XXXXXXXX")))
                body = body.replace("curl --disable", python + " -I " + shlex.quote(str(root / "curl.py")) + " --disable")
                body = body.replace("timeout 10 /usr/bin/python3", python + " -I " + shlex.quote(str(root / "timeout.py")) + " 10 /usr/bin/python3")
                body = body.replace('/usr/bin/python3 -I "$work/bootstrap.py"', python + " -I " + shlex.quote(str(root / "executed.py")) + ' "$work/bootstrap.py"')
                body = re.sub(r"\b[0-9a-f]{64}\b", digest, body)
                environment = {"PATH": "/usr/bin:/bin", "HOME": temporary, "DNS_VALUES": json.dumps(addresses), "HTTP_STATUS": status, "PAYLOAD": str(HERE / "bootstrap.py"), "TRACE": str(root / "trace"), "EXECUTED": str(root / "executed")}
                result = bootstrap.subprocess.run(["/bin/sh", "-c", body, "sh", "1.2.3", "/usr/lib/jvm/example/bin/java"], env=environment, stdout=bootstrap.subprocess.PIPE, stderr=bootstrap.subprocess.PIPE, timeout=3)
                self.assertEqual((root / "executed").exists(), accepted, result.stderr.decode())
                if any(not bootstrap.ipaddress.ip_address(value).is_global for value in addresses):
                    self.assertFalse((root / "trace").exists(), "private DNS reached curl")
                if accepted:
                    self.assertEqual(result.returncode, 0)
                    curl_arguments = json.loads((root / "trace").read_text())
                    self.assertIn("--resolve", curl_arguments)
                    self.assertIn("--noproxy", curl_arguments)
                    target = "[" + addresses[0] + "]" if ":" in addresses[0] else addresses[0]
                    self.assertEqual(curl_arguments[curl_arguments.index("--resolve") + 1], "raw.githubusercontent.com:443:" + target)
                    self.assertEqual(curl_arguments[curl_arguments.index("--noproxy") + 1], "*")


if __name__ == "__main__":
    unittest.main()
