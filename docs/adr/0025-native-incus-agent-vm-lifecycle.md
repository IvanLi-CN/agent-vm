---
status: accepted
---

# Native Incus Agent VM lifecycle

Agent execution uses one Incus VM per generic session identity, with root SSH through native proxy NAT and explicit leases. The deployment supplies the control host, Incus project, profile, image, network, ACL, address pool and local storage through configuration; the public implementation contains no environment inventory.

Lifecycle enforcement uses one-shot control actions and persistent native timers. Lease end closes owned NAT mappings and exact conntrack flows before stopping the guest, then retains its disk as a disposable stopped cache. Age-based and capacity-triggered cleanup can reclaim an eligible cache, while effective leases are protected.

The design excludes persistent custom services, gateways, implicit keepalives and shared host paths. Native components enforce ownership and cleanup; Python entrypoints execute bounded actions. A cached guest may be reused but carries no recovery guarantee.
