#!/usr/bin/env bash
set -Eeuo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
control_root="$repository_root/skills/agent-vm-ops/control"
test_root="$(mktemp -d /tmp/agent-vm-deployd-systemd.XXXXXX)"
keep_proof="${KEEP_PROOF_DIR:-0}"
declare -A path_backup
declare -A unit_backup
declare -A unit_was_active
group_created=0

test_paths=(
  /etc/agent-vm-native
  /opt/agent-vm-native
  /var/lib/agent-vm-native
  /run/agent-vm-native
)
managed_units=(
  agent-vm-deployd.service
  agent-vm-native@.service
  agent-vm-rqlite.service
)
runtime_units=(
  agent-vm-deployd.service
  agent-vm-native.service
  agent-vm-native@blue.service
  agent-vm-native@green.service
  agent-vm-rqlite.service
  agent-vm-caddy.service
  agent-vm-web.service
)

fail() {
  echo "Deployd systemd adapter acceptance: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

backup_name() {
  local value="$1"
  value="${value#/}"
  value="${value//\//__}"
  printf '%s/%s\n' "$test_root/original" "$value"
}

backup_path() {
  local path="$1"
  local backup
  backup="$(backup_name "$path")"
  path_backup["$path"]="$backup"
  if [[ -e "$path" || -L "$path" ]]; then
    mv -- "$path" "$backup"
  fi
}

backup_unit() {
  local unit="$1"
  local path="/etc/systemd/system/$unit"
  local backup
  backup="$(backup_name "$path")"
  unit_backup["$unit"]="$backup"
  if [[ -e "$path" || -L "$path" ]]; then
    mv -- "$path" "$backup"
  fi
}

restore_environment() {
  local main_status="${1:-0}"
  local restore_status=0
  local unit path backup
  set +e +u
  for unit in "${runtime_units[@]}"; do
    if ! systemctl stop "$unit" >/dev/null 2>&1; then
      restore_status=1
    fi
    if systemctl is-active --quiet "$unit"; then
      restore_status=1
    fi
  done
  if [[ "$restore_status" -eq 0 ]]; then
    for path in "${test_paths[@]}"; do
      if [[ -e "$path" || -L "$path" ]]; then
        rm -rf -- "$path"
        if [[ -e "$path" || -L "$path" ]]; then
          restore_status=1
          continue
        fi
      fi
      backup="${path_backup[$path]:-}"
      if [[ -n "$backup" && ( -e "$backup" || -L "$backup" ) ]]; then
        mkdir -p -- "$(dirname "$path")" || restore_status=1
        mv -- "$backup" "$path" || restore_status=1
      fi
    done
    for unit in "${managed_units[@]}"; do
      path="/etc/systemd/system/$unit"
      if [[ -e "$path" || -L "$path" ]]; then
        rm -f -- "$path" || restore_status=1
      fi
      backup="${unit_backup[$unit]:-}"
      if [[ -n "$backup" && ( -e "$backup" || -L "$backup" ) ]]; then
        mv -- "$backup" "$path" || restore_status=1
      fi
    done
    if ! systemctl daemon-reload >/dev/null 2>&1; then
      restore_status=1
    fi
    for unit in "${runtime_units[@]}"; do
      if [[ "${unit_was_active[$unit]:-0}" == 1 ]]; then
        if ! systemctl start "$unit" >/dev/null 2>&1 || ! systemctl is-active --quiet "$unit"; then
          restore_status=1
        fi
      fi
    done
    if [[ "$group_created" == 1 ]] && ! groupdel agent-vm-web >/dev/null 2>&1; then
      restore_status=1
    fi
  else
    echo "Deployd systemd adapter acceptance: refusing filesystem restore while a managed unit remains active" >&2
  fi
  if [[ "$restore_status" -ne 0 || "$main_status" -ne 0 ]]; then
    keep_proof=1
  fi
  if [[ "$keep_proof" == 1 ]]; then
    printf 'proof_dir=%s\n' "$test_root" >&2
  else
    if ! rm -rf -- "$test_root"; then
      restore_status=1
    fi
  fi
  if [[ "$restore_status" -ne 0 ]]; then
    return 1
  fi
  return "$main_status"
}

on_exit() {
  local main_status="$?"
  restore_environment "$main_status"
  local restore_status="$?"
  trap - EXIT
  if [[ "$restore_status" -ne 0 ]]; then
    exit "$restore_status"
  fi
  exit "$main_status"
}
trap on_exit EXIT

[[ "${EUID:-$(id -u)}" == 0 ]] || fail "real systemd acceptance requires root in the session-owned Agent VM"
for command in bun curl go git python3 sha256sum systemctl tar; do
  require_command "$command"
done
[[ "${AGENT_VM_REAL_SYSTEMD:-0}" == 1 ]] || fail "set AGENT_VM_REAL_SYSTEMD=1 to run the real systemd acceptance"

mkdir -p -- "$test_root/original"
for unit in "${runtime_units[@]}"; do
  if systemctl is-active --quiet "$unit"; then
    unit_was_active["$unit"]=1
  else
    unit_was_active["$unit"]=0
  fi
done
for unit in "${runtime_units[@]}"; do
  systemctl stop "$unit" >/dev/null 2>&1 || fail "could not stop pre-existing unit $unit"
  if systemctl is-active --quiet "$unit"; then
    fail "pre-existing unit $unit remained active after stop"
  fi
done
for unit in "${managed_units[@]}"; do
  backup_unit "$unit"
done
for path in "${test_paths[@]}"; do
  backup_path "$path"
done

if ! getent group agent-vm-web >/dev/null 2>&1; then
  groupadd --system agent-vm-web
  group_created=1
fi

fake_slot="$test_root/testslot"
go -C "$control_root" build -o "$fake_slot" ./internal/deployd/testslot

source_revision="$(git -C "$repository_root" rev-parse HEAD)"
go_arch="$(go env GOARCH)"
build_bundle="$test_root/built.tar"
bun run "$repository_root/skills/agent-vm-ops/scripts/build_go_release.ts" \
  --source-revision "$source_revision" \
  --target "linux/$go_arch" \
  --transport go-request \
  --output "$build_bundle" >"$test_root/build.json"

bundle_root="$test_root/bundle"
mkdir -p -- "$bundle_root"
tar -xf "$build_bundle" -C "$bundle_root"
real_control_binary="$test_root/real-agent-vm-control"
cp -- "$bundle_root/bin/agent-vm-control" "$real_control_binary"
chmod 0755 "$real_control_binary"
"$real_control_binary" version >/dev/null || fail "the real agent-vm-control binary did not pass its version smoke test"
cp -- "$fake_slot" "$bundle_root/bin/agent-vm-control"
chmod 0755 "$bundle_root/bin/agent-vm-control"
python3 - "$bundle_root/release-manifest.json" "$bundle_root/bin/agent-vm-control" <<'PY'
import hashlib
import json
import sys

manifest_path, control_path = sys.argv[1:]
with open(manifest_path, encoding="utf-8") as handle:
    manifest = json.load(handle)
digest = hashlib.sha256(open(control_path, "rb").read()).hexdigest()
for artifact in manifest["artifacts"]:
    if artifact["path"] == "bin/agent-vm-control":
        artifact["sha256"] = digest
        break
else:
    raise SystemExit("control artifact is missing from the release manifest")
with open(manifest_path, "w", encoding="utf-8") as handle:
    json.dump(manifest, handle, sort_keys=True, indent=2)
    handle.write("\n")
PY
real_bundle="$test_root/acceptance.tar"
bundle_members=(
  release-manifest.json client-release.json
  agent_vm.ts byte-fs.ts client-io.ts client-release.ts client.ts guest-source.ts sync-native.ts sync_worktree.ts
  agent_vm_control_bridge.ts
  bin/agent-vm-control bin/agent-vm-deployd bin/agent-vm-deploy
  systemd/agent-vm-native.service systemd/agent-vm-native@.service systemd/agent-vm-deployd.service
)
tar -cf "$real_bundle" -C "$bundle_root" "${bundle_members[@]}"

install -d -m 0750 /etc/agent-vm-native/slots /opt/agent-vm-native/bin \
  /var/lib/agent-vm-native/slots /var/lib/agent-vm-native/deployment \
  /run/agent-vm-native/blue /run/agent-vm-native/green
cp -- "$bundle_root/bin/agent-vm-deployd" /opt/agent-vm-native/bin/agent-vm-deployd
cp -- "$bundle_root/bin/agent-vm-deploy" /opt/agent-vm-native/bin/agent-vm-deploy
chmod 0755 /opt/agent-vm-native/bin/agent-vm-deployd /opt/agent-vm-native/bin/agent-vm-deploy

for slot in blue green; do
  slot_root="/var/lib/agent-vm-native/slots/$slot"
  mkdir -p -- "$slot_root"
  tar -xf "$real_bundle" -C "$slot_root"
  ln -s "/run/agent-vm-native/$slot/app.sock" "$slot_root/app.sock"
  cat >"/etc/agent-vm-native/slots/$slot/config.json" <<EOF
{"slot":"$slot","socket":"/run/agent-vm-native/$slot/app.sock"}
EOF
done
ln -s /var/lib/agent-vm-native/slots/blue /var/lib/agent-vm-native/active

metrics_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
cat > /etc/agent-vm-native/deployd.json <<EOF
{
  "socket": "/run/agent-vm-native/deployd.sock",
  "metrics_listen": "127.0.0.1:$metrics_port",
  "database": "/var/lib/agent-vm-native/deployment/deployment.sqlite3",
  "active_path": "/var/lib/agent-vm-native/active",
  "release_root": "/var/lib/agent-vm-native",
  "blue_socket": "/run/agent-vm-native/blue/app.sock",
  "green_socket": "/run/agent-vm-native/green/app.sock",
  "lease_seconds": 15,
  "retention_days": 30,
  "observe_seconds": 1
}
EOF

cp -- "$repository_root/deploy/assets/systemd/agent-vm-native@.service" \
  /etc/systemd/system/agent-vm-native@.service
cp -- "$repository_root/deploy/assets/systemd/agent-vm-deployd.service" \
  /etc/systemd/system/agent-vm-deployd.service
sleep_binary="$(command -v sleep)"
cat >/etc/systemd/system/agent-vm-rqlite.service <<EOF
[Unit]
Description=Deployd acceptance state-authority stub

[Service]
Type=simple
ExecStart=$sleep_binary 3600
EOF
systemctl daemon-reload

debug_unit() {
  systemctl status --no-pager --full "$1" >&2 || true
}

assert_exec_start() {
  local actual="$1"
  local expected="$2"
  local label="$3"
  python3 - "$actual" "$expected" "$label" <<'PY'
import shlex
import sys

actual, expected, label = sys.argv[1:]
marker = "argv[]="
if marker not in actual:
    raise SystemExit(f"{label} does not expose argv[]")
command = actual.split(marker, 1)[1]
if " ;" in command:
    command = command.split(" ;", 1)[0]
if shlex.split(command) != shlex.split(expected):
    raise SystemExit(f"{label} does not exactly match the expected argv")
PY
}

wait_active() {
  local unit="$1"
  for _ in $(seq 1 80); do
    if systemctl is-active --quiet "$unit"; then
      return 0
    fi
    sleep 0.1
  done
  debug_unit "$unit"
  return 1
}

systemctl start agent-vm-rqlite.service
wait_active agent-vm-rqlite.service || fail "acceptance state-authority stub did not start"
systemctl start agent-vm-native@blue.service agent-vm-native@green.service
wait_active agent-vm-native@blue.service || fail "blue slot did not start"
wait_active agent-vm-native@green.service || fail "green slot did not start"

for slot in blue green; do
  main_pid="$(systemctl show "agent-vm-native@$slot.service" --property=MainPID --value)"
  control_group="$(systemctl show "agent-vm-native@$slot.service" --property=ControlGroup --value)"
  fragment_path="$(systemctl show "agent-vm-native@$slot.service" --property=FragmentPath --value)"
  exec_start="$(systemctl show "agent-vm-native@$slot.service" --property=ExecStart --value)"
  [[ "$main_pid" =~ ^[1-9][0-9]*$ ]] || fail "$slot MainPID was not verified"
  [[ "$control_group" == */agent-vm-native@"$slot".service ]] || fail "$slot cgroup was not verified"
  [[ "$fragment_path" == /etc/systemd/system/agent-vm-native@.service ]] || fail "$slot fragment path was not verified"
  assert_exec_start "$exec_start" "/var/lib/agent-vm-native/slots/$slot/bin/agent-vm-control serve --config /etc/agent-vm-native/slots/$slot/config.json --manifest /var/lib/agent-vm-native/slots/$slot/release-manifest.json" "$slot ExecStart" || fail "$slot ExecStart was not verified"
done

systemctl start agent-vm-deployd.service
for _ in $(seq 1 100); do
  if ! systemctl is-active --quiet agent-vm-deployd.service; then
    debug_unit agent-vm-deployd.service
    fail "Deployd did not stay active"
  fi
  if [[ -S /run/agent-vm-native/deployd.sock ]] && curl --fail --silent --max-time 2 "http://127.0.0.1:$metrics_port/metrics" >/dev/null; then
    break
  fi
  sleep 0.1
done
[[ -S /run/agent-vm-native/deployd.sock ]] || fail "Deployd UDS did not start"
deployd_pid="$(systemctl show agent-vm-deployd.service --property=MainPID --value)"
deployd_group="$(systemctl show agent-vm-deployd.service --property=ControlGroup --value)"
deployd_fragment="$(systemctl show agent-vm-deployd.service --property=FragmentPath --value)"
deployd_exec="$(systemctl show agent-vm-deployd.service --property=ExecStart --value)"
[[ "$deployd_pid" =~ ^[1-9][0-9]*$ ]] || fail "Deployd MainPID was not verified"
[[ "$deployd_group" == */agent-vm-deployd.service ]] || fail "Deployd cgroup was not verified"
[[ "$deployd_fragment" == /etc/systemd/system/agent-vm-deployd.service ]] || fail "Deployd fragment path was not verified"
assert_exec_start "$deployd_exec" "/opt/agent-vm-native/bin/agent-vm-deployd --config /etc/agent-vm-native/deployd.json" "Deployd ExecStart" || fail "Deployd ExecStart was not verified"

assert_status() {
  local payload="$1"
  local active="$2"
  local stopped="$3"
  python3 - "$payload" "$active" "$stopped" <<'PY'
import json
import sys

value = json.loads(sys.argv[1])
expected_active, expected_stopped = sys.argv[2:]
observation = value["value"]["observation"]
if observation["active_slot"] != expected_active or observation["controller_slot"] != expected_active:
    raise SystemExit("active/controller slot mismatch")
slots = observation["slots"]
if not slots[expected_active]["process_up"] or not slots[expected_active]["controller_owned"]:
    raise SystemExit("active slot is not process-ready and controller-owned")
if slots[expected_stopped]["process_up"]:
    raise SystemExit("old slot is still running")
if slots[expected_stopped]["controller_owned"] or slots[expected_stopped]["traffic_accepting"]:
    raise SystemExit("old slot retained a role")
transactions = value["value"].get("transactions", [])
if len([slot for slot in slots.values() if slot.get("controller_owned")]) != 1:
    raise SystemExit("controller ownership is not unique")
if transactions:
    raise SystemExit("unexpected active transaction")
PY
}

status_json="$(/opt/agent-vm-native/bin/agent-vm-deploy status --json)"
python3 - "$status_json" <<'PY'
import json
import sys

value = json.loads(sys.argv[1])
observation = value["value"]["observation"]
if observation["active_slot"] != "blue" or observation["controller_slot"] != "blue":
    raise SystemExit("initial blue controller state was not observed")
if sum(1 for slot in observation["slots"].values() if slot.get("controller_owned")) != 1:
    raise SystemExit("initial controller ownership is not unique")
if not all(observation["slots"][slot]["process_up"] for slot in ("blue", "green")):
    raise SystemExit("both live slots were not observed")
PY

stage_json="$(/opt/agent-vm-native/bin/agent-vm-deploy stage --slot green --bundle "$real_bundle" --json)"
transaction_id="$(python3 - "$stage_json" <<'PY'
import json
import sys
print(json.loads(sys.argv[1])["value"]["transaction_id"])
PY
)"
/opt/agent-vm-native/bin/agent-vm-deploy verify --transaction "$transaction_id" --json >/dev/null
/opt/agent-vm-native/bin/agent-vm-deploy activate --transaction "$transaction_id" --json >/dev/null
if systemctl is-active --quiet agent-vm-native@blue.service; then
  fail "blue slot remained active after the verified switch"
fi
wait_active agent-vm-native@green.service || fail "green slot was not active after the switch"
status_json="$(/opt/agent-vm-native/bin/agent-vm-deploy status --json)"
assert_status "$status_json" green blue
metrics="$(curl --fail --silent --max-time 2 "http://127.0.0.1:$metrics_port/metrics")"
grep -Fq 'agent_vm_deployd_observation_up{source="systemd"} 1' <<<"$metrics" || fail "systemd observation was not exported"
grep -Fq 'agent_vm_deployd_slot_selected{slot="green"} 1' <<<"$metrics" || fail "green selected metric was not exported"
grep -Fq 'agent_vm_deployd_slot_controller_owned{slot="green"} 1' <<<"$metrics" || fail "green controller metric was not exported"

stage_json="$(/opt/agent-vm-native/bin/agent-vm-deploy stage --slot blue --bundle "$real_bundle" --json)"
recovery_transaction="$(python3 - "$stage_json" <<'PY'
import json
import sys
print(json.loads(sys.argv[1])["value"]["transaction_id"])
PY
)"
/opt/agent-vm-native/bin/agent-vm-deploy verify --transaction "$recovery_transaction" --json >/dev/null
systemctl stop agent-vm-deployd.service
systemctl start agent-vm-deployd.service
for _ in $(seq 1 100); do
  if systemctl is-active --quiet agent-vm-deployd.service && [[ -S /run/agent-vm-native/deployd.sock ]]; then
    recovery_status="$(/opt/agent-vm-native/bin/agent-vm-deploy transaction --transaction "$recovery_transaction" --json 2>/dev/null || true)"
    if grep -q 'needs_recovery' <<<"$recovery_status"; then
      break
    fi
  fi
  sleep 0.1
done
grep -q 'needs_recovery' <<<"${recovery_status:-}" || fail "Deployd restart did not mark the interrupted transaction for recovery"
recovery_metrics="$(curl --fail --silent --max-time 2 "http://127.0.0.1:$metrics_port/metrics")"
grep -Fq 'agent_vm_deployd_recovery_required 1' <<<"$recovery_metrics" || fail "recovery blocking was not exported"
/opt/agent-vm-native/bin/agent-vm-deploy recover --transaction "$recovery_transaction" --action rollback --json >/dev/null
status_json="$(/opt/agent-vm-native/bin/agent-vm-deploy status --json)"
assert_status "$status_json" green blue

printf '{"schema_version":1,"verdict":"pass","systemd":"real","slot_runtime":"testslot-adapter","real_control_binary":"version-smoke-passed","slots":"dual-process-and-uds","switch":"green","restart_recovery":"needs_recovery-and-bounded-rollback","metrics":"single-deployd-endpoint"}\n'
