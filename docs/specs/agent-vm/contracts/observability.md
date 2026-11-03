# Agent VM 观测合同

Dashboard 必须区分 control-environment capacity、physical Incus pool capacity、guest allocation 和 native admission。
Healthy Incus daemon 或 guest root filesystem quota 不能证明 CT root filesystem 可以 unpack image，也不能证明 native admission 还有空间容纳另一个 managed container。

## 权威来源层

| 关注项 | 权威来源 | Dashboard 含义 |
| --- | --- | --- |
| Incus pool physical capacity | `incus_storage_pool_size_bytes`、`incus_storage_pool_used_bytes` 及对应的 available series | Configured Incus pool 消耗的 physical bytes。 |
| Guest rootfs allocation | `incus_filesystem_size_bytes` 及相关 per-instance filesystem series | Guest allocation/quota。必须标注为 guest rootfs allocation，不得称为 physical disk usage。 |
| CT root 和 image staging | 通过 configured CT control entry 对 `/` 和 `/var/lib/incus/images` 执行的 deployment-owned read-only filesystem probe | Control-state 和 transient image-unpack capacity。Incus project metrics 无法提供该边界。 |
| Native admission | Native controller admission metrics | CPU、memory、disk 和 managed-container slots 的 allocated、reserved、remaining 和 limit values。 |
| Controller state | Native controller health、task-state 和 operation metrics | Readiness、draining、reclaim state、failures 和 cleanup backlog。 |

<a id="deployment-control-observation"></a>
## 部署控制观测

部署观测是独立的本机控制合同，定义见[Deployd 合同](deployment-controller.md)。
Deployd 提供部署事务阶段和恢复证据；受保护的运行时来源仍是 `selected`、`alive`、`serving`、`leader-owned` 和 `quiescent` 等实际状态的权威。
运维者通过 `agent-vm-deploy status --json` 读取组合视图，Prometheus 通过 deployd 的内部 `GET /metrics` 拉取指标。
该指标监听不增加公共 Caddy 路由或浏览器 API。

| 观测项 | 运行时来源 | 含义和规则 |
| --- | --- | --- |
| `selected_slot` | 原子 `/var/lib/agent-vm-native/active` 符号链接 | 哪个槽位接收新路由。不能证明进程健康、控制器主权或排空完成。 |
| `process` | `systemctl show agent-vm-native@blue.service` 和 `@green.service` | 每个槽位的进程状态、主 PID/启动标识和服务单元新鲜度。进程存活不等于就绪。 |
| `readiness` 和 `state_capabilities` | 每个槽位 Unix socket 的受保护就绪/健康响应和控制观测 | 槽位是否能提供声明的 release，并读写共享业务状态能力集合。 |
| `controller_role` | 受保护的槽位角色合同与 deployd 的进程持有型控制器租约观测 | 哪个槽位可以运行 scheduler、reconciliation、age GC 和 scheduler-driven native mutation。最多一个主控槽位，但两个槽位可以同时存活。 |
| `traffic_role` 和 `in_flight` | 受保护的槽位流量角色/计数器合同；Caddy access log 只能作为辅助证据 | 槽位是否接受新请求，以及仍有多少在途请求。`selected_slot` 本身不是排空信号。 |
| `native_quiescence` | 槽位 `admin-metrics`、`admin-status`、`doctor`、pending-operation 和 worker 观测 | native worker 和不确定 operation 是否已收敛。`unknown` 或过期证据会阻止 finalize。 |
| `transaction` | Deployd 的独立 SQLite 数据库 | Transaction ID、phase、target slot、phase event、failure evidence 和最新观测快照。它不是业务状态，也不能覆盖运行时事实。 |
| `freshness` 和 `provenance` | 观测时间戳、来源标识和 deployd 阶段记录 | 每个部署状态都必须说明事实的观测时间和来源。缺失或矛盾证据是 `unknown`，不是健康。 |

Deployd SQLite 只保存最新观测快照，不保存每次轮询。
它用于阶段转换原子性、并发运维读取、可索引的事务历史和崩溃恢复。
原始来源事实仍可从 systemd、active link 和受保护的槽位合同重新得到；JSONL 或 dashboard 输出只是导出，不是部署事务权威。
守护进程重启后先恢复最近的事务阶段，再重新观测所有运行时来源，之后才能允许新的阶段转换。
阶段不明确时进入 `needs_recovery`，并保持 fail closed。

当相关 project values 为 `-1` 时，`incus_project_limit` 不是 admission source。
Dashboard 必须省略对应的 limit line，不得绘制 guessed threshold。
Project-level process limits 和 unrelated restrictions 保持独立。

## Native 指标族

`agent-vm-deployd` 的 metrics encoder 必须暴露以下稳定 metric families。
每个 family 使用一个 unit；resource dimension 编码在 metric name 中，而不是 label 中。
CPU 使用 vCPU，memory 和 disk 使用 bytes，managed-container slots 使用 count。
现有 metric identifiers 中的 `vm` segment 是 Agent VM product 的 compatibility name，不得解释为要求使用 Incus VMs。

| 指标 | 类型 | 必需的 labels 或 values |
| --- | --- | --- |
| `agent_vm_admission_cpu_allocated_vcpu` | gauge | 已分配的 CPU，单位 vCPU |
| `agent_vm_admission_cpu_reserved_vcpu` | gauge | 已预留的 CPU，单位 vCPU |
| `agent_vm_admission_cpu_allocated_and_reserved_vcpu` | gauge | 已分配与已预留 CPU 的合计，单位 vCPU |
| `agent_vm_admission_cpu_remaining_vcpu` | gauge | 逻辑 CPU 余量，单位 vCPU |
| `agent_vm_admission_cpu_limit_vcpu` | gauge | 逻辑 CPU 上限；未验证时不暴露 |
| `agent_vm_admission_cpu_capacity_vcpu` | gauge | overcommit 前的 CT-visible CPU capacity |
| `agent_vm_cpu_pressure_some_avg10_percent` | gauge | CT-local CPU PSI 的 `some.avg10`；不可用时不暴露 |
| `agent_vm_cpu_pressure_full_avg10_percent` | gauge | CT-local CPU PSI 的 `full.avg10`；不可用时不暴露 |
| `agent_vm_cpu_startup_pressure_threshold_percent` | gauge | 配置的 `full.avg10` startup guard threshold |
| `agent_vm_cpu_startup_blocked` | gauge | default-request startup observation，取 `0` 或 `1` |
| `agent_vm_admission_memory_allocated_bytes` | gauge | 已分配的内存，单位 bytes |
| `agent_vm_admission_memory_reserved_bytes` | gauge | 已预留的内存，单位 bytes |
| `agent_vm_admission_memory_allocated_and_reserved_bytes` | gauge | 已分配与已预留内存的合计，单位 bytes |
| `agent_vm_admission_memory_remaining_bytes` | gauge | 逻辑内存余量，单位 bytes |
| `agent_vm_admission_memory_limit_bytes` | gauge | 逻辑内存上限；未验证时不暴露 |
| `agent_vm_admission_memory_capacity_bytes` | gauge | overcommit 前的 CT-visible memory capacity |
| `agent_vm_admission_disk_allocated_bytes` | gauge | 已分配的 physical-admission disk，单位 bytes |
| `agent_vm_admission_disk_reserved_bytes` | gauge | 已预留的 physical-admission disk，单位 bytes |
| `agent_vm_admission_disk_allocated_and_reserved_bytes` | gauge | 已分配与已预留 physical-admission disk 的合计 |
| `agent_vm_admission_disk_remaining_bytes` | gauge | physical disk 余量，单位 bytes |
| `agent_vm_admission_disk_limit_bytes` | gauge | physical disk 上限；未验证时不暴露 |
| `agent_vm_admission_disk_capacity_bytes` | gauge | admission 前的 physical disk capacity |
| `agent_vm_admission_instances_allocated_count` | gauge | 已分配的 managed-container slot 数量 |
| `agent_vm_admission_instances_reserved_count` | gauge | 已预留的 managed-container slot 数量 |
| `agent_vm_admission_instances_allocated_and_reserved_count` | gauge | 已分配与已预留 managed-container slot 的合计 |
| `agent_vm_admission_instances_remaining_count` | gauge | 剩余 managed-container slot 数量；没有 authoritative limit 时不暴露 |
| `agent_vm_admission_instances_limit_count` | gauge | managed-container slot 上限；未验证时不暴露 |
| `agent_vm_admission_instances_capacity_count` | gauge | 暴露时的 authoritative managed-container capacity |
| `agent_vm_admission_default_request_cpu_vcpu` | gauge | policy default managed-container CPU request |
| `agent_vm_admission_default_request_memory_bytes` | gauge | policy default managed-container memory request |
| `agent_vm_admission_default_request_disk_bytes` | gauge | policy default container rootfs allocation limit |
| `agent_vm_admission_default_request_physical_cost_bytes` | gauge | policy default container physical admission cost |
| `agent_vm_admission_default_vm_logical_slots` | gauge | 派生的 snapshot estimate；输入未验证时不暴露 |
| `agent_vm_admission_default_vm_startable_slots` | gauge | 应用 current startup gates 后的 derived snapshot estimate；输入未验证时不暴露 |
| `agent_vm_memory_current_bytes` | gauge | CT-local cgroup 的 `memory.current`；不可用时不暴露 |
| `agent_vm_memory_available_bytes` | gauge | 已验证的 CT-local LXCfs/cgroup 可用内存 |
| `agent_vm_memory_pressure_threshold_bytes` | gauge | CT-local pressure observation threshold |
| `agent_vm_memory_pressure` | gauge | `0` 或 `1`；仅 observation，绝不是 logical admission limit |
| `agent_vm_memory_oom_events_total` | counter | CT-local cgroup OOM event kind |
| `agent_vm_scrape_timestamp_seconds` | gauge | deployd 来源观测时间戳 |
| `agent_vm_admission_ready` | gauge | `0` 或 `1` |
| `agent_vm_admission_reclaim_required` | gauge | `0` 或 `1` |
| `agent_vm_admission_draining` | gauge | `0` 或 `1` |
| `agent_vm_filesystem_size_bytes` | gauge | `mountpoint=/` 或 `/var/lib/incus/images` |
| `agent_vm_filesystem_used_bytes` | gauge | `mountpoint=/` 或 `/var/lib/incus/images` |
| `agent_vm_filesystem_available_bytes` | gauge | `mountpoint=/` 或 `/var/lib/incus/images` |
| `agent_vm_operations_total` | counter | `action`、`outcome` 和 `phase` |
| `agent_vm_image_unpack_failures_total` | counter | `phase` 和 `reason` |
| `agent_vm_task_states` | gauge | `state=preparing|active|cached|closing|deleting|failed|absent` |
| `agent_vm_task_info` | gauge | task ID、instance/session identity、state、native status、guest availability 和 effective lease 的 bounded labels |
| `agent_vm_task_cpu_vcpu` | gauge | requested task CPU，按 bounded task ID 和 state 标记 |
| `agent_vm_task_memory_bytes` | gauge | requested task memory，按 bounded task ID 和 state 标记 |
| `agent_vm_task_disk_bytes` | gauge | requested task container rootfs allocation，按 bounded task ID 和 state 标记 |
| `agent_vm_task_lease_expires_at_seconds` | gauge | lease deadline，按 bounded task ID 和 state 标记 |
| `agent_vm_task_ssh_mapping` | gauge | bounded task-to-SSH mapping metadata，不含 credentials，按 task ID 和 state 标记 |
| `agent_vm_task_last_error_info` | gauge | stable error `code` 和 `phase`，不含 raw message text，按 task ID 和 state 标记 |
| `agent_vm_warnings_total` | counter | stable warning `code` |
| `agent_vm_up` | gauge | `0` 或 `1` |
| `agent_vm_observation_consistent` | gauge | 仅当 native metrics snapshot 通过 durable epoch check 时为 `1`，否则为 `0` |

每个槽位的 Go control service 是该槽位 native state 的 source of truth，并拥有三个固定且独立刷新的 snapshots：`capacity`、`inventory` 和 `health`。
槽位可以把 snapshot 原子写入自身的内部 textfile/JSON 目录，但该目录不是 Prometheus 的最终 scrape target。
槽位外的 `agent-vm-deployd` 读取两个槽位的受保护 snapshot 和控制观测，并在自己的 `GET /metrics` 中统一输出最终指标。
它必须保留每个来源的 failure 和 freshness semantics；Prometheus 不直接读取槽位文件，也不直接连接槽位 socket。
`agent_vm_admission_memory_limit_bytes` 是 logical reservation ceiling（本 deployment 为 65536 MiB = 68719476736 bytes），`agent_vm_admission_memory_capacity_bytes` 是 CT-visible capacity（32768 MiB = 34359738368 bytes）。
Service JSON 使用 `memory_mib`；deployd 的 metrics encoder 在发出 memory families 前必须将该字段转换为 bytes。
`agent_vm_memory_pressure` 是 runtime observation，不得用于减少 `agent_vm_admission_memory_remaining_bytes`。
CPU PSI 只有在 service 验证 cgroup v2 mount root、`/proc/self/cgroup` 中唯一 unified entry 以及当前 cgroup path 下的 pressure file 后才有效；deployd 必须保留 service 的 unverified state，不得将 host 或 ancestor pressure 重新标记为 CT-local。
在 deployd `/metrics` endpoint 和 Prometheus scrape target 就绪前，针对这些 families 的 Grafana query 不是 working panel。

`admin-metrics` JSON 是 deployd 的 capacity input contract：`observation_epoch` 是 sampled window 结束时的 durable state epoch，`observation_consistent` 记录 native probes 运行期间该 epoch 是否保持不变，`observation_attempts` 记录 bounded retry count，`observed_at` 是 freshness timestamp，`up` 是 controller read success flag，`resources` 是 logical admission object，`default_admission` 是 authoritative default-request 和 physical-cost diagnostic，`ct_cpu` 是 CT-local CPU PSI 和 default startup-pressure observation，`ct_memory` 是 CT-local runtime observation，`pending_operations` 列出 native outcome 仍 uncertain 的 journal entries。
Controller 保持每个 durable-state snapshot 短小，在 state transaction 或 lock 外运行 native probes，并在 rqlite cutover 后使用 linearizable state reads 比较 probe 前后的 durable epoch；只有 freshness 明确可接受时，native observation values 才可以使用较弱 consistency。
Epoch 只在 durable state transaction commit 时改变，successful read 或 no-op transaction 不改变。
Deployd 必须保留 `observation_consistent=false` 及其 warning，不得将可能混合的 snapshot 呈现为 authoritative。
Action 无法读取或解析时，deployd MUST 暴露对应 source 的 failure 和 freshness failure；当 `ct_memory.pressure` 为 `null` 时不得发出 `agent_vm_memory_pressure=0`，当 `ct_cpu.pressure` unverified 时不得发出 CPU pressure values。
Deployd 必须保留 `null` 或 absent limits 和 observations，不得将其强制转换为 zero。

Deployd MUST 将 `admin-metrics` 和 `admin-status` calls 视为 independent sampling lanes。
Slow 或 unavailable `admin-status` call MUST NOT 擦除 last valid `admin-metrics` observation 的 capacity families。
Failed `admin-status` observation MUST remove 或 invalidate task rows，不得静默保留 stale inventory。
反之，failed `admin-metrics` scrape MUST 将 capacity freshness 和 source health 标记为 failed；MUST NOT 被 newly completed inventory scrape 隐藏。
Source-specific timestamps 和 health diagnostics MUST 与 aggregate `agent_vm_up` value 保持可区分。
`admin-status` uses keyset pagination with a default page size of 25 and a maximum of 100, limits native probes to 8 concurrent tasks, applies a 1-second per-task and 5-second per-page deadline, and exposes `partial`、`unknown`、`observation_consistent`、`probed`、`verified` 和 `timed_out`。Unknown or `native_missing` task observations remain unverified and must not be rendered as healthy。

Prometheus MUST 只抓取 deployd 的 `/metrics` endpoint，并使用 `job="agent-vm-deployd"` 标识该 scrape target。
Deployd 输出的槽位级 series MUST 带 `slot` label；部署级 series 不得因为两个槽位同时存活而生成重复的无槽位 series。
Grafana 只通过 Prometheus datasource 查询这些 series，不读取 deployd SQLite、槽位 snapshot、Caddy access log 或 Unix socket。

固定合同短语为 independent sampling lanes、slow or unavailable `admin-status` call、erase capacity families、remove or invalidate task rows，以及 Source-specific timestamps and health diagnostics；这些短语仅用于保证观测语义可检索。

Filesystem metric families 是独立的 read-only CT-local observation，因为 one-shot controller 的 disk admission source 是 Incus data pool，而不是 CT root 或 image-staging mount。
Deployd 必须通过 configured CT control entry 使用 fixed、non-mutating filesystem probe 获取 `/` 和 `/var/lib/incus/images`，验证两个 mountpoints 和 byte counters，validation failure 时省略这些 families。
不得从 guest rootfs allocation、Incus pool usage 或 PVE metrics 推断这些 values。

对于 approved overcommit deployment profile，`agent_vm_cpu_startup_pressure_threshold_percent` 是 `50`。
这是 compound startup gate 的 deployment policy threshold，不是 logical CPU limit；public metric 对其他 consumers 仍保持可配置。

当 `admin-metrics.reclaim_required` 为 `null` 时，deployd 必须在该次 scrape 中省略 `agent_vm_admission_reclaim_required`；`null` 表示 disk admission unverified，不表示 reclamation unnecessary。

`admin-metrics` response 保持 disk domains 分离：`disk_admission.guest_allocation` 报告 sparse managed-container rootfs allocation sizes，`disk_admission.physical_admission` 报告 CT filesystem 相对于 low watermark 的 used/reserved/headroom。
Consumers 必须从 physical domain 映射 native disk admission metrics，不得将 sparse rootfs allocation totals 与 physical capacity 比较。
Managed-container slot admission 位于 `instances.admission`；当 Incus 不暴露 authoritative slot limit（包括 `-1`）时，其 limit 和 remaining values 为 null。

Default-request 和 slot-estimate families 是 deployd 生成的 derived observations，不是 controller limits。
Deployd 从 controller/operations contract 读取 policy default request 和 physical cost，将 compatibility field `logical_vm_slots` 计算为 verified logical CPU、logical memory 和 physical disk headroom ratios 的最小值并向下取整。
它在同一 snapshot 上应用 CT-visible CPU capacity 和 configured CPU startup-pressure gate 后，计算 compatibility field `startable_vm_slots`。
Input missing、contradictory 或 unverified 时省略两个 estimates。
绝不能用 fabricated zero 代替 unavailable estimate。

## Dashboard 合同

Dashboard 的固定查询问题包括 next managed container 还剩多少资源，以及网络查询 must not multiply a `pps` query by `8`；这些是现有观测合同的机器可检索表述。

对于 `admin-metrics`，只有 action read 成功且 initial Incus version/instance probe 成功时，`up` 才是 `true`。
Readiness、admission gates 和 runtime pressure 仍是独立 fields；因此 controller reachable 但 image unavailable 或 admission denied 时，可以出现 `up=true`、`admission_ready=false`。

`observed_at` 记录 final bounded sampling window 的开始时间，因此 slow native probe 不能让 older capacity 或 pressure sample 显得 fresh。
Deployd 必须从该 timestamp 计算 freshness，不得替换为 scrape completion time。

Provisioned dashboards 默认使用 `refresh: "auto"` 启用 automatic refresh，同时保留 user-selectable intervals。
Grafana 可以根据 current time range 选择 interval；reload 后 rendered control 必须显示 `Auto selected`。

Deployment render 两个 views。
`Agent VM / Capacity & Admission` 是 aggregate capacity view；`Agent VM / Inventory & Operations` 是 per-task table 和 lifecycle view。
前者使用 `admin-metrics`，后者使用 `admin-status`。
两者都是 read-only，必须暴露 scrape failure 和 freshness，不得静默保留 stale task rows。

Capacity dashboard 包含 CT root available/used、image-staging available/used、physical pool total/used/free，以及 native admission allocated/reserved/remaining/limit。
它包含 policy default request/cost、observed task-state counts 和 logical/startable default-container snapshot estimates。
Existing guest rootfs panel 重命名为 `Guest rootfs allocation`；独立的 physical pool panel 防止 quota totals 被误认为 real disk usage。
Capacity view 必须同时回答是否接近实际 wall，以及下一个 managed container 还剩多少 resource headroom。

CPU capacity view 显示 logical reservation composition、CT-visible CPU capacity、`some.avg10`、`full.avg10`、configured startup threshold 和 startup-blocked state。
Memory view 将 logical reservation composition 和 CT-visible capacity 与 `memory.current`、`MemAvailable`、pressure 和 OOM 分开显示。
Inventory dashboard 通过 state filters 显示 active、preparing、closing、deleting、failed、cached 和 absent tasks；cached rows 默认折叠，Incus 暴露 `-1` 时不伪造 native slot limit。
其 table 将 requested CPU 渲染为 vCPU，将 requested RAM 和 disk 渲染为 GiB，将 lease expiry 渲染为 Unix seconds，而不是含义不明的 raw byte counts。

每个具有 authoritative limit 的 chart 都将该 limit 渲染为 horizontal dashed line，包括 pool total、configured low/target watermarks、native CPU/memory/disk/managed-container-slot limits，以及 policy 定义的 guest warning/critical thresholds。
Source missing、unverified 或为 `-1` 时不渲染 line。

Dashboards 包含 admission state、reclaim-required 和 draining flags、按 phase 的 operation outcomes、image-unpack failures、warnings、uptime 和 scrape freshness 的 readiness/failure panels，以及 current task states 和 pending/failed cleanup counts。
这些 panels 用于解释 pool 仍有 free space 时发生的 image-unpack failure。
Inventory resource columns 使用 composite `task_id|state` key join，因此 lifecycle transition 不会把不同 state snapshots 的 task metadata 拼接在一起。

顶部 network TX/RX packets-per-second queries 必须直接使用 packet rates。
不得将 `pps` query 乘以 `8`；乘以 `8` 会错误转换 count rate，也不会得到 bytes per second。

## Provisioning 边界

Grafana database 不是这些 dashboards 的 source of truth。
公共仓库拥有[`skills/agent-vm/references/observability`](../../../../skills/agent-vm/references/observability/README.md)下 sanitized、parameterized dashboard baseline 和 file-provider template，以及 renderer 和 validator。
它不包含 deployment addresses、credentials、real datasource identity、real controller job、metrics listener paths 或 consumer-specific rendered export。

Deployment repository 使用显式 values render 该 baseline，并拥有生成的 JSON、provisioning file、Prometheus scrape configuration 和 deployd metrics listener configuration。
Renderer 写 output 前必须替换 Grafana organization ID、Prometheus datasource UID、controller job、Dashboard UID/title/folder 以及 container-visible dashboard path。
Dashboard path 是稳定的 `current` release directory；renderer 将两个 dashboard JSON files 写入 versioned release，并原子替换 `current` symlink；provisioning YAML 通过 atomic single-file replacement 安装。
Generated dashboards 将 native controller queries 隐藏在 `controller_job` variable 后，从 `agent_vm_admission_cpu_capacity_vcpu` 读取 CPU capacity，并且只从 authoritative metric families 推导 memory/disk limit lines。

Deployment source 位于 operator-owned `obs` stack：`grafana/dashboards/incus/incus-agent-vm.json` 由 `grafana/provisioning/dashboards/incus.yml` 加载，Grafana service 以 read-only 方式 mount 两个 paths。
Recovered runtime copy 可以复制回其 owning deployment checkout，用于 versioning 和 review。
不得替换 public template，也不得作为 deployment export 复制进 public repository。
在 owner、checksum 和 source revision 记录前，将 untracked runtime copy 视为 recovery artifact。

只有 provisioning source 包含 dashboard changes、scrape target 暴露上述 native families、Grafana 加载 provisioned dashboard，并且 CT root、image staging、physical pool、guest allocation 和 native admission 的 queries 都返回 fresh data 时，implementation 才算 complete。

## 验证

部署前，使用 rendered JSON 和 provisioning file 运行 public validator。
然后验证 dashboard JSON 具有 `refresh: "auto"`，reload 后 Grafana refresh control 保持 `Auto selected`，physical 和 guest disk panels 使用不同 titles，packet-rate expressions 不含 `* 8`，且每条 limit line 都由 metric 或 explicit policy threshold 支撑。
通过 configured Prometheus datasource query 每个 metric family，并验证 missing 或 `-1` limits 不会渲染为 zero 或 invented line。

## 参考

- [原生 Agent VM 规范](../SPEC.md)
- [Agent VM 运维手册](../OPERATIONS.md)
- [Agent VM 仅内存预留 admission ADR](../../../adr/0029-agent-vm-memory-reservation-only-admission.md)
- [Agent VM overcommit 的 CPU 启动压力 ADR](../../../adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md)
- [Agent VM 容量与清单观测 ADR](../../../adr/0033-agent-vm-capacity-and-inventory-observability.md)
