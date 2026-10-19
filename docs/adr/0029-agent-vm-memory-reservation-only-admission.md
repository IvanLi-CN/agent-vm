---
status: accepted
supersedes: 0026-agent-vm-cpu-memory-overselling.md (memory admission pressure only)
---

# Agent VM memory reservation-only admission

## Decision

Agent VM memory admission uses a logical reservation ceiling, not current CT runtime headroom. The deployment policy is a `32768 MiB` CT-visible memory capacity with `memory_budget_mib=32768` and `memory_overcommit_ratio=2`, producing a `65536 MiB` logical reservation limit. A request is admitted when its conservative reservation fits within that logical limit and all other admission gates pass.

The controller reports `allocated`, `reserved`, `allocated_and_reserved`, `remaining` and `limit` separately. `remaining` is `limit - allocated_and_reserved`; it is not `MemAvailable` and never claims that the logical ceiling is physically resident.

## Runtime pressure

The controller continues to observe CT-local `memory.current`, verified LXCfs `MemAvailable`, a pressure threshold and cgroup OOM events. Low `MemAvailable` sets `memory_pressure` and produces a warning for operations and dashboards. It does not reject acquire, resume, restart-resize or memory growth while logical reservation remains.

Missing, malformed or contradictory CT capacity measurements still fail closed with `resource_unverified`. Disk capacity, drain state, native inventory and project-policy conflicts remain independent admission gates. The policy does not increase CT memory, promise simultaneous physical residency of 64 GiB, add a balancer, or introduce a resident metrics service.

## Observability contract

The read-only `admin-metrics` protocol action exposes the policy, split reservation totals, CT-local memory observations, OOM state, task states, operation/failure summaries, warnings and freshness. A deployment-owned collector may invoke this action through the configured control entry and expose stable Prometheus families. Grafana must render the 32 GiB CT capacity and 64 GiB logical limit as separate authoritative lines and display runtime pressure separately.

## Consequences

The controller can continue logically valid VM operations while CT runtime headroom is low; operators must use the pressure and OOM signals to investigate physical stress. A logical admission success is not a promise that a guest will avoid reclaim, throttling or OOM. This decision keeps logical capacity and runtime pressure in one consistent vocabulary and removes the previous ambiguity where a pressure sample appeared to reduce the reservation ceiling.
