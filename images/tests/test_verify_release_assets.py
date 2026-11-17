import json
import shutil
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
from verify_assets import digest
from verify_release_assets import verify_release_assets


class ReleaseAssetVerificationTest(unittest.TestCase):
    def fixture(self, root):
        name = 'agent-vm-ubuntu24-go-unprivileged-amd64'
        with tarfile.open(root / (name + '.tar.gz'), 'w:gz') as archive:
            archive.addfile(tarfile.TarInfo('metadata.yaml'))
        manifest = {
            'schema': 1,
            'name': name,
            'profile': 'go',
            'source_revision': 'a' * 40,
            'architecture': 'amd64',
            'variant': 'unprivileged',
            'runtime_config': {'security.privileged': 'false', 'security.nesting': 'true'},
            'image_fingerprint': 'b' * 64,
            'sha256': digest(root / (name + '.tar.gz')),
        }
        report = {
            'ready': True,
            'ssh_ed25519': True,
            'database_clients': True,
            'docker': {key: True for key in ['docker_build', 'overlay2_whiteout', 'docker_run', 'compose', 'volume_persistence', 'dns_network', 'daemon_restart']},
        }
        for suffix, value in [('manifest.json', manifest), ('readiness.json', report), ('sbom.json', {'packages': [{'name': 'example', 'version': '1'}]})]:
            (root / (name + '.' + suffix)).write_text(json.dumps(value))
        (root / 'agent-vm-ubuntu24-SHA256SUMS').write_text(''.join(
            digest(path) + '  ' + path.name + '\n'
            for path in sorted(root.iterdir())
        ))

    def test_matches_downloaded_bytes_and_rejects_tampering(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            local = root / 'local'
            remote = root / 'remote'
            local.mkdir()
            self.fixture(local)
            shutil.copytree(local, remote)
            self.assertEqual(len(verify_release_assets(local, remote, 'a' * 40)), 1)
            (remote / 'agent-vm-ubuntu24-go-unprivileged-amd64.tar.gz').write_bytes(b'tampered')
            with self.assertRaises(ValueError):
                verify_release_assets(local, remote, 'a' * 40)

    def test_rejects_extra_downloaded_assets(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            local = root / 'local'
            remote = root / 'remote'
            local.mkdir()
            self.fixture(local)
            shutil.copytree(local, remote)
            (remote / 'unexpected').write_text('unexpected')
            with self.assertRaises(ValueError):
                verify_release_assets(local, remote, 'a' * 40)


if __name__ == '__main__':
    unittest.main()
