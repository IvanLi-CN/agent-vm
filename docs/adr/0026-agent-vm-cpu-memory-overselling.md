---
status: superseded
---

# Agent VM CPU and memory overselling

Agent VM permits configurable CPU and memory overselling while separating logical allocation ceilings from observed local capacity. A guest's configured vCPU and memory are limits, not a promise of exclusive physical capacity. The memory pressure gate in this ADR is superseded by the reservation-only memory policy in ADR-0029; the remaining CPU, disk, lifecycle and failure-boundary decisions remain historical context.

The control counts admitted non-cached objects and pending reservations atomically. Stopped caches consume disk but no CPU or memory allocation budget. New creation, stopped-cache restoration and growth fail when local memory metrics are unavailable or contradictory, or when the logical reservation ceiling is exceeded. Low local memory headroom is an observation and warning, not a memory reservation gate. Queries, renewal, retrieval and release of an effective lease remain available.

The base budgets must fit the configured control environment; their multiplied ceilings may exceed it. CPU and memory ratios are independent and explicit. Native Incus project quotas that count stopped caches must not be used as the active allocation ceiling. Existing ownership, lifecycle, disk and transfer contracts remain in force.

Memory-only live resize keeps the old reservation until native acknowledgement and guest continuity are verified. A single bounded convergence retry is allowed for the documented native timeout; there is no polling loop or third attempt. Partial changes preserve conservative accounting and expose recovery state.
