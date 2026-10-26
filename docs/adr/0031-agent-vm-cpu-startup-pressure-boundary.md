---
status: superseded
supersedes: 0026-agent-vm-cpu-memory-overselling.md (CPU startup scheduling boundary only)
superseded_by: 0032-agent-vm-cpu-startup-pressure-for-overcommit.md
---

# Agent VM CPU startup pressure boundary

## Context

Logical CPU overcommit answers whether a request fits the configured reservation ceiling. It does not prove that the CT scheduler can give a newly started managed-container init process enough runtime to complete guest-agent readiness. A CT can therefore accept a logically valid request while CPU PSI reports sustained scheduler starvation; the result is a long readiness timeout rather than a useful admission error.

## Decision

The controller observes CT-local CPU PSI separately from logical CPU allocation. It accepts the signal only after verifying the cgroup v2 mount root, the single unified `/proc/self/cgroup` entry, and `cpu.pressure` beneath that current cgroup path. Before `acquire`, cached resume, restart-capable resize, or explicit restart changes native boot state, it compares projected CPU allocation with CT-visible CPU capacity. When projected allocation is above that capacity and `full.avg10` is at least `cpu_startup_pressure_full_avg10_percent`, the operation fails before native start with `resource_pressure` and includes the threshold, observed PSI, projected CPU, and CT capacity. A missing, malformed, contradictory, or non-CT-local PSI observation fails startup admission with `resource_unverified`.

The former default startup-pressure threshold was 20 percent and was configurable in the deployment configuration. This is a schedulability guard, not a CPU allocation ceiling. It does not change logical remaining CPU, stop an effective lease, or block read, renewal, retrieval, release, or live memory-only resize. Restart checks occur before the existing guest is stopped. Missing or malformed PSI is reported as an observation failure and is never coerced to zero. The approved overcommit deployment policy supersedes this threshold choice; see [the successor ADR](0032-agent-vm-cpu-startup-pressure-for-overcommit.md).

## Consequences

New container starts fail quickly and explain why they cannot be expected to become ready under current scheduler pressure. The guard may refuse a logically admissible managed guest while pressure is high, so callers should retry the same user intent after pressure clears; they must not invent a new operation ID for an uncertain mutation. Runtime CPU pressure remains visible independently from reservation headroom.

The guard does not guarantee guest readiness under every workload. It only closes the known gap between logical CPU overcommit and severe CT-local scheduler starvation. CPU capacity, disk capacity, drain state, native inventory and logical reservation checks remain independent gates.
