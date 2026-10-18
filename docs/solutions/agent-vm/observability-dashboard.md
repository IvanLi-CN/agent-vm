---
title: Provision an Agent VM Capacity and Failure Dashboard
module: agent-vm
problem_type: observability
component: grafana-prometheus
tags:
  - grafana
  - prometheus
  - incus
  - capacity
  - admission
status: active
related_specs:
  - docs/specs/agent-vm/SPEC.md
  - docs/specs/agent-vm/contracts/observability.md
---

# Context

The `incus-agent-vm` dashboard must explain why VM creation can fail even when an Incus data pool appears to have free space. It needs to show the CT root filesystem, transient image-unpack staging, physical pool usage, guest rootfs allocation, native admission and cleanup failures as separate facts.

# Symptoms

- The dashboard reports guest filesystem quota but cannot show the filesystem used by `/var/lib/incus/images`.
- `incus_project_limit` is `-1` for CPU, memory, disk or instance count, so project metrics do not provide native admission ceilings.
- Image creation reports `Unable to unpack image, run out of disk space` while the final Incus pool still reports available space.
- The dashboard is provisioned and its runtime copy cannot be saved safely because provisioning will overwrite it.
- Auto-refresh was disabled by default and packet-per-second panels multiplied counts by `8`.

# Root cause

Incus pool metrics and guest filesystem metrics describe different layers. Image unpacking uses the CT-local staging path before final instance storage is allocated, while the native controller keeps admission reservations and state in a separate control boundary. A dashboard that uses only Incus project or guest metrics cannot explain either failure domain or the native limit.

The current public `agent-toolkit` checkout owns the native controller and its public contracts, but it does not own the `incus-agent-vm` Grafana provisioning JSON or a Prometheus scrape bridge for native controller metrics. Updating a Grafana database copy would therefore be temporary and would not be a repository implementation.

The deployment source was located in the operator-owned `obs` stack. Its dashboard path is `grafana/dashboards/incus/incus-agent-vm.json`, and its provisioning provider is `grafana/provisioning/dashboards/incus.yml`; the Grafana service mounts both paths read-only. The recovered dashboard snapshot is useful as the starting point for the owning deployment repository, but it must not be copied into this public repository. If that snapshot is untracked, version it in the owning repository before applying the dashboard changes.

# Resolution

## Repository-owned contract

Use [the observability contract](../../specs/agent-vm/contracts/observability.md) as the source for metric names, authority boundaries, line rules and verification. It deliberately separates:

- CT root and `/var/lib/incus/images` filesystem capacity.
- Physical Incus pool total, used and free bytes.
- Guest rootfs allocation and quota.
- Native CPU, memory, disk and VM-slot allocation, reservation, remaining and limit.
- Admission readiness, reclaim-required, draining, operation failures, image-unpack failures, warnings and task states.

## Deployment implementation

1. In the owning deployment repository, first import and version the recovered `incus-agent-vm.json` and `incus.yml` source files. Record their checksum and source revision in the deployment maintenance record; do not edit the Grafana database copy.
2. Add the native read-only scrape bridge or textfile collector and expose the metric families in the observability contract. Do not add a resident HTTP broker to the one-shot controller solely for dashboard access.
3. Add CT root and `/var/lib/incus/images` filesystem series to the Prometheus target. Do not derive them from `incus_project_limit`, guest rootfs size or pool free space.
4. Update the provisioned dashboard JSON: enable automatic refresh by default, add the capacity/admission row, rename the existing guest disk panel to `Guest rootfs allocation`, add physical pool and staging panels, add failure/state panels, and remove `* 8` from packet-per-second queries.
5. Configure horizontal dashed limit lines only from authoritative metrics or explicit policy thresholds. Do not draw a line for a missing or `-1` Incus project limit.
6. Reload provisioning and verify the dashboard through Grafana. Confirm that the dashboard metadata remains provisioned, every panel returns fresh data, and an image-unpack failure can be distinguished from pool exhaustion, CT-root exhaustion and guest-level ENOSPC.

## Current memory lines

The current CT-visible memory limit is `32768 MiB` (`32 GiB`). The declared deployment policy is a fixed `2x` memory target, so the dashboard renders a separate `65536 MiB` (`64 GiB`) policy line. The 64 GiB line is an explicit policy threshold, not proof that the controller exports a live limit; the native value must come from `agent_vm_admission_limit` when that metric is available. The repository's generic configuration example has its own defaults and must not be used as evidence for the deployment policy.

The capacity view must show the actual CPU and memory walls separately from policy targets, and must show physical pool available bytes separately from guest rootfs allocation. The answer to “how large can the next VM be?” comes from native `allocated`, `reserved`, `remaining` and `limit` metrics across CPU, memory and disk, together with the controller's readiness and reclaim state. Pool free bytes alone are only physical headroom, not a complete admission result.

## Acceptance

The dashboard is considered complete only when its source is versioned in the owning provisioning repository and the following are observable without host-manager access:

- CT root and image-staging used/available bytes.
- Physical pool total/used/free bytes.
- Guest rootfs allocation shown separately from physical usage.
- Allocated, reserved, remaining and limit values for CPU, memory, disk and VM slots.
- Admission ready, reclaim-required and draining state.
- Operation success/failure by phase, image-unpack failures, warnings, uptime, scrape freshness and task-state counts.
- Horizontal dashed lines for every available authoritative limit, with no invented line for `-1` or missing values.

# Guardrails / Reuse notes

- Do not edit a provisioned Grafana database and treat the result as a durable implementation.
- Do not use Incus project quota metrics as native admission limits when the values are `-1`.
- Do not label guest rootfs allocation as physical disk usage.
- Do not infer CT root or image-staging capacity from Incus pool capacity.
- Do not multiply packet-per-second queries by `8`.
- Keep deployment addresses, credentials, dashboard datasource identity and collector paths in the deployment repository or protected operator records.

# References

- [Agent VM observability contract](../../specs/agent-vm/contracts/observability.md)
- [Agent VM topology and recovery runbook](../../specs/agent-vm/OPERATIONS.md)
- [Native deployment boundary](../../../skills/agent-vm-ops/references/deployment.md)
