# Agent VM Observability Contract

The dashboard must distinguish control-environment capacity, physical Incus pool capacity, guest allocation, and native admission. A healthy Incus daemon or a guest root filesystem quota is not evidence that the CT root filesystem can unpack an image or that native admission has room for another VM.

## Authority layers

| Concern | Authoritative source | Dashboard meaning |
| --- | --- | --- |
| Incus pool physical capacity | `incus_storage_pool_size_bytes`, `incus_storage_pool_used_bytes`, and the corresponding available series | Physical bytes consumed by the configured Incus pool. |
| Guest rootfs allocation | `incus_filesystem_size_bytes` and related per-instance filesystem series | Guest allocation/quota. Label this as guest rootfs allocation; do not call it physical disk usage. |
| CT root and image staging | A native filesystem exporter for `/` and `/var/lib/incus/images` | Control-state and transient image-unpack capacity. Incus project metrics cannot provide this boundary. |
| Native admission | Native controller admission metrics | Allocated, reserved, remaining and limit values for CPU, memory, disk and VM slots. |
| Controller state | Native controller health, task-state and operation metrics | Readiness, draining, reclaim state, failures and cleanup backlog. |

`incus_project_limit` is not an admission source when the relevant project values are `-1`. The dashboard must omit the corresponding limit line instead of drawing a guessed threshold. Project-level process limits and unrelated restrictions remain separate.

## Native metric families

The deployment-facing collector must expose the following stable metric families. Resource values use the unit encoded in the metric name: CPU is vCPU, memory and disk are bytes, and VM slots are a count.

| Metric | Type | Required labels or values |
| --- | --- | --- |
| `agent_vm_admission_allocated` | gauge | `resource=cpu|memory|disk|instances` |
| `agent_vm_admission_reserved` | gauge | `resource=cpu|memory|disk|instances` |
| `agent_vm_admission_remaining` | gauge | `resource=cpu|memory|disk|instances` |
| `agent_vm_admission_limit` | gauge | `resource=cpu|memory|disk|instances`; absent when the limit is unverified |
| `agent_vm_admission_ready` | gauge | `0` or `1` |
| `agent_vm_admission_reclaim_required` | gauge | `0` or `1` |
| `agent_vm_admission_draining` | gauge | `0` or `1` |
| `agent_vm_filesystem_size_bytes` | gauge | `mountpoint=/` or `/var/lib/incus/images` |
| `agent_vm_filesystem_used_bytes` | gauge | `mountpoint=/` or `/var/lib/incus/images` |
| `agent_vm_filesystem_available_bytes` | gauge | `mountpoint=/` or `/var/lib/incus/images` |
| `agent_vm_operations_total` | counter | `action`, `outcome`, and `phase` |
| `agent_vm_image_unpack_failures_total` | counter | `phase` and `reason` |
| `agent_vm_task_states` | gauge | `state=preparing|active|cached|closing|failed|absent` |
| `agent_vm_warnings_total` | counter | stable warning `code` |
| `agent_vm_up` | gauge | `0` or `1` |

The one-shot controller remains the source of truth for native state and must not be replaced with a resident HTTP broker solely to serve these metrics. The deployment must provide an authorized, read-only scrape bridge or textfile collector that preserves the controller's failure and freshness semantics. Until that bridge exists, a Grafana query for these families is not a working panel.

## Dashboard contract

The provisioned dashboard enables automatic refresh by default with `refresh: "auto"`, while retaining explicit user-selectable intervals. Grafana may choose an interval based on the current time range; the rendered control must show `Auto selected` after a reload.

The first row contains CT root available/used, image-staging available/used, physical pool total/used/free, and native admission allocated/reserved/remaining/limit. The existing guest rootfs panel is renamed to `Guest rootfs allocation`; a separate physical pool panel prevents quota totals from being mistaken for real disk usage. The capacity view must answer both whether an actual wall is being approached and what resource headroom remains for the next VM.

Every chart with an authoritative limit renders that limit as a horizontal dashed line. This includes pool total, configured low/target watermarks, native CPU/memory/disk/VM-slot limits, and guest warning/critical thresholds where the policy defines them. A line is not rendered when the source is missing, unverified or `-1`.

The dashboard includes readiness and failure panels for admission state, reclaim-required and draining flags, operation outcomes by phase, image-unpack failures, warnings, uptime and scrape freshness, plus current task states and pending/failed cleanup counts. These panels are required to explain an image-unpack failure when the pool still has free space.

The top network TX/RX packets-per-second queries must use packet rates directly. They must not multiply a `pps` query by `8`; multiplying by `8` converts a count rate incorrectly and does not produce bytes per second.

## Provisioning boundary

The Grafana database is not a source of truth for this dashboard. The JSON, provisioning file, Prometheus scrape configuration and any collector configuration belong in the deployment repository that owns the `incus-agent-vm` dashboard. This public repository documents the metric and panel contract but does not contain deployment addresses, credentials, or a consumer-specific Grafana export.

The deployment source has been located in the operator-owned `obs` stack: `grafana/dashboards/incus/incus-agent-vm.json` is loaded by `grafana/provisioning/dashboards/incus.yml`, and the Grafana service mounts both paths read-only. A recovered runtime copy may be copied back into that owning deployment checkout for versioning and review, but it must not be copied into this public repository. Treat an untracked runtime copy as a recovery artifact until its owner, checksum and source revision are recorded.

An implementation is complete only when the provisioning source contains the dashboard changes, the scrape target exposes the native families above, Grafana loads the provisioned dashboard, and queries for CT root, image staging, physical pool, guest allocation and native admission all return fresh data.

## Verification

Verify the dashboard JSON has `refresh: "auto"`, a reload leaves Grafana's refresh control at `Auto selected`, the physical and guest disk panels have distinct titles, packet-rate expressions contain no `* 8`, and every limit line is backed by a metric or explicit policy threshold. Query each metric family through the configured Prometheus datasource and verify that missing or `-1` limits do not render as zero or as an invented line.

## References

- [Native Agent VM specification](../SPEC.md)
- [Agent VM operations runbook](../OPERATIONS.md)
- [Agent VM capacity and overselling ADR](../../adr/0026-agent-vm-cpu-memory-overselling.md)
