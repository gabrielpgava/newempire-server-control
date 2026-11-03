import argparse
import contextlib
import io
import json
import os
import pathlib
import re
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest import mock

import release


def github_cli(directory):
    executable = directory / 'gh'
    executable.write_text(f'#!{sys.executable}\n' + '''import json, os, pathlib, sys, threading
args = sys.argv[1:]
if args[0] == 'api':
    host = args[args.index('--hostname') + 1] if '--hostname' in args else os.environ['GH_HOST']
    if any('/releases/assets/' in value for value in args):
        if os.environ.get('GH_TEST_STALL') == '1':
            pathlib.Path(os.environ['GH_TEST_PID']).write_text(str(os.getpid()))
            sys.stdout.buffer.write(b'x')
            sys.stdout.buffer.flush()
            threading.Event().wait(3)
        else:
            sys.stdout.buffer.write(b'official' if host == 'github.com' else b'mirror')
    else:
        print(json.dumps({'host': host}))
elif args[:2] == ['release', 'upload']:
    repository = args[args.index('--repo') + 1]
    host = repository.split('/')[0] if repository.count('/') == 2 else os.environ['GH_HOST']
    pathlib.Path(os.environ['GH_TEST_UPLOAD_HOST']).write_text(host)
else:
    raise SystemExit('Unexpected CLI operation')
''')
    executable.chmod(0o755)
    return mock.patch.dict(os.environ, {
        'PATH': str(directory) + os.pathsep + os.environ['PATH'],
        'GH_HOST': 'mirror.example.invalid',
        'GH_TEST_PID': str(directory / 'downloader.pid'),
        'GH_TEST_UPLOAD_HOST': str(directory / 'upload-host'),
    })


class GitHubTransport(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = pathlib.Path(self.temporary.name)
        self.cli = github_cli(self.directory)
        self.cli.start()
        self.addCleanup(self.cli.stop)

    def test_api_pins_official_host_despite_ambient_host(self):
        self.assertEqual(release.GitHub().api('branches/main')['host'], 'github.com')

    def test_download_pins_official_host_despite_ambient_host(self):
        destination = self.directory / 'download'
        release.GitHub().download({'id': 17, 'name': 'asset'}, destination, 8)
        self.assertEqual(destination.read_bytes(), b'official')

    def test_upload_pins_official_host_despite_ambient_host(self):
        asset = self.directory / 'asset'
        asset.write_bytes(b'candidate')
        release.GitHub().upload('v1.2.3', asset)
        self.assertEqual((self.directory / 'upload-host').read_text(), 'github.com')


class Versions(unittest.TestCase):
    def test_bumps(self):
        cases = [
            ([], ['feat: first feature'], 'auto', 'stable', '0.1.0'),
            ([], ['fix: first repair'], 'auto', 'stable', '0.0.1'),
            ([], ['chore: foundations'], 'auto', 'stable', ''),
            (['v1.9.0', 'v1.10.0'], ['fix: repair'], 'auto', 'stable', '1.10.1'),
            (['v1.2.3'], ['fix: repair', 'feat(ui): screen'], 'auto', 'stable', '1.3.0'),
            (['v1.2.3'], ['refactor!: changed interface'], 'auto', 'stable', '2.0.0'),
            (['v1.2.3'], ['chore: changes\n\nBREAKING CHANGE: migrate'], 'patch', 'stable', '2.0.0'),
            (['v1.2.3'], ['feat: changes\n\nBREAKING-CHANGE: migrate'], 'minor', 'beta', '2.0.0-beta.1'),
            (['v1.0.0', 'v1.1.0-beta.1', 'v1.1.0-beta.9'], ['feat: changes'], 'auto', 'beta', '1.1.0-beta.10'),
            (['v1.0.0', 'v1.1.0-rc.1'], ['feat: changes'], 'auto', 'stable', '1.1.0'),
            (['v1.2.3'], ['docs: clarify'], 'patch', 'stable', '1.2.4'),
            (['v1.2.3'], [], 'major', 'rc', '2.0.0-rc.1'),
        ]
        for tags, messages, bump, channel, expected in cases:
            with self.subTest(expected=expected):
                self.assertEqual(release.next_version(tags, messages, bump, channel)[0], expected)

    def test_invalid_versions(self):
        for value in ('1.02.3', '1.0', '1.0.0-', '1.0.0-01', '1.0.0-a..b', '1.0.0+a..b', '1.0.0\nother', '1.2٣.0'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                release.version(value)
        self.assertEqual(release.version('v1.2.3-beta.1+build.7'), ((1, 2, 3), 'beta.1'))

    def test_since_stable_tag_uses_full_commit_body(self):
        with tempfile.TemporaryDirectory() as temporary:
            old = pathlib.Path.cwd()
            try:
                os.chdir(temporary)
                def git(*args):
                    subprocess.run(['git', *args], check=True, capture_output=True)
                git('init', '-b', 'main')
                git('config', 'user.name', 'Release Test')
                git('config', 'user.email', 'release-test@example.invalid')
                git('commit', '--allow-empty', '-m', 'feat!: old break')
                git('tag', 'v1.0.0')
                git('commit', '--allow-empty', '-m', 'feat: compatible change')
                git('tag', 'v1.1.0-beta.1')
                git('commit', '--allow-empty', '-m', 'fix: later repair')
                git('commit', '--allow-empty', '-m', 'docs: latest clarification')
                with mock.patch.dict(os.environ, {'RELEASE_BUMP': 'auto', 'RELEASE_CHANNEL': 'stable', 'RELEASE_RETRY_TAG': ''}), contextlib.redirect_stdout(io.StringIO()):
                    release.plan_release(argparse.Namespace(live=False, output='plan.json'))
                plan = json.loads(pathlib.Path('plan.json').read_text())
                self.assertEqual(plan['version'], '1.1.0')
                self.assertNotIn('old break', plan['body'])
                git('commit', '--allow-empty', '-m', 'feat!: new protocol')
                git('commit', '--allow-empty', '-m', 'docs: protocol migration guide')
                with mock.patch.dict(os.environ, {'RELEASE_BUMP': 'auto', 'RELEASE_CHANNEL': 'stable', 'RELEASE_RETRY_TAG': ''}), contextlib.redirect_stdout(io.StringIO()):
                    release.plan_release(argparse.Namespace(live=False, output='plan.json'))
                self.assertEqual(json.loads(pathlib.Path('plan.json').read_text())['version'], '2.0.0')
                git('commit', '--allow-empty', '-m', 'chore: cleanup', '-m', 'BREAKING CHANGE: new format')
                with mock.patch.dict(os.environ, {'RELEASE_BUMP': 'auto', 'RELEASE_CHANNEL': 'stable', 'RELEASE_RETRY_TAG': ''}), contextlib.redirect_stdout(io.StringIO()):
                    release.plan_release(argparse.Namespace(live=False, output='plan.json'))
                self.assertEqual(json.loads(pathlib.Path('plan.json').read_text())['version'], '2.0.0')
            finally:
                os.chdir(old)


class Policy(unittest.TestCase):
    def pr(self, title, labels=(), body=''):
        return {'pull_request': {'title': title, 'body': body,
                                'labels': [{'name': name} for name in labels]}}

    def test_titles_and_labels(self):
        for title, labels in [('feat: screen', ['release:feature']), ('fix: repair', []),
                              ('docs: clarify', []), ('refactor!: API', ['release:feature'])]:
            release.check_pr(self.pr(title, labels))
        for title, labels, body in [('feat: screen', [], ''), ('docs: clarify', ['release:feature'], ''),
                                    ('bad title', [], ''), ('fix: repair', [], 'BREAKING CHANGE: new API')]:
            with self.subTest(title=title), self.assertRaises(ValueError):
                release.check_pr(self.pr(title, labels, body))

    def test_guard_rejects_forks_unprotected_or_stale_main(self):
        github = mock.Mock()
        sha = 'a' * 40
        env = {'GITHUB_REPOSITORY': release.REPOSITORY, 'GITHUB_REF': 'refs/heads/main',
               'GITHUB_EVENT_NAME': 'push', 'GITHUB_REF_PROTECTED': 'true', 'GITHUB_SHA': sha}
        with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(release, 'run', return_value=sha):
            github.api.return_value = {'protected': True, 'commit': {'sha': sha}}
            release.guard(github, sha)
            github.api.return_value['commit']['sha'] = 'b' * 40
            with self.assertRaises(ValueError):
                release.guard(github, sha)
            with mock.patch.dict(os.environ, {'GITHUB_REF_PROTECTED': 'false'}), self.assertRaises(ValueError):
                release.guard(github, sha)
            with mock.patch.dict(os.environ, {'GITHUB_REPOSITORY': 'attacker/fork'}), self.assertRaises(ValueError):
                release.guard(github, sha)


class LocalGuard(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        old = pathlib.Path.cwd()
        self.addCleanup(os.chdir, old)
        os.chdir(self.temporary.name)
        self.git('init', '-q', '-b', 'main')
        self.git('config', 'user.name', 'Local Release Test')
        self.git('config', 'user.email', 'release-test@example.invalid')
        self.git('remote', 'add', 'origin', f'https://github.com/{release.REPOSITORY}.git')
        self.workflow = pathlib.Path('.github/workflows/release.yml')
        self.workflow.parent.mkdir(parents=True)
        self.workflow.write_text('name: Release\non:\n  workflow_dispatch:\npermissions:\n  contents: read\n')
        self.git('add', '.')
        self.git('commit', '-q', '-m', 'feat: fixture')
        self.sha = self.git('rev-parse', 'HEAD')
        self.github = mock.Mock()
        self.github.api.return_value = {'protected': True, 'commit': {'sha': self.sha}}

    def git(self, *args):
        return subprocess.run(['git', *args], check=True, capture_output=True, text=True).stdout.strip()

    def test_local_guard_needs_no_workflow_context(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            release.guard_local(self.github, self.sha)

    def test_local_guard_rejects_forks_dirty_non_main_and_wrong_sha(self):
        for condition in ('fork', 'dirty', 'untracked', 'branch', 'sha'):
            with self.subTest(condition=condition):
                if condition == 'fork':
                    self.git('remote', 'set-url', 'origin', 'https://github.com/attacker/fork.git')
                elif condition == 'dirty':
                    self.workflow.write_text('changed')
                elif condition == 'untracked':
                    pathlib.Path('unexpected').write_text('untracked')
                elif condition == 'branch':
                    self.git('switch', '-q', '-c', 'fixture-dev')
                with self.assertRaises(ValueError):
                    release.guard_local(self.github, 'b' * 40 if condition == 'sha' else self.sha)
                self.git('remote', 'set-url', 'origin', f'https://github.com/{release.REPOSITORY}.git')
                self.workflow.write_text('name: Release\non:\n  workflow_dispatch:\npermissions:\n  contents: read\n')
                pathlib.Path('unexpected').unlink(missing_ok=True)
                self.git('switch', '-q', 'main')

    def test_local_guard_rejects_unprotected_or_moved_main(self):
        for branch in ({'protected': False, 'commit': {'sha': self.sha}},
                       {'protected': True, 'commit': {'sha': 'b' * 40}}):
            self.github.api.return_value = branch
            with self.assertRaises(ValueError):
                release.guard_local(self.github, self.sha)

    def test_local_guard_rejects_cloud_triggers(self):
        for trigger in (
                'on:\n  push:\n', 'on: [release]\n', '"on":\n  push:\n',
                'on:\n  workflow_dispatch:\n  "release":\n    types: [published]\n',
                "on:\n  workflow_dispatch:\n  'push':\n",
                'on:\n  workflow_dispatch:\n  release :\n    types: [published]\n',
                'on:\n  workflow_dispatch:\n"on": [release]\n',
                "on:\n  workflow_dispatch:\n'on': [release]\n",
                'on:\n  workflow_dispatch:\non: [release]\n',
                'on: [release]\non:\n  workflow_dispatch:\n',
                'on:\n  workflow_dispatch:\non : [release]\n',
                'on:\n  workflow_dispatch:\n"o\\u006e": [release]\n'):
            with self.subTest(trigger=trigger):
                self.workflow.write_text('name: Release\n' + trigger)
                self.git('add', '.')
                self.git('commit', '-q', '-m', 'test: unsafe trigger')
                sha = self.git('rev-parse', 'HEAD')
                self.github.api.return_value['commit']['sha'] = sha
                with self.assertRaisesRegex(ValueError, 'workflow'):
                    release.guard_local(self.github, sha)


class LocalDistribution(unittest.TestCase):
    def make(self, *args):
        return subprocess.run(['make', '--no-print-directory', *args],
                              cwd=pathlib.Path(__file__).resolve().parent.parent,
                              capture_output=True, text=True, timeout=10)

    def test_missing_version_refuses_before_any_build(self):
        result = self.make('dist', 'VERSION=')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('VERSION is required', result.stderr)
        self.assertNotIn('npm --prefix', result.stdout)
        self.assertNotIn('go run', result.stdout)

    def refusal(self, output, message):
        with tempfile.TemporaryDirectory() as temporary:
            override = pathlib.Path(temporary) / 'frontend.mk'
            override.write_text('frontend:\n\t@echo FRONTEND_STARTED; exit 97\n')
            result = self.make('-f', 'Makefile', '-f', str(override), 'dist',
                               f'MAKE=make -f Makefile -f "{override}"',
                               'VERSION=1.2.3', f'OUT={output}')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(message, result.stderr)
        self.assertNotIn('FRONTEND_STARTED', result.stdout)
        self.assertNotIn('go run', result.stdout)

    def test_existing_output_and_dangling_symlink_refuse_before_any_build(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            directory = root / 'directory'
            directory.mkdir()
            sentinel = directory / 'retained'
            sentinel.write_bytes(b'keep existing output')
            regular = root / 'file'
            regular.write_bytes(b'keep existing file')
            dangling = root / 'dangling'
            dangling.symlink_to(root / 'missing')
            for output in (directory, regular, dangling):
                with self.subTest(output=output.name):
                    self.refusal(str(output), 'release output must not already exist')
            self.assertEqual(sentinel.read_bytes(), b'keep existing output')
            self.assertEqual(regular.read_bytes(), b'keep existing file')
            self.assertTrue(dangling.is_symlink())

    def test_unignored_checkout_output_refuses_before_any_build(self):
        self.refusal('candidate-output', 'release output must be outside the checkout or Git-ignored')

    def test_output_preflight_accepts_only_ignored_or_external_new_directories(self):
        self.assertTrue(hasattr(release, 'check_output'), 'The output preflight is missing')
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary) / 'checkout'
            root.mkdir()
            subprocess.run(['git', 'init', '-q', str(root)], check=True, capture_output=True)
            (root / '.gitignore').write_text('/dist/\n')
            old = pathlib.Path.cwd()
            try:
                os.chdir(root)
                for output in ('candidate-output', str(root / 'candidate-output'), '.git/output'):
                    with self.subTest(rejected=output), self.assertRaisesRegex(ValueError, 'Git-ignored'):
                        release.check_output(argparse.Namespace(output=output))
                for output in ('dist/new-output', str(root / 'dist/new-output'), str(root.parent / 'external')):
                    with self.subTest(accepted=output):
                        release.check_output(argparse.Namespace(output=output))
                        self.assertFalse(os.path.lexists(output))
            finally:
                os.chdir(old)

    def test_distribution_builds_frontend_both_architectures_and_sbom_without_signing(self):
        result = self.make('-n', 'dist', 'VERSION=1.2.3', 'OUT=dist/test-release')
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = result.stdout
        self.assertLess(commands.index('npm --prefix web run build'), commands.index('go run ./scripts/release build'))
        self.assertIn('-version "1.2.3"', commands)
        self.assertIn('-out "dist/test-release"', commands)
        self.assertIn('scripts/release.py sbom --version "1.2.3"', commands)
        for architecture in ('amd64', 'arm64'):
            self.assertIn(f'dist/test-release/new-empire-control_1.2.3_linux_{architecture}.tar.gz', commands)
        self.assertIn('--output "dist/test-release/sbom.cdx.json"', commands)
        self.assertNotIn(' sign ', commands)
        self.assertNotIn(' publish ', commands)

    def test_release_plan_is_read_only(self):
        result = self.make('-n', 'release-plan')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'python3 -B scripts/release.py plan')


class ManualWorkflow(unittest.TestCase):
    def workflow(self):
        path = pathlib.Path(__file__).resolve().parent.parent / '.github/workflows/release.yml'
        self.assertTrue(path.is_file(), 'The manual-only release workflow is missing')
        return path.read_text()

    def test_manual_only_protected_main_and_immutable_actions(self):
        workflow = self.workflow()
        triggers = workflow.split('\non:\n', 1)[1].split('\npermissions:', 1)[0]
        self.assertEqual(re.findall(r'^  ([a-z_]+):', triggers, re.MULTILINE), ['workflow_dispatch'])
        self.assertIn("github.event_name == 'workflow_dispatch'", workflow)
        self.assertIn("github.repository == 'gabrielpgava/newempire-server-control'", workflow)
        self.assertIn("github.ref == 'refs/heads/main'", workflow)
        self.assertIn('github.ref_protected', workflow)
        self.assertIn('cancel-in-progress: false', workflow)
        self.assertNotIn('ci.yml', workflow)
        for action in re.findall(r'^\s+- uses: (\S+)', workflow, re.MULTILINE):
            self.assertRegex(action, r'^actions/[a-z-]+@[0-9a-f]{40}$')
        self.assertEqual(workflow.count('ref: ${{ github.sha }}'), 2)
        self.assertEqual(workflow.count('persist-credentials: false'), 2)

    def test_read_only_preparation_and_environment_scoped_signing(self):
        workflow = self.workflow()
        prepare, publish = workflow.split('\n  publish:\n', 1)
        self.assertNotIn('secrets.', prepare)
        self.assertNotIn(': write', prepare)
        self.assertIn('environment: release', publish)
        for permission in ('contents', 'id-token', 'attestations'):
            self.assertIn(f'{permission}: write', publish)
        self.assertEqual(workflow.count('${{ secrets.RELEASE_SIGNING_KEY }}'), 1)
        self.assertLess(publish.index('scripts/release.py guard'), publish.index('secrets.RELEASE_SIGNING_KEY'))
        self.assertIn('make dist VERSION="$RELEASE_VERSION"', prepare)
        self.assertIn('scripts/release.py publish --plan', publish)
        self.assertIn('subject-path: ${{ runner.temp }}/release-bundle/assets/*', publish)
        self.assertIn('name: ${{ needs.prepare.outputs.artifact_name }}', publish)

    def test_secret_bearing_signer_is_built_in_publication_and_never_transferred(self):
        prepare, publish = self.workflow().split('\n  publish:\n', 1)
        self.assertNotIn('/release-tool', prepare)
        self.assertIn('path: |\n            ${{ runner.temp }}/release-bundle/plan.json\n            ${{ runner.temp }}/release-bundle/assets/', prepare)
        build = 'go build -mod=readonly -o "$RUNNER_TEMP/release-tool" ./scripts/release'
        self.assertIn(build, publish)
        self.assertLess(publish.index(build), publish.index('secrets.RELEASE_SIGNING_KEY'))
        self.assertIn('uses: actions/setup-go@', publish)
        self.assertNotIn('npm ', publish)
        self.assertNotIn('release-bundle/release-tool', publish)
        self.assertIn('"$RUNNER_TEMP/release-tool" sign -dir', publish)
        self.assertIn('--tool "$RUNNER_TEMP/release-tool"', publish)


class ReleaseNotes(unittest.TestCase):
    def test_planned_notes_do_not_certify_unrun_host_acceptance(self):
        sha = 'a' * 40
        with tempfile.TemporaryDirectory() as temporary:
            output = pathlib.Path(temporary) / 'plan.json'
            with mock.patch.object(release, 'run', side_effect=[sha, '', 'feat: first release', '- abcdef0 feat: first release']), \
                    mock.patch.dict(os.environ, {'RELEASE_BUMP': 'auto', 'RELEASE_CHANNEL': 'stable', 'RELEASE_RETRY_TAG': ''}), \
                    contextlib.redirect_stdout(io.StringIO()):
                release.plan_release(argparse.Namespace(live=False, output=str(output)))
            body = json.loads(output.read_text())['body']
        self.assertIn('## Verification limits', body)
        self.assertIn('do not certify production readiness', body)
        self.assertIn('Minecraft sustained readiness', body)
        self.assertIn('installation/update/rollback', body)
        self.assertIn(f'/blob/{sha}/docs/acceptance.md', body)
        self.assertTrue(body.endswith(release.marker(sha)))


class FakeGitHub:
    def __init__(self, plan, existing=False):
        self.plan = plan
        self.release = {'id': 17, 'draft': True, 'tag_name': plan['tag'],
                        'target_commitish': plan['commit'], 'prerelease': plan['prerelease'],
                        'body': plan['body']}
        self.exists = existing
        self.files = {}
        self.events = []
        self.corrupt_upload = False

    def releases(self):
        return [self.release] if self.exists else []

    def api(self, path, method='GET', data=None):
        if method == 'POST':
            self.events.append('draft')
            self.exists = True
            return self.release
        if method == 'PATCH':
            self.events.append('publish')
            self.release.update(draft=False, published_at='2026-11-02T00:00:00Z',
                                html_url='https://github.com/example/release')
            return self.release
        if path.startswith('commits/'):
            return {'sha': self.plan['commit']}
        return self.release

    def assets(self, release_id):
        return [{'id': name, 'name': name, 'size': len(value), 'state': 'uploaded'}
                for name, value in self.files.items()]

    def download(self, asset, destination, limit):
        self.events.append('download')
        value = self.files[asset['name']]
        if len(value) > limit:
            raise ValueError('oversized')
        destination.write_bytes(value)

    def upload(self, tag, path):
        self.events.append('upload')
        if path.name in self.files:
            raise AssertionError('An existing asset was overwritten')
        value = path.read_bytes()
        self.files[path.name] = b'x' * len(value) if self.corrupt_upload else value


class Publication(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = pathlib.Path(self.temporary.name)
        self.assets = self.directory / 'assets'
        self.assets.mkdir()
        self.plan = {'repository': release.REPOSITORY, 'commit': 'a' * 40,
                     'version': '1.2.3', 'tag': 'v1.2.3', 'prerelease': False,
                     'body': release.marker('a' * 40), 'retry': False}
        for name in ('new-empire-control_1.2.3_linux_amd64.tar.gz',
                     'new-empire-control_1.2.3_linux_arm64.tar.gz',
                     'release.json', 'release.json.sig', 'checksums.txt',
                     'sbom.cdx.json', 'documentation.tar.gz', 'provenance.jsonl'):
            (self.assets / name).write_bytes(name.encode())
        self.plan_path = self.directory / 'plan.json'
        self.github = FakeGitHub(self.plan)
        self.guard = mock.patch.object(release, 'guard').start()
        self.provenance = mock.patch.object(release, 'verify_provenance').start()
        self.run = mock.patch.object(release, 'run', return_value='').start()
        self.addCleanup(mock.patch.stopall)

    def publish(self, local=False):
        self.plan_path.write_text(json.dumps(self.plan))
        with contextlib.redirect_stdout(io.StringIO()):
            release.publish(argparse.Namespace(plan=str(self.plan_path), assets=str(self.assets), tool='tool', local=local), self.github)

    def local_assets(self):
        (self.assets / 'provenance.jsonl').unlink()
        for name in ('provenance.json', 'provenance.json.sig'):
            (self.assets / name).write_bytes(name.encode())
        self.local_guard = mock.patch.object(release, 'guard_local').start()

    def test_local_publish_verifies_source_bound_signatures_before_and_after_readback(self):
        self.local_assets()
        self.publish(local=True)
        self.assertEqual(self.github.events.count('download'), 9)
        self.assertEqual(self.github.events[-1], 'publish')
        self.assertEqual(self.local_guard.call_count, 2)
        self.guard.assert_not_called()
        self.provenance.assert_not_called()
        verifies = [call.args for call in self.run.call_args_list if call.args[0] == 'tool']
        self.assertEqual(len(verifies), 2)
        for call in verifies:
            self.assertEqual(call[1], 'verify-local')
            self.assertIn(self.plan['commit'], call)
            self.assertIn(self.plan['version'], call)

    def test_invalid_local_provenance_cannot_start_a_draft(self):
        self.local_assets()
        self.run.side_effect = ValueError('invalid local signature')
        with self.assertRaises(ValueError):
            self.publish(local=True)
        self.assertEqual(self.github.events, [])

    def test_local_retry_never_replaces_different_provenance(self):
        self.local_assets()
        self.plan['retry'] = True
        self.github.exists = True
        self.github.files['provenance.json'] = b'x' * len((self.assets / 'provenance.json').read_bytes())
        with self.assertRaises(ValueError):
            self.publish(local=True)
        self.assertNotIn('upload', self.github.events)
        self.assertNotIn('publish', self.github.events)

    def test_moved_local_main_leaves_verified_draft_unpublished(self):
        self.local_assets()
        self.local_guard.side_effect = [None, ValueError('main advanced')]
        with self.assertRaises(ValueError):
            self.publish(local=True)
        self.assertEqual(self.github.events.count('download'), 9)
        self.assertNotIn('publish', self.github.events)

    def test_stalled_download_is_reaped_without_publishing_or_changing_candidates(self):
        self.local_assets()
        self.github.download = release.GitHub().download
        before = {path.name: path.read_bytes() for path in self.assets.iterdir()}
        with github_cli(self.directory), mock.patch.dict(os.environ, {'GH_TEST_STALL': '1'}), \
                mock.patch.object(release, 'TRANSFER_TIMEOUT', 0.5, create=True):
            started = time.monotonic()
            with self.assertRaisesRegex(ValueError, 'Timed out'):
                self.publish(local=True)
            self.assertLess(time.monotonic() - started, 2)
        pid = int((self.directory / 'downloader.pid').read_text())
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)
        self.assertNotIn('publish', self.github.events)
        self.assertEqual({path.name: path.read_bytes() for path in self.assets.iterdir()}, before)

    def test_publish_is_last_mutation_after_readback(self):
        self.publish()
        self.assertEqual(self.github.events[0], 'draft')
        self.assertEqual(self.github.events[-1], 'publish')
        self.assertEqual(self.github.events.count('download'), 8)
        self.assertEqual(self.provenance.call_count, 2)
        self.assertEqual(self.guard.call_count, 2)
        self.assertEqual(sum(call.args[1] == 'verify' for call in self.run.call_args_list if len(call.args) > 1), 2)

    def test_existing_draft_requires_explicit_retry(self):
        self.github.exists = True
        with self.assertRaises(ValueError):
            self.publish()
        self.assertEqual(self.github.events, [])

    def test_retry_uploads_missing_assets_without_overwrite(self):
        self.plan['retry'] = True
        self.github.exists = True
        name = 'release.json'
        self.github.files[name] = (self.assets / name).read_bytes()
        self.publish()
        self.assertEqual(self.github.events.count('upload'), 7)

    def test_retry_reuses_existing_cryptographically_verified_bundle(self):
        self.plan['retry'] = True
        self.github.exists = True
        self.github.files['provenance.jsonl'] = b'previous valid bundle with its original timestamp'
        self.publish()
        self.assertEqual(self.github.events.count('upload'), 7)
        self.assertEqual(self.github.files['provenance.jsonl'], b'previous valid bundle with its original timestamp')

    def test_retry_rejects_corrupt_bytes_before_any_upload(self):
        self.plan['retry'] = True
        self.github.exists = True
        self.github.files['release.json'] = b'x' * len((self.assets / 'release.json').read_bytes())
        with self.assertRaises(ValueError):
            self.publish()
        self.assertNotIn('upload', self.github.events)
        self.assertNotIn('publish', self.github.events)

    def test_published_release_is_never_reused(self):
        self.plan['retry'] = True
        self.github.exists = True
        self.github.release['draft'] = False
        with self.assertRaises(ValueError):
            self.publish()
        self.assertEqual(self.github.events, [])

    def test_wrong_commit_draft_is_not_reused(self):
        self.plan['retry'] = True
        self.github.exists = True
        self.github.release['target_commitish'] = 'b' * 40
        with self.assertRaises(ValueError):
            self.publish()

    def test_uploaded_corruption_leaves_draft_unpublished(self):
        self.github.corrupt_upload = True
        with self.assertRaises(ValueError):
            self.publish()
        self.assertIn('draft', self.github.events)
        self.assertNotIn('publish', self.github.events)

    def test_missing_or_extra_assets_cannot_start_a_draft(self):
        (self.assets / 'unexpected.key').write_bytes(b'dummy')
        with self.assertRaises(ValueError):
            self.publish()
        self.assertEqual(self.github.events, [])

    def test_invalid_attestation_cannot_start_a_draft(self):
        self.provenance.side_effect = ValueError('invalid provenance')
        with self.assertRaises(ValueError):
            self.publish()
        self.assertEqual(self.github.events, [])

    def test_moved_remote_tag_cannot_publish(self):
        self.run.side_effect = lambda *args, **kwargs: ('b' * 40 + '\trefs/tags/v1.2.3') if args[0] == 'git' else ''
        with self.assertRaises(ValueError):
            self.publish()
        self.assertIn('draft', self.github.events)
        self.assertNotIn('publish', self.github.events)


class SBOM(unittest.TestCase):
    def test_collects_both_binary_build_infos_and_only_production_npm(self):
        with tempfile.TemporaryDirectory() as temporary:
            old = pathlib.Path.cwd()
            try:
                os.chdir(temporary)
                pathlib.Path('web').mkdir()
                pathlib.Path('web/package-lock.json').write_text(json.dumps({
                    'lockfileVersion': 3, 'packages': {'': {'name': 'test'},
                        'node_modules/react': {'version': '1.2.3'},
                        'node_modules/test-runner': {'version': '4.5.6', 'dev': True}}}))
                archives, infos = [], []
                for arch in ('amd64', 'arm64'):
                    archive = f'{arch}.tar.gz'
                    archives.append(archive)
                    with tarfile.open(archive, 'w:gz') as output:
                        header = tarfile.TarInfo('new-empire-control')
                        header.size = 4
                        output.addfile(header, io.BytesIO(b'test'))
                    infos.append(json.dumps({'GoVersion': 'go1.27.3', 'Settings': [
                        {'Key': 'GOOS', 'Value': 'linux'}, {'Key': 'GOARCH', 'Value': arch},
                        {'Key': 'vcs.revision', 'Value': 'a' * 40}, {'Key': 'vcs.modified', 'Value': 'false'}],
                        'Deps': [{'Path': 'example.invalid/library', 'Version': 'v1.2.3'}]}))
                args = argparse.Namespace(version='1.2.3', commit='a' * 40,
                                          binary=archives, output='sbom.json')
                with mock.patch.object(release, 'run', side_effect=infos):
                    release.sbom(args)
                document = json.loads(pathlib.Path('sbom.json').read_text())
                names = {component['name'] for component in document['components']}
                self.assertEqual(names, {'Go standard library', 'react', 'example.invalid/library'})
                pathlib.Path('sbom.json').unlink()
                with mock.patch.object(release, 'run', return_value=infos[0]), self.assertRaises(ValueError):
                    release.sbom(args)
                self.assertFalse(pathlib.Path('sbom.json').exists())
            finally:
                os.chdir(old)


if __name__ == '__main__':
    unittest.main()
