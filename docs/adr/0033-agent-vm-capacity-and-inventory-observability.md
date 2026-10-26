---
status: accepted
---

# Agent VM capacity and inventory observability

## Context

One dashboard cannot make aggregate admission, CT runtime pressure, and per-task lifecycle state equally readable. Aggregate metrics answer whether the next request fits the logical and physical gates; task state answers which managed-container records consume those reservations and whether a failure or transition is blocking progress. Mixing them produces charts that look full without explaining the composition.

Incus does not provide an authoritative managed-container slot limit for every deployment. A dashboard must not invent one from an unset or `-1` project quota. It may provide a bounded snapshot estimate for the configured default container request, but the estimate must be labelled as derived and must not be presented as a native slot limit.

## Decision

The deployment provides two dashboards backed by the same authorized read-only observation bridge:

1. `Agent VM / Capacity & Admission` uses `admin-metrics` for aggregate resources, CT-local pressure, admission gates, failures and freshness.
2. `Agent VM / Inventory & Operations` uses `admin-status` for bounded per-task inventory, lifecycle state, lease/mapping state and last structured error information.

The capacity dashboard shows, for CPU, memory and disk, `allocated`, `reserved`, `allocated_and_reserved`, logical `remaining`, and logical `limit` where authoritative. It also shows CT-visible capacity, current runtime observations, pressure, OOM and startup blocking separately. The CPU view includes `some.avg10`, `full.avg10`, the configured startup threshold, and the `startup_blocked` state; pressure is not folded into logical remaining.

The capacity dashboard shows observed task-state counts and two derived default-container estimates:

- `logical default-container slots` is the minimum of the verified logical CPU, logical memory and physical disk headroom divided by the default request/cost, rounded down.
- `current startable default-container slots` applies the current CT-visible CPU capacity and startup-pressure gate to the same snapshot.

Both values are snapshot estimates. They are omitted when required inputs are unverified, and they are never called `safe max`, `available VM limit`, or an Incus slot limit. The default request and physical admission cost are read from the controller/bridge input, not hardcoded in a public dashboard.

The inventory dashboard includes active, preparing, closing, deleting, failed, cached and absent managed records, with filters and a default order of active, preparing, failed, closing/deleting, then cached and absent. Cached rows are collapsed by default. Its bounded table fields are task ID, instance name, session ID, state, native status, guest availability, requested CPU/memory/disk, effective-lease state, lease expiry, SSH mapping, and the last structured error code/phase. It never exposes raw error text, credentials or deployment filesystem paths.

The public repository owns the metric names, source schemas, generic dashboard template and validator. The operator-owned `obs` repository owns the rendered JSON, collector, datasource, controller job, provisioning path and live rollout. The one-shot controller remains the source of truth; no resident HTTP broker is introduced solely for dashboards.

## Consequences

Operators can distinguish logical reservation headroom, physical disk headroom, current runtime pressure and task-state composition without treating any one of them as the others. A current-startable estimate can fall to zero during a CPU startup-pressure event even when logical CPU headroom remains; this is an explicit snapshot of the startup gate, not a change to the logical policy.

The two dashboards require the collector to invoke two read-only actions and to remove stale per-task series on every successful scrape. A failed action must expose `up=0` and freshness failure rather than fabricated zeros. The split adds a small amount of provisioning work but keeps the operational questions separate and testable.
