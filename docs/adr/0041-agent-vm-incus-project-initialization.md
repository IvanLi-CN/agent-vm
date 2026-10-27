---
status: accepted
---

# Agent VM Incus project initialization baseline

Agent VM uses an Incus project with stopped cache instances. Incus project-level aggregate limits count those instances, so a process quota can become an implicit instance-count ceiling even when the control service's CPU, memory and disk admission still has capacity. The existing profile-level process limit remains useful as per-instance protection.

The deployment baseline therefore leaves the Incus project aggregate keys `limits.processes`, `limits.cpu`, `limits.memory` and `limits.instances` unset. The Agent VM profile retains an explicit per-instance `limits.processes` value of `512`. CPU, memory and disk admission remain owned by the control service; profile devices and deployment-specific isolation settings remain operator-owned and are only inspected by the initializer.

Initialization is a separate, authorized maintenance operation. It is documented in [the Agent VM initialization reference](../../skills/agent-vm-ops/references/initialization.md) and automated by the read-only-by-default [Incus initializer](../../skills/agent-vm-ops/scripts/agent_vm_incus_init.ts). Normal client requests, reconciliation and garbage collection do not edit Incus project policy.

This decision removes the implicit aggregate process bottleneck without deleting instances, changing deployment identities, or expanding the control service's logical resource ceilings. Live deployment values and evidence remain outside the repository.
