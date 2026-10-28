---
status: accepted
---

# Agent VM verified native-init absence recovery

## Context

The Go control service records the durable task before invoking Incus. If native initialization fails before an owned instance exists, the task can retain a failed or closing state, reservations and a pending acquire journal even though no native object remains. The ordinary release path must fail closed when ownership cannot be verified, so this residue cannot converge by retrying ordinary cleanup alone.

The recovery boundary is narrow. A quota or project-policy change is not evidence that a historical native effect was absent, and a missing or malformed native query cannot be converted into absence. Pending operations also carry identity and request-digest evidence that must remain tied to the original session and generation.

## Decision

Reconciliation may recover this residue only when all of the following are true:

- The durable task is `failed` or `closing` and its structured failure phase is exactly `native-init`.
- The session lock reread confirms the same generation and a still-retryable task state.
- At least one pending operation for the session is present in the current or immediately previous generation.
- Incus authoritatively reports the task's managed instance as missing with an instance-specific diagnostic, and all journaled native workers have been observed exited. A confirmed pre-spawn failure may use an explicit `pre_spawn=true` exited marker without a process identity; an exited worker with a missing or non-positive process group remains blocked. A successfully returned present instance, including one with matching managed ownership, a foreign instance, unavailable or malformed identity, a timed-out, cancelled or truncated query, a generic project/remote/HTTP not-found diagnostic, or an unverified/running worker remains blocked.
- The durable port table is empty.
- Every pending operation for the session in the current or immediately previous generation is an `acquire`, `release` or `destroy` journal with a complete request, matching operation/session/generation identity and a canonical request digest. A generation-zero acquire for a newly allocated task after a deleted-session tombstone is accepted only when its exact operation ID is recorded in the native-init failure detail. Unknown actions, missing fields, digest mismatches and multiple pending acquire intents remain blocked.

The operation currently executing the maintenance request is excluded only by its exact operation ID while that request holds the maintenance lock; it is not treated as lifecycle evidence. Dispatch finalizes that maintenance journal after reconciliation. A destroy records its terminal outcome before deleting an absent task row; age-GC has no public operation journal and applies the same durable fence before removal, so a crash leaves durable state visible rather than a hidden pending operation.

After validation, reconciliation atomically finalizes all allowed terminal journal outcomes with the existing conditional task update fence that sets the task to `absent`, clears resource reservations, VM address, SSH port, lease and cleanup fields, and preserves the generation. A transaction failure rolls back both the journal outcomes and task transition. A pending destroy paired with an already verified zero-resource `absent` row is finalized and then removes that row during restart reconciliation under the same identity fence; its initial unattempted worker-preparation marker is consumable only because this branch proves there was no native effect. A repeated pass is idempotent and does not create an operation ID, replay a native effect or reset generation metadata. A recovered task is a zero-resource, port-free `absent` row: an explicit destroy may conditionally remove it, and a generation-zero or exact current absent-generation acquire may conditionally remove the row before allocating the next tombstoned generation; residual durable state remains blocked.

This exception reuses the existing protocol-4 action and journal formats. It does not add a public recovery action, schema migration, project quota change, profile change or broad garbage collector. It also does not delete durable port rows when they exist; port residue is evidence that requires explicit cleanup or inspection.

## Consequences

- A deterministic native-init absence can release stale admission reservations without claiming that an instance existed and was deleted.
- Foreign or present identity, uncertain native access, generic not-found diagnostics, unverified workers, malformed journals, residual ports and concurrent generation changes remain visible and fail closed.
- Journal finalization and the conditional task update share one SQLite transaction. Any failed conditional update or journal identity check leaves the task and all journals pending for explicit inspection; no native effect is replayed or inferred from an empty journal set.
- The behavior is compatible with protocol 4, existing public actions, SQLite schema and generation semantics. It must be validated with deterministic lifecycle fixtures and a disposable real-Incus Agent VM scenario.

## Related decisions

- [Agent VM topology and recovery boundary](0028-agent-vm-topology-recovery-boundary.md)
- [Native Incus Agent container lifecycle](0038-native-incus-agent-container-lifecycle.md)
- [Agent VM Incus project initialization baseline](0041-agent-vm-incus-project-initialization.md)
