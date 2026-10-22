---
status: accepted
---

# Agent VM control-plane update boundary

The Agent VM control plane remains authoritative in the operator-owned control environment because Incus access, durable lifecycle state, lease generations, native mappings, capacity admission, and systemd maintenance must be coordinated by one authority. The local `agent-vm` client remains a versioned SSH request client; it must not acquire Incus/PVE authority or become a second control plane.

Control-plane publication, update, and rollback belong to an operator-owned deployment tool or procedure, not the public `agent-vm` CLI. The remote one-shot control process remains a required canonical execution artifact shared by SSH requests and systemd timers; releases are explicitly published and auditable. Unattended control-plane updates are disabled by default and are not part of the core protocol.

A control-plane release is a logical compatibility unit containing the local client, the remote control script, systemd unit assets, and release metadata. It excludes deployment configuration, credentials, connection material, the state database and WAL/journals, locks, Incus VM data, and guest images. Publication uses the configured control SSH entry with an operator-only identity or narrowly scoped root-owned deployment wrapper; it does not add an sshd, gateway, PVE access path, or Agent VM guest permission.

Remote control-script or unit updates require admission drain before installation. Drain blocks new acquisition and restart admission, but does not kill in-flight requests or disable expiry, reconciliation, or cache-cleanup timers; publication waits for pending operations and task transitions to converge, replaces artifacts atomically, verifies health and timer state, and exits drain only after those checks succeed. A local-client-only update does not require drain.

Compatibility is evaluated per operation rather than promised globally between releases. A release manifest declares the protocol, request/response schema, semantic compatibility, and direction supported for each operation. Compatible operations may continue across different release revisions with a structured compatibility warning; an unsupported action, incompatible schema or semantic change, invalid checksum, or incompatible protocol is rejected before any state or native effect. A compatible evolution keeps the established protocol contract (currently protocol 4); a breaking evolution may require a new protocol and may legitimately require an update.

The publication transaction stages and verifies the selected release before drain, waits for in-flight and pending operations to converge without force-killing them, then atomically activates the artifacts and validates health, reconciliation, and timer state. A pre-activation failure leaves the current release untouched. A post-activation health failure permits one bounded rollback to the previous verified release; a failed rollback keeps admission drained and never rewrites state, generations, or uncertain operations. At least the current and previous verified releases are retained with their manifests and checksums.

This boundary accepts SSH/control-environment availability as a dependency in exchange for a single source of truth, bounded failure handling, and a narrow permission surface. It also keeps private deployment paths, credentials, inventories, and state outside the public repository.
