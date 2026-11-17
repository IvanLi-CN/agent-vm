#!/usr/bin/env bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
command -v curl gpg newuidmap
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg
printf '%s\n' "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list
apt-get update
python3 /root/.agent-vm-apt-install.py docker
mkdir -p /etc/docker
printf '%s\n' '{"storage-driver":"overlay2","features":{"containerd-snapshotter":false}}' > /etc/docker/daemon.json
systemctl enable docker
# Package installation can start the daemon before daemon.json exists.
systemctl restart docker
docker version
docker compose version
