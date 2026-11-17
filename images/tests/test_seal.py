import subprocess
import tempfile
import unittest
from pathlib import Path


class DockerStateSealingTest(unittest.TestCase):
    def clean(self, docker, containerd, mounts=''):
        source = (Path(__file__).resolve().parents[1] / 'provisioning/70-seal.sh').read_text()
        program = source.split("<<'DOCKER_STATE'\n", 1)[1].split('\nDOCKER_STATE', 1)[0]
        # Exercise the actual cleanup body against isolated temporary paths.
        program = 'import sys\n' + program.replace("Path('/var/lib/docker')", 'Path(sys.argv[1])').replace("Path('/var/lib/containerd')", 'Path(sys.argv[2])')
        program = program.replace("Path('/proc/self/mountinfo')", 'Path(sys.argv[3])')
        with tempfile.NamedTemporaryFile(mode='w') as mountinfo:
            mountinfo.write(mounts); mountinfo.flush()
            return subprocess.run(['python3', '-c', program, str(docker), str(containerd), mountinfo.name], capture_output=True, text=True)

    def test_cleans_only_disposable_daemon_data(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            for name in ['docker', 'containerd']:
                (root / name).mkdir()
                (root / name / 'build-state').write_text('runtime data')
            sentinel = root / 'docker-config'; sentinel.write_text('keep')
            result = self.clean(root / 'docker', root / 'containerd')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(list((root / 'docker').iterdir()), [])
            self.assertEqual(list((root / 'containerd').iterdir()), [])
            self.assertEqual(sentinel.read_text(), 'keep')

    def test_refuses_a_symlinked_parent_without_deleting_state(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve(); actual = root / 'actual'; actual.mkdir()
            for name in ['docker', 'containerd']:
                (actual / name).mkdir()
                (actual / name / 'sentinel').write_text('keep')
            linked = root / 'linked'; linked.symlink_to(actual, target_is_directory=True)
            result = self.clean(linked / 'docker', linked / 'containerd')
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('refusing to clean mounted or linked', result.stderr)
            for name in ['docker', 'containerd']:
                self.assertEqual((actual / name / 'sentinel').read_text(), 'keep')

    def test_refuses_nested_mount_before_deleting_state(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory).resolve(); docker=root/'docker'; docker.mkdir()
            (docker/'sentinel').write_text('keep')
            result=self.clean(docker, root/'containerd', f'1 0 0:1 / {docker}/overlay rw - overlay overlay rw\n')
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('refusing to clean mounted or linked', result.stderr)
            self.assertEqual((docker/'sentinel').read_text(), 'keep')


if __name__ == '__main__': unittest.main()
