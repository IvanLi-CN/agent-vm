# Native Incus Agent VM

## Context and Scope

This topic defines a one-shot client and control process for session-owned Linux VMs on a configured Incus host. The public repository documents the lifecycle contract and ships the implementation; deployments provide their own project, image, network, address pool, storage path, state database and control address through explicit configuration.

The design intentionally excludes resident brokers, HTTP APIs, polling loops, implicit keepalives, shared host workspaces, exported archives and guarantees that survive lease expiry.

The deployment topology is layered: the local Codex client reaches a configured Agent VM control endpoint; the control environment runs inside the observed CT109 LXC; the control environment owns session-scoped Incus VMs; and the physical or virtual source below CT109 is outside the verified public boundary. A control endpoint is an entry path, not evidence of the underlying host or its rebuild authority. The concrete address and live inventory remain in protected deployment records.

## Session and transport

Session-scoped requests carry a `session_id`; the sessionless read-only `admin-metrics` request omits it and rejects both `session_id` and `operation_id`. The CLI selects a session from explicit `--session-id` first and `AGENT_VM_SESSION_ID` second for session-scoped actions. A missing value returns `missing_session` before any remote write. Session identity is ownership metadata and is not an authentication mechanism. The client sends one versioned JSON request over the configured SSH control path and emits one JSON response; diagnostics go to stderr.

Protocol 4 is required by both client and control. Mutations include an operation ID and lease generation. The control persists requests and outcomes, rejects incompatible or conflicting retries, and never replays an uncertain native mutation automatically.

## Lifecycle

Each session owns at most one VM slot, including preparing, stopping, deleting and cached states. Acquire creates or reuses a stopped cache, records a new generation and waits for guest agent and IPv4 readiness within one bounded monotonic budget. If an unrelated lease-expiry cleanup briefly exposes a transitional native instance, acquire waits within a separate bounded 30-second admission-convergence budget, polling no more often than every 250 milliseconds, and keeps the same operation pending; only that exact transitional condition is retryable and all other unverified state fails closed. Lease expiry and release revoke access, remove only owned mappings and stop the guest before making the slot reusable. A stopped disk may remain as a disposable cache; no recovery guarantee is made.

Renewal is explicit and bounded. Acquire, status, SSH, execution and transfer do not renew a lease. A changed generation or expired lease rejects late readiness and native results. Durable state, locks and timers make repeated requests idempotent without hiding partial cleanup.

## Access and isolation

The deployment configuration supplies the control address and host port range. Proxy NAT uses the configured host address, leased port and fixed guest address. SSH keeps host-key verification enabled with explicit identity and known-host files. The managed Incus project must allow the required proxy devices, and the configured ACL must prove peer and host isolation before admission. Exact conntrack entries are removed during mapping cleanup.

## Capacity and resizing

Capacity observations are limited to the configured Incus pool filesystem and visible control-environment CPU and memory boundaries. External storage probes and infrastructure-manager inspection are unsupported. CPU and memory allocation ceilings use separately configurable overselling ratios; stopped caches consume disk only. New demand is refused when capacity measurements are missing or invalid, when the logical reservation ceiling is exceeded, when disk admission fails, while draining, or when a new native boot would run during verified severe CT-local CPU scheduler pressure. The CPU startup guard applies only when projected CPU allocation exceeds CT-visible CPU capacity and `full.avg10` reaches the configured `cpu_startup_pressure_full_avg10_percent` threshold; it does not change the logical CPU ceiling or stop an effective lease. Low CT memory headroom is an observation and warning under reservation-only memory admission; it does not reduce logical remaining memory or reject a request within the configured ceiling. Existing lease queries, retrieval, renewal and release remain available during pressure.

Disk changes require an explicit restart allowance and grow only. Guest root growth is an independent post-check: it diagnoses required capabilities and direct ext4/xfs topology before mutation, treats a filesystem already at least 95% of the requested virtual capacity as an idempotent success, and permits only one bounded partition-reread/settle retry for a classified transient failure. The old reservation remains until both virtual and guest filesystem capacity are verified. A single bounded convergence retry is allowed for the documented native balloon timeout; no polling loop or third attempt exists. Partial native or guest growth changes retain conservative reservations and a structured recovery state.

## Reclamation and operations

Manual garbage collection is dry-run by default and selects only stopped managed caches with no effective lease and confirmed access cleanup. The age timer and capacity admission may use different bounded selection policies, but neither can delete a valid lease, transition, resize target or unknown resource. Infrastructure operations require a separate explicit authorization and use the same configured paths and identity checks.

## Control-plane releases and compatibility

The control environment remains the sole authority for Incus operations, durable lifecycle state, lease generations, capacity admission, native mappings, and systemd maintenance. The local `agent-vm` CLI is a versioned SSH request client; publication, update, and rollback are operator-owned deployment actions and are not public CLI lifecycle commands. Unattended control-plane updates are disabled by default.

A control-plane release is a logical compatibility unit containing the local client, the remote one-shot control script, systemd unit assets, and a release manifest. The manifest records the selected source revision, exact artifact checksums, protocol, and per-operation request/response/semantic compatibility in each supported direction. Deployment configuration, credentials, connection material, state database and WAL/journals, locks, Incus VM data, and guest images are outside the release.

Compatible operations may continue across different release revisions with a structured warning. Compatibility is not inferred from parseability: an unsupported action, incompatible schema or semantic change, invalid checksum, or incompatible protocol MUST fail before any durable or native effect. A compatible evolution preserves the established protocol contract; a breaking evolution may require a new protocol and an explicit client update.

Remote control-script and unit publication MUST use the configured control SSH entry, an operator-only identity or narrowly scoped root-owned deployment wrapper, a private staging area, and atomic activation. It MUST NOT add an sshd, HTTP gateway, PVE path, or Agent VM guest permission. Remote publication is activated and verified before the local client is updated; a local-only client update does not require drain.

Remote publication MUST enter admission drain before activation, block new acquisition and restart admission, keep expiry/reconciliation/cache-cleanup timers available, and allow in-flight requests to finish without force-killing them. It MUST wait for pending operations and task transitions to converge, verify health, reconciliation, and timer state, and exit drain only after successful validation. A pre-activation failure leaves the current release untouched. A post-activation health failure permits one bounded rollback to the previous verified release; an unsuccessful rollback keeps admission drained and MUST NOT rewrite state, generations, or uncertain operations. At least the current and previous verified releases MUST be retained with their manifests and checksums.

## Requirements

### REQ-AGENT-VM-010 - Layered provenance

The public contract MUST distinguish the Codex client, configured control endpoint, CT109 LXC, Incus VM, and unknown underlying hosting source. It MUST NOT turn a configured address, CT identifier, or Incus identity into a claim about the next lower layer, and it MUST keep private deployment addresses and inventories outside the repository.

### REQ-AGENT-VM-011 - Rebuild and recovery fencing

Recovery MUST preserve the state database and WAL/journals, operation and audit records, generations, configuration, protected recovery copies, native timer assets, and Incus VM data. It MUST restore matching components under drain, reconcile durable records against native ownership and generations, and keep admission closed when state, provenance, capacity, or native identity is missing or inconsistent. It MUST NOT invent operation IDs, reset generations, replay uncertain native effects, or perform broad cleanup.

### REQ-AGENT-VM-012 - Sparse disk failure boundary

Disk admission MUST account for the configured CT-local Incus pool filesystem, request cost, watermarks, and durable reservations without claiming a full physical reservation for every virtual disk. Control-plane state and VM data MUST be treated as separate logical failure domains; ENOSPC or I/O failure MUST preserve conservative state and refuse unsafe new admission without evicting an effective lease.

### REQ-AGENT-VM-013 - Observability provenance

Operational dashboards MUST distinguish CT filesystem capacity, transient image staging, physical Incus pool usage, guest rootfs allocation, and native admission state. They MUST NOT infer an authoritative native limit from an unset Incus project quota or draw a limit line for a missing or `-1` source. The dashboard contract MUST expose failure phase, image-unpack failures, admission state and scrape freshness so an ENOSPC boundary can be identified without host-manager access. The public repository MUST ship a sanitized, parameterized dashboard baseline and validation path; deployment-specific rendered files, datasource identities, controller jobs and collector paths remain deployment-owned.

### REQ-AGENT-VM-014 - Control-plane update authority

Control-plane publication, update, and rollback MUST remain operator-owned deployment actions. The public `agent-vm` CLI MUST remain a lifecycle/request client and MUST NOT acquire Incus/PVE authority, expose a public self-update command, or become a second control plane. The remote one-shot control process MUST remain the canonical execution artifact for both SSH requests and systemd maintenance.

### REQ-AGENT-VM-015 - Method-level compatibility

A release manifest MUST declare protocol, request/response schema, semantic compatibility, and direction for each supported operation. A compatible operation MAY continue across release revisions with a structured warning. An unsupported action, incompatible schema or semantic change, invalid artifact checksum, or incompatible protocol MUST be rejected before any durable or native effect.

### REQ-AGENT-VM-016 - Publication transaction and rollback

Publication MUST stage and verify an explicit release before drain, enter drain before remote activation, preserve in-flight operations and maintenance timers, wait for pending operations and task transitions to converge, atomically activate artifacts, and verify health, reconciliation, and timer state before reopening admission. A pre-activation failure MUST leave the current release untouched. A post-activation health failure MAY perform one bounded rollback to the previous verified release; failed rollback MUST keep admission drained without rewriting state, generations, or uncertain operations.

### REQ-AGENT-VM-017 - Release scope and permission boundary

A control-plane release MUST contain the local client, remote control script, systemd assets, release metadata, and exact artifact checksums, while excluding deployment configuration, credentials, connection material, state databases, locks, Incus VM data, and guest images. Publication MUST use the configured control entry with operator-only or narrowly scoped root-owned deployment authority and MUST NOT add PVE, host, guest, extra SSH, HTTP, or gateway access. Unattended control-plane updates MUST remain disabled by default.

### REQ-AGENT-VM-018 - CPU startup pressure boundary

The controller MUST observe CT-local CPU PSI separately from logical CPU reservation. When a new acquire, cached resume, restart-capable resize, or explicit restart would project CPU allocation above CT-visible CPU capacity and verified `full.avg10` meets the configured startup-pressure threshold, it MUST refuse the native boot with structured `resource_pressure` evidence before stopping or starting an existing guest. The guard MUST NOT change logical CPU allocation ceilings, stop an effective lease, or block read, renewal, retrieval, release, or live memory-only resize; missing CPU PSI MUST remain an explicit observation failure rather than a fabricated zero.

## Synchronization and retrieval

The TypeScript/Bun synchronization helper transfers tracked, unignored worktree files to its own guest source directory and atomically replaces only helper-owned content. Git selection, manifests and native filesystem access preserve byte paths, symlink targets, modes and hashes; unsupported local root-path representation fails explicitly. It rejects submodules, symlink parents, reserved ownership filenames and unsafe names before transfer. The guest transaction is an in-memory Bun action guarded by the managed image's runtime/native prerequisites and an external execution deadline; missing Bun or atomic capability fails closed. Native locks, exclusive preparation, baseline identity comparison, fsync and atomic directory exchange preserve the current source and adjacent results across conflicts and failures. An uncertain commit keeps its source marker and precise recovery record and returns exit 75 rather than an inferred rollback. The [CLI contract](contracts/cli.md#source-synchronization-transaction) defines execution bounds, ownership and recovery. Export streams a tar archive directly to the caller and does not create a server-side archive.

## Verification

- `VER-AGENT-VM-010` covers: `REQ-AGENT-VM-010` through the topology/provenance sections of [the operations runbook](OPERATIONS.md), the accepted [topology and recovery ADR](../../adr/0028-agent-vm-topology-recovery-boundary.md), and `bin/test_agent_vm_documentation.py`.
- `VER-AGENT-VM-011` covers: `REQ-AGENT-VM-011` through the recovery procedure in [the operations runbook](OPERATIONS.md), the existing native reconciliation and operation-journal behavior tests, and `bin/test_agent_vm_documentation.py`.
- `VER-AGENT-VM-012` covers: `REQ-AGENT-VM-012` through the sparse-disk and failure-domain sections in [the operations runbook](OPERATIONS.md), the existing capacity/reclamation tests, and `bin/test_agent_vm_documentation.py`.
- `VER-AGENT-VM-013` covers: `REQ-AGENT-VM-013` through [the observability contract](contracts/observability.md), [the capacity and failure dashboard solution](../../solutions/agent-vm/observability-dashboard.md), and `bin/test_agent_vm_documentation.py`.
- `VER-AGENT-VM-014` covers: `REQ-AGENT-VM-014` through [the control-plane update ADR](../../adr/0030-agent-vm-control-plane-update-boundary.md), [the CLI contract](contracts/cli.md), [the deployment boundary](../../skills/agent-vm-ops/references/deployment.md), and the public boundary checks.
- `VER-AGENT-VM-015` covers: `REQ-AGENT-VM-015` through the compatibility section of [the CLI contract](contracts/cli.md), [the release contract](contracts/release.md), the release-manifest rules in [the deployment boundary](../../skills/agent-vm-ops/references/deployment.md), and compatibility behavior tests for supported and rejected operations.
- `VER-AGENT-VM-016` covers: `REQ-AGENT-VM-016` through [the release contract](contracts/release.md), the publication transaction and recovery sections of [the operations runbook](OPERATIONS.md), [the control-plane update ADR](../../adr/0030-agent-vm-control-plane-update-boundary.md), and staged activation, drain, rollback, and failure-state behavior tests.
- `VER-AGENT-VM-017` covers: `REQ-AGENT-VM-017` through [the release contract](contracts/release.md), [the deployment boundary](../../skills/agent-vm-ops/references/deployment.md), [the agent-vm-ops Skill](../../skills/agent-vm-ops/SKILL.md), the public boundary checks, and release manifest permission tests.
- `VER-AGENT-VM-018` covers: `REQ-AGENT-VM-018` through the CPU startup-pressure section of [the operations runbook](OPERATIONS.md), [the CLI contract](contracts/cli.md), [the observability contract](contracts/observability.md), and the deterministic CPU pressure admission tests.

## Related ADRs

- [Native Incus Agent VM lifecycle](../../adr/0025-native-incus-agent-vm-lifecycle.md)
- [Agent VM topology and recovery boundary](../../adr/0028-agent-vm-topology-recovery-boundary.md)
- [Agent VM memory reservation-only admission](../../adr/0029-agent-vm-memory-reservation-only-admission.md)
- [Agent VM control-plane update boundary](../../adr/0030-agent-vm-control-plane-update-boundary.md)
- [Agent VM CPU startup pressure boundary](../../adr/0031-agent-vm-cpu-startup-pressure-boundary.md)

## Related material

- [CLI contract](contracts/cli.md)
- [Control-plane release contract](contracts/release.md)
- [Reclamation design](RECLAMATION-DESIGN.md)
- [Retention design](RETENTION-DESIGN.md)
- [Observability contract](contracts/observability.md)
