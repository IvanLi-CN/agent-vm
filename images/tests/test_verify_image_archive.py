import importlib.util
import io
import tarfile
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('verify_image_archive', Path(__file__).resolve().parents[1] / 'scripts/verify_image_archive.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ArchivePortabilityTest(unittest.TestCase):
    def verify(self, members):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'image.tar.gz'
            with tarfile.open(path, 'w:gz') as archive:
                for name, kind in members:
                    member = tarfile.TarInfo(name)
                    member.type = kind
                    archive.addfile(member, io.BytesIO())
            module.verify_archive(path)

    def test_keeps_programs_and_empty_daemon_data_directories(self):
        self.verify([('rootfs/usr/bin/docker', tarfile.REGTYPE),
                     ('rootfs/etc/docker/daemon.json', tarfile.REGTYPE),
                     ('rootfs/var/lib/docker', tarfile.DIRTYPE),
                     ('rootfs/var/lib/containerd', tarfile.DIRTYPE)])

    def test_rejects_device_nodes_without_creating_them_on_the_test_host(self):
        for path, kind in [('rootfs/var/lib/docker/volumes/backingFsBlockDev', tarfile.BLKTYPE),
                           ('rootfs/var/lib/docker/overlay2/layer/diff/obsolete', tarfile.CHRTYPE),
                           ('rootfs/other/build-device', tarfile.BLKTYPE)]:
            with self.subTest(path=path), self.assertRaisesRegex(ValueError, 'device'):
                self.verify([(path, kind)])

    def test_rejects_runtime_state_even_when_it_has_no_device_nodes(self):
        for path, kind in [('rootfs/var/lib/docker/volumes/metadata.db', tarfile.REGTYPE),
                           ('rootfs/var/lib/containerd/io.containerd.content.v1.content', tarfile.DIRTYPE),
                           ('rootfs/var/lib/docker', tarfile.SYMTYPE)]:
            with self.subTest(path=path), self.assertRaisesRegex(ValueError, 'runtime state'):
                self.verify([(path, kind)])


if __name__ == '__main__':
    unittest.main()
