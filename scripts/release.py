"""Conventional versions and fail-closed GitHub draft publication (stdlib only)."""

import argparse
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import tarfile
import tempfile
from urllib.parse import quote

REPOSITORY = 'gabrielpgava/newempire-server-control'
WORKFLOW = f'{REPOSITORY}/.github/workflows/release.yml'
CONVENTIONAL = re.compile(r'^(feat|fix|perf|refactor|docs|test|build|ci|chore|style|revert)(\([^\r\n()]+\))?(!)?: [^\r\n]+$')
SEMVER = re.compile(r'^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$')
LEVELS = ['auto', 'patch', 'minor', 'major']


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs).stdout.strip()


def check_output(args):
    if os.path.lexists(args.output):
        raise ValueError('release output must not already exist')
    message = 'release output must be outside the checkout or Git-ignored'
    try:
        root = pathlib.Path(run('git', 'rev-parse', '--show-toplevel')).resolve()
    except subprocess.CalledProcessError:
        raise ValueError(message + ' (checkout could not be inspected)') from None
    try:
        relative = pathlib.Path(args.output).resolve().relative_to(root)
    except ValueError:
        return
    if subprocess.run(['git', 'check-ignore', '-q', '--', str(relative)],
                      cwd=root, capture_output=True).returncode != 0:
        raise ValueError(message)


def version(value):
    match = SEMVER.fullmatch(value)
    if not match:
        raise ValueError(f'Invalid SemVer: {value}')
    for part in (match[4], match[5]):
        if part and any(not item for item in part.split('.')):
            raise ValueError(f'Invalid SemVer identifier: {value}')
    if match[4] and any(item.isdigit() and len(item) > 1 and item[0] == '0'
                        for item in match[4].split('.')):
        raise ValueError(f'Leading zero in prerelease: {value}')
    return tuple(map(int, match.group(1, 2, 3))), match[4]


def release_level(message):
    title = message.splitlines()[0] if message else ''
    match = CONVENTIONAL.fullmatch(title)
    if (match and match[3]) or re.search(r'^BREAKING[ -]CHANGE:\s*\S', message, re.MULTILINE):
        return 3
    return {'feat': 2, 'fix': 1, 'perf': 1, 'revert': 1}.get(match[1], 0) if match else 0


def next_version(tags, messages, bump='auto', channel='stable'):
    if bump not in LEVELS or channel not in ('stable', 'beta', 'rc'):
        raise ValueError('Unsupported release bump or channel')
    stable = [(version(tag)[0], tag) for tag in tags if tag.startswith('v')
              and SEMVER.fullmatch(tag) and version(tag)[1] is None]
    base, base_tag = max(stable, default=((0, 0, 0), ''))
    level = max([LEVELS.index(bump)] + [release_level(msg) for msg in messages])
    if not level:
        return '', base_tag
    core = list(base)
    index = 3 - level
    core[index] += 1
    core[index + 1:] = [0] * (2 - index)
    result = '.'.join(map(str, core))
    if channel != 'stable':
        pattern = re.compile(rf'^v{re.escape(result)}-{channel}\.([1-9][0-9]*)$')
        numbers = [int(match[1]) for tag in tags if (match := pattern.fullmatch(tag))]
        result += f'-{channel}.{max(numbers, default=0) + 1}'
    return result, base_tag


def check_pr(event):
    pr = event['pull_request']
    title = pr['title']
    if not CONVENTIONAL.fullmatch(title):
        raise ValueError('PR title must be a Conventional Commit: type(scope)!: English summary')
    labels = {label['name'] for label in pr.get('labels', [])}
    level = release_level(title)
    breaking_body = release_level(title + '\n' + (pr.get('body') or '')) == 3
    if breaking_body and level != 3:
        raise ValueError('A BREAKING CHANGE requires ! in the squash-merge PR title')
    if (title.startswith('feat') or level == 3) and 'release:feature' not in labels:
        raise ValueError('Feature/breaking PRs require the release:feature label')
    if 'release:feature' in labels and not level:
        raise ValueError('release:feature requires a release-producing PR title')


class GitHub:
    def api(self, path, method='GET', data=None):
        args = ['gh', 'api', '--method', method, f'repos/{REPOSITORY}/{path}']
        if data is not None:
            args += ['--input', '-']
        raw = run(*args, input=json.dumps(data) if data is not None else None)
        return json.loads(raw) if raw else None

    def releases(self):
        page = 1
        while True:
            rows = self.api(f'releases?per_page=100&page={page}')
            yield from rows
            if len(rows) < 100:
                break
            page += 1

    def assets(self, release_id):
        page, result = 1, []
        while True:
            rows = self.api(f'releases/{release_id}/assets?per_page=100&page={page}')
            result.extend(rows)
            if len(rows) < 100:
                return result
            page += 1

    def download(self, asset, destination, limit):
        args = ['gh', 'api', f'repos/{REPOSITORY}/releases/assets/{asset["id"]}',
                '-H', 'Accept: application/octet-stream']
        with subprocess.Popen(args, stdout=subprocess.PIPE) as process:
            with destination.open('xb') as output:
                size = 0
                while chunk := process.stdout.read(1 << 20):
                    size += len(chunk)
                    if size > limit:
                        process.kill()
                        raise ValueError(f'Download exceeds the expected size: {asset["name"]}')
                    output.write(chunk)
            if process.wait(timeout=60) != 0:
                raise ValueError(f'Could not download draft asset: {asset["name"]}')

    def upload(self, tag, path):
        run('gh', 'release', 'upload', tag, str(path), '--repo', REPOSITORY)


def guard(github, sha):
    if (os.environ.get('GITHUB_REPOSITORY') != REPOSITORY
            or os.environ.get('GITHUB_REF') != 'refs/heads/main'
            or os.environ.get('GITHUB_EVENT_NAME') not in ('push', 'workflow_dispatch')
            or os.environ.get('GITHUB_REF_PROTECTED') != 'true'):
        raise ValueError('Releases require the official repository and protected main workflow context')
    if (not re.fullmatch('[0-9a-f]{40}', sha) or os.environ.get('GITHUB_SHA') != sha
            or run('git', 'rev-parse', 'HEAD') != sha):
        raise ValueError('Checkout must be exactly the triggering main SHA')
    branch = github.api('branches/main')
    if not branch['protected'] or branch['commit']['sha'] != sha:
        raise ValueError('main is unprotected or advanced; run the workflow at its current SHA')


def marker(sha):
    return f'<!-- release-commit:{sha} -->'


def validate_draft(release, plan):
    if (not release['draft'] or release['tag_name'] != plan['tag']
            or release['target_commitish'] != plan['commit']
            or release['prerelease'] != plan['prerelease']
            or not (release.get('body') or '').endswith(marker(plan['commit']))):
        raise ValueError('Existing release is published or does not match this exact draft plan')


def plan_release(args):
    sha = run('git', 'rev-parse', 'HEAD')
    github = GitHub()
    if args.live:
        guard(github, sha)
    tags = run('git', 'tag', '--merged', sha).splitlines()
    # Select the baseline before reading messages; prerelease tags never become it.
    _, base = next_version(tags, [])
    revision = f'{base}..{sha}' if base else sha
    messages = run('git', 'log', '-z', '--format=%B', revision).split('\x00')
    bump = os.environ.get('RELEASE_BUMP', 'auto')
    channel = os.environ.get('RELEASE_CHANNEL', 'stable')
    result, _ = next_version(tags, messages, bump, channel)
    retry = os.environ.get('RELEASE_RETRY_TAG', '')
    if retry:
        if not args.live or os.environ.get('GITHUB_EVENT_NAME') != 'workflow_dispatch':
            raise ValueError('Draft retry requires an explicit workflow_dispatch run')
        core, pre = version(retry)
        if not retry.startswith('v') or not result or core != version(result)[0]:
            raise ValueError('Retry tag must match the computed release version core')
        if (channel == 'stable' and retry != f'v{result}') or (channel != 'stable'
                and not re.fullmatch(rf'{channel}\.[1-9][0-9]*', pre or '')):
            raise ValueError('Retry tag does not match the selected channel')
        result = retry[1:]
    plan = {'repository': REPOSITORY, 'commit': sha, 'version': result,
            'tag': f'v{result}' if result else '', 'prerelease': channel != 'stable',
            'base': base, 'retry': bool(retry)}
    notes = run('git', 'log', '--format=- %h %s%n%b', revision)
    plan['body'] = (
        f'## Changes\n\n{notes or "Manual maintenance release."}\n\n'
        '## Verification limits\n\n'
        'Verified assets and local tests do not certify production readiness. '
        'Evaluate with disposable data before using valuable worlds. '
        'Minecraft sustained readiness, complete restart/online-backup/restore/firewall '
        'drills and official signed installation/update/rollback acceptance remain unverified. '
        'Publication alone does not prove that an update leaves a running Minecraft instance unchanged.\n\n'
        f'Consult the [source-bound acceptance ledger](https://github.com/{REPOSITORY}/blob/{sha}/docs/acceptance.md) '
        f'for recorded evidence and remaining gaps.\n\n{marker(sha)}')
    if args.live and result:
        existing = [row for row in github.releases() if row['tag_name'] == plan['tag']]
        if len(existing) > 1:
            raise ValueError('Duplicate release tags require manual investigation')
        if existing:
            if not retry:
                raise ValueError('Tag/release already exists; use the exact retry_tag for a matching failed draft')
            validate_draft(existing[0], plan)
        elif retry:
            raise ValueError('retry_tag must identify an existing failed draft')
        if plan['tag'] in tags and not existing:
            raise ValueError('Tag exists without its expected draft; refusing reuse')
    if args.output:
        pathlib.Path(args.output).write_text(json.dumps(plan, indent=2) + '\n')
    if args.live:
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            output.write(f'version={result}\n')
    print(json.dumps(plan, indent=2))


def digest(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def expected_assets(directory, release_version):
    names = {f'new-empire-control_{release_version}_linux_{arch}.tar.gz' for arch in ('amd64', 'arm64')}
    names |= {'documentation.tar.gz', 'sbom.cdx.json', 'checksums.txt',
              'release.json', 'release.json.sig', 'provenance.jsonl'}
    paths = list(directory.iterdir())
    if {path.name for path in paths} != names or any(path.is_symlink() or not path.is_file() for path in paths):
        raise ValueError('Release directory must contain exactly the required regular assets')
    return {path.name: (path.stat().st_size, digest(path)) for path in sorted(paths)}


def verify_provenance(directory, plan):
    for path in sorted(directory.iterdir()):
        if path.name == 'provenance.jsonl':
            continue
        run('gh', 'attestation', 'verify', str(path), '--repo', REPOSITORY,
            '--bundle', str(directory / 'provenance.jsonl'),
            '--signer-workflow', WORKFLOW, '--signer-digest', plan['commit'],
            '--source-digest', plan['commit'], '--source-ref', 'refs/heads/main',
            '--deny-self-hosted-runners')


def publish(args, github=None):
    github = github or GitHub()
    plan = json.loads(pathlib.Path(args.plan).read_text())
    if (plan['repository'] != REPOSITORY or plan['tag'] != 'v' + plan['version']
            or bool(version(plan['version'])[1]) != plan['prerelease']):
        raise ValueError('Invalid release plan identity or version')
    guard(github, plan['commit'])
    directory = pathlib.Path(args.assets).resolve()
    expected = expected_assets(directory, plan['version'])
    run(args.tool, 'verify', '-dir', str(directory))
    verify_provenance(directory, plan)
    releases = [row for row in github.releases() if row['tag_name'] == plan['tag']]
    if len(releases) > 1:
        raise ValueError('Duplicate releases require manual investigation')
    if releases:
        if not plan['retry']:
            raise ValueError('Existing draft requires an explicit retry_tag; no assets were overwritten')
        release = releases[0]
        validate_draft(release, plan)
    else:
        if plan['retry']:
            raise ValueError('The explicitly selected draft no longer exists')
        release = github.api('releases', 'POST', {
            'tag_name': plan['tag'], 'target_commitish': plan['commit'],
            'name': plan['tag'], 'body': plan['body'],
            'draft': True, 'prerelease': plan['prerelease']})
        validate_draft(release, plan)
    release_id = release['id']
    present = github.assets(release_id)
    if len({row['name'] for row in present}) != len(present):
        raise ValueError('Draft has duplicate asset names')
    with tempfile.TemporaryDirectory(prefix='release-readback-') as temporary:
        downloaded = pathlib.Path(temporary)
        # Read back existing assets first. A retry never overwrites even one byte.
        for asset in present:
            name = asset['name']
            # Attestations include fresh timestamps. Reuse a prior bundle only if
            # it verifies these exact payloads and this same protected-main SHA.
            if name == 'provenance.jsonl' and plan['retry'] and 0 < asset['size'] <= 16 << 20:
                github.download(asset, downloaded / name, asset['size'])
                expected[name] = (asset['size'], digest(downloaded / name))
                if asset['state'] != 'uploaded':
                    raise ValueError('Prior provenance upload is incomplete')
                continue
            if name not in expected or asset['state'] != 'uploaded' or asset['size'] != expected[name][0]:
                raise ValueError('Draft has an unexpected, incomplete, or mismatched asset')
            github.download(asset, downloaded / name, expected[name][0])
            if (downloaded / name).stat().st_size != expected[name][0] or digest(downloaded / name) != expected[name][1]:
                raise ValueError('Draft asset bytes differ; explicit manual investigation is required')
        for name in expected:
            if not (downloaded / name).exists():
                github.upload(plan['tag'], directory / name)
        remote = github.assets(release_id)
        if len(remote) != len(expected) or {row['name'] for row in remote} != set(expected):
            raise ValueError('Uploaded draft asset inventory does not match')
        for asset in remote:
            name = asset['name']
            if asset['state'] != 'uploaded' or asset['size'] != expected[name][0]:
                raise ValueError('Uploaded draft asset is incomplete or has the wrong size')
            if not (downloaded / name).exists():
                github.download(asset, downloaded / name, expected[name][0])
            if digest(downloaded / name) != expected[name][1]:
                raise ValueError('Read-back SHA-256 differs; draft remains unpublished')
        run(args.tool, 'verify', '-dir', str(downloaded))
        verify_provenance(downloaded, plan)
    guard(github, plan['commit'])
    validate_draft(github.api(f'releases/{release_id}'), plan)
    # A tag may have been created independently. Resolve annotated tags too.
    tags = run('git', 'ls-remote', '--tags', f'https://github.com/{REPOSITORY}.git',
               f'refs/tags/{plan["tag"]}', f'refs/tags/{plan["tag"]}^{{}}').splitlines()
    resolved = {ref: value for value, ref in (line.split() for line in tags)}
    target = resolved.get(f'refs/tags/{plan["tag"]}^{{}}', resolved.get(f'refs/tags/{plan["tag"]}'))
    if target is not None and target != plan['commit']:
        raise ValueError('Existing release tag does not resolve to the validated SHA')
    published = github.api(f'releases/{release_id}', 'PATCH', {
        'draft': False, 'make_latest': 'false' if plan['prerelease'] else 'true'})
    if published['draft'] or not published.get('published_at'):
        raise ValueError('GitHub did not confirm publication; inspect the release before retrying')
    if github.api(f'commits/{quote(plan["tag"], safe="")}')['sha'] != plan['commit']:
        raise ValueError('Published tag identity mismatch; administrator intervention required')
    if not plan['prerelease'] and github.api('releases/latest')['id'] != release_id:
        raise ValueError('Published stable release is not discoverable as latest')
    print(published['html_url'])


def sbom(args):
    version(args.version)
    components = []
    architectures = set()
    for filename in args.binary:
        with tarfile.open(filename, 'r:gz') as archive, tempfile.TemporaryDirectory() as temporary:
            members = archive.getmembers()
            if len(members) != 1 or members[0].name != 'new-empire-control' or not members[0].isfile() or not 0 < members[0].size <= 512 << 20:
                raise ValueError('SBOM input must be a binary-only release archive')
            binary = pathlib.Path(temporary) / 'new-empire-control'
            with archive.extractfile(members[0]) as source, binary.open('xb') as output:
                while chunk := source.read(1 << 20):
                    output.write(chunk)
            info = json.loads(run('go', 'version', '-m', '-json', str(binary)))
        settings = {entry['Key']: entry['Value'] for entry in info['Settings']}
        if settings.get('GOOS') != 'linux' or settings.get('vcs.revision') != args.commit or settings.get('vcs.modified') != 'false':
            raise ValueError('SBOM binary must be Linux and built from the exact clean source commit')
        architectures.add(settings['GOARCH'])
        components.append({'type': 'framework', 'name': 'Go standard library', 'version': info['GoVersion'],
                           'purl': f'pkg:golang/stdlib@{info["GoVersion"]}'})
        for dependency in info.get('Deps', []):
            if dependency.get('Replace'):
                raise ValueError('Release binaries may not contain replaced Go modules')
            name, value = dependency['Path'], dependency['Version']
            components.append({'type': 'library', 'name': name, 'version': value,
                               'purl': f'pkg:golang/{name}@{value}'})
    if architectures != {'amd64', 'arm64'}:
        raise ValueError('SBOM requires both Linux architectures')
    lock = json.loads(pathlib.Path('web/package-lock.json').read_text())
    if lock.get('lockfileVersion', 0) < 2 or not lock.get('packages'):
        raise ValueError('SBOM requires an npm lockfile with resolved packages')
    for path, package in lock['packages'].items():
        if not path or package.get('dev'):
            continue
        if package.get('link'):
            raise ValueError('Release SBOM cannot omit a linked production npm package')
        name = package.get('name') or path.rsplit('node_modules/', 1)[-1]
        value = package.get('version')
        if not value:
            raise ValueError(f'Unversioned npm package: {name}')
        components.append({'type': 'library', 'name': name, 'version': value,
                           'purl': f'pkg:npm/{quote(name, safe="/")}@{value}'})
    unique = {component['purl']: component for component in components}
    document = {'bomFormat': 'CycloneDX', 'specVersion': '1.6', 'version': 1,
                'metadata': {'component': {'type': 'application', 'name': 'new-empire-control',
                    'version': args.version, 'properties': [{'name': 'vcs:repository', 'value': REPOSITORY},
                    {'name': 'vcs:commit', 'value': args.commit}]}},
                'components': [unique[key] for key in sorted(unique)]}
    with pathlib.Path(args.output).open('x') as output:
        json.dump(document, output, separators=(',', ':'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    commands.add_parser('check-pr')
    commands.add_parser('guard')
    output = commands.add_parser('check-output', help='Refuse unsafe distro destinations before building')
    output.add_argument('--output', required=True)
    plan = commands.add_parser('plan', help='Read-only dry-run unless --live is given')
    plan.add_argument('--live', action='store_true')
    plan.add_argument('--output')
    release = commands.add_parser('publish')
    release.add_argument('--plan', required=True)
    release.add_argument('--assets', required=True)
    release.add_argument('--tool', required=True)
    bill = commands.add_parser('sbom')
    for name in ('version', 'commit', 'output'):
        bill.add_argument('--' + name, required=True)
    bill.add_argument('--binary', required=True, action='append', help='Repeat for both Linux archives')
    args = parser.parse_args()
    if args.command == 'check-pr':
        check_pr(json.loads(pathlib.Path(os.environ['GITHUB_EVENT_PATH']).read_text()))
    elif args.command == 'guard':
        guard(GitHub(), os.environ['GITHUB_SHA'])
    elif args.command == 'check-output':
        check_output(args)
    elif args.command == 'plan':
        plan_release(args)
    elif args.command == 'publish':
        publish(args)
    else:
        sbom(args)


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as exc:
        sys.exit(str(exc))
