---
status: accepted
title: Agent VM standalone repository ownership
---

# Context

Agent VM contains a local client, a native control service, compatibility
bridges, release tooling and lifecycle contracts. Keeping those parts inside a
general Skill collection makes the control-plane source boundary ambiguous and
allows the client and control release units to drift apart.

The extracted repository must remain compatible with the existing protocol 4
client and state semantics while becoming independently testable and
publishable. The initial snapshot is taken from the merged Agent VM
implementation in `agent-toolkit` at
`2651fc8a8d32eaa611027fc04aa7908843639222`.

# Decision

`IvanLi-CN/agent-vm` is the canonical source for the Agent VM client, Go
control service, compatibility bridge, release tooling, systemd assets and
Agent VM contracts. The extracted source keeps the `skills/agent-vm` and
`skills/agent-vm-ops` paths so source-linked clients and existing documentation
remain stable during the split.

The repository owns its own Bun package, Go module path, CI and validation
entrypoints. It has no runtime dependency on `agent-toolkit`; the old checkout
is historical migration context and must not become a second active
implementation. A follow-up cleanup in `agent-toolkit` may remove the duplicate
source after consumers have moved to this repository.

The control boundary remains a low-cost nested Incus system-container design.
KVM, QEMU and `/dev/kvm` are outside the architecture. SSH remains a temporary
compatibility adapter, while the protected Gin API and the shared Go business
service are transport-independent.

# Consequences

- Client, control, release and contract changes can be reviewed and released as
  one unit.
- The repository needs its own CI and release provenance instead of inheriting
  the general toolkit's membership checks.
- Deployment configuration, credentials, inventories, state databases and
  rendered dashboards remain operator-owned and are not extracted here.
- The initial history retains Agent VM implementation commits, but future
  changes should be made in this repository only.
