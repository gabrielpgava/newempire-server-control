import argparse
import contextlib
import io
import json
import os
import pathlib
import re
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

import release


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

    def publish(self):
        self.plan_path.write_text(json.dumps(self.plan))
        with contextlib.redirect_stdout(io.StringIO()):
            release.publish(argparse.Namespace(plan=str(self.plan_path), assets=str(self.assets), tool='tool'), self.github)

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
