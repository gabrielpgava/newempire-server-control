#!/usr/bin/env python3
"""Install a signed stable Linux release (latest by default). Run as root."""

import argparse
import contextlib
import grp
import http.client
import importlib.util
import ipaddress
import json
import os
import pathlib
import pwd
import re
import shlex
import shutil
import socket
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.parse

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("verify_release", HERE / "verify_release.py")
vr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(vr)
PANEL = "new-empire-control"
ETC = "/etc/new-empire-control"
INSTALL = "/opt/new-empire-control"
DATA = "/var/lib/new-empire-control"
MANAGED = "/srv/new-empire/instances"
HOSTS = {"github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"}
# The trust root is part of this reviewed installer, never downloaded with assets.
PINNED_KEY = "DVI1QlV02lloZudVLPo64ZEOjQiEQR5PKyMlA+eVdG8="
PINNED_KEY_SHA256 = "dd6804fd279cc3935f9dc3669dc900d81f7e1d0a1382b67f007568a615e5bd7f"


def command(*args):
    return subprocess.run(args, check=True, env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "LC_ALL": "C"}, stdin=subprocess.DEVNULL)


def required_versions():
    env = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "LC_ALL": "C"}
    sudo = subprocess.check_output(["/usr/bin/sudo", "--version"], env=env, stdin=subprocess.DEVNULL).decode("utf-8", errors="replace")
    match = re.search(r"Sudo version (\d+)\.(\d+)\.(\d+)", sudo)
    if not match or tuple(map(int, match.groups())) < (1, 9, 10):
        raise ValueError("sudo 1.9.10+ is required for argument regular expressions")
    systemd = subprocess.check_output(["/usr/bin/systemctl", "--version"], env=env, stdin=subprocess.DEVNULL).decode("utf-8", errors="replace")
    match = re.match(r"systemd (\d+)", systemd)
    if not match or int(match.group(1)) < 247:
        raise ValueError("systemd 247+ is required for the service hardening policy")


@contextlib.contextmanager
def directory(path, create=False, owner=(0, 0), mode=0o755):
    parts = pathlib.PurePosixPath(path).parts
    if not path.startswith("/") or ".." in parts or str(pathlib.PurePosixPath(path)) != path:
        raise ValueError("noncanonical installation path")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        root = os.fstat(fd)
        if root.st_uid != 0 or root.st_mode & 0o022:
            raise ValueError("filesystem root is not trusted")
        for index, part in enumerate(parts[1:]):
            final = index == len(parts) - 2
            created = False
            try:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            except FileNotFoundError:
                if not create:
                    raise
                os.mkdir(part, 0o700, dir_fd=fd)
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                created = True
            parent = fd
            fd = child
            if created:
                os.fchown(fd, *(owner if final else (0, 0)))
                os.fchmod(fd, mode if final else 0o755)
                os.fsync(parent)
            os.close(parent)
            info = os.fstat(fd)
            expected = owner if final else (0, 0)
            if (info.st_uid, info.st_gid) != expected or info.st_mode & 0o022:
                raise ValueError("installation directory has unsafe ownership/permissions: " + path)
        yield fd
    finally:
        os.close(fd)


def program(path):
    with directory(str(pathlib.Path(path).parent)) as parent:
        fd = os.open(pathlib.Path(path).name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022 or not info.st_mode & 0o111:
                raise ValueError("executable must be root-owned and not writable by other users: " + path)
        finally:
            os.close(fd)


def write_once(path, data, mode=0o644, gid=0, preserve=False):
    parent_path, name = os.path.split(path)
    with directory(parent_path) as parent:
        try:
            fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode, dir_fd=parent)
        except FileExistsError:
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
            with os.fdopen(fd, "rb") as existing:
                info = os.fstat(existing.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
                    raise ValueError("existing installed file is unsafe: " + path)
                if not preserve and existing.read(vr.LIMIT + 1) != data:
                    raise ValueError("existing installed file differs; review offline instead of overwriting: " + path)
            return
        with os.fdopen(fd, "wb") as output:
            output.write(data)
            output.flush()
            os.fchown(output.fileno(), 0, gid)
            os.fchmod(output.fileno(), mode)
            os.fsync(output.fileno())
        os.fsync(parent)


def public_socket(address, timeout=30, source_address=None):
    host, port = address
    if host not in HOSTS or port != 443 or source_address is not None:
        raise ValueError("unsupported download destination")
    addresses = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    if not addresses or any(not ipaddress.ip_address(item[4][0]).is_global for item in addresses):
        raise ValueError("release download resolved to a non-public address")
    for family, kind, protocol, _, target in addresses:
        connection = socket.socket(family, kind, protocol)
        try:
            connection.settimeout(timeout)
            connection.connect(target)
            return connection
        except OSError:
            connection.close()
    raise OSError("cannot connect to official release host")


def download(url, destination, limit):
    deadline = time.monotonic() + 300
    for _ in range(4):
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme != "https" or parsed.hostname not in HOSTS or parsed.port not in (None, 443) or parsed.username or parsed.password or parsed.fragment:
            raise ValueError("release redirect left the official HTTPS host allowlist")
        connection = http.client.HTTPSConnection(parsed.hostname, timeout=30)
        connection._create_connection = public_socket
        try:
            connection.request("GET", urllib.parse.urlunsplit(("", "", parsed.path, parsed.query, "")), headers={"User-Agent": "New-Empire-Installer", "Accept-Encoding": "identity"})
            response = connection.getresponse()
            if response.status in (301, 302, 303, 307, 308):
                url = urllib.parse.urljoin(url, response.getheader("Location", ""))
                continue
            if response.status != 200:
                raise ValueError("official release download failed with HTTP " + str(response.status))
            if response.getheader("Content-Length") and int(response.getheader("Content-Length")) > limit:
                raise ValueError("release download exceeds its size limit")
            total = 0
            with destination.open("xb") as output:
                while True:
                    if time.monotonic() > deadline:
                        raise ValueError("release download exceeded its five-minute limit")
                    data = response.read(min(65536, limit - total + 1))
                    if not data:
                        break
                    total += len(data)
                    if total > limit:
                        raise ValueError("release download exceeds its size limit")
                    output.write(data)
            return
        finally:
            connection.close()
    raise ValueError("too many release redirects")


def ensure_panel():
    try:
        group = grp.getgrnam(PANEL)
    except KeyError:
        program("/usr/sbin/groupadd")
        command("/usr/sbin/groupadd", "--system", PANEL)
        group = grp.getgrnam(PANEL)
    try:
        account = pwd.getpwnam(PANEL)
    except KeyError:
        program("/usr/sbin/useradd")
        command("/usr/sbin/useradd", "--system", "--gid", PANEL, "--no-create-home", "--home-dir", DATA, "--shell", "/usr/sbin/nologin", PANEL)
        account = pwd.getpwnam(PANEL)
    if account.pw_uid == 0 or group.gr_gid == 0 or account.pw_gid != group.gr_gid or account.pw_shell != "/usr/sbin/nologin" or account.pw_dir != DATA:
        raise ValueError("panel account already exists with unexpected identity/home/shell")
    return account.pw_uid, group.gr_gid


def current(version):
    with directory(INSTALL) as parent:
        try:
            info = os.stat("current", dir_fd=parent, follow_symlinks=False)
        except FileNotFoundError:
            os.symlink("versions/" + version, "current", dir_fd=parent)
            os.fsync(parent)
            return
        if not stat.S_ISLNK(info.st_mode) or info.st_uid != 0 or os.readlink("current", dir_fd=parent) != "versions/" + version:
            raise ValueError("another version is installed; use the verified updater, not the bootstrap installer")


def install(args, stage, java_paths):
    for path in (INSTALL, INSTALL + "/versions", ETC, ETC + "/instances", "/etc/sudoers.d", "/usr/local/libexec", MANAGED):
        with directory(path, create=True):
            pass
    # Do not switch a different running installation or rewrite its trust policy.
    with directory(INSTALL) as parent:
        try:
            target = os.readlink("current", dir_fd=parent)
        except FileNotFoundError:
            target = None
        if target is not None and target != "versions/" + args.version:
            raise ValueError("another version is installed; use the verified updater")
    uid, gid = ensure_panel()
    for path, owner in ((DATA, (uid, gid)), (INSTALL + "/updates", (0, 0))):
        with directory(path, create=True, owner=owner, mode=0o700):
            pass
    with directory(INSTALL + "/versions/" + args.version, create=True):
        pass
    binary = stage / "new-empire-control"
    write_once(INSTALL + "/versions/" + args.version + "/new-empire-control", binary.read_bytes(), mode=0o755)
    for source, destination, mode in (
        ("control", INSTALL + "/control", 0o755),
        ("new-empire-runtime", "/usr/local/libexec/new-empire-runtime", 0o755),
        ("systemd/new-empire-control.service", "/etc/systemd/system/new-empire-control.service", 0o644),
        ("systemd/new-empire-update.service", "/etc/systemd/system/new-empire-update.service", 0o644),
        ("sudoers/new-empire-control", "/etc/sudoers.d/new-empire-control", 0o440),
    ):
        write_once(destination, (HERE / source).read_bytes(), mode=mode)
    write_once(ETC + "/release-public-key.pem", (stage / "pinned.pem").read_bytes())
    write_once(ETC + "/release-public-key.txt", (PINNED_KEY + "\n").encode())
    policy = {"panel_user": PANEL, "managed_root": MANAGED, "adopt_roots": [], "java_paths": java_paths, "max_memory_mb": 32768}
    config = {"listen": "127.0.0.1:8080", "public_url": "http://127.0.0.1:8080", "data_dir": DATA, "instance_dir": MANAGED, "backup_dir": DATA + "/backups", "adopt_roots": [], "java_paths": java_paths, "runtime_helper": "/usr/local/libexec/new-empire-runtime", "secure_cookies": False, "max_upload_bytes": 34359738368, "release_repository": vr.REPOSITORY, "release_public_key": PINNED_KEY, "install_dir": INSTALL}
    write_once(ETC + "/runtime.json", (json.dumps(policy, indent=2) + "\n").encode(), mode=0o600, preserve=True)
    write_once(ETC + "/config.json", (json.dumps(config, indent=2) + "\n").encode(), mode=0o640, gid=gid, preserve=True)
    current(args.version)
    command("/usr/sbin/visudo", "--check")
    command("/usr/bin/systemctl", "daemon-reload")
    command("/usr/bin/systemctl", "enable", "--now", "new-empire-control.service")


def local_addresses():
    try:
        program("/usr/bin/hostname")
        raw = subprocess.check_output(["/usr/bin/hostname", "-I"], timeout=3, stdin=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"})
        if len(raw) > 65536:
            return []
    except (OSError, ValueError, subprocess.SubprocessError):
        return []
    networks = [ipaddress.ip_network(value) for value in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7")]
    result = []
    for value in raw.decode("ascii", errors="replace").split():
        try:
            address = ipaddress.ip_address(value)
        except ValueError:
            continue
        if "%" not in value and any(address in network for network in networks) and str(address) not in result:
            result.append(str(address))
    return result


@contextlib.contextmanager
def setup_directory(path, owner):
    # Only secret reporting accepts panel-owned ancestors; installation paths stay root-only.
    parts = pathlib.PurePosixPath(path).parts
    if not path.startswith("/") or ".." in parts or str(pathlib.PurePosixPath(path)) != path:
        raise ValueError("noncanonical setup path")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in (None, *parts[1:]):
            if part is not None:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                os.close(fd)
                fd = child
            info = os.fstat(fd)
            if (info.st_uid, info.st_gid) not in ((0, 0), owner) or info.st_mode & 0o022:
                raise ValueError("unsafe setup directory")
        yield fd
    finally:
        os.close(fd)


def print_installation_access():
    try:
        with directory(ETC):
            config = json.loads(vr.read_regular(ETC + "/config.json", 65536))
        url = config["public_url"]
        if not isinstance(url, str) or not re.fullmatch(r"[!-~]+", url):
            raise ValueError("invalid dashboard URL")
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password:
            raise ValueError("invalid dashboard URL")
        print("Dashboard URL: " + url)
        print("Default localhost address (fresh installs): http://127.0.0.1:8080 (http://localhost:8080)")
        addresses = local_addresses()
        print("Local interface addresses (not dashboard listeners): " + (", ".join(addresses) if addresses else "unavailable"))
        print("Loopback is the default. Remote access requires an SSH tunnel or a configured HTTPS reverse proxy; no network binding was changed.")
        data_dir = config.get("data_dir", "data")
        if data_dir is None:
            data_dir = "data"
        data_dir = os.path.normpath(os.path.join(DATA, data_dir))
        account = pwd.getpwnam(PANEL)
        token_path = str(pathlib.Path(data_dir) / "setup-token")
        read_command = "sudo cat -- " + shlex.quote(token_path)
        if not sys.stdout.isatty():
            print("First-owner code omitted from non-interactive output. If setup is required, read it locally: " + read_command)
            return
        for attempt in range(20):
            try:
                with setup_directory(data_dir, (account.pw_uid, account.pw_gid)) as parent:
                    fd = os.open("setup-token", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
                    with os.fdopen(fd, "rb") as source:
                        info = os.fstat(source.fileno())
                        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != account.pw_uid or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size != 44:
                            raise ValueError("unsafe setup-code file")
                        token = source.read(45)
                if not re.fullmatch(rb"[A-Za-z0-9_-]{43}\n", token):
                    raise ValueError("invalid setup-code file")
                print("First-owner setup code: " + token.decode("ascii").rstrip("\n"))
                print("Use this code to create the first owner in the dashboard. Keep it private; do not share terminal recordings.")
                return
            except FileNotFoundError:
                if attempt < 19:
                    time.sleep(0.5)
        print("Setup code unavailable: startup may be pending or first-owner setup may be complete. If required, read it locally: " + read_command)
    except (OSError, ValueError, TypeError, KeyError):
        print("Access/setup-code reporting unavailable: inspect the installed configuration and protected setup-token ownership/permissions locally. No code was generated or replaced.")


def approved_java(values):
    if not values:
        found = shutil.which("java", path="/usr/sbin:/usr/bin:/sbin:/bin")
        if not found:
            raise ValueError("Java was not found; install a compatible runtime first or pass --java /absolute/path")
        values = [found]
    paths = []
    for java in values:
        path = str(pathlib.Path(java).resolve(strict=True))
        if not re.fullmatch(r"/[A-Za-z0-9_./-]+", path) or path in paths:
            raise ValueError("Java paths must be distinct canonical executable paths without special characters")
        program(path)
        paths.append(path)
    return paths


def latest_version(stage):
    path = stage / "latest.json"
    download(f"https://github.com/{vr.REPOSITORY}/releases/latest/download/release.json", path, 65536)
    raw = vr.read_regular(path, 65536)
    value = json.loads(raw, object_pairs_hook=vr.unique_object)
    if not isinstance(value, dict):
        raise ValueError("latest release manifest must be an object")
    vr.parse_manifest(raw, value["version"], "amd64")
    return value["version"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", help="stable SemVer without v; defaults to the latest official stable release")
    parser.add_argument("--java", action="append", help="root-approved Java executable; defaults to installed java; repeat for multiple runtimes")
    parser.add_argument("--archive", help="local native release archive; otherwise download from the fixed official repository")
    parser.add_argument("--manifest", help="local signed release.json (required with --archive)")
    parser.add_argument("--signature", help="local release.json.sig (required with --archive)")
    default_key = pathlib.Path(ETC + "/release-public-key.pem")
    parser.add_argument("--public-key", default=str(default_key if default_key.exists() else HERE / "release-public-key.pem"), help="root-owned pinned Ed25519 PEM; must match the embedded trust root")
    args = parser.parse_args()
    if args.version is not None and not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", args.version):
        parser.error("--version must be an explicit stable SemVer without v")
    if bool(args.archive) != bool(args.manifest) or bool(args.archive) != bool(args.signature):
        parser.error("--archive, --manifest and --signature must be supplied together")
    if args.archive and not args.version:
        parser.error("offline installation requires an explicit --version")
    if os.getuid() != 0 or os.geteuid() != 0 or os.uname().sysname != "Linux" or not pathlib.Path("/run/systemd/system").is_dir():
        parser.error("installation requires a root administrator on a booted Linux/systemd host")
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(os.uname().machine)
    if arch is None:
        parser.error("only Linux amd64 and arm64 are supported")
    try:
        if vr.PINNED_KEY != PINNED_KEY or vr.PINNED_KEY_SHA256 != PINNED_KEY_SHA256:
            raise ValueError("installer verification trust roots disagree")
        for path in ("/usr/bin/openssl", "/usr/bin/systemctl", "/usr/sbin/visudo", "/usr/sbin/nft", "/usr/bin/sudo", "/usr/sbin/nologin"):
            program(path)
        required_versions()
        java_paths = approved_java(args.java)
        command("/usr/sbin/visudo", "--check", "--file", str(HERE / "sudoers/new-empire-control"))
        with tempfile.TemporaryDirectory(prefix="new-empire-install-", dir="/var/tmp") as temporary:
            stage = pathlib.Path(temporary)
            if args.version is None:
                args.version = latest_version(stage)
            for local, name, limit in ((args.manifest, "release.json", 65536), (args.signature, "release.json.sig", 1024), (args.archive, f"new-empire-control_{args.version}_linux_{arch}.tar.gz", vr.LIMIT)):
                if local:
                    (stage / name).write_bytes(vr.read_regular(local, limit))
                else:
                    download(f"https://github.com/{vr.REPOSITORY}/releases/download/v{args.version}/{name}", stage / name, limit)
            vr.verify(argparse.Namespace(manifest=str(stage / "release.json"), signature=str(stage / "release.json.sig"), public_key=args.public_key, version=args.version, arch=arch, archive=str(stage / f"new-empire-control_{args.version}_linux_{arch}.tar.gz"), output=str(stage / "new-empire-control")))
            install(args, stage, java_paths)
        print("Installed signed release " + args.version + "; enabled new-empire-control.service.")
        print_installation_access()
        print("Minecraft instances require independent root registration with install-service; none were started.")
    except (ValueError, TypeError, KeyError, OSError, EOFError, tarfile.TarError, subprocess.SubprocessError) as error:
        parser.exit(1, "Installation stopped: " + str(error) + ". Existing instance data was not modified. Review partial host setup before retrying.\n")


if __name__ == "__main__":
    main()
