import base64
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

import release


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / 'internal/runtime').mkdir(parents=True)
        (self.root / 'dist').mkdir()
        self.manifest = release.check_version()
        (self.root / 'manifest.json').write_text(json.dumps(self.manifest))
        (self.root / 'internal/runtime/server.go').write_text('const pluginVersion = "0.0.1"\n')
        self.pub = {'id': 'release-test', 'public_key': base64.b64encode(bytes(32)).decode()}

    def packages(self, corrupt=None):
        for platform in release.PLATFORMS:
            manifest = copy.deepcopy(self.manifest)
            executable = f'runtimes/{platform}/payments'
            manifest['components']['server']['executables'] = {platform: executable}
            manifest['files'] = {executable: hashlib.sha256(b'binary').hexdigest()}
            signature = {**self.pub, 'key_id': self.pub['id'], 'algorithm': 'ed25519'}
            if corrupt:
                corrupt(manifest, signature)
            with zipfile.ZipFile(self.root / 'dist' / f'zboard-payments-v0.0.1-{platform}.zbplugin', 'w') as package:
                package.writestr('manifest.json', json.dumps(manifest))
                package.writestr('signature.json', json.dumps(signature))
                package.writestr(executable, b'binary')

    def test_tag_and_runtime_must_match_manifest(self):
        with self.assertRaises(ValueError):
            release.check_version(self.root, 'v0.0.2')
        (self.root / 'internal/runtime/server.go').write_text('const pluginVersion = "0.0.2"\n')
        with self.assertRaises(ValueError):
            release.check_version(self.root)

    def test_signing_key_must_match_configured_identity_and_refuses_overwrite(self):
        env = {'PLUGIN_PUBLISHER_ID': self.pub['id'], 'PLUGIN_PUBLIC_KEY': self.pub['public_key'],
               'PLUGIN_SIGNING_KEY': base64.b64encode(bytes(64)).decode()}
        output = self.root / 'key'
        release.write_key(output, env)
        self.assertEqual(output.stat().st_mode & 0o777, 0o600)
        with self.assertRaises(FileExistsError):
            release.write_key(output, env)
        env['PLUGIN_PUBLIC_KEY'] = base64.b64encode(bytes([1]) * 32).decode()
        with self.assertRaises(ValueError):
            release.write_key(self.root / 'wrong-key', env)

    def test_official_packager_empty_host_fields_match_omitted_source_fields(self):
        source = copy.deepcopy(self.manifest)
        source['requires'].pop('recommended_zboard_version')
        source['requires'].pop('tested_zboard_versions')
        (self.root / 'manifest.json').write_text(json.dumps(source))
        self.packages()
        release.metadata(self.root, 'v0.0.1', 'a' * 40, self.pub)

    def test_metadata_has_four_exact_release_assets(self):
        self.packages()
        entry = release.metadata(self.root, 'v0.0.1', 'a' * 40, self.pub)
        artifacts = entry['releases'][0]['artifacts']
        self.assertEqual([a['platform'] for a in artifacts], list(release.PLATFORMS))
        for artifact in artifacts:
            self.assertIn('/releases/download/v0.0.1/', artifact['url'])
            self.assertEqual(len(artifact['sha256']), 64)

    def test_metadata_rejects_changed_publisher_data_schema_or_payload(self):
        mutations = [lambda m, s: s.update(public_key='wrong'),
                     lambda m, s: m['data'].update(version=99),
                     lambda m, s: m['files'].update({next(iter(m['files'])): '0' * 64})]
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                self.packages(mutation)
                with self.assertRaises(ValueError):
                    release.metadata(self.root, 'v0.0.1', 'a' * 40, self.pub)


if __name__ == '__main__':
    unittest.main()
