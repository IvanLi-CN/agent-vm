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

The deployment-facing collector must expose the following stable metric families. Each family has one unit; the resource dimension is encoded in the metric name rather than a label. CPU is vCPU, memory and disk are bytes, and VM slots are a count.

| Metric | Type | Required labels or values |
| --- | --- | --- |
| `agent_vm_admission_cpu_allocated_vcpu` | gauge | allocated CPU vCPU |
| `agent_vm_admission_cpu_reserved_vcpu` | gauge | reserved CPU vCPU |
| `agent_vm_admission_cpu_allocated_and_reserved_vcpu` | gauge | allocated plus reserved CPU vCPU |
| `agent_vm_admission_cpu_remaining_vcpu` | gauge | logical CPU headroom in vCPU |
| `agent_vm_admission_cpu_limit_vcpu` | gauge | logical CPU limit; absent when unverified |
| `agent_vm_admission_cpu_capacity_vcpu` | gauge | CT-visible CPU capacity before overcommit |
| `agent_vm_cpu_pressure_some_avg10_percent` | gauge | CT-local CPU PSI `some.avg10`; absent when unavailable |
| `agent_vm_cpu_pressure_full_avg10_percent` | gauge | CT-local CPU PSI `full.avg10`; absent when unavailable |
| `agent_vm_cpu_startup_pressure_threshold_percent` | gauge | configured `full.avg10` startup guard threshold |
| `agent_vm_cpu_startup_blocked` | gauge | `0` or `1` for the default-request startup observation |
| `agent_vm_admission_memory_allocated_bytes` | gauge | allocated memory bytes |
| `agent_vm_admission_memory_reserved_bytes` | gauge | reserved memory bytes |
| `agent_vm_admission_memory_allocated_and_reserved_bytes` | gauge | allocated plus reserved memory bytes |
| `agent_vm_admission_memory_remaining_bytes` | gauge | logical memory headroom in bytes |
| `agent_vm_admission_memory_limit_bytes` | gauge | logical memory limit; absent when unverified |
| `agent_vm_admission_memory_capacity_bytes` | gauge | CT-visible memory capacity before overcommit |
| `agent_vm_admission_disk_allocated_bytes` | gauge | allocated physical-admission disk bytes |
| `agent_vm_admission_disk_reserved_bytes` | gauge | reserved physical-admission disk bytes |
| `agent_vm_admission_disk_allocated_and_reserved_bytes` | gauge | allocated plus reserved physical-admission disk bytes |
| `agent_vm_admission_disk_remaining_bytes` | gauge | physical disk headroom in bytes |
| `agent_vm_admission_disk_limit_bytes` | gauge | physical disk limit; absent when unverified |
| `agent_vm_admission_disk_capacity_bytes` | gauge | physical disk capacity before admission |
| `agent_vm_admission_instances_allocated_count` | gauge | allocated VM slot count |
| `agent_vm_admission_instances_reserved_count` | gauge | reserved VM slot count |
| `agent_vm_admission_instances_allocated_and_reserved_count` | gauge | allocated plus reserved VM slot count |
| `agent_vm_admission_instances_remaining_count` | gauge | remaining VM slot count; absent without an authoritative limit |
| `agent_vm_admission_instances_limit_count` | gauge | VM slot limit; absent when unverified |
| `agent_vm_admission_instances_capacity_count` | gauge | authoritative VM slot capacity when exposed |
| `agent_vm_memory_current_bytes` | gauge | CT-local cgroup `memory.current`; absent when unavailable |
| `agent_vm_memory_available_bytes` | gauge | CT-local verified LXCfs/cgroup available memory |
| `agent_vm_memory_pressure_threshold_bytes` | gauge | CT-local pressure observation threshold |
| `agent_vm_memory_pressure` | gauge | `0` or `1`; observation only, never a logical admission limit |
| `agent_vm_memory_oom_events_total` | counter | CT-local cgroup OOM event kind |
| `agent_vm_scrape_timestamp_seconds` | gauge | collector observation timestamp |
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

The one-shot controller remains the source of truth for native state and must not be replaced with a resident HTTP broker solely to serve these metrics. The deployment must provide an authorized, read-only scrape bridge or textfile collector that preserves the controller's failure and freshness semantics. `agent_vm_admission_memory_limit_bytes` is the logical reservation ceiling (for the deployment, 65536 MiB = 68719476736 bytes), while `agent_vm_admission_memory_capacity_bytes` is the CT-visible capacity (32768 MiB = 34359738368 bytes). The controller JSON uses `memory_mib`; the Prometheus bridge must convert that field to bytes before emitting the memory families. `agent_vm_memory_pressure` is a runtime observation and must not be used to reduce `agent_vm_admission_memory_remaining_bytes`. CPU PSI is valid only when the controller verifies the cgroup v2 mount root, the single unified entry in `/proc/self/cgroup`, and the pressure file under that current cgroup path; an exporter must preserve the controller's unverified state rather than relabeling host or ancestor pressure as CT-local. Until that bridge exists, a Grafana query for these families is not a working panel.

The `admin-metrics` JSON is the bridge input contract: `observed_at` is the freshness timestamp, `up` is the controller read success flag, `resources` is the logical admission object, `ct_cpu` is the CT-local CPU PSI and default startup-pressure observation, `ct_memory` is the CT-local runtime observation, and `pending_operations` lists journal entries whose native outcome remains uncertain. A collector must expose `agent_vm_up=0` and freshness failure when the action cannot be read or parsed; it must not emit `agent_vm_memory_pressure=0` when `ct_memory.pressure` is `null` or CPU pressure values when `ct_cpu.pressure` is unverified. The bridge must preserve `null` or absent limits and observations instead of coercing them to zero.

When `admin-metrics.reclaim_required` is `null`, the collector must omit `agent_vm_admission_reclaim_required` for that scrape; `null` means disk admission is unverified, not that reclamation is unnecessary.

The `admin-metrics` response keeps disk domains separate: `disk_admission.guest_allocation` reports sparse guest virtual-disk sizes, while `disk_admission.physical_admission` reports CT filesystem used/reserved/headroom against its low watermark. Consumers must map native disk admission metrics from the physical domain and must not compare sparse guest allocation totals with physical capacity. VM-slot admission is under `instances.admission`; its limit and remaining values are null when Incus exposes no authoritative slot limit, including `-1`.

## Dashboard contract

The provisioned dashboard enables automatic refresh by default with `refresh: "auto"`, while retaining explicit user-selectable intervals. Grafana may choose an interval based on the current time range; the rendered control must show `Auto selected` after a reload.

The first row contains CT root available/used, image-staging available/used, physical pool total/used/free, and native admission allocated/reserved/remaining/limit. The existing guest rootfs panel is renamed to `Guest rootfs allocation`; a separate physical pool panel prevents quota totals from being mistaken for real disk usage. The capacity view must answer both whether an actual wall is being approached and what resource headroom remains for the next VM.

Every chart with an authoritative limit renders that limit as a horizontal dashed line. This includes pool total, configured low/target watermarks, native CPU/memory/disk/VM-slot limits, and guest warning/critical thresholds where the policy defines them. A line is not rendered when the source is missing, unverified or `-1`.

The dashboard includes readiness and failure panels for admission state, reclaim-required and draining flags, operation outcomes by phase, image-unpack failures, warnings, uptime and scrape freshness, plus current task states and pending/failed cleanup counts. These panels are required to explain an image-unpack failure when the pool still has free space.

The top network TX/RX packets-per-second queries must use packet rates directly. They must not multiply a `pps` query by `8`; multiplying by `8` converts a count rate incorrectly and does not produce bytes per second.

## Provisioning boundary

The Grafana database is not a source of truth for this dashboard. This public repository owns a sanitized, parameterized dashboard baseline and file-provider template under [`skills/agent-vm/references/observability`](../../../skills/agent-vm/references/observability/README.md), together with the renderer and validator. It does not contain deployment addresses, credentials, a real datasource identity, a real controller job, collector paths, or a consumer-specific rendered export.

The deployment repository renders that baseline with its explicit values and owns the resulting JSON, provisioning file, Prometheus scrape configuration and collector configuration. The renderer must replace the Grafana organization ID, Prometheus datasource UID, controller job, Dashboard UID/title/folder, and container-visible dashboard path before writing output. The generated dashboard keeps native controller queries behind a hidden `controller_job` variable, reads CPU capacity from `agent_vm_admission_cpu_capacity_vcpu`, and derives memory/disk limit lines only from authoritative metric families.

The deployment source has been located in the operator-owned `obs` stack: `grafana/dashboards/incus/incus-agent-vm.json` is loaded by `grafana/provisioning/dashboards/incus.yml`, and the Grafana service mounts both paths read-only. A recovered runtime copy may be copied back into that owning deployment checkout for versioning and review. It must not replace the public template or be copied into this public repository as a deployment export. Treat an untracked runtime copy as a recovery artifact until its owner, checksum and source revision are recorded.

An implementation is complete only when the provisioning source contains the dashboard changes, the scrape target exposes the native families above, Grafana loads the provisioned dashboard, and queries for CT root, image staging, physical pool, guest allocation and native admission all return fresh data.

## Verification

Run the public validator against the rendered JSON and provisioning file before deployment. Then verify the dashboard JSON has `refresh: "auto"`, a reload leaves Grafana's refresh control at `Auto selected`, the physical and guest disk panels have distinct titles, packet-rate expressions contain no `* 8`, and every limit line is backed by a metric or explicit policy threshold. Query each metric family through the configured Prometheus datasource and verify that missing or `-1` limits do not render as zero or as an invented line.

## References

- [Native Agent VM specification](../SPEC.md)
- [Agent VM operations runbook](../OPERATIONS.md)
- [Agent VM memory reservation-only admission ADR](../../adr/0029-agent-vm-memory-reservation-only-admission.md)
