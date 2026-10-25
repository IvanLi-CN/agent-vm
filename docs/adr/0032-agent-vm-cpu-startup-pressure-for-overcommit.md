---
status: accepted
supersedes: 0031-agent-vm-cpu-startup-pressure-boundary.md
---

# Agent VM CPU startup pressure for overcommit

## Context

The Agent VM deployment deliberately oversells CPU. Logical reservation answers whether a request fits the configured policy ceiling; CT-local CPU pressure answers whether a new QEMU process is likely to obtain enough scheduler time to become ready. Treating those observations as the same limit makes the deployment reject valid oversubscribed requests too early, while ignoring scheduler pressure makes readiness failures opaque.

The previous startup guard used a `full.avg10` threshold of 20 percent. That threshold is too restrictive for a deployment whose policy accepts CPU contention and does not promise uninterrupted CPU service to guests. The logical CPU ceiling remains an independent admission boundary.

## Decision

The approved deployment policy sets `cpu_startup_pressure_full_avg10_percent` to `50`. The public configuration schema remains configurable and the reusable example remains a generic default; `50` becomes effective only when the operator installs and verifies that deployment configuration.

The startup guard retains this compound condition:

```text
projected CPU allocation > CT-visible CPU capacity
AND verified CPU PSI full.avg10 >= 50 percent
```

When both conditions hold, acquire, cached resume, restart-capable resize and explicit restart fail before native boot with structured `resource_pressure`. The guard does not change the logical CPU ceiling, subtract pressure from logical remaining CPU, stop an effective lease, or block read, renewal, retrieval, release or live memory-only resize.

`some.avg10` and `full.avg10` remain separate observations. `some.avg10` indicates that at least one runnable task was waiting for CPU service; `full.avg10` indicates that all non-idle work was stalled. Neither is CPU utilization or an allocation count. Missing or contradictory PSI remains an observation failure and is never converted to zero.

## Consequences

The approved policy permits more starts while `full.avg10` is between 20 and 50 percent, accepting that guest readiness may take longer under contention. A request that exceeds CT-visible CPU capacity is still blocked when the verified full-pressure threshold is reached. Logical CPU headroom and runtime scheduler pressure must therefore be shown as different values in operations and dashboards.

This policy does not guarantee guest performance, remove disk/memory/drain/integrity gates, or create a host-level CPU guarantee. Changing the threshold is a deployment-policy change and requires updating the installed configuration, the observation source and the deployment's acceptance evidence together.
