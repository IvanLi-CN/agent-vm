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

if (( dry_run == 0 )); then
  mkdir -p -- "$root_dir"
fi

validate_release_root() {
  [[ -d "$root_dir" && ! -L "$root_dir" ]] || die "release root is missing or symlinked: $root_dir"
  root_dir="$(readlink -f -- "$root_dir")" || die "release root cannot be canonicalized"
  [[ -d "$root_dir" && ! -L "$root_dir" ]] || die "canonical release root is missing or symlinked: $root_dir"
  [[ -d "$root_dir/slots" && ! -L "$root_dir/slots" ]] || die "slot root is missing or symlinked: $root_dir/slots"
  [[ "$(readlink -f -- "$root_dir/slots")" == "$root_dir/slots" ]] || die "slot root path is not canonical: $root_dir/slots"
}

validate_release_root

candidate="$root_dir/slots/$slot"
candidate_socket="$candidate/app.sock"
expected_socket="$runtime_root/$slot/app.sock"
active="$root_dir/active"
pending_file="$root_dir/.active.cutover.pending"

validate_slot_layout() {
  local slot_name="$1"
  local slot_dir="$root_dir/slots/$slot_name"
  local socket_link="$slot_dir/app.sock"
  local expected_slot_socket="$runtime_root/$slot_name/app.sock"
  [[ -d "$slot_dir" && ! -L "$slot_dir" ]] || die "slot directory is missing or symlinked: $slot_dir"
  [[ -f "$slot_dir/release-manifest.json" && ! -L "$slot_dir/release-manifest.json" ]] || die "slot release manifest is missing or symlinked: $slot_name"
  [[ "$(readlink -f -- "$slot_dir")" == "$root_dir/slots/$slot_name" ]] || die "slot directory resolves outside the release root: $slot_name"
  [[ -L "$socket_link" ]] || die "slot socket link is missing: $slot_name"
  [[ "$(readlink -- "$socket_link")" == "$expected_slot_socket" ]] || die "slot socket link is not the expected runtime socket: $slot_name"
}

validate_candidate_layout() {
  validate_slot_layout "$slot"
}

validate_candidate_layout

read_current_slot() {
  if [[ -e "$active" && ! -L "$active" ]]; then
    die "active must be an absent path or a symlink"
  fi
  if [[ -L "$active" ]]; then
    current_target="$(readlink -f -- "$active")"
    case "$current_target" in
      "$root_dir/slots/blue"|"$root_dir/slots/green") ;;
      *) die "active points outside the two verified slots" ;;
    esac
    [[ -d "$current_target" ]] || die "active target slot is missing"
    current_slot="${current_target##*/}"
    validate_slot_layout "$current_slot"
  else
    current_target=""
    current_slot="none"
  fi
}

active_matches_current() {
  if [[ "$current_slot" == "none" ]]; then
    [[ ! -e "$active" && ! -L "$active" ]]
  else
    [[ -L "$active" ]] && [[ "$(readlink -f -- "$active")" == "$root_dir/slots/$current_slot" ]]
  fi
}

if (( dry_run == 1 )); then
  read_current_slot
  printf '{"dry_run":true,"candidate":"%s","current":"%s","socket":"%s"}\n' \
    "$slot" "$current_slot" "$expected_socket"
  exit 0
fi

[[ "$EUID" -eq 0 ]] || die "health verification must run as root for the protected Unix endpoint"
command -v curl >/dev/null 2>&1 || die "curl is required for the protected health check"
command -v flock >/dev/null 2>&1 || die "flock is required for the active link lock"
command -v mktemp >/dev/null 2>&1 || die "mktemp is required for unique cutover staging"
command -v stat >/dev/null 2>&1 || die "stat is required for cutover recovery metadata"
command -v sync >/dev/null 2>&1 || die "sync is required for cutover recovery durability"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required for cutover recovery identity"
[[ -S "$expected_socket" ]] || die "candidate runtime socket is not present"

mkdir -p -- "$root_dir"
lock_path="$root_dir/.cutover.lock"
exec 8<"$root_dir" || die "cannot open the release-root lock directory"
flock --wait 30 8 || die "another active link operation is in progress"
lock_seed="$(mktemp "$root_dir/.cutover-lock.XXXXXX")" || die "cannot create the active link lock seed"
if [[ -L "$lock_path" ]]; then
  rm -f -- "$lock_seed"
  die "active link lock is symlinked"
fi
if [[ ! -e "$lock_path" ]]; then
  if ! ln -- "$lock_seed" "$lock_path"; then
    if [[ -L "$lock_path" ]]; then
      rm -f -- "$lock_seed"
      die "active link lock is symlinked"
    fi
    [[ -f "$lock_path" ]] || {
      rm -f -- "$lock_seed"
      die "cannot install the active link lock"
    }
  fi
fi
rm -f -- "$lock_seed"
[[ -f "$lock_path" && ! -L "$lock_path" ]] || die "active link lock is not a regular file"
exec 9<>"$lock_path" || die "cannot open the active link lock"
flock --wait 30 9 || die "another active link operation is in progress"
stage_dir=""
temporary_link=""
rollback_link=""
pending_previous_manifest=""
pending_candidate_manifest=""
stage_dir="$(mktemp -d -- "$root_dir/.active-stage.XXXXXX")" || die "cannot create unique cutover staging directory"
temporary_link="$stage_dir/active"
rollback_link="$stage_dir/rollback"
cleanup() {
  if [[ -n "$stage_dir" ]]; then
    rm -rf -- "$stage_dir"
  fi
  flock --unlock 9 2>/dev/null || true
  exec 9>&-
  flock --unlock 8 2>/dev/null || true
  exec 8>&-
}
trap cleanup EXIT
# Every writer of the protected active link must hold this kernel lock and the
# release-root directory lock. The directory lock serializes lock-path
# creation and opening; the regular lock file remains the documented
# cross-process lock identity. Both locks remain valid across readiness checks
# and atomic replacement.
read_current_slot

health_check() {
  local socket_path="$1"
  local health_body
  health_body="$(curl --fail --silent --show-error --max-time "$health_timeout" \
    --unix-socket "$socket_path" \
    http://localhost/internal/v1/health)" || return 1
  [[ "$health_body" == *'"ready":true'* ]]
}

active_target_is_candidate() {
  [[ -L "$active" ]] || return 1
  [[ "$(readlink -f -- "$active")" == "$candidate" ]]
}

active_candidate_layout_matches() {
  active_target_is_candidate || return 1
  [[ -L "$candidate_socket" ]] || return 1
  [[ "$(readlink -- "$candidate_socket")" == "$expected_socket" ]]
}

post_cutover_check() {
  active_candidate_layout_matches || return 1
  health_check "$active/app.sock" || return 1
  (assert_manifest_digest "$slot" "$pending_candidate_manifest") || return 1
  active_candidate_layout_matches
}

sync_path() {
  sync -f "$1" || die "cannot flush cutover path: $1"
}

slot_manifest_digest() {
  local slot_name="$1"
  local manifest_path="$root_dir/slots/$slot_name/release-manifest.json"
  local digest
  [[ -f "$manifest_path" && ! -L "$manifest_path" ]] || die "slot release manifest is missing or symlinked: $slot_name"
  digest="$(sha256sum -- "$manifest_path")" || die "cannot hash slot release manifest: $slot_name"
  digest="${digest%% *}"
  [[ "$digest" =~ ^[0-9a-f]{64}$ ]] || die "slot release manifest hash is invalid: $slot_name"
  printf '%s\n' "$digest"
}

assert_manifest_digest() {
  local slot_name="$1"
  local expected_digest="$2"
  [[ "$(slot_manifest_digest "$slot_name")" == "$expected_digest" ]] || die "slot release manifest changed during cutover recovery: $slot_name"
}

write_pending_record() {
  local pending_tmp
  local previous_manifest="none"
  if [[ "$current_slot" != "none" ]]; then
    previous_manifest="$(slot_manifest_digest "$current_slot")"
  fi
  pending_previous_manifest="$previous_manifest"
  local candidate_manifest
  candidate_manifest="$(slot_manifest_digest "$slot")"
  pending_candidate_manifest="$candidate_manifest"
  pending_tmp="$(mktemp "$stage_dir/pending.XXXXXX")" || die "cannot create the cutover recovery staging file"
  printf 'previous=%s\ncandidate=%s\nprevious_manifest=%s\ncandidate_manifest=%s\n' \
    "$current_slot" "$slot" "$previous_manifest" "$candidate_manifest" >"$pending_tmp" || die "cannot write the cutover recovery record"
  chmod 0600 "$pending_tmp" || die "cannot protect the cutover recovery record"
  sync_path "$pending_tmp"
  mv -Tf -- "$pending_tmp" "$pending_file" || die "cannot install the cutover recovery record"
  sync_path "$root_dir"
}

clear_pending_record() {
  rm -f -- "$pending_file"
  sync_path "$root_dir"
}

recover_pending_cutover() {
  [[ -e "$pending_file" || -L "$pending_file" ]] || return 0
  [[ -f "$pending_file" && ! -L "$pending_file" ]] || die "cutover recovery record is not a regular file"
  [[ "$(stat -c "%a" "$pending_file")" == "600" ]] || die "cutover recovery record is not mode 0600"

  local previous_record=""
  local candidate_record=""
  local previous_manifest_record=""
  local candidate_manifest_record=""
  local line_count=0
  local line
  while IFS= read -r line || [[ -n "$line" ]]; do
    line_count=$((line_count + 1))
    case "$line" in
      previous=blue|previous=green|previous=none) previous_record="${line#previous=}" ;;
      candidate=blue|candidate=green) candidate_record="${line#candidate=}" ;;
      previous_manifest=*) previous_manifest_record="${line#previous_manifest=}" ;;
      candidate_manifest=*) candidate_manifest_record="${line#candidate_manifest=}" ;;
      *) die "cutover recovery record is malformed" ;;
    esac
  done <"$pending_file"
  [[ "$line_count" -eq 4 && -n "$previous_record" && -n "$candidate_record" && -n "$previous_manifest_record" && -n "$candidate_manifest_record" ]] || die "cutover recovery record is incomplete"
  if [[ "$previous_record" == "none" ]]; then
    [[ "$previous_manifest_record" == "none" ]] || die "cutover recovery previous manifest identity is invalid"
  else
    [[ "$previous_manifest_record" =~ ^[0-9a-f]{64}$ ]] || die "cutover recovery previous manifest identity is invalid"
  fi
  [[ "$candidate_manifest_record" =~ ^[0-9a-f]{64}$ ]] || die "cutover recovery candidate manifest identity is invalid"
  validate_slot_layout "$candidate_record"
  assert_manifest_digest "$candidate_record" "$candidate_manifest_record"

  if [[ "$current_slot" == "$previous_record" ]]; then
    if [[ "$previous_record" != "none" ]]; then
      validate_slot_layout "$previous_record"
      assert_manifest_digest "$previous_record" "$previous_manifest_record"
      [[ -S "$runtime_root/$previous_record/app.sock" ]] || die "previous slot socket is not present for interrupted cutover recovery"
      health_check "$root_dir/slots/$previous_record/app.sock" || die "previous slot is not ready for interrupted cutover recovery"
    fi
    clear_pending_record
    return 0
  fi
  [[ "$current_slot" == "$candidate_record" ]] || die "cutover recovery record does not match active slot"
  validate_slot_layout "$candidate_record"
  assert_manifest_digest "$candidate_record" "$candidate_manifest_record"
  if [[ -S "$runtime_root/$candidate_record/app.sock" ]] && health_check "$root_dir/slots/$candidate_record/app.sock"; then
    if [[ "$previous_record" != "none" ]]; then
      validate_slot_layout "$previous_record"
      assert_manifest_digest "$previous_record" "$previous_manifest_record"
    fi
    clear_pending_record
    return 0
  fi
  if [[ "$previous_record" == "none" ]]; then
    rm -f -- "$active" || die "interrupted cutover recovery could not remove active link"
    sync_path "$root_dir"
  else
    validate_slot_layout "$previous_record"
    assert_manifest_digest "$previous_record" "$previous_manifest_record"
    [[ -S "$runtime_root/$previous_record/app.sock" ]] || die "previous slot socket is not present for interrupted cutover recovery"
    health_check "$root_dir/slots/$previous_record/app.sock" || die "previous slot is not ready for interrupted cutover recovery"
    ln -s -- "$root_dir/slots/$previous_record" "$rollback_link" || die "interrupted cutover recovery could not stage previous link"
    mv -Tf -- "$rollback_link" "$active" || die "interrupted cutover recovery could not restore previous link"
    sync_path "$root_dir"
    health_check "$active/app.sock" || die "previous slot failed interrupted cutover recovery"
  fi
  clear_pending_record
  read_current_slot
}

recover_pending_cutover
validate_candidate_layout
[[ -S "$expected_socket" ]] || die "candidate runtime socket is not present"

health_check "$expected_socket" || die "candidate health check failed"
active_matches_current || die "active changed while candidate was becoming ready"

validate_candidate_layout
active_matches_current || die "active changed before cutover"
write_pending_record
ln -s -- "$candidate" "$temporary_link"
# `active` is itself a symlink to a slot directory. GNU mv without -T treats
# that directory target as a container and moves the temporary link inside it;
# -T makes the replacement operate on the link path, preserving atomic cutover.
mv -Tf -- "$temporary_link" "$active"
sync_path "$root_dir"
if ! post_cutover_check; then
  if ! active_target_is_candidate; then
    die "candidate failed post-cutover health and active changed; refusing rollback"
  fi
  if [[ "$current_slot" == "none" ]]; then
    rm -f -- "$active" || die "candidate failed post-cutover health and active rollback failed"
  else
    validate_slot_layout "$current_slot"
    assert_manifest_digest "$current_slot" "$pending_previous_manifest"
    ln -s -- "$root_dir/slots/$current_slot" "$rollback_link"
    mv -Tf -- "$rollback_link" "$active" || die "candidate failed post-cutover health and active rollback failed"
    sync_path "$root_dir"
    health_check "$active/app.sock" || die "candidate failed post-cutover health; previous slot rollback is not ready"
  fi
  clear_pending_record
  die "candidate failed post-cutover health; rolled back to $current_slot"
fi
clear_pending_record
printf '{"dry_run":false,"previous":"%s","active":"%s","health":"ready"}\n' \
  "$current_slot" "$slot"
