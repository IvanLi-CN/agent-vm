#!/usr/bin/env bash
set -euo pipefail
# Install the first-boot dependency before removing build-time host identities.
cat > /etc/systemd/system/agent-vm-host-keys.service <<'UNIT'
[Unit]
Description=Generate instance SSH host keys
Before=ssh.service ssh.socket
ConditionPathExists=!/etc/ssh/ssh_host_ed25519_key

[Service]
Type=oneshot
ExecStart=/usr/bin/ssh-keygen -A
RemainAfterExit=yes
UNIT
install -d /etc/systemd/system/ssh.service.d
cat > /etc/systemd/system/ssh.service.d/agent-vm-host-keys.conf <<'UNIT'
[Unit]
Requires=agent-vm-host-keys.service
After=agent-vm-host-keys.service
UNIT
systemctl daemon-reload
for algorithm in rsa ecdsa ed25519; do
  rm -f -- "/etc/ssh/ssh_host_${algorithm}_key" "/etc/ssh/ssh_host_${algorithm}_key.pub"
done
: > /etc/machine-id
rm -f -- /etc/apt/apt.conf.d/80-agent-vm-build-network
