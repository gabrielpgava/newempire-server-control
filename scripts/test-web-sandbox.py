#!/usr/bin/env python3
"""Test/build the frontend without network or host credentials; retain only bounded dist files."""

import argparse
import io
import os
import pathlib
import shutil
import subprocess
import tarfile
import tempfile
import threading
import uuid


LIMIT = 64 << 20
TIMEOUT_SECONDS = 600


def validate_archive(raw):
    if len(raw) > LIMIT:
        raise ValueError('Frontend artifact exceeds 64 MiB')
    archive = tarfile.open(fileobj=io.BytesIO(raw), mode='r:')
    total = 0
    seen = set()
    for entry in archive.getmembers():
        name = pathlib.PurePosixPath(entry.name)
        if name.is_absolute() or '..' in name.parts or '\\' in entry.name or not name.parts or name.parts[0] != 'dist':
            raise ValueError('Unsafe frontend artifact path')
        if entry.name in seen or not (entry.isdir() or entry.isfile()):
            raise ValueError('Frontend artifact contains links, special files, or duplicates')
        seen.add(entry.name)
        total += entry.size
        if entry.size < 0 or total > LIMIT:
            raise ValueError('Expanded frontend artifact exceeds 64 MiB')
    if 'dist/index.html' not in seen:
        raise ValueError('Frontend artifact is missing index.html')
    return archive


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dependencies', required=True, type=pathlib.Path,
                        help='Private directory containing Linux node_modules installed with npm ci --ignore-scripts')
    args = parser.parse_args()
    root = pathlib.Path(__file__).resolve().parent.parent
    dependencies = (args.dependencies / 'node_modules').resolve(strict=True)
    scratch = pathlib.Path(tempfile.mkdtemp(prefix='nec-web-', dir=os.environ.get('TMPDIR')))
    source = scratch / 'source'
    shutil.copytree(root / 'web', source,
                    ignore=shutil.ignore_patterns('node_modules', 'dist', '.env*', 'test-results', 'playwright-report'))
    image = 'node:' + (root / '.node-version').read_text().strip() + '-bookworm-slim'
    name = 'nec-web-test-' + uuid.uuid4().hex[:16]
    command = ['docker', 'run', '--rm', '--name', name, '--network', 'none', '--read-only',
               '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--cpus', '2',
               '--memory', '3g', '--pids-limit', '256', '--ulimit', 'fsize=67108864:67108864',
               '--tmpfs', '/scratch:rw,exec,nosuid,nodev,size=2147483648',
               '--mount', f'type=bind,src={source},dst=/source,readonly',
               '--mount', f'type=bind,src={dependencies},dst=/dependencies,readonly', image,
               'sh', '-c',
               'mkdir -p /scratch/web /scratch/home && cp -R /source/. /scratch/web/ && '
               'cp -R /dependencies /scratch/web/node_modules && cd /scratch/web && '
               'env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=/scratch/home TMPDIR=/scratch '
               'npm_config_cache=/scratch/npm npm test -- --maxWorkers=2 >&2 && '
               'env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=/scratch/home TMPDIR=/scratch '
               'npm_config_cache=/scratch/npm npm run build >&2 && tar -cf - dist']
    process = subprocess.Popen(command, stdout=subprocess.PIPE)
    timer = threading.Timer(TIMEOUT_SECONDS, lambda: subprocess.run(['docker', 'kill', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
    timer.start()
    raw = bytearray()
    try:
        while True:
            chunk = process.stdout.read(65536)
            if not chunk:
                break
            raw.extend(chunk)
            if len(raw) > LIMIT:
                subprocess.run(['docker', 'kill', name], check=False)
                raise ValueError('Frontend build produced too much output')
        status = process.wait()
        if status != 0:
            raise RuntimeError(f'Sandboxed frontend validation failed (exit {status}; watchdog limit {TIMEOUT_SECONDS} seconds)')
    finally:
        timer.cancel()
        process.stdout.close()
    archive = validate_archive(bytes(raw))
    destination = root / 'web' / 'dist'
    staged = scratch / 'validated'
    staged.mkdir()
    for entry in archive.getmembers():
        relative = pathlib.PurePosixPath(entry.name).relative_to('dist')
        target = staged.joinpath(*relative.parts)
        if entry.isdir():
            target.mkdir(parents=True, exist_ok=True)
        else:
            target.parent.mkdir(parents=True, exist_ok=True)
            with archive.extractfile(entry) as reader, target.open('xb') as writer:
                shutil.copyfileobj(reader, writer)
    if destination.exists():
        shutil.rmtree(destination)  # Vite's ignored generated distribution only.
    shutil.copytree(staged, destination)
    print('Sandboxed frontend tests and build passed; validated web/dist retained.')


if __name__ == '__main__':
    main()
