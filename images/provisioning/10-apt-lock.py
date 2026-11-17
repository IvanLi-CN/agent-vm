#!/usr/bin/env python3
"""Install only package versions whose signed APT metadata matches the lock."""
import json, subprocess, sys
from pathlib import Path
lock=json.loads(Path('/root/.agent-vm-apt-lock.json').read_text())
group=lock[sys.argv[1]]
selected=[name+'='+value['version'] for name,value in sorted(group.items())]
metadata=subprocess.check_output(['apt-cache','show',*selected],text=True)
seen={}
for paragraph in metadata.split('\n\n'):
    fields={}
    for line in paragraph.splitlines():
        if line and not line[0].isspace() and ': ' in line:
            key,value=line.split(': ',1);fields[key]=value
    name=fields.get('Package')
    if name in group and fields.get('Version')==group[name]['version'] and fields.get('SHA256')==group[name]['sha256']:
        seen[name]=True
missing=sorted(set(group)-set(seen))
if missing:raise SystemExit('Locked APT packages unavailable or checksum mismatch: '+', '.join(missing))
subprocess.run(['apt-get','install','-y','--no-install-recommends',*selected],check=True)
