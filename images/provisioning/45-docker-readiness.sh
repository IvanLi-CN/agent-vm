#!/usr/bin/env bash
# Run only inside a disposable image-build or acceptance guest.
set -euo pipefail
verify_nested_docker() (
  set -euo pipefail
  test "$(docker info --format '{{.Driver}}')" = overlay2
  test "$(docker context inspect --format '{{.Endpoints.docker.Host}}')" = unix:///var/run/docker.sock
  test -S /var/run/docker.sock
  work=$(mktemp -d /tmp/agent-vm-docker-check.XXXXXX)
  project="avmcheck$(date +%s)$$"
  image="${project}:test"
  volume="${project}-data"
  cleanup() {
    docker compose -p "$project" -f "$work/compose.yaml" down --remove-orphans >/dev/null 2>&1 || true
    docker volume rm "$volume" >/dev/null 2>&1 || true
    docker image rm "$image" >/dev/null 2>&1 || true
    # work is an exact directory created by this subshell.
    rm -r -- "$work"
  }
  trap cleanup EXIT
  cp "$(command -v busybox)" "$work/busybox"
  cat > "$work/Dockerfile" <<'DOCKER'
FROM scratch
COPY busybox /bin/busybox
RUN ["/bin/busybox", "sh", "-c", "echo layer-one > /proof; echo removed > /obsolete"]
RUN ["/bin/busybox", "sh", "-c", "rm_cmd=/bin/busybox; $rm_cmd rm /obsolete; echo layer-two >> /proof"]
ENTRYPOINT ["/bin/busybox"]
DOCKER
  docker build --network=none --tag "$image" "$work"
  docker run --rm "$image" sh -ec 'test ! -e /obsolete; test "$(/bin/busybox cat /proof)" = "$(printf "layer-one\nlayer-two")"'
  docker run --rm --user 65534:65534 "$image" sh -ec 'test "$(/bin/busybox id -u)" = 65534'
  docker volume create "$volume" >/dev/null
  docker run --rm -v "$volume:/data" "$image" sh -ec 'echo nested-docker-ok > /data/proof'
  cat > "$work/compose.yaml" <<YAML
services:
  server:
    image: $image
    pull_policy: never
    command: [httpd, -f, -p, '8080', -h, /data]
    volumes: [data:/data]
  probe:
    image: $image
    pull_policy: never
    command:
      - sh
      - -ec
      - |
        # Use an absolute DNS name so inherited guest search domains cannot change the query.
        for attempt in 1 2 3 4 5 6 7 8 9 10; do
          if /bin/busybox nslookup server. && [ "\$(/bin/busybox wget -qO- http://server.:8080/proof)" = nested-docker-ok ]; then exit 0; fi
          /bin/busybox sleep 1
        done
        exit 1
volumes:
  data:
    external: true
    name: $volume
YAML
  docker compose -p "$project" -f "$work/compose.yaml" up -d server
  docker compose -p "$project" -f "$work/compose.yaml" run --rm probe
  systemctl restart docker
  docker compose -p "$project" -f "$work/compose.yaml" up -d server
  docker compose -p "$project" -f "$work/compose.yaml" run --rm probe
  install -d /etc/agent-vm-image
  printf '%s\n' '{"docker_build":true,"overlay2_whiteout":true,"docker_run":true,"compose":true,"volume_persistence":true,"dns_network":true,"daemon_restart":true}' | tee /etc/agent-vm-image/docker-readiness.json
)
verify_nested_docker
