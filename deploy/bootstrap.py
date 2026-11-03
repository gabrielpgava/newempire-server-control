#!/usr/bin/env python3
"""Stage the reviewed native installer; run only after verifying this file's hash."""

import hashlib
import http.client
import ipaddress
import multiprocessing
import os
import pathlib
import socket
import subprocess
import sys
import tempfile
import time

SOURCE_COMMIT = "0bc59f9cb537982048c4d1cbef355ccf982d072c"
FILES = {
    "install.py": "05e2f5ceb013b807ef4ee248cccbae24db666c7002bf14bb3a73f2a3f0918a96",
    "install.sh": "e9171efef22e7dc941a4cf866161cc549a0bd4b0035352fd3e24bb74ab2f6467",
    "verify_release.py": "b4f57f344492e5655ab1950d4a17afde7c23bff29ce3a25350535f438450439d",
    "release-public-key.pem": "b1c24b973d56dee39b5316ba70c3e7c9648a23621bfc329060ed5270781ab5af",
    "control": "6c80bcc0efc1e36d4de73eba84e02e1765d662b5c94663e8d29206abda53d54f",
    "new-empire-runtime": "513469445bba1d54c058b94c262db2e58d776ab56a98701fae7ed6e0028a3d96",
    "systemd/new-empire-control.service": "e5c36842d55aae33b841ab91dc589f69cd4a0e9d251e70ad67981f2191020a9d",
    "systemd/new-empire-update.service": "9ff9f54fe1dff3b1c3c29dc67725ad24f6d7de4c038b8f353731e54c0b80a54c",
    "sudoers/new-empire-control": "d4c1ce3e2c81386de1ad3074524284c4ab9ac5891c42af7d1677059ce0d0b2cd",
}
HOST = "raw.githubusercontent.com"
LIMIT = 65536
TRANSFER_TIMEOUT = 30


# No deployment module can be imported until its bytes have been authenticated.
def public_socket(address, timeout=10, source_address=None):
    host, port = address
    if host != HOST or port != 443 or source_address is not None:
        raise ValueError("unsupported bootstrap destination")
    addresses = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    if not addresses or any(not ipaddress.ip_address(item[4][0]).is_global for item in addresses):
        raise ValueError("bootstrap host resolved to a non-public address")
    for family, kind, protocol, _, target in addresses:
        connection = socket.socket(family, kind, protocol)
        try:
            connection.settimeout(timeout)
            connection.connect(target)
            return connection
        except OSError:
            connection.close()
    raise OSError("cannot connect to the official bootstrap host")


def fetch(name):
    if name not in FILES:
        raise ValueError("unknown deployment file")
    deadline = time.monotonic() + 30
    connection = http.client.HTTPSConnection(HOST, timeout=10)
    connection._create_connection = public_socket
    try:
        connection.request("GET", f"/gabrielpgava/newempire-server-control/{SOURCE_COMMIT}/deploy/{name}", headers={"User-Agent": "New-Empire-Bootstrap", "Accept-Encoding": "identity"})
        response = connection.getresponse()
        if response.status != 200:
            raise ValueError("deployment download refused HTTP " + str(response.status) + "; redirects are not followed")
        length = response.getheader("Content-Length")
        if length is not None and not 0 <= int(length) <= LIMIT:
            raise ValueError("deployment download exceeds its size limit")
        data = bytearray()
        while True:
            if time.monotonic() > deadline:
                raise ValueError("deployment download exceeded its time limit")
            block = response.read1(min(16384, LIMIT - len(data) + 1))
            if not block:
                break
            data.extend(block)
            if len(data) > LIMIT:
                raise ValueError("deployment download exceeds its size limit")
        if length is not None and int(length) != len(data):
            raise ValueError("deployment download was truncated")
        return bytes(data)
    finally:
        connection.close()


def transfer(output, name):
    try:
        output.send_bytes(b"\x01" + fetch(name))
    except Exception:
        output.send_bytes(b"\x00")
    finally:
        output.close()


def download(name):
    if name not in FILES:
        raise ValueError("unknown deployment file")
    # A separate process bounds DNS, TLS, headers and framing, not just body reads.
    context = multiprocessing.get_context("fork")
    incoming, outgoing = context.Pipe(duplex=False)
    process = context.Process(target=transfer, args=(outgoing, name), daemon=True)
    try:
        process.start()
        outgoing.close()
        if not incoming.poll(TRANSFER_TIMEOUT):
            raise ValueError("deployment download exceeded its time limit")
        message = incoming.recv_bytes(LIMIT + 1)
        if message[:1] != b"\x01":
            raise ValueError("deployment download failed")
        return message[1:]
    except EOFError:
        raise ValueError("deployment download ended before a complete response") from None
    finally:
        incoming.close()
        outgoing.close()
        if process.pid is not None:
            if process.is_alive():
                process.terminate()
            process.join(timeout=1)
            if process.is_alive():
                process.kill()
                process.join(timeout=1)


def stage_deployment(stage):
    for name, expected in FILES.items():
        data = download(name)
        if hashlib.sha256(data).hexdigest() != expected:
            raise ValueError("deployment SHA-256 mismatch: " + name)
        destination = stage / name
        destination.parent.mkdir(parents=True, exist_ok=True)
        with destination.open("xb") as output:
            output.write(data)


def main():
    if os.geteuid() != 0 or os.getuid() != 0 or sys.platform != "linux":
        print("Bootstrap requires a root administrator on Linux.", file=sys.stderr)
        return 1
    try:
        # The existing key verifier requires root-owned, non-writable ancestors.
        with tempfile.TemporaryDirectory(prefix="new-empire-deploy-", dir="/root") as temporary:
            stage = pathlib.Path(temporary)
            stage_deployment(stage)
            subprocess.run(["/bin/sh", str(stage / "install.sh"), *sys.argv[1:]], check=True, env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "LC_ALL": "C"})
        return 0
    except (OSError, ValueError, http.client.HTTPException, subprocess.SubprocessError) as error:
        print("Bootstrap stopped: " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
