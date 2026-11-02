import importlib.util
import io
import pathlib
import tarfile
import unittest

spec = importlib.util.spec_from_file_location('web_sandbox', pathlib.Path(__file__).with_name('test-web-sandbox.py'))
web_sandbox = importlib.util.module_from_spec(spec)
spec.loader.exec_module(web_sandbox)


class FrontendArtifactTests(unittest.TestCase):
    def artifact(self, name, link=False):
        out = io.BytesIO()
        with tarfile.open(fileobj=out, mode='w') as archive:
            index = tarfile.TarInfo('dist/index.html')
            index.size = 2
            archive.addfile(index, io.BytesIO(b'OK'))
            entry = tarfile.TarInfo(name)
            if link:
                entry.type = tarfile.SYMTYPE
                entry.linkname = '/etc/passwd'
                archive.addfile(entry)
            else:
                archive.addfile(entry, io.BytesIO())
        return out.getvalue()

    def test_accepts_only_regular_distribution_entries(self):
        self.assertIsNotNone(web_sandbox.validate_archive(self.artifact('dist/assets/app.js')))
        for name, link in [('dist/../../escape', False), ('/escape', False), ('dist/link', True)]:
            with self.assertRaises(ValueError):
                web_sandbox.validate_archive(self.artifact(name, link))
