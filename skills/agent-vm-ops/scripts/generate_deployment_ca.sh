#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat >&2 <<'EOF'
usage: generate_deployment_ca.sh --output-dir <empty-directory> --hostname <dns-name>

Creates a local deployment CA and an rqlite server certificate.
Caddy obtains its own public certificate with DNS-01 and does not use a static leaf.
The output directory must not contain files. The private keys are mode 0600.
EOF
}

output_dir=""
hostname_value=""
output_dir_created=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output-dir)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      output_dir="$2"
      shift 2
      ;;
    --hostname)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      hostname_value="$2"
      shift 2
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

[[ -n "$output_dir" && -n "$hostname_value" ]] || { usage; exit 2; }
[[ "$output_dir" = /* ]] || { echo "output directory must be absolute" >&2; exit 2; }

assert_no_symlinked_ancestors() {
  local path="$1"
  while :; do
    if [[ -L "$path" ]]; then
      if [[ "$(uname -s)" != Darwin || "$path" != "/var" ]]; then
        echo "path contains a symlinked directory: $path" >&2
        exit 2
      fi
    fi
    if [[ -e "$path" && ! -d "$path" ]]; then
      echo "path component is not a directory: $path" >&2
      exit 2
    fi
    [[ "$path" == "/" ]] && return 0
    path="$(dirname -- "$path")"
  done
}

assert_no_symlinked_ancestors "$output_dir"
if [[ ! "$hostname_value" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]]; then
  echo "hostname must be a DNS name without a port" >&2
  exit 2
fi
if [[ "$hostname_value" == *..* || "$hostname_value" == *"<"* || "$hostname_value" == *">"* ]]; then
  echo "hostname contains an invalid placeholder or empty label" >&2
  exit 2
fi

if [[ -L "$output_dir" ]]; then
  echo "output path must not be a symlink" >&2
  exit 2
elif [[ -e "$output_dir" ]]; then
  [[ -d "$output_dir" ]] || { echo "output path is not a directory" >&2; exit 2; }
  if [[ -n "$(find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
    echo "output directory must be empty" >&2
    exit 2
  fi
else
  mkdir -p -- "$output_dir"
  output_dir_created=1
fi
assert_no_symlinked_ancestors "$output_dir"
[[ -d "$output_dir" && ! -L "$output_dir" ]] || { echo "output path is missing or symlinked" >&2; exit 2; }

command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 2; }

umask 077
assert_no_symlinked_ancestors "$output_dir/rqlite"
mkdir -- "$output_dir/rqlite"
assert_no_symlinked_ancestors "$output_dir/rqlite"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/agent-vm-ca.XXXXXX")"
cleanup() {
  local original_status="$?"
  if (( original_status != 0 )); then
    rm -f -- "$output_dir/ca.key" "$output_dir/ca.crt" \
      "$output_dir/rqlite/server.key" "$output_dir/rqlite/server.crt"
    rmdir -- "$output_dir/rqlite" 2>/dev/null || true
    if (( output_dir_created == 1 )); then
      rmdir -- "$output_dir" 2>/dev/null || true
    fi
  fi
  rm -rf -- "$temporary_dir"
  exit "$original_status"
}
trap cleanup EXIT

cat >"$temporary_dir/server.ext" <<EOF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:${hostname_value},DNS:localhost,IP:127.0.0.1
EOF

openssl req -x509 -newkey rsa:3072 -sha256 -nodes \
  -days 825 \
  -subj "/CN=Agent VM deployment CA" \
  -keyout "$output_dir/ca.key" \
  -out "$output_dir/ca.crt" \
  >/dev/null 2>&1

create_leaf() {
  local name="$1"
  openssl req -new -newkey rsa:2048 -sha256 -nodes \
    -subj "/CN=${hostname_value}" \
    -keyout "$output_dir/$name/server.key" \
    -out "$temporary_dir/$name.csr" \
    >/dev/null 2>&1
  openssl x509 -req -sha256 \
    -days 397 \
    -in "$temporary_dir/$name.csr" \
    -CA "$output_dir/ca.crt" \
    -CAkey "$output_dir/ca.key" \
    -CAcreateserial \
    -CAserial "$temporary_dir/ca.srl" \
    -extfile "$temporary_dir/server.ext" \
    -out "$output_dir/$name/server.crt" \
    >/dev/null 2>&1
  chmod 0600 "$output_dir/$name/server.key"
  chmod 0644 "$output_dir/$name/server.crt"
}

create_leaf rqlite
chmod 0600 "$output_dir/ca.key"
chmod 0644 "$output_dir/ca.crt"

printf '{"output_dir":"%s","hostname":"%s","files":["ca.crt","ca.key","rqlite/server.crt","rqlite/server.key"]}\n' \
  "$output_dir" "$hostname_value"
