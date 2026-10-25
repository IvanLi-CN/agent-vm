# Agent VM CLI Contract

The installed executable is `agent-vm`, a direct TypeScript command line client under installed Bun. The canonical source is [agent_vm.ts](../../../../skills/agent-vm/scripts/agent_vm.ts), with its sibling modules retained for source-linked execution. The client and one-shot Bun control process must both speak protocol 4. Local Bun must support `bun:ffi`, `Bun.build` and direct TypeScript execution; OpenSSH and Git remain native tools, and sync additionally uses rsync. The public source does not install or download runtimes.

## Identity and configuration

Global `--session-id <id>` selects the ownership identity and is accepted before or after a command, including nested commands. If omitted, `AGENT_VM_SESSION_ID` is used. Explicit input wins. Session-scoped commands fail with JSON error code `missing_session` before local keys/journals or remote writes when neither is present. `version` and local help do not require an identity. `doctor` permits an omitted identity. `admin metrics` is sessionless, ignores the session environment and rejects an explicit `--session-id`. Tokens after `exec --` or `ssh --` belong to the native command and are never parsed as client identity options.

The control endpoint is selected from `AGENT_VM_CONTROL_HOST` and `AGENT_VM_REMOTE_CONTROL`, with deployment-safe defaults only for the executable name and remote path. The operations control process reads a JSON file whose environment-specific fields are required: project, profile, image, network, ACL, pool, control address, port range, guest address range, storage path, state database and timer directory. Missing or invalid values fail closed with `config_missing` or `config_invalid`.

Control-plane publication, update, and rollback are operator-owned deployment actions. The public `agent-vm` CLI has no `update` command and does not install remote files, modify deployment configuration, or acquire Incus/PVE authority.

## Commands

| Command | Contract |
| --- | --- |
| `version` | Print local protocol and client version without contacting the control host. |
| `doctor` | Read-only component and admission diagnostics. |
| `status` | Read-only state for the selected session. |
| `acquire [--cpu N] [--memory SIZE] [--disk SIZE]` | Create or reuse the selected session VM. |
| `renew` | Extend the current lease within the configured maximum. |
| `release` | Revoke access, stop the guest and retain an eligible cache. |
| `resize [--cpu N] [--memory SIZE] [--disk SIZE] [--live] [--allow-restart]` | Apply an explicitly authorized resource change. |
| `ssh [-- SSH_ARGS]` | Open native SSH with generated strict connection settings. |
| `exec -- ARGV` | Run an argument-vector command; use `sh -lc` explicitly for shell syntax. |
| `sync --name NAME` | Transfer the current worktree through the bundled synchronization helper. |
| `upload SOURCE DESTINATION` / `download SOURCE DESTINATION` | Transfer files online without renewing. |
| `export PATH` | Stream a tar archive on stdout; diagnostics go to stderr. |
| `port add NAME --guest-port N [--protocol tcp\|udp] [--host-port N]` | Create an exact native proxy mapping. |
| `port list` / `port remove NAME` | Inspect or remove an exact mapping. |
| `admin status` | Read-only native component and maintenance status. |
| `admin metrics` | Read-only controller metrics snapshot for authorized scrape bridges; it includes resource allocation, CT memory, disk capacity/admission, task, warning and failure summaries; it does not create a session or mutate state. |
| `admin drain enter\|status\|exit` | Control admission while allowing safe retrieval and release. |
| `admin session list` | List managed session records. |
| `admin reconcile [--apply]` / `admin gc [--apply]` | Preview by default; apply only inside an authorized operations task. |

The `admin-metrics` result has stable top-level fields: `observed_at` (ISO-8601 string), `up`, `draining` and `admission_ready` (booleans), `reclaim_required` (boolean or `null` when disk capacity is unverified), `resources`, `capacity`, `disk_admission`, `ct_cpu`, `ct_memory`, `instances`, `task_states`, `operations`, `pending_operations`, `image_unpack_failures`, `warnings`, `warning_counts` and `doctor`. `resources` contains `allocated`, `reserved`, `allocated_and_reserved`, `remaining`, `limits`, `ct_capacity`, `memory`, `memory_pressure` and `cpu_pressure`; `ct_cpu` contains CT-visible `capacity_vcpu`, the verified CPU PSI observation, and the default-request `startup_pressure` result. `ct_memory` contains CT-local `capacity_bytes`, `current_bytes`, `available_bytes`, `pressure_threshold_bytes`, `pressure` and `oom_events`. `pending_operations` is a list of `{operation_id, action, session_id, generation, created_at}` records and distinguishes an uncertain journal from no recent operation. Unverified numeric or boolean observations are `null` or omitted according to the field contract; they are never converted to a healthy zero or false value.

Unknown flags and command layouts must be rejected; callers should use `--help` for the installed build.

## Request and response

Session-scoped requests contain `protocol`, `action` and `session_id`; the sessionless read-only `admin-metrics` request omits `session_id` and `operation_id`. Mutations also contain `generation` and `operation_id`. Success envelopes contain `ok`, `protocol`, `server_time`, `action`, `session_id`, `generation`, `operation_id`, `outcome`, `result`, and the action's `response` schema/semantic metadata, and MAY contain a structured `warnings` array; a client MUST match every supplied request identity field, including `generation`, before finalizing a local journal. A legacy control response that omits this metadata is accepted only with a structured compatibility warning. Error diagnostics are JSON on stderr with a nonzero exit status and MUST contain `ok: false`, `protocol`, the request identity fields that were supplied, and a structured `error`; the controller marks `error.detail.outcome_verified` when it has confirmed that no native effect can have occurred, while native-phase or durable-state uncertainty remains pending. The client accepts exactly one matching error envelope; unrelated, duplicate or type-mismatched envelopes leave the journal pending. SSH stderr is retained only within a 64 KiB diagnostic bound; a truncated failure is `remote_failed` and its mutation outcome remains unverified. A transport timeout reports the operation ID without claiming whether the mutation completed. A state-database failure reports `sqlite_unavailable` with `phase`, SQLite code/name, SQL operation category and `state_committed`; `null` means a commit result is unknown and the original operation remains subject to status/reconciliation.

## Control-plane compatibility

The release manifest is the authority for control-plane release identity, artifact checksums, protocol, and per-action request/response/semantic compatibility in each direction. A compatible action MAY run across different release revisions and MUST surface a structured compatibility warning. The client and control process MUST reject incompatible protocol, schema, semantic behavior, unknown action, or unverified artifact before any state or native effect; compatibility is never inferred from JSON parseability alone.

The complete manifest shape, wrapper action allowlist, publisher order, rollback limit, and private deployment boundary are defined in the [control-plane release contract](release.md). The public client only reads its local sidecar and has no update, publish, install, or rollback command.

The Bun client sidecar identifies `agent_vm.ts` as its `client_artifact` and declares the complete fixed `client_dependencies` array for `byte-fs.ts`, `client-io.ts`, `client-release.ts`, `client.ts`, `guest-source.ts`, `sync-native.ts` and `sync_worktree.ts`. Each dependency has `name`, sibling-relative `path`, `role: support`, exact `sha256` and fixed numeric `mode` (0755 for `sync_worktree.ts`, 0644 for the other support files). The entrypoint has `role: client` and mode 0755. A present sidecar must verify every module, reject missing/duplicate/extra dependencies, symlinks and altered modes/bytes, and fail before SSH or mutation journaling. The legacy Python artifact declaration is rejected. Missing sidecars retain the explicit legacy protocol-4 warnings. Local state and operation journals keep their existing session hash, paths and request shape; retries send the journaled capability, operation ID and generation even after local release metadata changes. Integer tokens remain exact beyond JavaScript's safe-number range, and fractional tokens are distinct from integer identity fields.

## Source synchronization transaction

The maintained local helper is [sync_worktree.ts](../../../../skills/agent-vm/scripts/sync_worktree.ts); [guest-source.ts](../../../../skills/agent-vm/scripts/guest-source.ts) is bundled in memory and run as a one-shot guest action. The helper first inspects the Git index for submodules, then selects tracked/unignored files using NUL-delimited byte paths. It skips tracked deletions and `.git`, refuses the reserved owner filename and symlink parents, hashes regular file bytes, and preserves exact POSIX modes and symlink target bytes. JSON paths use UTF-8 with surrogateescape for invalid bytes, matching the prior manifest representation; native byte-path syscalls avoid runtime string substitution. A local worktree root that cannot be represented exactly in subprocess argv is explicitly unsupported before transport. macOS filesystems may reject invalid UTF-8 names; their codec tests do not establish Linux filesystem behavior.

Only `/workspace/worktrees/<safe-name>/src`, that workspace's exact transaction stage/record and its lock are helper-owned. Adjacent results and unowned sources remain untouched. Names are one safe segment and transaction IDs are 32 lowercase hex digits. The guest checks canonical real parent directories and opens metadata without following symlinks. Native `flock` serializes begin/commit/cleanup with a maximum 60-second lock wait. Begin exclusively creates the stage and prepared record; it records the source device/inode/transaction baseline. Commit rejects a changed baseline or manifest, fsyncs regular files, ownership metadata and directories, then uses Linux `renameat2(RENAME_NOREPLACE)` for an absent source or `RENAME_EXCHANGE` for replacement. Atomic capability failure preserves the prior source and recovery material; there is no copy/delete replacement fallback. The committed marker and parent directory are durable before the committed record is acknowledged.

The Linux managed guest requires installed Bun with `bun:ffi`, libc byte-path calls, `flock` and `renameat2`, plus GNU `timeout` and rsync. Bun/timeout absence fails before staging; native capability resolution precedes parent creation. The SSH command runs `timeout --signal=KILL 180s bun -e <in-memory-bundle>`, with a 240-second client transport budget, bounded stdout/stderr and a 32-MiB guest request limit. No guest service or opportunistic runtime installation exists. Local rsync uses a private mode-0600 NUL listing and SSH configuration, removes temporary configuration on every path, and preserves transfer exit codes. Successful synchronization does not acquire or renew a lease.

A lost, malformed, truncated or timed-out commit acknowledgement returns exit 75 and retains the exact transaction recovery record; the helper never automatically aborts after commit starts. The source owner marker detects an exchange completed before the record update. `abort` reports `unverified` and retains committed recovery material. A deliberate `finish` confirms the source marker and deletes only the exact owned stage (including its recorded baseline identity after exchange) and record. An unconfirmed finish, foreign stage or superseded source retains evidence. A cleanup failure after confirmed commit reports success with a diagnostic and retained recovery material.

Compatible evolution keeps protocol 4. A breaking protocol or semantic change may require a coordinated release and an explicit client update. The remote control release is published and health-checked before the local client release is changed, so an older client can continue using compatible actions while the new client is not allowed to assume unsupported remote actions.

Acquire, restart and restart-capable resize use a longer bounded transport timeout. Acquire may spend up to 30 seconds waiting for an unrelated native lease-expiry cleanup to leave its transitional state; only `resource_unverified` with the exact message `transitional native instance blocks admission` is retried, while all other `resource_unverified` errors fail closed immediately. Before native startup, the control process can return `resource_pressure` when projected CPU exceeds CT-visible capacity and verified CPU PSI `full.avg10` reaches or exceeds the configured startup threshold; the response includes the observed pressure, threshold, projected CPU and CT capacity, and an existing guest is left untouched. A definitive pre-native `resource_pressure` may be retried with a new operation ID after status confirms that no native effect is active; an uncertain timeout or pending operation must keep its original operation ID and generation. This wait preserves protocol, generation and operation ID and does not create a second intent. No command implicitly renews a lease.

Guest filesystem growth reports `guest_filesystem` evidence including `status` (`grown`, `grown_after_retry` or `already_grown`, including when a bounded retry confirms that a prior mutation completed), virtual capacity, filesystem capacity, the verification threshold, filesystem type and direct device topology. A guest growth failure is `guest_resize_unverified` with bounded stdout/stderr, the failing phase, command, return code, truncation flags when applicable, process-group cleanup evidence on command timeout, capacity evidence and a bounded attempt/retry chain; acquire cleanup must leave no active lease or exposed SSH mapping, while the durable partial state remains inspectable and retryable.

## SSH and ports

Generated SSH configuration uses the configured control address, leased port, explicit identity and known-host files, a generation-bound host-key alias, `StrictHostKeyChecking yes` and `IdentitiesOnly yes`. It must not disable host checking or introduce a gateway. Port allocation considers listeners, native mappings, forwards, reservations and the configured ephemeral range. Lease cleanup removes mappings and their exact conntrack flows before the guest becomes reusable.

## Capacity and cleanup

Capacity responses identify their local filesystem scope, observed sources, allocation budgets, allocated and reserved values, logical remaining values and pressure observations. Missing or contradictory capacity measurements refuse new demand without stopping an effective lease. Under reservation-only memory admission, low CT `MemAvailable` is reported as pressure but does not reject a request within the logical memory ceiling. CPU PSI is a separate startup-schedulability guard: it does not reduce logical CPU remaining, and it does not stop a running lease. Manual cleanup is dry-run by default and rechecks ownership, generation and eligibility under object locks before deletion.

The dashboard and metric authority rules are defined in the [observability contract](observability.md). Guest rootfs allocation, physical Incus pool usage, CT root/image staging capacity and native admission are separate measurements.

## Topology and recovery boundary

The configured SSH control address is an entrypoint for the one-shot control process. It is not an identity for the underlying host, CT109 LXC, Incus host, storage system or rebuild authority. The public contract does not provide a host or CT rebuild command; see the [topology and recovery runbook](../OPERATIONS.md) for the evidence boundary and stop conditions.

The control environment is the observed CT109 LXC and owns session-scoped Incus VMs inside that CT. A concrete private address, CT identifier and Incus identity are independent facts. Missing provenance, missing durable state, or an unverified native inventory blocks recovery rather than authorizing a guessed host operation or broad cleanup.

During recovery, preserve the configured state database and WAL/journals, local operation journals, operation/audit records, generations, canonical configuration, protected connection material, native timer assets and Incus VM data. Restore matching client/control/unit revisions under drain, then inspect `agent-vm doctor`, `agent-vm admin status`, `agent-vm admin session list` and dry-run `agent-vm admin reconcile` before any apply. `admin reconcile --apply` is limited to confirmed ownership/generation and cleanup/rearm actions, plus repairing malformed or inconsistent nonzero resource/reservation records for an `absent` task after native instance and port absence are confirmed; `admin gc --apply` and destructive cleanup are not recovery shortcuts.

Remote control-script or unit publication enters drain before activation. Drain blocks new acquisition and restart admission but permits status, retrieval, release, and expiry/reconciliation/cache-cleanup timers; it does not force-kill in-flight operations. Publication waits for pending operations and task transitions to converge, atomically activates verified artifacts, and checks health, reconciliation, and timers before `admin drain exit`. A pre-activation failure leaves the current release untouched; post-activation health failure permits one bounded rollback to the previous verified release, while failed rollback keeps admission drained.

A transport timeout or pending operation is uncertain. Retain the original operation ID, request and generation, inspect status/reconciliation, and never create a new operation ID or reset a generation to make the request fit. If the durable/native state cannot be matched, keep admission drained and stop. Run `agent-vm admin drain exit` only after health, state, generation, timer and CT-local capacity checks pass, then repeat `doctor` and `admin status`.

## Reclamation and retry behavior

Reconciliation and garbage collection select only records that are expired, released or otherwise eligible under the configured policy. They re-read the record while holding the object lock, verify the session and generation, and remove only resources owned by that record. A failed stop, detach, proxy cleanup or metadata update leaves a retryable record and reports the failed phase; it does not silently mark the resource reusable. The same operation ID is safe to retry after a transport timeout, while a new operation ID is required for a new intent.

Acquire and restart are bounded operations that may return an uncertain outcome after transport failure. Callers inspect `status` or reconciliation and retry with the original operation ID and generation. Renewal never occurs implicitly as a side effect of `ssh`, `exec`, `sync`, upload, download or export.

## Resource and live-memory rules

CPU, memory and disk requests are parsed into canonical units before admission. Admission accounts for configured budgets, overcommit ratios and all effective reservations. A request is rejected when capacity measurements are missing or contradictory, when the requested value exceeds the configured logical policy, when disk admission fails, or while draining; memory pressure is reported separately and does not alter logical remaining memory. An existing effective lease remains available for retrieval and release.

`resize --live` is limited to a supported memory-only change. CPU, disk and unsupported live changes fail without falling back to a restart. `--allow-restart` is required for restart-capable changes and uses the longer bounded timeout. A live resize records the requested and observed values in the response so callers can distinguish a pending reconciliation from a completed change.

## Output and admission

Human-readable status is for operators; automation consumes the JSON envelope and checks `protocol`, `action`, `session_id`, `generation`, `operation_id`, `outcome` and `result`. Diagnostics are written to stderr and never mixed into an archive stream from `export`. Admission drain blocks new acquisition and restart admission while permitting status, retrieval and release, and exits only after the control process has durably recorded the new admission state. A restart that has crossed the short admission fence before drain may finish; the native stop/start work does not hold the global admission lock.
