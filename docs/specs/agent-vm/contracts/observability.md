# Agent VM Observability Contract

The dashboard must distinguish control-environment capacity, physical Incus pool capacity, guest allocation, and native admission. A healthy Incus daemon or a guest root filesystem quota is not evidence that the CT root filesystem can unpack an image or that native admission has room for another managed container.

## Authority layers

| Concern | Authoritative source | Dashboard meaning |
| --- | --- | --- |
| Incus pool physical capacity | `incus_storage_pool_size_bytes`, `incus_storage_pool_used_bytes`, and the corresponding available series | Physical bytes consumed by the configured Incus pool. |
| Guest rootfs allocation | `incus_filesystem_size_bytes` and related per-instance filesystem series | Guest allocation/quota. Label this as guest rootfs allocation; do not call it physical disk usage. |
| CT root and image staging | A deployment-owned read-only filesystem probe for `/` and `/var/lib/incus/images` through the configured CT control entry | Control-state and transient image-unpack capacity. Incus project metrics cannot provide this boundary. |
| Native admission | Native controller admission metrics | Allocated, reserved, remaining and limit values for CPU, memory, disk and managed-container slots. |
| Controller state | Native controller health, task-state and operation metrics | Readiness, draining, reclaim state, failures and cleanup backlog. |

`incus_project_limit` is not an admission source when the relevant project values are `-1`. The dashboard must omit the corresponding limit line instead of drawing a guessed threshold. Project-level process limits and unrelated restrictions remain separate.

## Native metric families

The deployment-facing collector must expose the following stable metric families. Each family has one unit; the resource dimension is encoded in the metric name rather than a label. CPU is vCPU, memory and disk are bytes, and managed-container slots are a count. The `vm` segment in existing metric identifiers is a compatibility name for the Agent VM product and must not be interpreted as a requirement for Incus VMs.

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
| `agent_vm_admission_instances_allocated_count` | gauge | allocated managed-container slot count |
| `agent_vm_admission_instances_reserved_count` | gauge | reserved managed-container slot count |
| `agent_vm_admission_instances_allocated_and_reserved_count` | gauge | allocated plus reserved managed-container slot count |
| `agent_vm_admission_instances_remaining_count` | gauge | remaining managed-container slot count; absent without an authoritative limit |
| `agent_vm_admission_instances_limit_count` | gauge | managed-container slot limit; absent when unverified |
| `agent_vm_admission_instances_capacity_count` | gauge | authoritative managed-container capacity when exposed |
| `agent_vm_admission_default_request_cpu_vcpu` | gauge | policy default managed-container CPU request |
| `agent_vm_admission_default_request_memory_bytes` | gauge | policy default managed-container memory request |
| `agent_vm_admission_default_request_disk_bytes` | gauge | policy default container rootfs allocation limit |
| `agent_vm_admission_default_request_physical_cost_bytes` | gauge | policy default container physical admission cost |
| `agent_vm_admission_default_vm_logical_slots` | gauge | derived snapshot estimate; absent when inputs are unverified |
| `agent_vm_admission_default_vm_startable_slots` | gauge | derived snapshot estimate with current startup gates; absent when inputs are unverified |
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
| `agent_vm_task_states` | gauge | `state=preparing|active|cached|closing|deleting|failed|absent` |
| `agent_vm_task_info` | gauge | bounded labels for task ID, instance/session identity, state, native status, guest availability and effective lease |
| `agent_vm_task_cpu_vcpu` | gauge | requested task CPU, labelled by bounded task ID and state |
| `agent_vm_task_memory_bytes` | gauge | requested task memory, labelled by bounded task ID and state |
| `agent_vm_task_disk_bytes` | gauge | requested task container rootfs allocation, labelled by bounded task ID and state |
| `agent_vm_task_lease_expires_at_seconds` | gauge | lease deadline, labelled by bounded task ID and state |
| `agent_vm_task_ssh_mapping` | gauge | bounded task-to-SSH mapping metadata, without credentials, labelled by task ID and state |
| `agent_vm_task_last_error_info` | gauge | stable error `code` and `phase`, without raw message text, labelled by task ID and state |
| `agent_vm_warnings_total` | counter | stable warning `code` |
| `agent_vm_up` | gauge | `0` or `1` |
| `agent_vm_observation_consistent` | gauge | `1` only when the native metrics snapshot passed its durable epoch check; `0` otherwise |

The Go control service is the source of truth for native state and owns three fixed, independently refreshed snapshots: `capacity`, `inventory` and `health`. It publishes each source atomically to its configured textfile target and reports source-specific health and freshness. A read-only relay may fetch an already generated snapshot through `GET /internal/v1/snapshots/:source`; it must not trigger a new sample or read arbitrary files. The deployment collector must preserve the service's failure and freshness semantics. `agent_vm_admission_memory_limit_bytes` is the logical reservation ceiling (for the deployment, 65536 MiB = 68719476736 bytes), while `agent_vm_admission_memory_capacity_bytes` is the CT-visible capacity (32768 MiB = 34359738368 bytes). The service JSON uses `memory_mib`; the Prometheus bridge must convert that field to bytes before emitting the memory families. `agent_vm_memory_pressure` is a runtime observation and must not be used to reduce `agent_vm_admission_memory_remaining_bytes`. CPU PSI is valid only when the service verifies the cgroup v2 mount root, the single unified entry in `/proc/self/cgroup`, and the pressure file under that current cgroup path; an exporter must preserve the service's unverified state rather than relabeling host or ancestor pressure as CT-local. Until that bridge exists, a Grafana query for these families is not a working panel.

The `admin-metrics` JSON is the controller bridge input contract: `observation_epoch` is the durable state epoch at the end of the sampled window, `observation_consistent` records whether that epoch remained unchanged while native probes ran, `observation_attempts` records the bounded retry count, `observed_at` is its freshness timestamp, `up` is the controller read success flag, `resources` is the logical admission object, `default_admission` is the authoritative default-request and physical-cost diagnostic, `ct_cpu` is the CT-local CPU PSI and default startup-pressure observation, `ct_memory` is the CT-local runtime observation, and `pending_operations` lists journal entries whose native outcome remains uncertain. The controller keeps each durable-state snapshot short, runs native probes outside the state transaction or lock, and compares the durable epoch before and after the probe; the epoch changes only when a durable state transaction commits, not for a successful read or no-op transaction. A collector must preserve `observation_consistent=false` and its warning rather than presenting a possibly mixed snapshot as authoritative. A collector must expose `agent_vm_up=0` and freshness failure when the action cannot be read or parsed; it must not emit `agent_vm_memory_pressure=0` when `ct_memory.pressure` is `null` or CPU pressure values when `ct_cpu.pressure` is unverified. The bridge must preserve `null` or absent limits and observations instead of coercing them to zero.

The bridge MUST treat the `admin-metrics` and `admin-status` calls as independent sampling lanes. A slow or unavailable `admin-status` call MUST NOT erase capacity families from the last valid `admin-metrics` observation. A failed `admin-status` scrape MUST remove or invalidate task rows rather than silently retaining stale inventory. Conversely, a failed `admin-metrics` scrape MUST mark capacity freshness and source health as failed; it MUST NOT be hidden by a newly completed inventory scrape. Source-specific timestamps and health diagnostics MUST remain distinguishable from the aggregate `agent_vm_up` value.

The filesystem metric families are a separate read-only CT-local observation because the one-shot controller's disk admission source is the Incus data pool, not the CT root or image-staging mount. The deployment collector must obtain `/` and `/var/lib/incus/images` from the configured CT control entry with a fixed, non-mutating filesystem probe, validate both mountpoints and byte counters, and omit the families when validation fails. It must not infer these values from guest rootfs allocation, Incus pool usage or PVE metrics.

For the approved overcommit deployment profile, `agent_vm_cpu_startup_pressure_threshold_percent` is `50`. This is a deployment policy threshold for the compound startup gate, not a logical CPU limit; the public metric remains configurable for other consumers.

When `admin-metrics.reclaim_required` is `null`, the collector must omit `agent_vm_admission_reclaim_required` for that scrape; `null` means disk admission is unverified, not that reclamation is unnecessary.

The `admin-metrics` response keeps disk domains separate: `disk_admission.guest_allocation` reports sparse managed-container rootfs allocation sizes, while `disk_admission.physical_admission` reports CT filesystem used/reserved/headroom against its low watermark. Consumers must map native disk admission metrics from the physical domain and must not compare sparse rootfs allocation totals with physical capacity. Managed-container slot admission is under `instances.admission`; its limit and remaining values are null when Incus exposes no authoritative slot limit, including `-1`.

The default-request and slot-estimate families are derived bridge observations, not controller limits. The bridge reads the policy default request and physical cost from the controller/operations contract and calculates the compatibility field `logical_vm_slots` as the minimum of the verified logical CPU, logical memory and physical disk headroom ratios, rounded down. It calculates the compatibility field `startable_vm_slots` from the same snapshot after applying CT-visible CPU capacity and the configured CPU startup-pressure gate. It omits both estimates when an input is missing, contradictory or unverified. It must never emit a fabricated zero as a substitute for an unavailable estimate.

## Dashboard contract

For `admin-metrics`, `up` is `true` only when the action was read successfully and its initial Incus version/instance probe succeeded. Readiness, admission gates and runtime pressure remain separate fields; a reachable controller with an unavailable image or an admission denial may therefore have `up=true` and `admission_ready=false`.

`observed_at` records the start of the final bounded sampling window, so a slow native probe cannot make an older capacity or pressure sample appear fresh. A collector must calculate freshness from this timestamp and must not replace it with scrape completion time.

The provisioned dashboards enable automatic refresh by default with `refresh: "auto"`, while retaining explicit user-selectable intervals. Grafana may choose an interval based on the current time range; the rendered control must show `Auto selected` after a reload.

The deployment renders two views. `Agent VM / Capacity & Admission` is the aggregate capacity view; `Agent VM / Inventory & Operations` is the per-task table and lifecycle view. The first uses `admin-metrics`; the second uses `admin-status`. Both are read-only and must expose scrape failure and freshness rather than silently retaining stale task rows.

The capacity dashboard contains CT root available/used, image-staging available/used, physical pool total/used/free, and native admission allocated/reserved/remaining/limit. It includes the policy default request/cost, observed task-state counts and the logical/startable default-container snapshot estimates. The existing guest rootfs panel is renamed to `Guest rootfs allocation`; a separate physical pool panel prevents quota totals from being mistaken for real disk usage. The capacity view must answer both whether an actual wall is being approached and what resource headroom remains for the next managed container.

The CPU capacity view shows logical reservation composition, CT-visible CPU capacity, `some.avg10`, `full.avg10`, the configured startup threshold and startup-blocked state. The memory view shows logical reservation composition and CT-visible capacity separately from `memory.current`, `MemAvailable`, pressure and OOM. The inventory dashboard shows active, preparing, closing, deleting, failed, cached and absent tasks with state filters; cached rows are collapsed by default and no native slot limit is invented when Incus exposes `-1`. Its table renders requested CPU as vCPU, requested RAM and disk as GiB, and lease expiry as Unix seconds rather than ambiguous raw byte counts.

Every chart with an authoritative limit renders that limit as a horizontal dashed line. This includes pool total, configured low/target watermarks, native CPU/memory/disk/managed-container-slot limits, and guest warning/critical thresholds where the policy defines them. A line is not rendered when the source is missing, unverified or `-1`.

The dashboards include readiness and failure panels for admission state, reclaim-required and draining flags, operation outcomes by phase, image-unpack failures, warnings, uptime and scrape freshness, plus current task states and pending/failed cleanup counts. These panels are required to explain an image-unpack failure when the pool still has free space. Inventory resource columns are joined by the composite `task_id|state` key, so a lifecycle transition cannot combine task metadata from different state snapshots.

The top network TX/RX packets-per-second queries must use packet rates directly. They must not multiply a `pps` query by `8`; multiplying by `8` converts a count rate incorrectly and does not produce bytes per second.

## Provisioning boundary

The Grafana database is not a source of truth for these dashboards. This public repository owns a sanitized, parameterized dashboard baseline and file-provider template under [`skills/agent-vm/references/observability`](../../../../skills/agent-vm/references/observability/README.md), together with the renderer and validator. It does not contain deployment addresses, credentials, a real datasource identity, a real controller job, collector paths, or a consumer-specific rendered export.

The deployment repository renders that baseline with its explicit values and owns the resulting JSON, provisioning file, Prometheus scrape configuration and collector configuration. The renderer must replace the Grafana organization ID, Prometheus datasource UID, controller job, Dashboard UID/title/folder, and container-visible dashboard path before writing output. The dashboard path is a stable `current` release directory; the renderer writes both dashboard JSON files into a versioned release and atomically replaces the `current` symlink, while the provisioning YAML is installed through an atomic single-file replacement. The generated dashboards keep native controller queries behind a hidden `controller_job` variable, read CPU capacity from `agent_vm_admission_cpu_capacity_vcpu`, and derive memory/disk limit lines only from authoritative metric families.

The deployment source has been located in the operator-owned `obs` stack: `grafana/dashboards/incus/incus-agent-vm.json` is loaded by `grafana/provisioning/dashboards/incus.yml`, and the Grafana service mounts both paths read-only. A recovered runtime copy may be copied back into that owning deployment checkout for versioning and review. It must not replace the public template or be copied into this public repository as a deployment export. Treat an untracked runtime copy as a recovery artifact until its owner, checksum and source revision are recorded.

An implementation is complete only when the provisioning source contains the dashboard changes, the scrape target exposes the native families above, Grafana loads the provisioned dashboard, and queries for CT root, image staging, physical pool, guest allocation and native admission all return fresh data.

## Verification

Run the public validator against the rendered JSON and provisioning file before deployment. Then verify the dashboard JSON has `refresh: "auto"`, a reload leaves Grafana's refresh control at `Auto selected`, the physical and guest disk panels have distinct titles, packet-rate expressions contain no `* 8`, and every limit line is backed by a metric or explicit policy threshold. Query each metric family through the configured Prometheus datasource and verify that missing or `-1` limits do not render as zero or as an invented line.

## References

- [Native Agent VM specification](../SPEC.md)
- [Agent VM operations runbook](../OPERATIONS.md)
- [Agent VM memory reservation-only admission ADR](../../../adr/0029-agent-vm-memory-reservation-only-admission.md)
- [Agent VM CPU startup pressure for overcommit ADR](../../../adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md)
- [Agent VM capacity and inventory observability ADR](../../../adr/0033-agent-vm-capacity-and-inventory-observability.md)
