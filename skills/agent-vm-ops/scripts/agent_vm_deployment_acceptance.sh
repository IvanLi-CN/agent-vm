#!/usr/bin/env bash
set -Eeuo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
proof_dir="$(mktemp -d /tmp/agent-vm-deployment.XXXXXX)"
rqlited_pid=""
backend_pid=""
caddy_pid=""
rqlite_auth_file=""
rqlite_auth_config=""
keep_proof="${KEEP_PROOF_DIR:-0}"
runtime_home="$proof_dir/home"
runtime_path="/usr/local/bin:/usr/bin:/bin"
cleanup_failed=0
mkdir -p -- "$runtime_home"
script_pgid=""

process_group_for() {
  local pid="$1"
  local group_id
  local child_pid
  group_id="$(ps -o pgid= -p "$pid" 2>/dev/null | tr -d ' ' || true)"
  if [[ "$group_id" == "$script_pgid" ]]; then
    child_pid="$(pgrep -P "$pid" 2>/dev/null | head -n 1 || true)"
    if [[ -n "$child_pid" ]]; then
      group_id="$(ps -o pgid= -p "$child_pid" 2>/dev/null | tr -d ' ' || true)"
    fi
  fi
  if [[ "$group_id" =~ ^[0-9]+$ && "$group_id" != "$script_pgid" ]]; then
    printf '%s\n' "$group_id"
  fi
}

process_group_alive() {
  local group_id="$1"
  [[ -n "$group_id" ]] && kill -0 -- "-$group_id" 2>/dev/null
}

stop_process() {
  local pid="$1"
  local group_id
  [[ -n "$pid" ]] || return 0
  if ! kill -0 "$pid" 2>/dev/null; then
    wait "$pid" 2>/dev/null || true
    return 0
  fi
  group_id="$(process_group_for "$pid")"
  if [[ -n "$group_id" ]]; then
    kill -TERM -- "-$group_id" 2>/dev/null || true
  fi
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do
    if ! kill -0 "$pid" 2>/dev/null && ! process_group_alive "$group_id"; then
      wait "$pid" 2>/dev/null || true
      return 0
    fi
    sleep 0.1
  done
  if [[ -n "$group_id" ]]; then
    kill -KILL -- "-$group_id" 2>/dev/null || true
  fi
  kill -KILL "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do
    if ! kill -0 "$pid" 2>/dev/null && ! process_group_alive "$group_id"; then
      wait "$pid" 2>/dev/null || true
      return 0
    fi
    sleep 0.1
  done
  return 1
}

cleanup() {
  if [[ -n "$caddy_pid" ]]; then
    stop_process "$caddy_pid" || cleanup_failed=1
  fi
  if [[ -n "$backend_pid" ]]; then
    stop_process "$backend_pid" || cleanup_failed=1
  fi
  if [[ -n "$rqlited_pid" ]]; then
    stop_process "$rqlited_pid" || cleanup_failed=1
  fi
  if (( cleanup_failed != 0 )); then
    printf 'cleanup incomplete; live acceptance process tree retained at %s\n' "$proof_dir" >&2
    return 0
  fi
  if [[ -n "$rqlite_auth_file" ]]; then
    rm -f -- "$rqlite_auth_file"
  fi
  if [[ -n "$rqlite_auth_config" ]]; then
    rm -f -- "$rqlite_auth_config"
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
require_command flock
require_command python3
require_command sha256sum
require_command tar
require_command openssl
require_command systemd-analyze
require_command xcaddy
require_command go
require_command setsid
require_command pgrep
require_command ps
script_pgid="$(ps -o pgid= -p "$$" | tr -d ' ')"

static_result="$proof_dir/static.json"
bun run "$repository_root/skills/agent-vm-ops/scripts/validate_blue_green_deployment.ts" >"$static_result" || {
  cat "$static_result" >&2
  fail "static deployment validation failed"
}

caddy_version="${CADDY_VERSION:-}"
[[ -n "$caddy_version" ]] || fail "CADDY_VERSION must be an explicit v2 release selected for this isolated run"
caddy_binary="$proof_dir/caddy"
expected_caddy_sha512_args=()
if [[ -n "${CADDY_SHA512:-}" ]]; then
  expected_caddy_sha512_args=(--expected-sha512 "$CADDY_SHA512")
fi
build_path="$(dirname "$(command -v bun)"):$(dirname "$(command -v xcaddy)"):$(dirname "$(command -v go)"):$runtime_path"
acceptance_goproxy="${AGENT_VM_ACCEPTANCE_GOPROXY:-https://proxy.golang.org,direct}"
env -i \
  PATH="$build_path" \
  HOME="$runtime_home" \
  GOPROXY="$acceptance_goproxy" \
  GOSUMDB=sum.golang.org \
  "$(command -v bun)" run "$repository_root/skills/agent-vm-ops/scripts/build_caddy.ts" \
  --caddy-version "$caddy_version" \
  --target linux/amd64 \
  --output "$caddy_binary" \
  "${expected_caddy_sha512_args[@]}" >"$proof_dir/caddy-build.json"
[[ -x "$caddy_binary" ]] || fail "Caddy custom build was not found"
caddy_build_sha512="$(python3 - "$proof_dir/caddy-build.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    receipt = json.load(handle)
if receipt.get("observed_caddy_version") != receipt.get("requested_version"):
    raise SystemExit("Caddy build receipt version mismatch")
if not receipt.get("sha512") or len(receipt["sha512"]) != 128:
    raise SystemExit("Caddy build receipt has no SHA-512")
print(receipt["sha512"])
PY
)"

ca_dir="$proof_dir/ca"
bash "$repository_root/skills/agent-vm-ops/scripts/generate_deployment_ca.sh" \
  --output-dir "$ca_dir" --hostname deployment-test.local >"$proof_dir/ca.json"

caddy_config="$proof_dir/Caddyfile"
sed \
  -e "s#/var/lib/agent-vm-native/caddy#$proof_dir/caddy-data#g" \
  "$repository_root/skills/agent-vm-ops/assets/blue-green/Caddyfile.example" >"$caddy_config"
mkdir -p -- "$proof_dir/caddy-data"
env -i \
  PATH="$runtime_path" \
  HOME="$runtime_home" \
  XDG_CONFIG_HOME="$proof_dir/xdg-config" \
  XDG_DATA_HOME="$proof_dir/xdg-data" \
  AGENT_VM_DOMAIN="localhost:8443" \
  DNSPOD_TOKEN="isolated-dnspod-token" \
  "$caddy_binary" validate --config "$caddy_config" --adapter caddyfile >"$proof_dir/caddy-validate.json" 2>&1

backend_socket="$proof_dir/backend.sock"
smoke_config="$proof_dir/Caddyfile.smoke"
sed -e "s#/var/lib/agent-vm-native/active/app.sock#$backend_socket#g" "$caddy_config" \
  | awk -v cert="$ca_dir/rqlite/server.crt" -v key="$ca_dir/rqlite/server.key" '
      /^[[:space:]]*tls( force_automate)? \{$/ { print "\ttls " cert " " key; skip_tls=1; next }
      skip_tls && /^[[:space:]]*\}$/ { skip_tls=0; next }
      !skip_tls { print }
    ' >"$smoke_config"
setsid --wait env -i PATH="$runtime_path" HOME="$runtime_home" PYTHONUNBUFFERED=1 \
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
setsid --wait env -i \
  PATH="$runtime_path" \
  HOME="$runtime_home" \
  XDG_CONFIG_HOME="$proof_dir/xdg-config" \
  XDG_DATA_HOME="$proof_dir/xdg-data" \
  AGENT_VM_DOMAIN="localhost:$caddy_port" \
  DNSPOD_TOKEN="isolated-dnspod-token" \
  "$caddy_binary" run --config "$smoke_config" --adapter caddyfile >"$proof_dir/caddy.log" 2>&1 &
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
stop_process "$caddy_pid"
caddy_pid=""
stop_process "$backend_pid"
backend_pid=""

unit_dir="$repository_root/skills/agent-vm-ops/assets/systemd"
systemd_root="$proof_dir/systemd-root"
mkdir -p -- \
  "$systemd_root/etc/systemd/system" \
  "$systemd_root/etc/agent-vm-native/rqlite" \
  "$systemd_root/etc/agent-vm-native/slots/test_instance" \
  "$systemd_root/opt/agent-vm-native/slots/test_instance/bin" \
  "$systemd_root/bin" \
  "$systemd_root/usr/bin" \
  "$systemd_root/usr/local/bin"
cp "$unit_dir/agent-vm-rqlite.service" "$unit_dir/agent-vm-native@.service" "$unit_dir/agent-vm-caddy.service" "$systemd_root/etc/systemd/system/"
touch \
  "$systemd_root/etc/agent-vm-native/Caddyfile" \
  "$systemd_root/etc/agent-vm-native/caddy.env" \
  "$systemd_root/etc/agent-vm-native/rqlite/auth.json" \
  "$systemd_root/etc/agent-vm-native/slots/test_instance/config.json" \
  "$systemd_root/opt/agent-vm-native/release-manifest.json" \
  "$systemd_root/opt/agent-vm-native/slots/test_instance/release-manifest.json"
chmod 0600 "$systemd_root/etc/agent-vm-native/rqlite/auth.json"
touch \
  "$systemd_root/bin/sh" \
  "$systemd_root/usr/bin/curl" \
  "$systemd_root/usr/bin/sleep" \
  "$systemd_root/usr/bin/stat" \
  "$systemd_root/usr/bin/test" \
  "$systemd_root/usr/local/bin/caddy" \
  "$systemd_root/usr/local/bin/rqlited" \
  "$systemd_root/opt/agent-vm-native/slots/test_instance/bin/agent-vm-control"
chmod 0755 \
  "$systemd_root/bin/sh" \
  "$systemd_root/usr/bin/curl" \
  "$systemd_root/usr/bin/sleep" \
  "$systemd_root/usr/bin/stat" \
  "$systemd_root/usr/bin/test" \
  "$systemd_root/usr/local/bin/caddy" \
  "$systemd_root/usr/local/bin/rqlited" \
  "$systemd_root/opt/agent-vm-native/slots/test_instance/bin/agent-vm-control"
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
cat >"$systemd_root/etc/systemd/system/network-online.target" <<'EOF'
[Unit]
Description=Isolated network readiness target
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
rqlite_arm64_archive="$proof_dir/rqlite-arm64.tar.gz"
rqlite_arm64_url="https://github.com/rqlite/rqlite/releases/download/v10.5.2/rqlite-v10.5.2-linux-arm64.tar.gz"
rqlite_arm64_sha256="44b4bd7ca39720dee3b916f9ef59da117c07ddc008704ac25e56385e56951b8a"
curl --fail --silent --show-error --location --retry 3 --output "$rqlite_arm64_archive" "$rqlite_arm64_url"
printf '%s  %s\n' "$rqlite_arm64_sha256" "$rqlite_arm64_archive" | sha256sum --check --status - || fail "rqlite arm64 checksum failed"
mkdir -p -- "$proof_dir/rqlite"
tar --extract --gzip --file "$rqlite_archive" --directory "$proof_dir/rqlite"
rqlited="$proof_dir/rqlite/rqlite-v10.5.2-linux-amd64/rqlited"
[[ -x "$rqlited" ]] || fail "rqlited binary was not found"
rqlite_version="$("$rqlited" -version 2>&1 || true)"
grep -q '10.5.2' <<<"$rqlite_version" || fail "rqlite version is not 10.5.2"

control_secret="isolated-control-${BASHPID}-secret"
backup_secret="isolated-backup-${BASHPID}-secret"
rqlite_auth_file="$proof_dir/auth.json"
(
  umask 077
  cat >"$rqlite_auth_file" <<EOF
[
  {"username":"agent-vm-control","password":"$control_secret","perms":["execute","query","ready","status"]},
  {"username":"agent-vm-backup","password":"$backup_secret","perms":["backup","query","ready","status"]}
]
EOF
)
chmod 0600 "$rqlite_auth_file"
http_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
raft_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
rqlite_data="$proof_dir/rqlite-data"
mkdir -p -- "$rqlite_data"
setsid --wait env -i PATH="$runtime_path" HOME="$runtime_home" \
  "$rqlited" \
  -node-id isolated-agent-vm \
  -http-addr "127.0.0.1:$http_port" \
  -raft-addr "127.0.0.1:$raft_port" \
  -auth "$rqlite_auth_file" \
  -http-cert "$ca_dir/rqlite/server.crt" \
  -http-key "$ca_dir/rqlite/server.key" \
  "$rqlite_data" >"$proof_dir/rqlite.log" 2>&1 &
rqlited_pid=$!
rqlite_base="https://127.0.0.1:$http_port"
rqlite_auth_config="$proof_dir/rqlite-curl.conf"
(
  umask 077
  printf 'user = "agent-vm-backup:%s"\n' "$backup_secret" >"$rqlite_auth_config"
)
rqlite_ready=0
for _ in $(seq 1 120); do
  if ! kill -0 "$rqlited_pid" 2>/dev/null; then
    cat "$proof_dir/rqlite.log" >&2
    fail "rqlited exited before becoming ready"
  fi
  if curl --fail --silent --show-error --cacert "$ca_dir/ca.crt" \
    --config "$rqlite_auth_config" "$rqlite_base/status" >/dev/null 2>&1; then
    rqlite_ready=1
    break
  fi
  sleep 0.25
done
(( rqlite_ready == 1 )) || { cat "$proof_dir/rqlite.log" >&2; fail "rqlite did not become ready"; }
rm -f -- "$rqlite_auth_config"
rqlite_auth_config=""
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
INSERT INTO meta(key, value) VALUES ('state_epoch', 'epoch-1');
CREATE TABLE tasks (session_id TEXT NOT NULL, generation INTEGER NOT NULL);
INSERT INTO tasks(session_id, generation) VALUES ('session-1', 7);
""")
connection.commit()
connection.close()
PY
evidence_private_key="$proof_dir/evidence-private.pem"
evidence_public_key="$proof_dir/evidence-public.pem"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$evidence_private_key" >/dev/null 2>&1
openssl pkey -in "$evidence_private_key" -pubout -out "$evidence_public_key" >/dev/null 2>&1
chmod 0600 "$evidence_private_key" "$evidence_public_key"

write_evidence_payload() {
  local output_path="$1"
  local evidence_id="$2"
  local workers_quiescent="$3"
  local source_sha256="$4"
  local source_size="$5"
  local trusted_commit="$6"
  local state_epoch="$7"
  local state_generation="$8"
  local tail_verified="$9"
  local backup_sha256="${10}"
  python3 - "$output_path" "$evidence_id" "$workers_quiescent" "$source_sha256" "$source_size" "$trusted_commit" "$state_epoch" "$state_generation" "$tail_verified" "$backup_sha256" <<'PY'
import json
import sys
from datetime import datetime, timedelta, timezone

output, evidence_id, workers, source_sha256, source_size, trusted_commit, state_epoch, state_generation, tail_verified, backup_sha256 = sys.argv[1:]
now = datetime.now(timezone.utc)
payload = {
    "schema_version": 1,
    "producer": "agent-vm-control",
    "evidence_id": evidence_id,
    "state_write_fenced": True,
    "workers_quiescent": workers == "true",
    "sqlite_writers_closed": True,
    "release_fence_path": "/run/agent-vm-native/release-activation.fence",
    "fence_generation": 1,
    "state_epoch": state_epoch,
    "state_generation": state_generation,
    "trusted_commit": trusted_commit,
    "observed_at": now.isoformat().replace("+00:00", "Z"),
    "expires_at": (now + timedelta(hours=1)).isoformat().replace("+00:00", "Z"),
}
if source_sha256 != "-":
    payload["source_sha256"] = source_sha256
    payload["source_size"] = int(source_size)
if tail_verified != "-":
    payload["tail_verified"] = tail_verified == "true"
    payload["coverage_boundary_verified"] = True
    payload["last_verified_commit"] = trusted_commit
    payload["backup_sha256"] = backup_sha256
with open(output, "w", encoding="utf-8") as handle:
    json.dump(payload, handle, sort_keys=True, separators=(",", ":"))
PY
}

sign_evidence() {
  local payload_path="$1"
  local evidence_path="$2"
  local canonical_path="${payload_path}.canonical"
  local signature_path="${payload_path}.sig"
  cp "$payload_path" "$canonical_path"
  openssl dgst -sha256 -sign "$evidence_private_key" -out "$signature_path" "$canonical_path"
  local signature
  signature="$(openssl base64 -A -in "$signature_path")"
  python3 - "$payload_path" "$evidence_path" "$signature" <<'PY'
import json
import sys

payload_path, evidence_path, signature = sys.argv[1:]
with open(payload_path, encoding="utf-8") as handle:
    payload = json.load(handle)
with open(evidence_path, "w", encoding="utf-8") as handle:
    json.dump({"payload": payload, "signature": signature}, handle, sort_keys=True, separators=(",", ":"))
PY
  chmod 0600 "$evidence_path"
}

sqlite_source_sha256="$(sha256sum "$sqlite_source" | awk '{print $1}')"
sqlite_source_size="$(stat -c '%s' "$sqlite_source")"
state_generation="6728b2001bc842af236929770c506c5dca42faca834859e693d7b9ff7d969cf6"
invalid_evidence="$proof_dir/invalid-evidence.json"
write_evidence_payload "$proof_dir/invalid-payload.json" invalid-deployment-0001 false "$sqlite_source_sha256" "$sqlite_source_size" commit-1 epoch-1 "$state_generation" - -
sign_evidence "$proof_dir/invalid-payload.json" "$invalid_evidence"
if bun run "$repository_root/skills/agent-vm-ops/scripts/agent_vm_state_migration.ts" \
  preflight --source "$sqlite_source" --evidence "$invalid_evidence" --evidence-public-key "$evidence_public_key" --output-dir "$proof_dir/invalid-output" >/dev/null 2>&1; then
  fail "migration preflight accepted incomplete fence evidence"
fi
evidence="$proof_dir/evidence.json"
write_evidence_payload "$proof_dir/evidence-payload.json" isolated-deployment-0001 true "$sqlite_source_sha256" "$sqlite_source_size" commit-1 epoch-1 "$state_generation" - -
sign_evidence "$proof_dir/evidence-payload.json" "$evidence"
migration_dir="$proof_dir/migration"
bun run "$repository_root/skills/agent-vm-ops/scripts/agent_vm_state_migration.ts" \
  preflight --source "$sqlite_source" --evidence "$evidence" --evidence-public-key "$evidence_public_key" --output-dir "$migration_dir" >"$proof_dir/migration.json"
backup_sha256="$(python3 - "$migration_dir/snapshot-manifest.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["backup_sha256"])
PY
)"
tail_evidence="$proof_dir/tail-evidence.json"
write_evidence_payload "$proof_dir/tail-payload.json" isolated-deployment-0002 true - - commit-1 epoch-1 "$state_generation" true "$backup_sha256"
sign_evidence "$proof_dir/tail-payload.json" "$tail_evidence"
bun run "$repository_root/skills/agent-vm-ops/scripts/agent_vm_state_migration.ts" \
  restore-check --backup-manifest "$migration_dir/snapshot-manifest.json" \
  --tail-evidence "$tail_evidence" --evidence-public-key "$evidence_public_key" --output "$proof_dir/restore.json" >"$proof_dir/restore-result.json"

printf '{"schema_version":1,"verdict":"pass","caddy_major":2,"caddy_build":"operator-selected-v2","caddy_sha512":"%s","rqlite":"10.5.2","checks":["custom Caddy DNSPod build","rqlite artifact checksums","CA and rqlite leaf certificate","Caddy configuration","systemd units","authenticated TLS rqlite","blue-green cutover guard","evidence-gated snapshot","restore admissibility"]}\n' "$caddy_build_sha512"
