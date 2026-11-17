#!/usr/bin/env bash
set -euo pipefail
verify_ssh() (
  set -euo pipefail
  systemctl is-active --quiet ssh
  test -s /etc/ssh/ssh_host_ed25519_key.pub
  work=$(mktemp -d /root/agent-vm-ssh-check.XXXXXX)
  install -d -m 700 /root/.ssh
  existed=false
  if test -f /root/.ssh/authorized_keys; then cp -p /root/.ssh/authorized_keys "$work/authorized_keys"; existed=true; fi
  cleanup() {
    if "$existed"; then cp -p "$work/authorized_keys" /root/.ssh/authorized_keys; else rm -f /root/.ssh/authorized_keys; fi
    rm -r -- "$work"
  }
  trap cleanup EXIT
  ssh-keygen -q -t ed25519 -N '' -f "$work/key"
  cat "$work/key.pub" >> /root/.ssh/authorized_keys
  chmod 600 /root/.ssh/authorized_keys
  printf '127.0.0.1 %s\n' "$(cat /etc/ssh/ssh_host_ed25519_key.pub)" > "$work/known_hosts"
  result=$(ssh -i "$work/key" -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o HostKeyAlgorithms=ssh-ed25519 -o UserKnownHostsFile="$work/known_hosts" -o ConnectTimeout=5 root@127.0.0.1 'printf ssh-ok; id -u')
  test "$result" = ssh-ok0
)
verify_ssh
