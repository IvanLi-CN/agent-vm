#!/bin/sh
set -eu

CONTROL_ROOT=/opt/agent-vm-native
CONTROL="$CONTROL_ROOT/bin/agent-vm-control"
BRIDGE="$CONTROL_ROOT/agent_vm_control_bridge.ts"
CLIENT="$CONTROL_ROOT/agent_vm.ts"
SOCKET=/run/agent-vm-native/control.sock
MANIFEST="$CONTROL_ROOT/release-manifest.json"
UNIT=/etc/systemd/system/agent-vm-native.service
REQUEST='{"protocol":4,"action":"doctor"}'

fail() {
    printf 'acceptance_failed=%s\n' "$1" >&2
    exit 1
}

command -v systemctl >/dev/null 2>&1 || fail missing_systemctl
command -v systemd-analyze >/dev/null 2>&1 || fail missing_systemd_analyze
command -v curl >/dev/null 2>&1 || fail missing_curl
command -v incus >/dev/null 2>&1 || fail missing_incus
command -v python3 >/dev/null 2>&1 || fail missing_python

[ -x "$CONTROL" ] || fail go_control_missing
[ -f "$BRIDGE" ] || fail bridge_missing
[ -x "$CLIENT" ] || fail client_missing
[ -f "$MANIFEST" ] || fail manifest_missing
[ -f "$CONTROL_ROOT/systemd/agent-vm-native.service" ] || fail bundled_unit_missing
[ -f "$UNIT" ] || fail installed_unit_missing

revision=$(python3 - "$MANIFEST" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    manifest = json.load(handle)
if manifest.get("format") != 2 or manifest.get("protocol") != 4:
    raise SystemExit("manifest_format_or_protocol_invalid")
if manifest.get("toolchain", {}).get("cgo_enabled") is not False:
    raise SystemExit("manifest_cgo_policy_invalid")
print(manifest["source_revision"])
PY
) || fail invalid_manifest

systemctl is-active --quiet agent-vm-native.service || fail service_not_active
systemctl is-enabled --quiet agent-vm-native.service || fail service_not_enabled
systemd-analyze verify "$UNIT" || fail unit_invalid

health=$(curl --fail --silent --show-error --max-time 5 --unix-socket "$SOCKET" http://localhost/internal/v1/health) || fail uds_health_failed
python3 -c 'import json, sys
value = json.loads(sys.argv[1])
if value.get("ok") is not True or value.get("protocol") != 4 or value.get("ready") is not True:
    raise SystemExit("health_not_ready")' "$health" || fail uds_health_invalid

direct=$(printf '%s\n' "$REQUEST" | "$CONTROL" request --socket "$SOCKET" --timeout 30s) || fail direct_request_failed
bridge=$(printf '%s\n' "$REQUEST" | /usr/local/bin/bun "$BRIDGE") || fail bridge_request_failed
python3 - "$direct" "$bridge" <<'PY' || fail transport_identity_mismatch
import json
import sys

direct, bridge = (json.loads(item) for item in sys.argv[1:])
for value in (direct, bridge):
    if value.get("ok") is not True or value.get("protocol") != 4 or value.get("action") != "doctor":
        raise SystemExit("invalid_transport_envelope")
    if value.get("result", {}).get("transport") != "http-over-uds":
        raise SystemExit("unexpected_transport")
if direct.get("release") != bridge.get("release"):
    raise SystemExit("release_identity_differed")
PY

worker_output=$(printf 'agent-vm-native-acceptance\n' | "$CONTROL" native-worker --timeout 10s -- /bin/cat) || fail native_worker_failed
[ "$worker_output" = "agent-vm-native-acceptance" ] || fail native_worker_output_mismatch

image=$(incus image list --format csv -c f | sed -n '1p')
[ -n "$image" ] || fail no_incus_image
name="agent-vm-native-acceptance-$$"
cleanup() {
    incus delete "$name" --force >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

incus init "$image" "$name" --config security.nesting=true --config boot.autostart=false >/dev/null || fail nested_init_failed
incus start "$name" >/dev/null || fail nested_start_failed
ready=false
for _ in $(seq 1 30); do
    state=$(incus exec "$name" -- systemctl is-system-running 2>/dev/null || true)
    case "$state" in
        running|degraded)
            ready=true
            break
            ;;
    esac
    sleep 1
done
[ "$ready" = true ] || fail nested_systemd_not_ready
incus exec "$name" -- sh -c 'test "$(cat /proc/1/comm)" = systemd; test ! -e /dev/kvm' || fail nested_container_boundary_failed
incus stop "$name" --force >/dev/null || fail nested_stop_failed

printf 'acceptance=passed\n'
printf 'source_revision=%s\n' "$revision"
printf 'systemd=active-and-enabled\n'
printf 'transport=uds-and-bun-bridge\n'
printf 'native_worker=verified\n'
printf 'nested_incus=systemd-container\n'
printf 'kvm=not-required\n'
