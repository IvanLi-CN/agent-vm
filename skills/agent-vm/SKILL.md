---
name: agent-vm
description: Use a session-owned Incus Linux VM for code execution, Docker/Compose, heavy validation, source transfer and online retrieval. Service or host maintenance uses agent-vm-ops.
---

# Agent VM

Use the installed `agent-vm` CLI. Read [the command contract](../../docs/specs/agent-vm/contracts/cli.md) for command details; the bundled client source is [scripts/agent_vm.py](scripts/agent_vm.py). If the CLI, session identity, protocol or native service is missing, report the exact failure. Do not install infrastructure, revive a retired backend or use local Docker without the owner's explicit authorization.

## Start and work

Require matching protocol 4 client/control. Run `agent-vm doctor` and `agent-vm status`; provide a stable session with `--session-id <id>` or `AGENT_VM_SESSION_ID` before any remote mutation, then use `agent-vm acquire` to allocate or reuse that session's unique VM. Preparing, cleanup and cached objects also occupy its one slot. Inspect server state and deadline, not the laptop clock. Defaults are Debian 13, 2 vCPU, 4 GiB RAM and a 20 GiB sparse disk.

For a new VM use `acquire --cpu 8 --memory 12GiB --disk 40GiB` when the project needs those resources. Omitted fields retain a stopped cache's sizes; explicit fields adjust it before startup, with disk growth only. An active lease accepts identical explicit sizes; a difference requires `resize` and never silently interrupts the guest. Check both `effective_lease` and `guest_available`: an effective lease with an unexpectedly stopped guest is unavailable. Inspect the reported cause before explicitly restarting; do not infer OOM from shared CT counters.

The first lease lasts 30 minutes. Explicit `agent-vm renew` adds 15 minutes up to two hours remaining. Acquire, SSH, queries, execution and transfer never renew; renew before expiry when the task needs more time. Expired renewal fails and lease end closes mappings/flows and stops the VM. There is no promise that expired files can be recovered.

Use `agent-vm ssh` for normal root SSH and `agent-vm exec -- <argv>` for commands. For shell syntax call a shell explicitly, for example `agent-vm exec -- sh -lc 'cd /workspace && make test'`. Guest root, ordinary files, background processes and the project's own Docker/Compose workflow are available. This does not authorize CT/PVE maintenance, Incus socket use or management of other sessions.

## Retain and renew

Keep this session's VM while the current task is likely to use it again, including reuse of build caches. Finishing one validation, waiting for CI/review/owner feedback, publishing a PR or reaching a readiness handoff does not alone end that need. When handing back a retained VM, state its expected follow-up use and server deadline.

While actively progressing or tracking the task, use explicit `agent-vm renew` before the server deadline when the remaining lease will not cover expected follow-up work; a guest command need not be running. Keep the existing renewal limits and respect drain refusals. Returning control to the owner or interrupted execution carries no promise of continued renewal. Do not create a background keepalive. Expiry is the fallback for abandoned or interrupted work; use voluntary release at actual closure below.

## Source, ports and capacity

Use `agent-vm sync --name <safe-name>` or `python3 <loaded-skill-dir>/scripts/sync_worktree.py --name <safe-name>` from the Git worktree. The helper writes only `/workspace/worktrees/<name>/src`; keep results beside `src`. It selects tracked/unignored files, replaces only its own source directory, excludes `.git` and rejects submodules before writes. For submodules or other data use explicit native upload/download or trusted SSH/rsync. Normal SSH is not limited to the helper path.

Native clients use `agent-vm ssh-info --format ssh-config`, saved with mode 0600, then `ssh -F <config> agent-vm` (or rsync with that configuration). Keep host checking enabled and consume the returned port/key settings; do not assemble permanent ports or gateway routes. Delete temporary configuration when finished.

Use `agent-vm port add <name> --guest-port <port> [--protocol tcp|udp]` for direct access through the configured control host and allocated port; this is transport mapping, not application authentication. Inspect `port list`, and remove a mapping explicitly when no longer needed. Lease end removes all mappings; acquiring a new lease restores SSH alone.

Use `resize` for project resource needs, with at least one of CPU, memory or disk. The default path, including CPU/disk changes, requires restart: inspect the reported plan and give `--allow-restart` only when interrupting current guest processes is acceptable. Disk only grows. Check reported virtual and guest filesystem sizes separately. Resize does not renew. Insufficient CT-local capacity may reclaim other sessions' eligible stopped expired caches automatically under the configured policy; effective leases are protected. If local capacity remains insufficient or cannot be measured, report the denial instead of bypassing admission. Do not request PVE access or inspect its storage.

After a large-memory build or experiment, check current and background workloads. If remaining work fits a smaller size, explicitly use `agent-vm resize --memory <needed-size> --live`, for example `4GiB` when adequate, to return memory while preserving this VM and its build cache. Live mode accepts memory alone and never falls back to restart. For reduction only, the specific shrinking-balloon timeout permits one fixed 45-second wait followed by at most one guarded same-target completion attempt; inspect `native_attempts`, which preserves any first failure. A successful native result verifies guest continuity and lowers accounting; inspect the result and status. Linux MemTotal may remain unchanged after ballooning. Live reduction remains available under CT memory pressure; live growth still uses normal admission. A partial failure keeps conservative accounting and needs inspection plus explicitly allowed restart recovery or release, not repeated blind live changes. Resize never renews; renew explicitly before a restart-capable adjustment when less than five minutes remain, or before live memory adjustment when less than three minutes remain. Never reduce another session or shrink while its own ongoing workload needs the allocation.

CPU and RAM use configurable allocation ceilings derived from their budget and overcommit ratio; the public client does not assume a deployment-specific value. Stopped caches do not consume CPU or memory allocation ceilings. Memory admission is reservation-only: logical remaining memory is `budget * overcommit_ratio - allocated_and_reserved`, while low CT-visible headroom is reported as pressure and does not reject a request within the logical ceiling. Missing or contradictory CT capacity metrics still fail closed as `resource_unverified`; inspect structured admission diagnostics when refused. The project has no global VM-count ceiling; stopped caches still consume disk space. `doctor.ready` describes component health; `admission_ready` describes direct admission of a default new VM at the observation time. Inspect `default_admission.reasons` and actual requested sizes; an existing lease may remain usable even when new allocation is refused. Existing leases can still renew, retrieve and release during pressure. Overselling permits logical allocations beyond CT-visible memory but does not guarantee simultaneous peak use or protection from native OOM; never bypass control or stop another effective lease.

## Portable observability bundle

The public observability bundle lives under [references/observability](references/observability/README.md). It contains a parameterized Grafana dashboard template, a file-provider provisioning template, an example configuration, and Bun render/validation commands. Render it into the deployment repository with `scripts/render_observability_dashboard.ts`; keep the rendered JSON, provisioning file, datasource identity, controller job, collector configuration, and host paths in that deployment repository.

The template uses a hidden `controller_job` variable for native controller queries, reads CPU capacity from `agent_vm_admission_cpu_capacity_vcpu`, and does not encode a deployment's memory or disk policy as a fixed label. `scripts/validate_observability_dashboard.ts` checks the required metric/panel contract, automatic refresh, datasource replacement, and packet-rate expressions before rendered files are written.

## Finish

Enter closure when the PR has merged and no further VM work is expected, a non-PR task is complete without expected VM follow-up, or the task is cancelled or confirmed to end further VM use. Complete any post-merge VM verification first. If only remote CI or release tracking remains, release the VM and continue that tracking through its existing workflow contract. A phase boundary or readiness handoff alone is not closure.

Retrieve required files online with download, native SSH/rsync or `agent-vm export <path> > <local.tar>`. Export streams directly; there is no server archive store. Resolve an uncertain synchronization commit by inspecting its reported recovery materials; do not pretend success or blindly roll it back.

Inspect `agent-vm status` before closure. For an effective lease, finish guest work, use `agent-vm release`, then inspect status again to confirm no effective lease, closed mappings and stopped/cache state. For an already-ended lease, report the observed cause when known and current cleanup state. A dispatched command or observed expiry is not proof of successful voluntary release. Do not acquire or renew merely to manufacture closure. If cleanup is incomplete, report the observed state and blocker; infrastructure recovery belongs to an explicitly authorized operations task.

Release retains a disposable disk, eligible for reclamation immediately after confirmed cleanup, with no minimum retention time. The 24-hour threshold selects the age sweep; capacity demand or explicitly authorized manual GC can reclaim it earlier. Duplicate release does not refresh its age. Use `destroy` only with explicit authorization to discard the current VM's data. Infrastructure reconciliation or reclaim administration belongs to `$agent-vm-ops` and a maintenance task.
