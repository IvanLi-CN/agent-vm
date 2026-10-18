---
status: accepted
---

# Agent VM topology and recovery boundary

## Context

The Agent VM lifecycle is verified through a configured SSH control entry and one session-owned Incus VM inside the deployment's CT109 LXC. That evidence establishes the control path and the CT-to-VM relationship, but it does not identify the physical or virtual machine that creates or hosts CT109, the infrastructure manager that can rebuild it, or the storage system below the CT.

A control entry is an address and access path, not a provenance record. Treating it as the underlying host would make a host-unavailable incident look like a known rebuild path and could cause recovery to delete or recreate data without a verified owner or generation.

## Decision

The public contract names five layers: the local Codex client, the configured Agent VM control endpoint, the observed CT109 LXC control environment, the session-owned Incus VM inside CT109, and an explicitly unknown underlying hosting source. The concrete private control address, deployment inventory, credentials, filesystem paths, revision hashes, and storage provider remain in protected operator records rather than this repository.

The configured control endpoint is never treated as the physical host, the CT owner, the Incus host, or the storage source. CT109 is not substituted for the control endpoint, and Incus is not substituted for either of them. PVE, a hypervisor, an external storage system, and a host rebuild API are not assumed unless an owner-authorized external record proves them.

Control-plane recovery must preserve the matching client/control revision, canonical configuration, state database and WAL/journals, operation and audit records, generations, timer assets, protected connection material, and the Incus pool/VM data. Recovery restores matching components, enters or confirms drain, reconciles durable records against native ownership and generations, and reopens admission only after all state and capacity checks pass. A pending operation, generation mismatch, missing state, unknown native resource, or unavailable provenance is a stop condition; recovery must not invent IDs, reset generations, replay uncertain native effects, or perform broad cleanup.

Disk admission is intentionally sparse and oversold: it measures the configured CT-local Incus pool filesystem and accounts for request cost and durable reservations, not a full physical reservation equal to each VM's virtual disk size. Control-plane state and VM data remain separate logical failure domains without a promise of separate physical media. ENOSPC or I/O failures preserve conservative state and block unsafe admission; they do not justify evicting an effective lease or guessing the underlying storage source.

## Consequences

- Operators have an executable recovery boundary without a speculative PVE, hypervisor, CT-host, or storage procedure.
- The existing `admin status`, `admin session list`, `admin reconcile`, and drain commands remain the only public control surfaces for reconciliation and admission; no new deployment tool or host inventory is added.
- A deployment that lacks authoritative CT/rebuild provenance or durable state cannot be declared recovered. The correct handoff is an explicit blocker with the evidence required to continue.
- Public documentation can describe verified roles and failure handling without publishing a private address, personal path, credential, or live production inventory.

## Related material

- [Native Incus Agent VM specification](../specs/agent-vm/SPEC.md)
- [Agent VM topology and recovery runbook](../specs/agent-vm/OPERATIONS.md)
- [Native Incus Agent VM lifecycle](0025-native-incus-agent-vm-lifecycle.md)
- [Agent VM CPU and memory overselling](0026-agent-vm-cpu-memory-overselling.md)
