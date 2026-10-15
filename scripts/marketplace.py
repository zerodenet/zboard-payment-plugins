#!/usr/bin/env python3
"""Describe already signed packages for the marketplace; never execute them."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import zipfile

ROOT = Path(__file__).resolve().parents[1]


def generate(directory, tag, published_at):
    listing = json.loads((ROOT / 'marketplace-listing.json').read_text())
    metadata = json.loads((directory / 'release-metadata.json').read_text())
    if metadata['publisher'] != listing['publisher'] or metadata['repository'] != listing['repository']:
        raise ValueError('release publisher or repository differs from listing')
    if metadata['source']['version'] != tag or not re.fullmatch(r'[a-f0-9]{40}', metadata['source']['commit']):
        raise ValueError('release source differs from requested tag')
    artifacts = []
    declarations = None
    for asset in metadata['releases'][0]['artifacts']:
        name = asset['url'].rsplit('/', 1)[1]
        if asset['url'] != f"{listing['repository']}/releases/download/{tag}/{name}":
            raise ValueError('artifact is outside this immutable release')
        path = directory / name
        data = path.read_bytes()
        if len(data) != asset['size'] or hashlib.sha256(data).hexdigest() != asset['sha256']:
            raise ValueError('artifact size or digest differs from published metadata')
        with zipfile.ZipFile(path) as archive:
            manifest = json.loads(archive.read('manifest.json'))
            signature = json.loads(archive.read('signature.json'))
        if manifest['id'] != listing['id'] or 'v' + manifest['version'] != tag:
            raise ValueError('package identity differs from listing or tag')
        if (signature['algorithm'] != 'ed25519' or signature['key_id'] != listing['publisher']['id']
                or signature['public_key'] != listing['publisher']['public_key']):
            raise ValueError('package signing identity differs from listing')
        bounds = re.fullmatch(r'>=([0-9]+\.[0-9]+\.[0-9]+) <([0-9]+\.[0-9]+\.[0-9]+)', manifest['requires']['zboard'])
        if not bounds:
            raise ValueError('unsupported host-version expression')
        target = {'host':'zboard', 'package_id':manifest['id'],
                  'host_version':{'min':bounds[1], 'max_exclusive':bounds[2]},
                  'surfaces':manifest['surfaces'], 'capabilities':manifest['capabilities']}
        if declarations is not None and declarations != target:
            raise ValueError('platform packages disagree on host declarations')
        declarations = target
        os_name, arch = asset['platform'].split('-')
        artifacts.append({'os':os_name, 'arch':arch, 'url':asset['url'], 'size':len(data),
                          'sha256':asset['sha256'], 'signature':{'algorithm':'ed25519','value':signature['signature']}})
    if not artifacts:
        raise ValueError('release has no signed artifacts')
    declarations['artifacts'] = artifacts
    return {'schema_version':1, 'product_id':listing['id'], 'repository':listing['repository'],
            'publisher':listing['publisher'], 'listing':listing,
            'source':{'tag':tag, 'commit':metadata['source']['commit']},
            'release':{'version':tag.removeprefix('v'), 'channel':'stable', 'published_at':published_at,
                       'notes_url':f"{listing['repository']}/releases/tag/{tag}", 'targets':[declarations]}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', type=Path, default=ROOT / 'dist')
    parser.add_argument('--tag', required=True)
    parser.add_argument('--published-at', required=True)
    args = parser.parse_args()
    document = generate(args.directory, args.tag, args.published_at)
    (args.directory / 'marketplace-entry.json').write_text(json.dumps(document,ensure_ascii=False,indent=2)+'\n')


if __name__ == '__main__':
    main()
