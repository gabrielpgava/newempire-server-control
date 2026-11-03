import json
import pathlib
import tempfile
import unittest

import check


class RepositoryChecks(unittest.TestCase):
    def test_node_version_authority(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            with self.assertRaises(ValueError):
                check.node_version_file(root)
            (root / 'web').mkdir()
            (root / 'web/.node-version').write_text('26.12.0\n')
            with self.assertRaises(ValueError):
                check.node_version_file(root)
            (root / '.node-version').write_text('26.13.0\n')
            self.assertEqual(check.node_version_file(root), root / '.node-version')

    def test_exactly_one_api_contract(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            (root / 'docs/api').mkdir(parents=True)
            with self.assertRaises(ValueError):
                check.openapi_file(root)
            path = root / 'docs/api/openapi.json'
            path.write_text(json.dumps({'openapi': '3.1.0'}))
            self.assertEqual(check.openapi_file(root), path)
            (root / 'docs/api/openapi.yaml').touch()
            with self.assertRaises(ValueError):
                check.openapi_file(root)
