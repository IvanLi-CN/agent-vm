#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat >&2 <<'EOF'
usage: agent_vm_cutover.sh --slot <blue|green> [options]

Options:
  --root <absolute-path>          release root (default: /var/lib/agent-vm-native)
  --runtime-root <absolute-path>  runtime socket root (default: /run/agent-vm-native)
  --health-timeout <seconds>      per-request health timeout (default: 2)
  --dry-run                       validate the candidate without changing active
EOF
}

root_dir="/var/lib/agent-vm-native"
runtime_root="/run/agent-vm-native"
slot=""
health_timeout=2
dry_run=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --root|--runtime-root|--slot|--health-timeout)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      case "$1" in
        --root) root_dir="$2" ;;
        --runtime-root) runtime_root="$2" ;;
        --slot) slot="$2" ;;
        --health-timeout) health_timeout="$2" ;;
      esac
      shift 2
      ;;
    --dry-run)
      dry_run=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

die() {
  echo "cutover: $*" >&2
  exit 1
}

[[ "$root_dir" = /* && "$runtime_root" = /* ]] || die "root paths must be absolute"
[[ "$slot" == blue || "$slot" == green ]] || die "slot must be blue or green"
[[ "$health_timeout" =~ ^[1-9][0-9]*$ ]] || die "health timeout must be a positive integer"

candidate="$root_dir/slots/$slot"
candidate_socket="$candidate/app.sock"
expected_socket="$runtime_root/$slot/app.sock"
active="$root_dir/active"
[[ -d "$candidate" ]] || die "candidate slot directory is missing: $candidate"
[[ -f "$candidate/release-manifest.json" ]] || die "candidate release manifest is missing"
[[ -L "$candidate_socket" ]] || die "candidate socket link is missing"
[[ "$(readlink -- "$candidate_socket")" == "$expected_socket" ]] || die "candidate socket link is not the expected runtime socket"

if [[ -e "$active" && ! -L "$active" ]]; then
  die "active must be an absent path or a symlink"
fi
if [[ -L "$active" ]]; then
  current_target="$(readlink -f -- "$active")"
  case "$current_target" in
    "$root_dir/slots/blue"|"$root_dir/slots/green") ;;
    *) die "active points outside the two verified slots" ;;
  esac
  current_slot="${current_target##*/}"
else
  current_slot="none"
fi

if (( dry_run == 1 )); then
  printf '{"dry_run":true,"candidate":"%s","current":"%s","socket":"%s"}\n' \
    "$slot" "$current_slot" "$expected_socket"
  exit 0
fi

[[ "$EUID" -eq 0 ]] || die "health verification must run as root for the protected Unix endpoint"
command -v curl >/dev/null 2>&1 || die "curl is required for the protected health check"
[[ -S "$expected_socket" ]] || die "candidate runtime socket is not present"

lock_dir="$root_dir/.cutover.lock"
mkdir -p -- "$root_dir"
lock_acquired=0
for _ in $(seq 1 300); do
  if mkdir -- "$lock_dir" 2>/dev/null; then
    lock_acquired=1
    break
  fi
  sleep 0.1
done
(( lock_acquired == 1 )) || die "another cutover is in progress"
temporary_link="$root_dir/.active.$$.tmp"
cleanup() {
  rm -f -- "$temporary_link"
  rmdir -- "$lock_dir" 2>/dev/null || true
}
trap cleanup EXIT

health_body="$(curl --fail --silent --show-error --max-time "$health_timeout" \
  --unix-socket "$expected_socket" \
  http://localhost/internal/v1/health)" || die "candidate health check failed"
[[ "$health_body" == *'"ready":true'* ]] || die "candidate health response is not ready"

ln -s -- "$candidate" "$temporary_link"
mv -f -- "$temporary_link" "$active"
printf '{"dry_run":false,"previous":"%s","active":"%s","health":"ready"}\n' \
  "$current_slot" "$slot"
