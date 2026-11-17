#!/usr/bin/env bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update
if [ "$#" -gt 0 ]; then
  apt-get install -y --no-install-recommends "$@"
fi
download_verify() {
  url="$1"; sha="$2"; out="$3"
  curl -fsSL "$url" -o "$out"
  printf '%s  %s\n' "$sha" "$out" | sha256sum -c -
}
tmpdir="$(mktemp -d)"
download_verify "${BUN_URL}" "${BUN_SHA256}" "$tmpdir/bun.zip"
unzip -oq "$tmpdir/bun.zip" -d "$tmpdir/bun"
install -m 0755 "$tmpdir/bun/bun-linux-x64/bun" /usr/local/bin/bun
download_verify "${NODE_URL}" "${NODE_SHA256}" "$tmpdir/node.tar.gz"
tar -xzf "$tmpdir/node.tar.gz" -C /opt
ln -sfn /opt/node-v${NODE_VERSION}-linux-x64 /opt/node
ln -sfn /opt/node/bin/node /usr/local/bin/node
ln -sfn /opt/node/bin/npm /usr/local/bin/npm
rm -rf "$tmpdir"
systemctl enable ssh
ssh-keygen -A
systemctl start ssh
install -d -m 0755 /etc/agent-vm-image
printf '%s\n' ubuntu-24.04-amd64 > /etc/agent-vm-image/base
