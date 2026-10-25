---
title: Provision Agent VM Capacity and Failure Dashboards
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

The public `agent-toolkit` checkout owns the native controller, its public contracts, and a parameterized Grafana baseline under `skills/agent-vm/references/observability/`. Updating a Grafana database copy is still temporary, but the public template plus renderer is a repository implementation that can be instantiated by each deployment.

The dashboard also lacked a clear CPU startup-admission explanation: logical CPU reservation, CT-visible CPU capacity and CT-local PSI were shown as if they were one limit. The approved overcommit policy keeps the logical ceiling independent and uses `full.avg10 >= 50%` only when projected CPU is above CT-visible capacity. The `some.avg10` and `full.avg10` series are pressure observations, not CPU usage or allocation counts.

The deployment source was located in the operator-owned `obs` stack. Its dashboard path is `grafana/dashboards/incus/incus-agent-vm.json`, and its provisioning provider is `grafana/provisioning/dashboards/incus.yml`; the Grafana service mounts both paths read-only. The recovered dashboard snapshot is useful as deployment evidence and as the visual baseline for the public template, but the deployment instance must remain in the owning repository. If that snapshot is untracked, version it in the owning repository before applying dashboard changes.

# Resolution

## Repository-owned contract

Use [the observability contract](../../specs/agent-vm/contracts/observability.md) as the source for metric names, authority boundaries, line rules and verification. It deliberately separates:

- CT root and `/var/lib/incus/images` filesystem capacity.
- Physical Incus pool total, used and free bytes.
- Guest rootfs allocation and quota.
- Native CPU, memory, disk and VM-slot allocation, reservation, remaining and limit.
- Admission readiness, reclaim-required, draining, operation failures, image-unpack failures, warnings and task states.
- Logical and currently startable default-VM snapshot estimates, with their request/cost inputs and explicit non-authoritative wording.

The rendered deployment is split into two dashboards:

- `Agent VM / Capacity & Admission`: aggregate reservation composition, CT-visible capacity, CPU PSI/startup gate, runtime pressure, disk/image staging and admission/failure state.
- `Agent VM / Inventory & Operations`: all managed task states, bounded per-task resources/lease/mapping data, structured error code/phase, and pending cleanup/operation state.

The first reads `admin-metrics`; the second reads `admin-status`. A read-only bridge must remove stale task rows after successful scrapes and expose `up=0` and freshness failure when either action fails. It must not add a resident HTTP broker.

## Deployment implementation

1. In the owning deployment repository, copy [the public observability config example](../../../skills/agent-vm/references/observability/observability.config.json.example), fill its deployment-owned values, set `dashboard_path` to the stable `current` directory inside the Grafana container, and render the two dashboard/provisioning outputs with `render_observability_dashboard.ts` or the deployment's documented split-render wrapper. The renderer publishes the dashboard pair through an atomic `current` symlink; install the provisioning YAML with a single temporary-file-plus-rename operation.
2. Add the native read-only scrape bridge or textfile collector and expose the metric families in the observability contract. Do not add a resident HTTP broker to the one-shot controller solely for dashboard access.
3. Add CT root and `/var/lib/incus/images` filesystem series to the Prometheus target. Do not derive them from `incus_project_limit`, guest rootfs size or pool free space.
4. Run `validate_observability_dashboard.ts` against the rendered files, then review the generated diff. Deployment-specific panels may be added in the owning repository when their data authority is documented.
5. Configure horizontal dashed limit lines only from authoritative metrics or explicit policy thresholds. Do not draw a line for a missing or `-1` Incus project limit.
6. Reload provisioning and verify the dashboard through Grafana. Confirm that the dashboard metadata remains provisioned, every panel returns fresh data, and an image-unpack failure can be distinguished from pool exhaustion, CT-root exhaustion and guest-level ENOSPC.

## Current memory lines

The current deployment's CT-visible memory capacity is `32768 MiB` (`32 GiB`) and its configured logical memory budget is also `32768 MiB` with a `2x` `memory_overcommit_ratio`, producing one authoritative logical limit of `65536 MiB` (`64 GiB`). The public template deliberately labels these series as `CT capacity` and `logical limit` and reads their values from the authoritative metrics; it must not bake this deployment policy into a reusable asset. The dashboard must not derive these values from PVE UI working-set values, a project quota of `-1`, or a pressure sample.

The capacity view must show the actual CPU and memory walls separately from policy targets, and must show physical pool available bytes separately from guest rootfs allocation. The answer to “how large can the next VM be?” comes from native `allocated`, `reserved`, `remaining` and `limit` metrics across CPU, memory and disk, together with the controller's readiness and reclaim state. Pool free bytes alone are only physical headroom, not a complete admission result.

The capacity view must also show the policy default VM request/cost, observed task-state counts, logical default-VM slots and currently startable default-VM slots. The two slot values are bounded snapshot estimates: calculate the logical estimate from the minimum verified CPU, memory and physical-disk headroom ratios, then apply current CT-visible CPU capacity and the 50 percent startup-pressure policy for the startable estimate. Omit them when inputs are unverified. Never label them as a native or safe maximum.

The inventory view must show active, preparing, closing, deleting, failed, cached and absent records, with active/transitional rows first, failed cleanup next and cached/absent rows collapsed by default. Keep task ID, instance/session identity, state, native status, guest availability, requested resources, effective lease, expiry, SSH mapping and structured last-error code/phase. Render requested CPU as vCPU, requested RAM and disk as GiB, and lease expiry as Unix seconds so the table does not present raw byte counts as ambiguous short values. Do not expose raw error messages, credentials or deployment paths.

## Acceptance

The reusable dashboard path is considered complete when the public template renders and passes validation. A deployment is complete only when its rendered source is versioned in the owning provisioning repository and the following are observable without host-manager access:

- CT root and image-staging used/available bytes.
- Physical pool total/used/free bytes.
- Guest rootfs allocation shown separately from physical usage.
- Allocated, reserved, remaining and limit values for CPU, memory, disk and VM slots.
- Admission ready, reclaim-required and draining state.
- Operation success/failure by phase, image-unpack failures, warnings, uptime, scrape freshness and task-state counts.
- Horizontal dashed lines for every available authoritative limit, with no invented line for `-1` or missing values.
- CT-visible memory capacity and logical memory reservation limit are separate metric-backed lines; pressure and OOM are observations and do not move the logical line.
- CPU PSI `some.avg10`, `full.avg10`, the 50 percent startup threshold and `startup_blocked` are visible separately from logical CPU remaining; the pressure lines do not move the logical line.
- The two dashboards use automatic refresh and remain visibly separate after a reload; the inventory table does not retain stale task rows after a successful scrape.

# Guardrails / Reuse notes

- Do not edit a provisioned Grafana database and treat the result as a durable implementation.
- Do not use Incus project quota metrics as native admission limits when the values are `-1`.
- Do not label guest rootfs allocation as physical disk usage.
- Do not infer CT root or image-staging capacity from Incus pool capacity.
- Do not treat CPU PSI as CPU utilization, logical reservation usage or a native VM-slot limit.
- Do not call a derived default-VM slot estimate a safe maximum or authoritative Incus capacity.
- Do not multiply packet-per-second queries by `8`.
- Keep deployment addresses, credentials, dashboard datasource identity, controller job, rendered dashboard, and collector paths in the deployment repository or protected operator records. Keep only placeholders and generic defaults in the public bundle.

# References

- [Agent VM observability contract](../../specs/agent-vm/contracts/observability.md)
- [Agent VM topology and recovery runbook](../../specs/agent-vm/OPERATIONS.md)
- [Native deployment boundary](../../../skills/agent-vm-ops/references/deployment.md)
- [Agent VM CPU startup pressure for overcommit ADR](../../adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md)
- [Agent VM capacity and inventory observability ADR](../../adr/0033-agent-vm-capacity-and-inventory-observability.md)
