#!/usr/bin/env bash
set -euo pipefail
# Only run in the disposable build guest. Docker checks leave host-specific
# device nodes, whiteouts and volume metadata that must never be shipped.
systemctl stop docker.socket docker.service containerd.service
for unit in docker.socket docker.service containerd.service; do
  if systemctl is-active --quiet "$unit"; then
    printf 'image sealing requires an inactive %s\n' "$unit" >&2
    exit 1
  fi
done
python3 - <<'DOCKER_STATE'
import shutil
from pathlib import Path

mounts = [line.split()[4] for line in Path('/proc/self/mountinfo').read_text().splitlines()]
for path in (Path('/var/lib/docker'), Path('/var/lib/containerd')):
    if path.is_symlink() or any(mount == str(path) or mount.startswith(str(path) + '/') for mount in mounts):
        raise SystemExit('refusing to clean mounted or linked Docker state: ' + str(path))
    if path.exists():
        shutil.rmtree(path)
    path.mkdir(mode=0o711)
DOCKER_STATE
# Install the first-boot dependency before removing build-time host identities.
cat > /etc/systemd/system/agent-vm-host-keys.service <<'UNIT'
[Unit]
Description=Generate instance SSH host keys
Before=ssh.service
ConditionPathExists=!/etc/ssh/ssh_host_ed25519_key

[Service]
Type=oneshot
ExecStart=/usr/bin/ssh-keygen -A
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT
install -d /etc/systemd/system/ssh.service.d
cat > /etc/systemd/system/ssh.service.d/agent-vm-host-keys.conf <<'UNIT'
[Unit]
Requires=agent-vm-host-keys.service
After=agent-vm-host-keys.service
UNIT
systemctl daemon-reload
systemctl enable agent-vm-host-keys.service
for algorithm in rsa ecdsa ed25519; do
  rm -f -- "/etc/ssh/ssh_host_${algorithm}_key" "/etc/ssh/ssh_host_${algorithm}_key.pub"
done
: > /etc/machine-id
rm -f -- /etc/apt/apt.conf.d/80-agent-vm-build-network
