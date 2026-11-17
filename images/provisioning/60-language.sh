#!/usr/bin/env bash
set -euo pipefail
work=$(mktemp -d /root/agent-vm-language.XXXXXX)
trap 'rm -r -- "$work"' EXIT
download_verify() {
  host="${1#https://}"; host="${host%%/*}"
  timeout 60 bash -c 'until getent ahostsv4 "$1" >/dev/null; do sleep 1; done' -- "$host"
  curl --retry 3 --connect-timeout 30 --max-time 600 -fsSL "$1" -o "$3"
  printf '%s  %s\n' "$2" "$3" | sha256sum -c -
}
case "$IMAGE_PROFILE" in
  base) ;;
  rust)
    download_verify "$RUST_URL" "$RUST_SHA256" "$work/rust.tar.xz"
    tar -xJf "$work/rust.tar.xz" -C "$work"
    "$work/rust-$RUST_VERSION-x86_64-unknown-linux-gnu/install.sh" --prefix=/usr/local --without=rust-docs
    rustc --version
    cargo --version
    ;;
  java)
    download_verify "$ZULU_URL" "$ZULU_SHA256" "$work/zulu.tar.gz"
    install -d /opt/zulu
    tar -xzf "$work/zulu.tar.gz" -C /opt/zulu --strip-components=1
    for tool in /opt/zulu/bin/*; do ln -sfn "$tool" "/usr/local/bin/$(basename "$tool")"; done
    printf '%s\n' 'export JAVA_HOME=/opt/zulu' > /etc/profile.d/agent-vm-java.sh
    java -version
    javac -version
    ;;
  go)
    download_verify "$GO_URL" "$GO_SHA256" "$work/go.tar.gz"
    tar -xzf "$work/go.tar.gz" -C /usr/local
    for tool in go gofmt; do ln -sfn "/usr/local/go/bin/$tool" "/usr/local/bin/$tool"; done
    go version
    ;;
  *) echo "unknown image profile: $IMAGE_PROFILE" >&2; exit 2 ;;
esac
