import copy
import hashlib
import json
import unittest
from unittest.mock import patch

import correct_attribution as attribution


class AttributionTests(unittest.TestCase):
    def setUp(self):
        self.old = {'publisher': {'id': 'zerodenet', 'public_key': 'unchanged'},
                    'listing': {'maintainers': ['kovawx']},
                    'release': {'version': '0.0.1', 'artifacts': ['unchanged']}}
        self.new = copy.deepcopy(self.old)
        self.new['listing']['maintainers'] = ['zerodenet']
        self.raw = attribution.encoded(self.old)
        self.name = 'marketplace-entry.json'

    def corrected(self, original=None, generated=None):
        with patch.dict(attribution.ORIGINALS, {self.name: hashlib.sha256(self.raw).hexdigest()}), \
                patch.dict(attribution.CORRECTED, {self.name: hashlib.sha256(attribution.encoded(self.new)).hexdigest()}):
            return attribution.correction(self.name, self.raw if original is None else original,
                                          attribution.encoded(self.new if generated is None else generated))

    def test_only_approved_attribution_changes_and_reruns_are_noops(self):
        self.assertEqual(json.loads(self.corrected()), self.new)
        self.assertIsNone(self.corrected(original=attribution.encoded(self.new)))

    def test_rejects_non_attribution_and_unapproved_original_changes(self):
        modified = copy.deepcopy(self.new)
        modified['release']['artifacts'] = ['different-package']
        with self.assertRaises(ValueError):
            self.corrected(generated=modified)
        with self.assertRaises(ValueError):
            self.corrected(original=self.raw + b' ')
        with self.assertRaises(ValueError):
            self.corrected(original=attribution.encoded(self.new) + b' ')

    def test_rejects_other_publisher_version_or_maintainer(self):
        for key, value in [('publisher', {'id': 'other'}), ('release', {'version': '0.0.2'}),
                           ('listing', {'maintainers': ['someone-else']})]:
            modified = {**self.old, key: value}
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.corrected(original=attribution.encoded(modified))


if __name__ == '__main__':
    unittest.main()
