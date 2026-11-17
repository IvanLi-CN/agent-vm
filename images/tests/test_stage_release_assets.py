import json
import shutil
import tarfile
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
from stage_release_assets import stage, SUMS, SUFFIXES
from verify_assets import digest, verify


class ReleaseImageStagingTest(unittest.TestCase):
    def fixture(self, root):
        for language in ['base', 'go', 'rust', 'java']:
            for mode in ['privileged', 'unprivileged']:
                directory = root / ('guest-image-' + language + '-' + mode)
                directory.mkdir(parents=True)
                name = 'agent-vm-ubuntu24-' + language + '-' + mode + '-amd64'
                with tarfile.open(directory / (name + '.tar.gz'), 'w:gz') as archive:
                    archive.addfile(tarfile.TarInfo('metadata.yaml'))
                manifest = dict(schema=1, name=name, profile=language, variant=mode,
                                architecture='amd64', source_revision='a' * 40,
                                image_fingerprint='b' * 64,
                                runtime_config={'security.privileged': str(mode == 'privileged').lower(), 'security.nesting': 'true'},
                                sha256=digest(directory / (name + '.tar.gz')))
                report = dict(ready=True, ssh_ed25519=True, database_clients=True,
                              docker={key: True for key in ['docker_build', 'overlay2_whiteout', 'docker_run', 'compose', 'volume_persistence', 'dns_network', 'daemon_restart']})
                for suffix, value in [('manifest.json', manifest), ('readiness.json', report), ('sbom.json', {'packages': [{'name': 'example', 'version': '1'}]})]:
                    (directory / (name + '.' + suffix)).write_text(json.dumps(value))
                self.checksums(directory)

    def checksums(self, directory):
        (directory / SUMS).write_text(''.join(digest(path) + '  ' + path.name + '\n' for path in sorted(directory.iterdir()) if path.name != SUMS))

    def test_stages_complete_set_for_immutable_release_and_recovery(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); source = root / 'input'; output = root / 'frozen'
            self.fixture(source)
            assets = stage(source, output, 'a' * 40)
            self.assertEqual(len(assets), 33)
            self.assertEqual(len(verify(output, 'a' * 40)), 8)
            self.assertEqual(assets, stage(source, output, 'a' * 40))
            self.assertEqual(len((output / SUMS).read_text().splitlines()), 32)

    def test_rejects_incomplete_corrupt_or_foreign_inputs_before_staging(self):
        for fault in ['missing_base', 'missing', 'tamper', 'source', 'foreign', 'symlink', 'wrong_slot', 'device']:
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary); source = root / 'input'; output = root / 'frozen'
                self.fixture(source)
                directory = source / 'guest-image-go-privileged'
                archive = next(directory.glob('*.tar.gz'))
                if fault == 'missing_base': shutil.rmtree(source / 'guest-image-base-privileged')
                elif fault == 'missing': archive.unlink()
                elif fault == 'device':
                    with tarfile.open(archive, 'w:gz') as tar:
                        node=tarfile.TarInfo('rootfs/var/lib/docker/volumes/backingFsBlockDev'); node.type=tarfile.BLKTYPE; tar.addfile(node)
                    path=next(directory.glob('*.manifest.json')); value=json.loads(path.read_text()); value['sha256']=digest(archive); path.write_text(json.dumps(value)); self.checksums(directory)
                elif fault == 'tamper': archive.write_bytes(b'tampered')
                elif fault == 'foreign': (directory / 'foreign').write_text('unexpected')
                elif fault == 'symlink': archive.unlink(); archive.symlink_to('/etc/hosts')
                else:
                    path = next(directory.glob('*.manifest.json')); value = json.loads(path.read_text())
                    value['source_revision' if fault == 'source' else 'profile'] = 'c' * 40 if fault == 'source' else 'rust'
                    path.write_text(json.dumps(value)); self.checksums(directory)
                with self.assertRaises((ValueError, FileNotFoundError)):
                    stage(source, output, 'a' * 40)
                self.assertFalse(output.exists())

    def test_never_overwrites_frozen_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); source = root / 'input'; output = root / 'frozen'
            self.fixture(source); assets = stage(source, output, 'a' * 40)
            path = Path(next(value for key, value in assets.items() if key.endswith('.tar.gz')))
            path.write_bytes(b'frozen-different-bytes')
            with self.assertRaises(ValueError): stage(source, output, 'a' * 40)
            self.assertEqual(path.read_bytes(), b'frozen-different-bytes')

    def test_conflicting_checksum_target_is_checked_before_any_asset_copy(self):
        for symlink in [False, True]:
            with self.subTest(symlink=symlink), tempfile.TemporaryDirectory() as temporary:
                root=Path(temporary); source=root/'input'; output=root/'frozen'
                self.fixture(source); output.mkdir()
                if symlink:
                    sentinel=root/'sentinel'; sentinel.write_text('keep')
                    (output/SUMS).symlink_to(sentinel)
                else:
                    (output/SUMS).write_text('different frozen checksums')
                with self.assertRaises(ValueError): stage(source, output, 'a'*40)
                self.assertEqual({path.name for path in output.iterdir()}, {SUMS})
                if symlink: self.assertEqual(sentinel.read_text(), 'keep')


if __name__ == '__main__': unittest.main()
