"""Small, dependency-free repository contract checks used by make check."""

import json
import pathlib
import sys


def node_version_file(root=pathlib.Path('.')):
    path = root / '.node-version'
    if path.is_file() and path.read_text().strip():
        return path
    raise ValueError('A nonempty root .node-version is required')


def openapi_file(root=pathlib.Path('.')):
    files = [root / f'docs/api/openapi.{ext}' for ext in ('json', 'yaml', 'yml')]
    found = [path for path in files if path.is_file()]
    if len(found) != 1:
        raise ValueError('Exactly one docs/api/openapi.json, .yaml, or .yml is required')
    return found[0]


def repository(root=pathlib.Path('.')):
    for name in ('LICENSE', 'README.md', '.go-version', 'go.mod', 'go.sum',
                 'web/package-lock.json', 'web/package.json', 'cmd/control/main.go',
                 'deploy/release-public-key.txt', 'docs/releases.md',
                 'docs/development.md', 'docs/testing.md'):
        if not (root / name).is_file():
            raise ValueError(f'Mandatory file is missing: {name}')
    node_version_file(root)
    openapi_file(root)
    scripts = json.loads((root / 'web/package.json').read_text())['scripts']
    for name in ('typecheck', 'test', 'build'):
        if not scripts.get(name):
            raise ValueError(f'web/package.json must define {name}')


def main():
    if len(sys.argv) != 2:
        raise ValueError('Usage: check.py node-version-file|openapi-file|repository')
    commands = {'node-version-file': node_version_file,
                'openapi-file': openapi_file, 'repository': repository}
    result = commands[sys.argv[1]]()
    if result is not None:
        print(result)


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError) as exc:
        sys.exit(str(exc))
