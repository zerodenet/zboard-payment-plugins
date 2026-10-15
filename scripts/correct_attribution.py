#!/usr/bin/env python3
"""Prepare the explicitly approved v0.0.1 unsigned attribution correction."""
import argparse
import copy
import hashlib
import json
from pathlib import Path


ORIGINALS = {
    'marketplace-entry.json': 'ee72360a46a9da868d417ebf18148a8d8e7e9eff084dd467e0ad4a8a7f325ac8',
    'release-metadata.json': '3d81d42772fb83606df5ddc72ec2f64c038da42753ed2526cf82fcc25df0c63f',
}
CORRECTED = {
    'marketplace-entry.json': 'fef865188cf63aa686b1b3e395029df57f9485fe208aef552a754c914d764ae5',
    'release-metadata.json': 'f1cc142ae123e0a2a5173069cfa3f1b00c313567d2fa687abf1e3e5d02ceb863',
}


def encoded(document):
    return (json.dumps(document, ensure_ascii=False, indent=2) + '\n').encode()


def correction(name, original, generated):
    document = json.loads(original)
    target = copy.deepcopy(document)
    if name == 'marketplace-entry.json':
        maintainers = target['listing']
        version = target['release']['version']
    else:
        maintainers = target
        version = target['source']['version'].removeprefix('v')
    if version != '0.0.1' or document['publisher']['id'] != 'zerodenet':
        raise ValueError('attribution correction is restricted to zerodenet v0.0.1')
    if maintainers['maintainers'] not in (['kovawx'], ['zerodenet']):
        raise ValueError('unexpected existing maintainer')
    maintainers['maintainers'] = ['zerodenet']
    if name == 'marketplace-entry.json' and target != json.loads(generated):
        raise ValueError('generated metadata changes more than attribution')
    result = encoded(target)
    if document == target:
        if hashlib.sha256(original).hexdigest() != CORRECTED[name]:
            raise ValueError('corrected metadata does not match the approved digest')
        return None
    if hashlib.sha256(original).hexdigest() != ORIGINALS[name]:
        raise ValueError('original metadata does not match the approved archive digest')
    return result


def prepare(published, generated, output):
    changes = []
    # Validate every input before producing any upload candidates.
    for name in ORIGINALS:
        original = (published / name).read_bytes()
        result = correction(name, original, generated.read_bytes())
        if result is not None:
            changes.append((name, original, result))
    output.mkdir(parents=True, exist_ok=False)
    (output / 'archive').mkdir()
    plan = []
    for name, original, result in changes:
        before = hashlib.sha256(original).hexdigest()
        archive = f'{Path(name).stem}.before-attribution-{before}.json'
        (output / 'archive' / archive).write_bytes(original)
        (output / name).write_bytes(result)
        plan.append({'name': name, 'archive': archive, 'before_sha256': before,
                     'after_sha256': hashlib.sha256(result).hexdigest()})
    (output / 'plan.json').write_bytes(encoded({'version': 'v0.0.1', 'changes': plan}))
    return plan


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--published', type=Path, required=True)
    parser.add_argument('--generated', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(prepare(args.published, args.generated, args.output)))


if __name__ == '__main__':
    main()
