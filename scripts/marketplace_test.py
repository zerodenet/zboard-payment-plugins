import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

import marketplace


class MarketplaceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.folder = Path(self.tmp.name)
        self.listing = json.loads((marketplace.ROOT / 'marketplace-listing.json').read_text())
        manifest = json.loads((marketplace.ROOT / 'manifest.json').read_text())
        name = 'zboard-payments-v0.0.1-linux-amd64.zbplugin'
        self.package = self.folder / name
        signature = {'algorithm':'ed25519','key_id':self.listing['publisher']['id'],
                     'public_key':self.listing['publisher']['public_key'],'signature':'fixture-only'}
        with zipfile.ZipFile(self.package, 'w') as archive:
            archive.writestr('manifest.json', json.dumps(manifest))
            archive.writestr('signature.json', json.dumps(signature))
        data = self.package.read_bytes()
        self.metadata = {'publisher':self.listing['publisher'],'repository':self.listing['repository'],
                         'source':{'version':'v0.0.1','commit':'1'*40},
                         'releases':[{'artifacts':[{'platform':'linux-amd64',
                          'url':self.listing['repository']+'/releases/download/v0.0.1/'+name,
                          'size':len(data),'sha256':hashlib.sha256(data).hexdigest()}]}]}
        self.save()

    def save(self):
        (self.folder / 'release-metadata.json').write_text(json.dumps(self.metadata))

    def test_signed_declarations_and_exact_artifact_identity_are_preserved(self):
        document = marketplace.generate(self.folder, 'v0.0.1', '2026-10-15T02:18:31Z')
        self.assertEqual(document['listing'], self.listing)
        target = document['release']['targets'][0]
        self.assertEqual(target['capabilities'], self.listing['targets'][0]['capabilities'])
        self.assertEqual(target['artifacts'][0]['sha256'], self.metadata['releases'][0]['artifacts'][0]['sha256'])
        self.assertEqual(document['source']['commit'], '1'*40)

    def test_rejects_publisher_changes(self):
        self.metadata['publisher'] = copy.deepcopy(self.metadata['publisher'])
        self.metadata['publisher']['id'] = 'someone-else'
        self.save()
        with self.assertRaises(ValueError):
            marketplace.generate(self.folder, 'v0.0.1', '2026-10-15T02:18:31Z')

    def test_rejects_modified_package_and_cross_release_asset(self):
        self.package.write_bytes(self.package.read_bytes()+b'changed')
        with self.assertRaises(ValueError):
            marketplace.generate(self.folder, 'v0.0.1', '2026-10-15T02:18:31Z')
        self.metadata['releases'][0]['artifacts'][0]['url'] = 'https://github.com/other/repo/releases/download/v0.0.1/file.zbplugin'
        self.save()
        with self.assertRaises(ValueError):
            marketplace.generate(self.folder, 'v0.0.1', '2026-10-15T02:18:31Z')

    def test_rejects_different_tag(self):
        with self.assertRaises(ValueError):
            marketplace.generate(self.folder, 'v0.0.2', '2026-10-15T02:18:31Z')


if __name__ == '__main__':
    unittest.main()
