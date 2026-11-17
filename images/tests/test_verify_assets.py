import tarfile
import importlib.util,json,tempfile,unittest,sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'scripts'))
spec=importlib.util.spec_from_file_location('verify_assets',Path(__file__).resolve().parents[1]/'scripts/verify_assets.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
class AssetVerificationTest(unittest.TestCase):
    def fixture(self,root):
        name='agent-vm-ubuntu24-go-unprivileged-amd64'
        with tarfile.open(root/(name+'.tar.gz'), 'w:gz') as archive:
            archive.addfile(tarfile.TarInfo('metadata.yaml'))
        manifest={'schema':1,'name':name,'source_revision':'a'*40,'architecture':'amd64','variant':'unprivileged','runtime_config':{'security.privileged':'false','security.nesting':'true'},'image_fingerprint':'b'*64,'sha256':module.digest(root/(name+'.tar.gz'))}
        report={'ready':True,'ssh_ed25519':True,'database_clients':True,'docker':{key:True for key in ['docker_build','overlay2_whiteout','docker_run','compose','volume_persistence','dns_network','daemon_restart']}}
        for suffix,value in [('manifest.json',manifest),('readiness.json',report),('sbom.json',{'packages':[{'name':'example','version':'1'}]})]:(root/(name+'.'+suffix)).write_text(json.dumps(value))
        self.checksums(root)
        return name
    def checksums(self,root):
        (root/'agent-vm-ubuntu24-SHA256SUMS').write_text(''.join(module.digest(path)+'  '+path.name+'\n' for path in sorted(root.iterdir()) if path.name!='agent-vm-ubuntu24-SHA256SUMS'))
    def test_checksums_and_exact_source_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);name=self.fixture(root)
            self.assertEqual(module.verify(root,'a'*40)[0]['variant'],'unprivileged')
            with self.assertRaises(ValueError):module.verify(root,'c'*40)
            (root/(name+'.tar.gz')).write_bytes(b'tampered')
            with self.assertRaises(ValueError):module.verify(root,'a'*40)
    def test_rechecks_runtime_and_readiness_after_checksum(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);name=self.fixture(root)
            path=root/(name+'.readiness.json');data=json.loads(path.read_text());data['docker']['compose']=False;path.write_text(json.dumps(data));self.checksums(root)
            with self.assertRaises(ValueError):module.verify(root,'a'*40)
            path=root/'agent-vm-ubuntu24-SHA256SUMS';path.write_text('a'*64+'  ../escape\n')
            with self.assertRaises(ValueError):module.verify(root,'a'*40)
if __name__=='__main__':unittest.main()
