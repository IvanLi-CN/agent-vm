# Native Incus Agent VM

## Scope

This topic defines a one-shot client and control process for session-owned Linux VMs on a configured Incus host. The public repository documents the lifecycle contract and ships the implementation; deployments provide their own project, image, network, address pool, storage path, state database and control address through explicit configuration.

The design intentionally excludes resident brokers, HTTP APIs, polling loops, implicit keepalives, shared host workspaces, exported archives and guarantees that survive lease expiry.

## Session and transport

Every request carries a generic `session_id`. The CLI selects it from explicit `--session-id` first and `AGENT_VM_SESSION_ID` second. A missing value returns `missing_session` before any remote write. Session identity is ownership metadata and is not an authentication mechanism. The client sends one versioned JSON request over the configured SSH control path and emits one JSON response; diagnostics go to stderr.

Protocol 4 is required by both client and control. Mutations include an operation ID and lease generation. The control persists requests and outcomes, rejects incompatible or conflicting retries, and never replays an uncertain native mutation automatically.

## Lifecycle

Each session owns at most one VM slot, including preparing, stopping, deleting and cached states. Acquire creates or reuses a stopped cache, records a new generation and waits for guest agent and IPv4 readiness within one bounded monotonic budget. Lease expiry and release revoke access, remove only owned mappings and stop the guest before making the slot reusable. A stopped disk may remain as a disposable cache; no recovery guarantee is made.

Renewal is explicit and bounded. Acquire, status, SSH, execution and transfer do not renew a lease. A changed generation or expired lease rejects late readiness and native results. Durable state, locks and timers make repeated requests idempotent without hiding partial cleanup.

## Access and isolation

The deployment configuration supplies the control address and host port range. Proxy NAT uses the configured host address, leased port and fixed guest address. SSH keeps host-key verification enabled with explicit identity and known-host files. The managed Incus project must allow the required proxy devices, and the configured ACL must prove peer and host isolation before admission. Exact conntrack entries are removed during mapping cleanup.

## Capacity and resizing

Capacity observations are limited to the configured Incus pool filesystem and visible control-environment CPU and memory boundaries. External storage probes and infrastructure-manager inspection are unsupported. CPU and memory allocation ceilings use separately configurable overselling ratios; stopped caches consume disk only. New demand is refused when local measurements are missing, invalid or below the configured pressure threshold. Existing lease queries, retrieval, renewal and release remain available during pressure.

Disk changes require an explicit restart allowance and grow only. Memory-only live changes require a running owned guest, unchanged boot identity and native acknowledgement. The old reservation remains until verification succeeds. A single bounded convergence retry is allowed for the documented native balloon timeout; no polling loop or third attempt exists. Partial native changes retain conservative reservations and a structured recovery state.

## Reclamation and operations

Manual garbage collection is dry-run by default and selects only stopped managed caches with no effective lease and confirmed access cleanup. The age timer and capacity admission may use different bounded selection policies, but neither can delete a valid lease, transition, resize target or unknown resource. Infrastructure operations require a separate explicit authorization and use the same configured paths and identity checks.

## Synchronization and retrieval

The synchronization helper transfers tracked, unignored worktree files to its own guest source directory and atomically replaces only helper-owned content. It rejects submodules and unsafe names. Export streams a tar archive directly to the caller and does not create a server-side archive.

## Related material

- [CLI contract](contracts/cli.md)
- [Reclamation design](RECLAMATION-DESIGN.md)
- [Retention design](RETENTION-DESIGN.md)
