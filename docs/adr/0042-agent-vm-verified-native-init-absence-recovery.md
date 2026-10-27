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
- Incus authoritatively reports the task's managed instance as not found. Present, foreign, unavailable and malformed native identity remains blocked.
- The durable port table is empty.
- Every pending operation for the session in the current or immediately previous generation is an `acquire`, `release` or `destroy` journal with a complete request, matching operation/session/generation identity and a canonical request digest. Unknown actions, missing fields, digest mismatches and multiple pending acquire intents remain blocked.

After validation, reconciliation finalizes the allowed terminal journal outcomes first. It then uses the existing conditional task update fence to set the task to `absent`, clear resource reservations, VM address, SSH port, lease and cleanup fields, and preserve the generation. A repeated pass is idempotent and does not create an operation ID, replay a native effect or reset generation metadata.

This exception reuses the existing protocol-4 action and journal formats. It does not add a public recovery action, schema migration, project quota change, profile change or broad garbage collector. It also does not delete durable port rows when they exist; port residue is evidence that requires explicit cleanup or inspection.

## Consequences

- A deterministic native-init absence can release stale admission reservations without claiming that an instance existed and was deleted.
- Foreign identity, uncertain native access, malformed journals, residual ports and concurrent generation changes remain visible and fail closed.
- Journal finalization precedes the conditional task update. A failed conditional update leaves the task and terminal journal outcomes as durable evidence for explicit inspection; no native effect is replayed or inferred from an empty journal set.
- The behavior is compatible with protocol 4, existing public actions, SQLite schema and generation semantics. It must be validated with deterministic lifecycle fixtures and a disposable real-Incus Agent VM scenario.

## Related decisions

- [Agent VM topology and recovery boundary](0028-agent-vm-topology-recovery-boundary.md)
- [Native Incus Agent container lifecycle](0038-native-incus-agent-container-lifecycle.md)
- [Agent VM Incus project initialization baseline](0041-agent-vm-incus-project-initialization.md)
