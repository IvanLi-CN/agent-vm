# Agent VM topology and recovery runbook

This document is the public operational boundary for the Native Incus Agent VM contract. It records the topology that is actually established by the deployment evidence and the recovery actions that the existing client and control contract can perform. It is not a host inventory and does not claim a physical or virtual source below the observed CT.

This runbook describes the current pre-blue-green SSH/SQLite recovery baseline.
The target deployment uses the unified service, stable project Caddy and shared
rqlite defined by [the system architecture](../../architecture/system.md) and
[ADR 0044](../../adr/0044-agent-vm-state-migration-to-rqlite.md). After the
one-time migration, do not restore or open the protected SQLite material as a
second live authority; perform drain, export, import and verification according
to that ADR.

## Topology and provenance

| Layer | Established role | Boundary |
| --- | --- | --- |
| Codex client | The local `agent-vm` CLI selects a session, records the local operation journal, and sends one protocol-4 request over configured SSH. | The client can identify its configured control entry; it cannot infer or rebuild the machine behind that entry. |
| Agent VM control endpoint | The configured SSH alias/address reaches the one-shot control process and its canonical configuration, durable state, locks, and native timer assets. | This is a control entrypoint, not proof of the underlying host, LXC owner, storage provider, or rebuild authority. A concrete private address from an operator record stays outside this public repository. |
| CT109 LXC | Deployment evidence establishes CT109 as the LXC containing the control environment and the Incus client/daemon boundary used by this deployment. | CT109 is a deployment fact, not a claim about the machine that creates, runs, or can recreate it. |
| Incus container inside CT109 | Each session owns at most one non-ephemeral Incus system container, reached through native proxy NAT and an explicit lease/generation. | The container's stopped cache and guest data are separate from the control database logically, but the public contract does not promise a separate physical disk or an independent storage failure domain. |
| Underlying hosting source | Unknown underlying hosting source; no public or verified evidence identifies the physical/virtual machine, hypervisor, infrastructure manager, or storage system below CT109. | Do not substitute a guessed PVE, Incus host, CT host, physical disk, or provisioning API for missing evidence. |

The verified deployment records include a matching revision, protected copies of control files/configuration, and durable state/journal material. They also record that host reboot and underlying-storage behavior were not established by the lifecycle evidence. A control SSH failure, including a host-unavailable diagnostic, proves only that the configured entry was unavailable at that observation; it does not identify the missing layer or justify a rebuild command.

The public repository deliberately keeps resolved addresses, private filesystem paths, credentials, live inventories, and deployment-specific checksums in protected operator records. The configured address and CT identifier must be treated as separate facts even when they appear in the same private record.

## Recovery boundary

Recovery is split into control-plane restoration, state/native reconciliation, and admission reopening. A host or CT rebuild is outside this repository's authority: it may begin only after an owner-authorized infrastructure procedure provides the identity of the control target, the rebuild authority, the storage/pool disposition, and the evidence that the CT and its Incus data are the intended deployment.

### Preserve before replacement

Preserve the following from the control environment and its protected backups before replacing files or restarting a recovered control process:

- The configured state database, its WAL/journal files, operation records, audit records, task rows, lease generations, resource reservations, last-error details, and the parent directory's ownership and mode.
- The canonical configuration, installed client/control/systemd revision and checksums, timer directory, protected recovery copies, SSH identity/known-host material, and the exact configured project/profile/image/network/ACL/pool/state paths.
- The Incus pool and all owned container data, including stopped sparse caches; the control database does not contain the guest filesystem and must not be treated as a substitute for it.
- The lock directory and its ownership/mode. Active lock files are process state and may be recreated only after the recovered process has exclusive access; deleting the durable database or operation journal is not a lock-recovery step.

Do not rotate, compact, convert, or rewrite pending operation records or generations while the native outcome is uncertain. Do not delete an Incus instance, port mapping, timer, cache, or conntrack entry merely because its control record is absent or incomplete.

### Restore the matching control components

1. If the control endpoint is unavailable, stop at the provenance boundary and obtain the owner-authorized CT/rebuild evidence. Do not try another address, enter CT109 through an assumed host, or use a guessed infrastructure API.
2. When the intended control environment is available, keep admission drained before touching the installed client, one-shot control script, configuration, or native units. If the drain state cannot be proven, keep the control service stopped and treat admission as closed.
3. Restore one approved release manifest's client/control/unit artifacts and the canonical configuration atomically, with protected ownership and mode. Restore the existing state database and journals in place; do not initialize a new empty database beside the old one and do not downgrade the protocol to make old records fit.
4. Restore or verify the native expiry/reconciliation units and their timer directory. Do not claim recovery from a unit file alone: the state database, native instances, mappings, and generation metadata must still be reconciled.
5. Run read-only health and inventory checks: `agent-vm doctor`, `agent-vm admin status`, `agent-vm admin session list`, and `agent-vm admin reconcile`. The last command is a dry-run unless `--apply` is explicitly supplied.

### Reconcile pending operations and generations

The local client journal and the control `operations` table are the identity records for a mutation. A transport timeout or `outcome: pending` means that the native effect is uncertain; it is never a success signal.

- Keep the original `operation_id`, request payload, and generation together. Never invent a replacement ID, alter the generation, or replay an uncertain native effect as a new intent.
- Inspect `status`, `admin status`, `admin session list`, and the dry-run reconciliation result before submitting anything else. A completed or error record may be replayed only with its original operation ID and identical request; a pending record must remain blocked until the native state is unambiguous.
- Compare every durable task's session, task identity, generation, state, lease deadline, resource reservation, native instance ownership, native status, ports, and timer token. A generation mismatch, missing record, foreign identity, or unavailable inspection is a stop condition.
- Apply `agent-vm admin reconcile --apply` only for records whose ownership, generation, lease state, and native cleanup/rearm action are all confirmed by the dry-run. Reconciliation may verify and repair an `absent` record with malformed or inconsistent nonzero resource/reservation state after confirming that its native instance and ports are absent; it may also close incomplete cleanup or rearm the current generation. It must not manufacture a generation or reopen a stale mapping.
- Keep an effective lease available for status, retrieval, renewal, and release according to the existing contract. An effective lease with a stopped or unverified guest is unavailable; do not infer OOM or restart it automatically.

### Reopen admission

Admission may be restored only after all of the following are observed in the intended control environment:

- The approved release manifest, compatible client/control artifacts, canonical configuration, database/journals, ownership/modes, and native units are present and readable.
- `doctor` reports component health, the control database is writable, the configured CT-local Incus pool filesystem can be measured, and the image/project/network/ACL policy is verified.
- Every task and port record is either reconciled to a confirmed native state or remains explicitly blocked; no pending operation, generation mismatch, unknown resource, partial resize, or cleanup transition is being silently ignored.
- Required timers and expiry/reconciliation behavior are verified for the restored generations, with no guessed host or storage provenance substituted for missing evidence.

Only then may an authorized operator run `agent-vm admin drain exit` and immediately repeat `agent-vm doctor` and `agent-vm admin status`. Any failed check keeps admission drained. `admin gc --apply`, `destroy`, cache deletion, or broad port/conntrack cleanup is not a substitute for state recovery.

### Missing state is a hard stop

If the state database, operation journal, generation records, protected configuration, or authoritative native inventory is missing, corrupt, or inconsistent, stop the control plane and preserve the remaining data. Do not guess ownership, reset generations, create a fresh state database, reacquire sessions, delete caches, or clean mappings globally.

The minimum evidence needed to continue is an owner-authorized mapping between the control entry and CT109, the matching state/config backups, the installed revision/checksums, the intended Incus project/pool, and a native inventory that can prove task ownership and generations. Without that evidence, the physical/virtual host source and rebuild path remain blockers; the correct outcome is an incident report, not cleanup or admission.

## Control-plane publication and compatibility

Publication is a manual operator-owned action through the configured control SSH entry. The release manifest covers the local client, one-shot control script, systemd assets, source revision, exact checksums, protocol, and per-operation request/response/semantic compatibility. It excludes configuration, credentials, connection material, state databases and WAL/journals, locks, Incus container data, and guest images.

Stage and verify the manifest, ownership, modes, syntax, checksums, and unit contents before entering drain. Drain blocks new acquisition and restart admission, leaves status/retrieval/release and expiry/reconciliation/cache-cleanup timers available, and does not force-kill in-flight processes. Wait for pending operations and task transitions to converge, activate the verified release atomically, and validate health, reconciliation, and timers before reopening admission.

An older client may continue using an action only when the manifest declares that action compatible; the response carries a structured compatibility warning. Unknown actions, incompatible protocol/schema/semantics, or unverified artifacts fail before side effects. Publish and health-check the remote release before changing the local client. A pre-activation failure leaves the current release untouched; a post-activation health failure permits one bounded rollback to the previous verified release. Failed rollback keeps admission drained and never rewrites state, generations, or uncertain operations.

## Sparse disk and failure domains

Agent VM disk admission does not reserve the full requested rootfs allocation for every container. Incus container root filesystems may be sparse. Admission uses the measured free space of the configured CT-local Incus pool filesystem, configured low/target watermarks, request cost, and durable reservations; stopped caches consume disk but no CPU or memory allocation budget. The rootfs limit is therefore an allocation limit and not a promise of physically reserved capacity.

The control plane consists of the state database, WAL/journals, operation/audit records, canonical configuration, locks, and native timer assets. Container data consists of Incus instance metadata, sparse backing storage, guest filesystems, and stopped caches. These are separate logical failure domains and ownership/recovery domains, not a promise that the deployment uses separate physical disks or separate underlying storage.

An ENOSPC or I/O failure in the CT-local Incus pool can prevent sparse growth, new container creation, cache restoration, or guest filesystem expansion. Disk admission must refuse new demand when its measurement is missing, invalid or below the configured disk threshold; an existing effective lease must not be evicted. CT memory pressure is reported separately and does not reject a logically available memory reservation. A partial native change keeps its conservative reservation and error details until inspection and explicit recovery or release.

CPU overselling has a separate startup-schedulability boundary. The approved overcommit deployment policy sets `cpu_startup_pressure_full_avg10_percent` to `50`; a new boot is refused with `resource_pressure` only when projected CPU exceeds CT-visible CPU capacity and verified CPU PSI `full.avg10` reaches that threshold. This check happens before a restart stops an existing guest, does not change logical CPU limits, and does not stop effective leases. `some.avg10` and `full.avg10` remain pressure observations, not CPU utilization or allocation counts. A missing PSI sample is reported as an observation failure; it is never converted to zero. The public example remains configurable and must not be treated as evidence that the installed deployment uses a particular threshold until the installed configuration is verified.

An ENOSPC or I/O failure in the control-state filesystem is more fundamental: operation identity, generations, cleanup, and timer decisions cannot be trusted if they cannot be durably recorded. Keep admission closed, stop mutation and cleanup that require durable state, preserve the database/journals, and restore writable control storage before reconciliation. Do not infer that container data is safe merely because the Incus daemon responds.

Guest-level ENOSPC is a separate observation: a guest command may fail while the control plane and CT pool remain healthy. Preserve the command error and lease state, and use normal retrieval/release rules; do not report guest ENOSPC as proof that the CT filesystem or underlying host is full.

Container rootfs growth is a separate fail-closed boundary after the container rootfs mutation. Before changing the guest, record capability and topology evidence and require an Incus storage/profile path that can enforce and verify the requested rootfs limit; a root filesystem already at least 95% of the requested rootfs capacity is an idempotent success. Only a classified busy or stale-filesystem failure may receive one bounded reread and settle retry. Bound guest command diagnostics to retained stdout prefixes and stderr tails, then preserve the original guest phase, command, return code, truncation flags when present, process-group cleanup evidence on command timeout, capacity evidence, and bounded attempt/retry chain in the structured error.

If acquire cannot verify guest root growth, it must not retain an active lease or SSH mapping. Cleanup leaves the record in the normal retryable cached or cleanup transition with `acquire_partial` details; inspect the guest and durable state before retrying the same operation.

No part of this contract prevents all ENOSPC conditions or provides a recovery guarantee for an expired lease. It defines the safe admission and stop behavior when capacity or provenance cannot be verified.

## Capacity and inventory observation

Use `agent-vm admin metrics` as the aggregate source for logical admission, CT-visible capacity, pressure, disk readiness, operation outcomes, image-unpack failures, warnings, freshness and task-state counts. Use `agent-vm admin status` as the bounded inventory source for per-task state, native status, guest availability, resources, lease expiry, SSH mapping and structured last-error code/phase. Both actions are read-only; a scrape bridge must not mutate leases or native state.

The capacity view must show `allocated`, `reserved`, `allocated_and_reserved`, logical `remaining` and logical `limit` separately for CPU, memory and disk, alongside CT-visible capacity and runtime pressure. It must also show the policy default request/cost and two snapshot estimates: logical default-container slots and currently startable default-container slots. Calculate the first from the minimum verified CPU, memory and physical-disk headroom ratios. Apply the current CT CPU capacity and startup-pressure gate to calculate the second. Omit either estimate when an input is unverified, and never call either estimate a native or safe maximum.

The inventory view covers active, preparing, closing, deleting, failed, cached and absent rows. Sort active and transitional work first, then failed cleanup, then cached/absent; collapse cached rows by default and provide state filters. Keep identifiers, state, bounded resources, lease/mapping state and error code/phase, but omit raw error text, credentials and deployment filesystem paths. Remove stale per-task series after every successful scrape and expose `up=0` plus freshness failure when either read-only source cannot be read or parsed.

## Related contracts

- [Native Incus Agent VM specification](SPEC.md)
- [Agent VM CLI contract](contracts/cli.md)
- [Native deployment boundary](../../../skills/agent-vm-ops/references/deployment.md)
- [Agent VM topology and recovery boundary](../../adr/0028-agent-vm-topology-recovery-boundary.md)
- [Agent VM control-plane update boundary](../../adr/0030-agent-vm-control-plane-update-boundary.md)
