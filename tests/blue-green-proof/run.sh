#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
RQLITE_VERSION="10.5.2"
RQLITE_ASSET="rqlite-v10.5.2-linux-amd64.tar.gz"
RQLITE_URL="https://github.com/rqlite/rqlite/releases/download/v10.5.2/${RQLITE_ASSET}"
RQLITE_SHA256="1a04a7c2c542e8e1c1347bd31af0fa9ef7043febb3c4d1806d36bc1702c6fa0f"

PROOF_DIR="$(mktemp -d "${TMPDIR:-/tmp}/agent-vm-rqlite-proof.XXXXXX")"
KEEP_PROOF_DIR="${KEEP_PROOF_DIR:-0}"

cleanup() {
  if [[ "${KEEP_PROOF_DIR}" != "1" ]]; then
    if [[ -e "${PROOF_DIR}/rqlited.started" ]]; then
      if [[ ! -f "${PROOF_DIR}/rqlited.pid" ]]; then
        printf 'cleanup_skipped=rqlited_pid_unknown proof_dir=%s\n' "${PROOF_DIR}" >&2
        return 0
      fi
      pid="$(<"${PROOF_DIR}/rqlited.pid")"
      if [[ ! "${pid}" =~ ^[0-9]+$ ]] || [[ ! -d /proc ]] || [[ -e "/proc/${pid}" ]]; then
        printf 'cleanup_skipped=unable_to_confirm_rqlited_pid_exit proof_dir=%s\n' "${PROOF_DIR}" >&2
        return 0
      fi
    fi
    rm -rf -- "${PROOF_DIR}"
  else
    printf 'proof_dir=%s\n' "${PROOF_DIR}" >&2
  fi
}
trap cleanup EXIT

forward_signal() {
  local signal="$1"
  if [[ -z "${BUN_PID:-}" ]]; then
    [[ "${signal}" == "TERM" ]] && exit 143
    exit 130
  fi
  kill "-${signal}" "${BUN_PID}" 2>/dev/null || true
}
trap 'forward_signal TERM' TERM
trap 'forward_signal INT' INT

if [[ -n "${RQLITED_BIN:-}" ]]; then
  RQLITED_BIN="$(CDPATH= cd -- "$(dirname -- "${RQLITED_BIN}")" && pwd)/$(basename -- "${RQLITED_BIN}")"
else
  DOWNLOAD_DIR="${PROOF_DIR}/download"
  mkdir -p -- "${DOWNLOAD_DIR}"
  ARCHIVE="${DOWNLOAD_DIR}/${RQLITE_ASSET}"
  curl --fail --silent --show-error --location --retry 3 --output "${ARCHIVE}" "${RQLITE_URL}"
  printf '%s  %s\n' "${RQLITE_SHA256}" "${ARCHIVE}" | sha256sum --check --status -
  tar --extract --gzip --file "${ARCHIVE}" --directory "${DOWNLOAD_DIR}"
  RQLITED_BIN="${DOWNLOAD_DIR}/rqlite-v${RQLITE_VERSION}-linux-amd64/rqlited"
fi

[[ -x "${RQLITED_BIN}" ]]
BUN_PID=""
bun "${SCRIPT_DIR}/run-rqlite-proof.ts" \
  --rqlited "${RQLITED_BIN}" \
  --proof-dir "${PROOF_DIR}" &
BUN_PID=$!
set +e
wait "${BUN_PID}"
status=$?
if [[ "${status}" -eq 143 || "${status}" -eq 130 ]]; then
  wait "${BUN_PID}"
  status=$?
fi
set -e
trap - INT TERM
exit "${status}"
