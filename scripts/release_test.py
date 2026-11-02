import argparse
import contextlib
import io
import json
import os
import pathlib
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
                with mock.patch.dict(os.environ, {'RELEASE_BUMP': 'auto', 'RELEASE_CHANNEL': 'stable', 'RELEASE_RETRY_TAG': ''}), contextlib.redirect_stdout(io.StringIO()):
                    release.plan_release(argparse.Namespace(live=False, output='plan.json'))
                plan = json.loads(pathlib.Path('plan.json').read_text())
                self.assertEqual(plan['version'], '1.1.0')
                self.assertNotIn('old break', plan['body'])
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
