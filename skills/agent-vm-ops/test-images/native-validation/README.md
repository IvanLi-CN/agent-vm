# Native validation image

This directory defines the project-specific test image used for Linux,
systemd, native process-tree and nested-Incus validation. It is derived from
the pinned Debian 13 amd64 container image and is intentionally separate from
the generic Agent VM image.

The image contains the test toolchain and host prerequisites only. It does not
contain a checkout, release bundle, deployment configuration, credentials,
state databases or guest images belonging to a deployment.

## Required image properties

- Incus container image, not a VM image.
- Debian 13 amd64 base.
- Go 1.27.3 with `CGO_ENABLED=0` build support.
- `build-essential` for the explicit Go race-test path (`CGO_ENABLED=1`).
- Bun 1.4.4, copied from the generic Agent VM image.
- Incus 6.x with a cached Debian 13 inner image.
- systemd, OpenSSH, conntrack-tools, `ss`, filesystem growth tools, Git,
  rsync, Python and the native acceptance dependencies.
- Launch profile enables `security.nesting=true`; the image must not require
  KVM, QEMU or `/dev/kvm`.

The image is published with the alias `agent-vm-native-validation`. Record the
published fingerprint and base-image fingerprint in the build evidence; do
not commit an exported image archive.

## Acceptance

Install a candidate release under `/opt/agent-vm-native` and run:

```sh
skills/agent-vm-ops/scripts/agent_vm_native_acceptance.sh
```

The acceptance script is the authority for the installed bundle, systemd
activation, UDS/Bun transport, native worker cleanup and nested Incus checks.
