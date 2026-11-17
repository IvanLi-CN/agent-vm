import json, runpy, unittest
from pathlib import Path
from unittest.mock import patch

SCRIPT=Path(__file__).resolve().parents[1]/'provisioning/10-apt-lock.py'
class AptLockTest(unittest.TestCase):
    def execute(self, metadata):
        lock={'docker':{'example':{'version':'1.0','sha256':'a'*64}}}
        with patch('sys.argv',[str(SCRIPT),'docker']),patch('pathlib.Path.read_text',return_value=json.dumps(lock)),patch('subprocess.check_output',return_value=metadata),patch('subprocess.run') as install:
            try:runpy.run_path(str(SCRIPT),run_name='__main__')
            except SystemExit:
                install.assert_not_called()
                raise
            return install.call_args
    def test_matching_signed_metadata_installs_exact_version(self):
        call=self.execute('Package: example\nVersion: 1.0\nSHA256: '+'a'*64+'\n')
        self.assertEqual(call.args[0],['apt-get','install','-y','--no-install-recommends','example=1.0'])
    def test_changed_hash_or_missing_version_prevents_install(self):
        for metadata in ['Package: example\nVersion: 1.0\nSHA256: '+'b'*64+'\n','Package: example\nVersion: 2.0\nSHA256: '+'a'*64+'\n','']:
            with self.subTest(metadata=metadata),self.assertRaises(SystemExit):self.execute(metadata)
if __name__=='__main__':unittest.main()
