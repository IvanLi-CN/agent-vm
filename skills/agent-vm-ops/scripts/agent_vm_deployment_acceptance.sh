#!/usr/bin/env bash
set -Eeuo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
proof_dir="$(mktemp -d /tmp/agent-vm-deployment.XXXXXX)"
rqlited_pid=""
backend_pid=""
caddy_pid=""
keep_proof="${KEEP_PROOF_DIR:-0}"

cleanup() {
  if [[ -n "$caddy_pid" ]]; then
    kill -TERM "$caddy_pid" 2>/dev/null || true
    wait "$caddy_pid" 2>/dev/null || true
  fi
  if [[ -n "$backend_pid" ]]; then
    kill -TERM "$backend_pid" 2>/dev/null || true
    wait "$backend_pid" 2>/dev/null || true
  fi
  if [[ -n "$rqlited_pid" ]]; then
    kill -TERM "$rqlited_pid" 2>/dev/null || true
    wait "$rqlited_pid" 2>/dev/null || true
  fi
  if [[ "$keep_proof" != 1 ]]; then
    rm -rf -- "$proof_dir"
  else
    printf 'proof_dir=%s\n' "$proof_dir" >&2
  fi
}
trap cleanup EXIT

fail() {
  echo "deployment acceptance: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

require_command bun
require_command curl
require_command sha256sum
require_command sha512sum
require_command tar
require_command openssl
require_command systemd-analyze

static_result="$proof_dir/static.json"
bun run "$repository_root/skills/agent-vm-ops/scripts/validate_blue_green_deployment.ts" >"$static_result" || {
  cat "$static_result" >&2
  fail "static deployment validation failed"
}

caddy_archive="$proof_dir/caddy.tar.gz"
caddy_url="https://github.com/caddyserver/caddy/releases/download/v2.11.7/caddy_2.11.7_linux_amd64.tar.gz"
caddy_sha512="a7a433a1b133efc3c8d10eb0b99d52a24b5ef5c322dc77f5282182b1c0402139ab83f3a99f0c52409df77d20123fb0b523edad8a66d8f5e49136197bf61ef0e7"
curl --fail --silent --show-error --location --retry 3 --output "$caddy_archive" "$caddy_url"
printf '%s  %s\n' "$caddy_sha512" "$caddy_archive" | sha512sum --check --status - || fail "Caddy checksum failed"
mkdir -p -- "$proof_dir/caddy"
tar --extract --gzip --file "$caddy_archive" --directory "$proof_dir/caddy"
caddy_binary="$proof_dir/caddy/caddy"
[[ -x "$caddy_binary" ]] || fail "Caddy binary was not found"
"$caddy_binary" version | grep -q '2.11.7' || fail "Caddy version is not 2.11.7"

ca_dir="$proof_dir/ca"
bash "$repository_root/skills/agent-vm-ops/scripts/generate_deployment_ca.sh" \
  --output-dir "$ca_dir" --hostname deployment-test.local >"$proof_dir/ca.json"

caddy_config="$proof_dir/Caddyfile"
sed \
  -e "s#/etc/agent-vm-native/tls/caddy/server.crt#$ca_dir/caddy/server.crt#g" \
  -e "s#/etc/agent-vm-native/tls/caddy/server.key#$ca_dir/caddy/server.key#g" \
  "$repository_root/skills/agent-vm-ops/assets/blue-green/Caddyfile.example" >"$caddy_config"
AGENT_VM_DOMAIN="localhost:8443" "$caddy_binary" validate --config "$caddy_config" --adapter caddyfile >"$proof_dir/caddy-validate.json" 2>&1

backend_socket="$proof_dir/backend.sock"
smoke_config="$proof_dir/Caddyfile.smoke"
sed -e "s#/var/lib/agent-vm-native/active/app.sock#$backend_socket#g" "$caddy_config" >"$smoke_config"
python3 - "$backend_socket" >"$proof_dir/backend.log" 2>&1 <<'PY' &
import http.server
import os
import socketserver
import sys

socket_path = sys.argv[1]
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        payload = b'{"backend":"healthy"}\n'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
    def log_message(self, format, *args):
        pass

class Server(socketserver.UnixStreamServer):
    allow_reuse_address = True

try:
    os.unlink(socket_path)
except FileNotFoundError:
    pass
with Server(socket_path, Handler) as server:
    server.serve_forever()
PY
backend_pid=$!
for _ in $(seq 1 40); do
  [[ -S "$backend_socket" ]] && break
  sleep 0.05
done
[[ -S "$backend_socket" ]] || fail "Unix backend did not start"
caddy_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
AGENT_VM_DOMAIN="localhost:$caddy_port" "$caddy_binary" run --config "$smoke_config" --adapter caddyfile >"$proof_dir/caddy.log" 2>&1 &
caddy_pid=$!
auth_header_token="isolated-${BASHPID}-token"
query_marker="isolated-${BASHPID}-query"
smoke_ready=0
for _ in $(seq 1 80); do
  if ! kill -0 "$caddy_pid" 2>/dev/null; then
    cat "$proof_dir/caddy.log" >&2
    fail "Caddy exited before the smoke request"
  fi
  if curl --fail --silent --show-error --cacert "$ca_dir/ca.crt" \
    -H "Authorization: Bearer $auth_header_token" \
    "https://localhost:$caddy_port/health?secret=$query_marker" | grep -q 'backend'; then
    smoke_ready=1
    break
  fi
  sleep 0.1
done
(( smoke_ready == 1 )) || { cat "$proof_dir/caddy.log" >&2; fail "Caddy Unix-socket smoke request failed"; }
blocked_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --cacert "$ca_dir/ca.crt" "https://localhost:$caddy_port/internal/v1/health")"
[[ "$blocked_code" == 404 ]] || fail "Caddy exposed an internal endpoint"
if grep -q "$query_marker\|$auth_header_token" "$proof_dir/caddy.log"; then
  fail "Caddy access log retained sensitive request data"
fi
kill -TERM "$caddy_pid" 2>/dev/null || true
wait "$caddy_pid" 2>/dev/null || true
caddy_pid=""
kill -TERM "$backend_pid" 2>/dev/null || true
wait "$backend_pid" 2>/dev/null || true
backend_pid=""

unit_dir="$repository_root/skills/agent-vm-ops/assets/systemd"
systemd_root="$proof_dir/systemd-root"
mkdir -p -- \
  "$systemd_root/etc/systemd/system" \
  "$systemd_root/etc/agent-vm-native/slots/test_instance" \
  "$systemd_root/opt/agent-vm-native/slots/test_instance/bin" \
  "$systemd_root/usr/local/bin"
cp "$unit_dir/agent-vm-rqlite.service" "$unit_dir/agent-vm-native@.service" "$unit_dir/agent-vm-caddy.service" "$systemd_root/etc/systemd/system/"
touch \
  "$systemd_root/etc/agent-vm-native/Caddyfile" \
  "$systemd_root/etc/agent-vm-native/slots/test_instance/config.json" \
  "$systemd_root/opt/agent-vm-native/release-manifest.json" \
  "$systemd_root/opt/agent-vm-native/slots/test_instance/release-manifest.json"
touch "$systemd_root/usr/local/bin/caddy" "$systemd_root/usr/local/bin/rqlited" "$systemd_root/opt/agent-vm-native/slots/test_instance/bin/agent-vm-control"
chmod 0755 "$systemd_root/usr/local/bin/caddy" "$systemd_root/usr/local/bin/rqlited" "$systemd_root/opt/agent-vm-native/slots/test_instance/bin/agent-vm-control"
printf '%s\n' \
  'root:x:0:0:root:/root:/bin/sh' \
  'caddy:x:995:995:caddy:/var/lib/caddy:/usr/sbin/nologin' \
  'agent-vm-rqlite:x:994:994:rqlite:/var/lib/agent-vm-native/rqlite:/usr/sbin/nologin' \
  'agent-vm-web:x:993:993:web:/var/lib/agent-vm-native:/usr/sbin/nologin' \
  >"$systemd_root/etc/passwd"
printf '%s\n' \
  'root:x:0:' \
  'caddy:x:995:' \
  'agent-vm-rqlite:x:994:' \
  'agent-vm-web:x:993:' \
  >"$systemd_root/etc/group"
cat >"$systemd_root/etc/systemd/system/sysinit.target" <<'EOF'
[Unit]
Description=Isolated verification target
EOF
systemd-analyze --root="$systemd_root" verify \
  agent-vm-rqlite.service \
  agent-vm-native@test_instance.service \
  agent-vm-caddy.service \
  >"$proof_dir/systemd-verify.txt" 2>&1 || {
      cat "$proof_dir/systemd-verify.txt" >&2
      fail "systemd unit verification failed"
    }

rqlite_archive="$proof_dir/rqlite.tar.gz"
rqlite_url="https://github.com/rqlite/rqlite/releases/download/v10.5.2/rqlite-v10.5.2-linux-amd64.tar.gz"
rqlite_sha256="1a04a7c2c542e8e1c1347bd31af0fa9ef7043febb3c4d1806d36bc1702c6fa0f"
curl --fail --silent --show-error --location --retry 3 --output "$rqlite_archive" "$rqlite_url"
printf '%s  %s\n' "$rqlite_sha256" "$rqlite_archive" | sha256sum --check --status - || fail "rqlite checksum failed"
mkdir -p -- "$proof_dir/rqlite"
tar --extract --gzip --file "$rqlite_archive" --directory "$proof_dir/rqlite"
rqlited="$proof_dir/rqlite/rqlite-v10.5.2-linux-amd64/rqlited"
[[ -x "$rqlited" ]] || fail "rqlited binary was not found"
rqlite_version="$("$rqlited" -version 2>&1 || true)"
grep -q '10.5.2' <<<"$rqlite_version" || fail "rqlite version is not 10.5.2"

control_secret="isolated-control-${BASHPID}-secret"
backup_secret="isolated-backup-${BASHPID}-secret"
cat >"$proof_dir/auth.json" <<EOF
[
  {"username":"agent-vm-control","password":"$control_secret","perms":["execute","query","ready","status"]},
  {"username":"agent-vm-backup","password":"$backup_secret","perms":["backup","query","ready","status"]}
]
EOF
http_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
raft_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
rqlite_data="$proof_dir/rqlite-data"
mkdir -p -- "$rqlite_data"
"$rqlited" \
  -node-id isolated-agent-vm \
  -http-addr "127.0.0.1:$http_port" \
  -raft-addr "127.0.0.1:$raft_port" \
  -auth "$proof_dir/auth.json" \
  -http-cert "$ca_dir/rqlite/server.crt" \
  -http-key "$ca_dir/rqlite/server.key" \
  "$rqlite_data" >"$proof_dir/rqlite.log" 2>&1 &
rqlited_pid=$!
rqlite_base="https://127.0.0.1:$http_port"
rqlite_ready=0
for _ in $(seq 1 120); do
  if ! kill -0 "$rqlited_pid" 2>/dev/null; then
    cat "$proof_dir/rqlite.log" >&2
    fail "rqlited exited before becoming ready"
  fi
  if curl --fail --silent --show-error --cacert "$ca_dir/ca.crt" \
    --user "agent-vm-backup:$backup_secret" "$rqlite_base/status" >/dev/null 2>&1; then
    rqlite_ready=1
    break
  fi
  sleep 0.25
done
(( rqlite_ready == 1 )) || { cat "$proof_dir/rqlite.log" >&2; fail "rqlite did not become ready"; }
if curl --fail --silent --cacert "$ca_dir/ca.crt" "$rqlite_base/status" >/dev/null 2>&1; then
  fail "rqlite accepted an unauthenticated request"
fi

layout_root="$proof_dir/layout"
runtime_root="$proof_dir/runtime"
mkdir -p -- "$layout_root/slots/blue" "$layout_root/slots/green" "$runtime_root/blue" "$runtime_root/green"
printf '{"slot":"blue"}\n' >"$layout_root/slots/blue/release-manifest.json"
printf '{"slot":"green"}\n' >"$layout_root/slots/green/release-manifest.json"
ln -s "$runtime_root/blue/app.sock" "$layout_root/slots/blue/app.sock"
ln -s "$runtime_root/green/app.sock" "$layout_root/slots/green/app.sock"
bash "$repository_root/skills/agent-vm-ops/scripts/agent_vm_cutover.sh" \
  --root "$layout_root" --runtime-root "$runtime_root" --slot green --dry-run >"$proof_dir/cutover.json"
ln -s "$layout_root/slots/blue" "$layout_root/active"
if bash "$repository_root/skills/agent-vm-ops/scripts/agent_vm_cutover.sh" \
  --root "$layout_root" --runtime-root "$runtime_root" --slot red --dry-run >/dev/null 2>&1; then
  fail "cutover accepted an unknown slot"
fi

sqlite_source="$proof_dir/source.sqlite3"
python3 - "$sqlite_source" <<'PY'
import sqlite3
import sys
connection = sqlite3.connect(sys.argv[1])
connection.executescript("""
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO meta(key, value) VALUES ('state_epoch', 'epoch-1'), ('generation', '7');
""")
connection.commit()
connection.close()
PY
invalid_evidence="$proof_dir/invalid-evidence.json"
printf '{"evidence_id":"invalid","state_write_fenced":true,"workers_quiescent":false,"sqlite_writers_closed":true}\n' >"$invalid_evidence"
if bun run "$repository_root/skills/agent-vm-ops/scripts/agent_vm_state_migration.ts" \
  preflight --source "$sqlite_source" --evidence "$invalid_evidence" --output-dir "$proof_dir/invalid-output" >/dev/null 2>&1; then
  fail "migration preflight accepted incomplete fence evidence"
fi
evidence="$proof_dir/evidence.json"
printf '{"evidence_id":"isolated-deployment-0001","trusted_commit":"commit-1","state_write_fenced":true,"workers_quiescent":true,"sqlite_writers_closed":true}\n' >"$evidence"
migration_dir="$proof_dir/migration"
bun run "$repository_root/skills/agent-vm-ops/scripts/agent_vm_state_migration.ts" \
  preflight --source "$sqlite_source" --evidence "$evidence" --output-dir "$migration_dir" >"$proof_dir/migration.json"
tail_evidence="$proof_dir/tail-evidence.json"
printf '{"evidence_id":"isolated-deployment-0002","tail_verified":true,"last_verified_commit":"commit-1","state_write_fenced":true,"workers_quiescent":true,"sqlite_writers_closed":true}\n' >"$tail_evidence"
bun run "$repository_root/skills/agent-vm-ops/scripts/agent_vm_state_migration.ts" \
  restore-check --backup-manifest "$migration_dir/snapshot-manifest.json" \
  --tail-evidence "$tail_evidence" --output "$proof_dir/restore.json" >"$proof_dir/restore-result.json"

printf '{"schema_version":1,"verdict":"pass","caddy":"2.11.7","rqlite":"10.5.2","checks":["pinned artifact checksums","CA and leaf certificates","Caddy configuration","systemd units","authenticated TLS rqlite","blue-green cutover guard","evidence-gated snapshot","restore admissibility"]}\n'
