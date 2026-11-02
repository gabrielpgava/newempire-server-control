#!/usr/bin/env python3
"""Verify the official Ed25519 manifest and extract its single Linux binary."""

import argparse
import base64
import hashlib
import gzip
import io
import json
import os
import pathlib
import re
import stat
import subprocess
import tarfile

REPOSITORY = "gabrielpgava/newempire-server-control"
LIMIT = 512 * 1024 * 1024
ED25519_DER_PREFIX = bytes.fromhex("302a300506032b6570032100")
PINNED_KEY = "DVI1QlV02lloZudVLPo64ZEOjQiEQR5PKyMlA+eVdG8="
PINNED_KEY_SHA256 = "dd6804fd279cc3935f9dc3669dc900d81f7e1d0a1382b67f007568a615e5bd7f"


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate JSON key")
        value[key] = item
    return value


def trusted_key(path):
    path = pathlib.Path(path)
    if not path.is_absolute() or ".." in path.parts:
        raise ValueError("pinned public key must have an absolute canonical path")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        root = os.fstat(fd)
        if root.st_uid != 0 or root.st_mode & 0o022:
            raise ValueError("filesystem root is not trusted")
        for index, part in enumerate(path.parts[1:]):
            flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
            if index < len(path.parts) - 2:
                flags |= os.O_DIRECTORY
            child = os.open(part, flags, dir_fd=fd)
            os.close(fd)
            fd = child
            info = os.fstat(fd)
            if info.st_uid != 0 or info.st_mode & 0o022:
                raise ValueError("public key and ancestors must be root-owned and not group/world writable")
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise ValueError("public key must be a regular file")
        with os.fdopen(os.dup(fd), "rb") as source:
            data = source.read(16385)
        if len(data) > 16384:
            raise ValueError("public key is oversized")
        return data
    finally:
        os.close(fd)


def parse_manifest(raw, version, arch):
    value = json.loads(raw, object_pairs_hook=unique_object)
    if list(value) != ["version", "repository", "schema", "assets"]:
        raise ValueError("unexpected manifest fields/order")
    if value["version"] != version or not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version):
        raise ValueError("manifest version differs from the explicit stable version")
    if value["repository"] != REPOSITORY or type(value["schema"]) is not int or value["schema"] != 1:
        raise ValueError("manifest repository/schema is invalid")
    assets = value["assets"]
    if not isinstance(assets, list) or len(assets) != 2:
        raise ValueError("manifest must describe both supported Linux architectures")
    for asset, expected_arch in zip(assets, ("amd64", "arm64")):
        if list(asset) != ["name", "os", "arch", "size", "sha256"]:
            raise ValueError("unexpected asset fields/order")
        if asset["name"] != f"new-empire-control_{version}_linux_{expected_arch}.tar.gz" or asset["os"] != "linux" or asset["arch"] != expected_arch:
            raise ValueError("release asset identity mismatch")
        if type(asset["size"]) is not int or not 0 < asset["size"] <= LIMIT or not re.fullmatch("[0-9a-f]{64}", asset["sha256"]):
            raise ValueError("invalid release size/checksum")
    canonical = json.dumps(value, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    if canonical != raw:
        raise ValueError("manifest is not canonical JSON")
    return next(asset for asset in assets if asset["arch"] == arch)


def extract_binary(archive, asset, destination):
    if len(archive) != asset["size"] or hashlib.sha256(archive).hexdigest() != asset["sha256"]:
        raise ValueError("release archive size/checksum mismatch")
    with gzip.GzipFile(fileobj=io.BytesIO(archive)) as compressed:
        expanded = compressed.read(LIMIT + 10241)
    if len(expanded) > LIMIT + 10240 or expanded[:100].split(b"\0", 1)[0] != b"new-empire-control" or expanded[156:157] not in (b"0", b"\0"):
        raise ValueError("oversized archive or unsupported tar headers")
    with tarfile.open(fileobj=io.BytesIO(expanded), mode="r:") as package:
        item = package.next()
        if item is None or item.name != "new-empire-control" or not item.isfile() or item.islnk() or item.pax_headers or item.sparse or not 64 <= item.size <= LIMIT:
            raise ValueError("archive must contain exactly one regular new-empire-control binary")
        end = 512 + ((item.size + 511) // 512) * 512
        if item.offset_data != 512 or len(expanded) < end + 1024 or any(expanded[end:]):
            raise ValueError("archive has hidden entries or an invalid end marker")
        source = package.extractfile(item)
        with open(destination, "xb") as output:
            header = source.read(64)
            machine = 62 if asset["arch"] == "amd64" else 183
            if len(header) != 64 or header[:7] != b"\x7fELF\x02\x01\x01" or header[7] not in (0, 3) or int.from_bytes(header[18:20], "little") != machine or int.from_bytes(header[16:18], "little") not in (2, 3):
                raise ValueError("binary is not a matching Linux ELF64 executable")
            output.write(header)
            remaining = item.size - 64
            while remaining:
                block = source.read(min(1024 * 1024, remaining))
                if not block:
                    raise ValueError("truncated binary")
                output.write(block)
                remaining -= len(block)
        if package.next() is not None:
            raise ValueError("archive contains unexpected additional entries")


def read_regular(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise ValueError("release inputs must be regular files, not links or devices")
        data = source.read(limit + 1)
    if len(data) > limit:
        raise ValueError("release input exceeds its size limit")
    return data


def verify(args):
    destination = pathlib.Path(args.output)
    manifest = read_regular(args.manifest, 65536)
    signature_text = read_regular(args.signature, 1024)
    signature = base64.b64decode(signature_text, validate=True)
    if len(signature) != 64 or base64.b64encode(signature) != signature_text:
        raise ValueError("signature must be canonical base64 Ed25519")
    key = trusted_key(args.public_key)
    keyfile = destination.parent / "pinned.pem"
    sigfile = destination.parent / "manifest.sig"
    signedfile = destination.parent / "manifest.json"
    keyfile.write_bytes(key)
    sigfile.write_bytes(signature)
    signedfile.write_bytes(manifest)
    subprocess.run(["/usr/bin/openssl", "pkeyutl", "-verify", "-pubin", "-inkey", str(keyfile), "-rawin", "-in", str(signedfile), "-sigfile", str(sigfile)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    der = subprocess.check_output(["/usr/bin/openssl", "pkey", "-pubin", "-in", str(keyfile), "-outform", "DER"], stderr=subprocess.DEVNULL)
    if len(der) != 44 or not der.startswith(ED25519_DER_PREFIX):
        raise ValueError("pinned key is not Ed25519")
    if base64.b64encode(der[12:]).decode() != PINNED_KEY or hashlib.sha256(der[12:]).hexdigest() != PINNED_KEY_SHA256:
        raise ValueError("public key differs from the embedded release trust root")
    asset = parse_manifest(manifest, args.version, args.arch)
    archive = read_regular(args.archive, asset["size"])
    extract_binary(archive, asset, destination)
    (destination.parent / "public-key.base64").write_bytes(base64.b64encode(der[12:]))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("manifest", "signature", "public-key", "version", "archive", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--arch", required=True, choices=("amd64", "arm64"))
    args = parser.parse_args()
    try:
        verify(args)
    except (ValueError, TypeError, KeyError, OSError, tarfile.TarError, subprocess.SubprocessError) as error:
        parser.exit(1, "Release verification failed: " + type(error).__name__ + ". No installation was performed.\n")


if __name__ == "__main__":
    main()
