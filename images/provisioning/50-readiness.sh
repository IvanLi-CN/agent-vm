#!/usr/bin/env bash
set -euo pipefail
test -s /etc/ssh/ssh_host_ed25519_key
systemctl is-active --quiet ssh
systemctl is-active --quiet docker
command -v bun node npm git rsync python3 psql pg_dump pg_restore mysql mysqldump growpart resize2fs xfs_growfs lsblk timeout
docker compose version
bun --version | grep -Fx "$BUN_VERSION"
node --version | grep -Fx "v$NODE_VERSION"
case "$IMAGE_PROFILE" in
 rust) rustc --version | grep -F "rustc $RUST_VERSION "; cargo --version ;;
 java) java -version 2>&1 | grep -F "$ZULU_VERSION"; javac -version ;;
 go) go version | grep -F "go$GO_VERSION " ;;
esac
jq -n --slurpfile docker /etc/agent-vm-image/docker-readiness.json '{ready:true,ssh_ed25519:true,database_clients:true,docker:$docker[0]}' > /etc/agent-vm-image/readiness.json
dpkg-query -W -f='${Package}\t${Version}\n' | python3 -c 'import sys,json;print(json.dumps({"schema":1,"packages":[dict(zip(["name","version"],line.rstrip().split("\t",1))) for line in sys.stdin]},sort_keys=True))' > /etc/agent-vm-image/sbom.json
