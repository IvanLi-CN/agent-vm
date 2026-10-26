---
status: superseded
superseded_by: 0038-native-incus-agent-container-lifecycle.md
---

# Native Incus Agent VM lifecycle (historical)

This historical decision used one Incus VM per generic session identity. It is superseded by [the native Incus Agent container lifecycle](0038-native-incus-agent-container-lifecycle.md), which uses an Incus system container and does not require KVM or QEMU. The SSH, proxy NAT and lease rules below remain relevant where they do not depend on the old instance type.

Lifecycle enforcement uses one-shot control actions and persistent native timers. Lease end closes owned NAT mappings and exact conntrack flows before stopping the guest, then retains its disk as a disposable stopped cache. Age-based and capacity-triggered cleanup can reclaim an eligible cache, while effective leases are protected.

The design excludes persistent custom services, gateways, implicit keepalives and shared host paths. Native components enforce ownership and cleanup; Python entrypoints execute bounded actions. A cached guest may be reused but carries no recovery guarantee.
