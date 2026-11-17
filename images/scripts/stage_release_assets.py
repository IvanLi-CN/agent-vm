#!/usr/bin/env python3
"""Stage six verified guest images before freezing an immutable product release."""
import argparse
import json
import shutil
from pathlib import Path

from verify_assets import digest, verify

SUMS = 'agent-vm-ubuntu24-SHA256SUMS'
SUFFIXES = ['tar.gz', 'manifest.json', 'sbom.json', 'readiness.json']


def stage(input_root, output, revision):
    combinations = [(language, mode) for language in ['go', 'rust', 'java']
                    for mode in ['privileged', 'unprivileged']]
    directories = {'guest-image-' + language + '-' + mode for language, mode in combinations}
    if not input_root.is_dir() or {p.name for p in input_root.iterdir()} != directories:
        raise ValueError('release requires exactly six language/runtime artifacts')
    sources = {}
    for language, mode in combinations:
        directory = input_root / ('guest-image-' + language + '-' + mode)
        asset = 'agent-vm-ubuntu24-' + language + '-' + mode + '-amd64'
        names = {asset + '.' + suffix for suffix in SUFFIXES}
        if directory.is_symlink() or {p.name for p in directory.iterdir()} != names | {SUMS}:
            raise ValueError('unexpected image artifact contents: ' + directory.name)
        if any(p.is_symlink() or not p.is_file() for p in directory.iterdir()):
            raise ValueError('image artifacts must be regular files')
        verified = verify(directory, revision)
        manifest = json.loads((directory / (asset + '.manifest.json')).read_text())
        if len(verified) != 1 or verified[0]['name'] != asset or manifest.get('profile') != language or manifest.get('variant') != mode:
            raise ValueError('image artifact does not match its matrix slot')
        for name in names:
            sources[name] = directory / name
    # Validate every slot before writing any reservation artifacts.
    output.mkdir(parents=True, exist_ok=True)
    for name, source in sources.items():
        target = output / name
        if target.is_symlink() or (target.exists() and digest(target) != digest(source)):
            raise ValueError('refusing to overwrite a different staged image asset: ' + name)
    for name, source in sources.items():
        target = output / name
        if not target.exists():
            shutil.copyfile(source, target)
    sums = ''.join(digest(output / name) + '  ' + name + '\n' for name in sorted(sources))
    sums_path = output / SUMS
    if sums_path.is_symlink() or (sums_path.exists() and sums_path.read_text() != sums):
        raise ValueError('refusing to change staged image checksums')
    sums_path.write_text(sums)
    return {name: str((output / name).resolve()) for name in sorted([*sources, SUMS])}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--input-root', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--source-revision', required=True)
    args = parser.parse_args()
    print(json.dumps({'assets': stage(args.input_root, args.output, args.source_revision)}))
