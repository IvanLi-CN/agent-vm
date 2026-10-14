# Agent VM retention and voluntary release

This accepted design explains why Agent VM retention follows expected task continuation rather than individual validation phases. [Agent VM specification](SPEC.md#lifecycle) is the normative lifecycle contract; [agent-vm CLI contract](contracts/cli.md) defines the public operation boundary. This document is design background, not another operating entrypoint.

## Retention and closure boundary

Releasing after every validation would turn build-cache reuse into a race with global reclamation and repeat expensive setup during one continuing task. The accepted boundary is actual task closure: PR merge without further VM work, completion of non-PR work without expected follow-up, cancellation or a confirmed end to VM use. Readiness handoffs and ordinary waits do not establish that boundary by themselves.

An expected continuation is a reason to retain an effective lease, not an independent retention guarantee. An actively progressing Agent may explicitly renew for that use; execution interruption or returning control does not provide ongoing renewal. Expiry remains the fallback. The existing native lease and disposable stopped-cache semantics are unchanged.

## Responsibility and evidence

[agent-vm](../../../skills/agent-vm/SKILL.md#retain-and-renew) owns the retention, renewal and Finish procedure. [project-engineering-workflow](../../../skills/project-engineering-workflow/SKILL.md#step-6-post-merge) invokes it at the relevant phase and closure points. This keeps one operating policy while preserving the engineering flow's independent review, CI and publication duties.

[Implementation coverage](contracts/cli.md#reclamation-and-retry-behavior) records the public retry and cleanup boundary separately from native deployment and live-model evidence. Closure is established by observed lease/access/guest state; a command dispatch or prior expiry cannot substitute for that observation. Existing data-loss authorization remains unchanged.

This document clarifies retention policy only; it does not add a CLI, control-process, timer or deployment requirement.
