#!/usr/bin/env python3
"""Verify downloaded image assets before a target-server catalog import."""
import argparse, hashlib, json, re
from pathlib import Path
from verify_image_archive import verify_archive

def digest(path):
    value=hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda:stream.read(1024*1024),b''):value.update(chunk)
    return value.hexdigest()

def verify(directory, revision):
    if not re.fullmatch(r'[0-9a-f]{40}',revision):raise ValueError('source revision must be a full Git SHA')
    checks={}
    for line in (directory/'agent-vm-ubuntu24-SHA256SUMS').read_text().splitlines():
        match=re.fullmatch(r'([0-9a-f]{64})  ([A-Za-z0-9_.-]+)',line)
        if not match or match[2] in checks:raise ValueError('invalid or duplicate checksum entry')
        checks[match[2]]=match[1]
    manifests=sorted(directory.glob('*.manifest.json'))
    if not manifests:raise ValueError('no image manifest found')
    verified=[]
    for path in manifests:
        asset=path.name.removesuffix('.manifest.json')
        for suffix in ['tar.gz','manifest.json','sbom.json','readiness.json']:
            name=asset+'.'+suffix
            if name not in checks or digest(directory/name)!=checks[name]:raise ValueError('checksum mismatch: '+name)
        verify_archive(directory/(asset+'.tar.gz'))
        manifest=json.loads(path.read_text())
        mode=manifest.get('variant')
        if manifest.get('schema')!=1 or manifest.get('name')!=asset or manifest.get('source_revision')!=revision or manifest.get('architecture')!='amd64' or mode not in ['privileged','unprivileged']:raise ValueError('invalid image provenance or runtime variant')
        config=manifest.get('runtime_config',{})
        if config.get('security.privileged')!=str(mode=='privileged').lower() or config.get('security.nesting')!='true':raise ValueError('inconsistent runtime configuration')
        if manifest.get('sha256')!=checks[asset+'.tar.gz'] or not re.fullmatch(r'[0-9a-f]{64}',manifest.get('image_fingerprint','')):raise ValueError('invalid image identity')
        report=json.loads((directory/(asset+'.readiness.json')).read_text())
        if report.get('ready') is not True or report.get('ssh_ed25519') is not True or report.get('database_clients') is not True:raise ValueError('guest is not ready')
        for key in ['docker_build','overlay2_whiteout','docker_run','compose','volume_persistence','dns_network','daemon_restart']:
            if report.get('docker',{}).get(key) is not True:raise ValueError('missing Docker workload evidence: '+key)
        if not json.loads((directory/(asset+'.sbom.json')).read_text()).get('packages'):raise ValueError('empty SBOM')
        verified.append({'name':asset,'fingerprint':manifest['image_fingerprint'],'variant':mode})
    return verified

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory',type=Path,required=True)
    parser.add_argument('--source-revision',required=True)
    args=parser.parse_args()
    print(json.dumps({'verified':verify(args.directory,args.source_revision)},indent=2))
