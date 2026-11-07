# Agent VM 观测合同

看板必须区分控制环境容量、Incus 数据池的物理容量、来宾资源分配和原生准入。Incus 守护进程健康或来宾 rootfs 配额正常，不能证明控制环境的根文件系统可以解包镜像，也不能证明原生准入还能容纳新容器。

## 权威来源层

| 关注项 | 权威来源 | 看板含义 |
| --- | --- | --- |
| Incus 数据池物理容量 | `incus_storage_pool_size_bytes`、`incus_storage_pool_used_bytes` 及对应的可用容量序列 | 配置的数据池实际消耗的字节数。 |
| 来宾 rootfs 分配 | `incus_filesystem_size_bytes` 及相关逐实例文件系统序列 | 来宾分配或配额，必须标注为来宾 rootfs 分配，不能称为物理磁盘用量。 |
| 控制环境根目录与镜像暂存目录 | 槽位服务对 `/` 和 `/var/lib/incus/images` 执行的只读文件系统采样 | 控制状态存储和临时镜像解包的容量；Incus 项目指标不能提供该信息。 |
| 原生准入 | 原生控制服务的准入指标 | CPU、内存、磁盘和容器数量的已分配值、预留值、余量及上限。 |
| 控制服务状态 | 控制服务健康、任务状态和操作指标 | 就绪、排空、回收、失败和待清理状态。 |

<a id="deployment-control-observation"></a>
## 部署控制观测

部署观测是独立的本机控制合同，定义见[Deployd 合同](deployment-controller.md)。Deployd 提供部署事务阶段和恢复证据；受保护的运行时来源仍是 `selected`、`alive`、`serving`、`leader-owned` 和 `quiescent` 等实际状态的权威。运维者通过 `agent-vm-deploy status --json` 读取组合视图，Prometheus 通过 deployd 的内部 `GET /metrics` 拉取指标。该指标监听不增加公共 Caddy 路由或浏览器 API。

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

Deployd SQLite 只保存最新观测快照，不保存每次轮询。它用于阶段转换原子性、并发运维读取、可索引的事务历史和崩溃恢复。原始来源事实仍可从 systemd、active 链接 和受保护的槽位合同重新得到；JSONL 或看板输出只是导出，不是部署事务权威。守护进程重启后先恢复最近的事务阶段，再重新观测所有运行时来源，之后才能允许新的阶段转换。阶段不明确时进入 `needs_recovery`，并保持闭锁。

相关项目配额为 `-1` 时，`incus_project_limit` 不能作为准入上限。看板必须省略相应的上限线，不能绘制猜测的阈值。项目级进程限制和其他限制保持独立。

## 原生指标族

`agent-vm-deployd` 的指标编码器必须输出以下稳定指标族。每个指标族只使用一种单位，资源维度写入指标名。CPU 使用 vCPU，内存和磁盘使用 bytes，容器数量使用 count。现有指标名中的 `vm` 是 Agent VM 产品的兼容名称，不表示要求使用 Incus 虚拟机。

| 指标 | 类型 | 必需的标签或值 |
| --- | --- | --- |
| `agent_vm_admission_cpu_allocated_vcpu` | gauge | 已分配的 CPU，单位 vCPU |
| `agent_vm_admission_cpu_reserved_vcpu` | gauge | 已预留的 CPU，单位 vCPU |
| `agent_vm_admission_cpu_allocated_and_reserved_vcpu` | gauge | 已分配与已预留 CPU 的合计，单位 vCPU |
| `agent_vm_admission_cpu_remaining_vcpu` | gauge | 逻辑 CPU 余量，单位 vCPU |
| `agent_vm_admission_cpu_limit_vcpu` | gauge | 逻辑 CPU 上限；未验证时不暴露 |
| `agent_vm_admission_cpu_capacity_vcpu` | gauge | 超额预留前控制环境可见的 CPU 容量 |
| `agent_vm_cpu_pressure_some_avg10_percent` | gauge | 控制环境内的 CPU PSI 的 `some.avg10`；不可用时不暴露 |
| `agent_vm_cpu_pressure_full_avg10_percent` | gauge | 控制环境内的 CPU PSI 的 `full.avg10`；不可用时不暴露 |
| `agent_vm_cpu_startup_pressure_threshold_percent` | gauge | 配置的 `full.avg10` 启动保护阈值 |
| `agent_vm_cpu_startup_blocked` | gauge | 默认请求的启动观测，取 `0` 或 `1` |
| `agent_vm_admission_memory_allocated_bytes` | gauge | 已分配的内存，单位 bytes |
| `agent_vm_admission_memory_reserved_bytes` | gauge | 已预留的内存，单位 bytes |
| `agent_vm_admission_memory_allocated_and_reserved_bytes` | gauge | 已分配与已预留内存的合计，单位 bytes |
| `agent_vm_admission_memory_remaining_bytes` | gauge | 逻辑内存余量，单位 bytes |
| `agent_vm_admission_memory_limit_bytes` | gauge | 逻辑内存上限；未验证时不暴露 |
| `agent_vm_admission_memory_capacity_bytes` | gauge | 超额预留前控制环境可见的内存容量 |
| `agent_vm_admission_disk_allocated_bytes` | gauge | 已分配的物理准入磁盘，单位 bytes |
| `agent_vm_admission_disk_reserved_bytes` | gauge | 已预留的物理准入磁盘，单位 bytes |
| `agent_vm_admission_disk_allocated_and_reserved_bytes` | gauge | 已分配与已预留物理准入磁盘的合计 |
| `agent_vm_admission_disk_remaining_bytes` | gauge | 物理磁盘余量，单位 bytes |
| `agent_vm_admission_disk_limit_bytes` | gauge | 物理磁盘上限；未验证时不暴露 |
| `agent_vm_admission_disk_capacity_bytes` | gauge | 准入前的物理磁盘容量 |
| `agent_vm_admission_instances_allocated_count` | gauge | 已分配的受管容器数量 |
| `agent_vm_admission_instances_reserved_count` | gauge | 已预留的受管容器数量 |
| `agent_vm_admission_instances_allocated_and_reserved_count` | gauge | 已分配与已预留受管容器的合计 |
| `agent_vm_admission_instances_remaining_count` | gauge | 剩余受管容器数量；没有权威上限时不暴露 |
| `agent_vm_admission_instances_limit_count` | gauge | 受管容器数量上限；未验证时不暴露 |
| `agent_vm_admission_instances_capacity_count` | gauge | 仅在存在权威来源时输出的容器数量容量 |
| `agent_vm_admission_default_request_cpu_vcpu` | gauge | 默认容器 CPU 请求量 |
| `agent_vm_admission_default_request_memory_bytes` | gauge | 默认容器内存请求量 |
| `agent_vm_admission_default_request_disk_bytes` | gauge | 默认容器 rootfs 分配上限 |
| `agent_vm_admission_default_request_physical_cost_bytes` | gauge | 默认容器物理准入成本 |
| `agent_vm_admission_default_vm_logical_slots` | gauge | 由快照推导的估算值；输入未验证时不暴露 |
| `agent_vm_admission_default_vm_startable_slots` | gauge | 应用当前启动保护后的快照估算值；输入未验证时不暴露 |
| `agent_vm_memory_current_bytes` | gauge | 控制环境内 cgroup 的 `memory.current`；不可用时不暴露 |
| `agent_vm_memory_available_bytes` | gauge | 已验证的控制环境内 LXCfs/cgroup 可用内存 |
| `agent_vm_memory_pressure_threshold_bytes` | gauge | 控制环境压力观测阈值 |
| `agent_vm_memory_pressure` | gauge | `0` 或 `1`；仅为观测值，不是逻辑准入上限 |
| `agent_vm_memory_oom_events_total` | counter | 控制环境内 cgroup OOM 事件类型 |
| `agent_vm_capacity_scrape_timestamp_seconds`、`agent_vm_inventory_scrape_timestamp_seconds`、`agent_vm_health_scrape_timestamp_seconds` | gauge | 各采样来源的原始观测时间戳 |
| `agent_vm_admission_ready` | gauge | `0` 或 `1` |
| `agent_vm_admission_reclaim_required` | gauge | `0` 或 `1` |
| `agent_vm_admission_draining` | gauge | `0` 或 `1` |
| `agent_vm_filesystem_up` | gauge | 每个文件系统的采样状态，标签为 `mountpoint`；失败为 `0`，不输出伪造的容量 |
| `agent_vm_filesystem_size_bytes` | gauge | `mountpoint` 为 `/`、`/var/lib/incus/images` 或配置的上传暂存目录 |
| `agent_vm_filesystem_used_bytes` | gauge | `mountpoint` 为 `/`、`/var/lib/incus/images` 或配置的上传暂存目录 |
| `agent_vm_filesystem_available_bytes` | gauge | `mountpoint` 为 `/`、`/var/lib/incus/images` 或配置的上传暂存目录 |
| `agent_vm_operations_total` | counter | `action`、`outcome` 和 `phase` |
| `agent_vm_image_unpack_failures_count` | gauge | 已保留审计记录中的镜像解包失败总数；成功读取空审计时为 `0`，审计不可读时缺失 |
| `agent_vm_image_unpack_failures_total` | counter | `phase` 和 `reason` |
| `agent_vm_task_states` | gauge | `state=preparing|active|cached|closing|deleting|failed|absent` |
| `agent_vm_task_info` | gauge | 有界的任务、实例、会话、状态、原生状态、来宾可用性和有效 lease 标签 |
| `agent_vm_task_cpu_vcpu` | gauge | 任务请求的 CPU，使用有界任务 ID 和状态标签 |
| `agent_vm_task_memory_bytes` | gauge | 任务请求的内存，使用有界任务 ID 和状态标签 |
| `agent_vm_task_disk_bytes` | gauge | 任务请求的 rootfs 分配量，使用有界任务 ID 和状态标签 |
| `agent_vm_task_lease_expires_at_seconds` | gauge | lease 到期时间，使用有界任务 ID 和状态标签 |
| `agent_vm_task_ssh_mapping` | gauge | 有界任务到 SSH 映射，不含凭据，按任务 ID 和状态标记 |
| `agent_vm_task_last_error_info` | gauge | 稳定错误 `code` 和 `phase`，不含原始错误文本，按任务 ID 和状态标记 |
| `agent_vm_warnings_total` | counter | 稳定警告 `code` |
| `agent_vm_up` | gauge | `0` 或 `1` |
| `agent_vm_observation_consistent` | gauge | 仅当原生快照通过持久 epoch 检查时为 `1`，否则为 `0` |

每个槽位的 Go 服务提供独立刷新的 `capacity`、`inventory` 和 `health` 快照，可以原子写入专属的内部 textfile/JSON 目录。Deployd 读取受保护的快照和控制观测，在唯一的 `GET /metrics` 中统一输出，保留各来源的失败和新鲜度语义。Prometheus 不直接读取槽位文件或连接槽位 socket。

`agent_vm_admission_memory_limit_bytes` 表示逻辑预留上限，`agent_vm_admission_memory_capacity_bytes` 表示控制环境可见容量；例如 2 倍内存超额预留时，两者分别为 64 GiB 和 32 GiB。服务 JSON 使用 `memory_mib`，指标编码器必须转换为 bytes。内存压力只是运行时观测，不能扣减逻辑内存余量。CPU PSI 必须来自已经验证的 cgroup v2 挂载根、`/proc/self/cgroup` 中唯一的 unified entry 和当前 cgroup 的 pressure 文件，不能用宿主或祖先 cgroup 的值冒充。Deployd 和 Prometheus 拉取尚未就绪时，Grafana 查询不能视为已工作的面板。

`admin-metrics` 是容量输入合同：`observation_epoch` 是采样结束时的持久状态 epoch，`observation_consistent` 表示原生探测期间 epoch 是否保持一致，`observation_attempts` 是有界重试次数，`observed_at` 是采样窗口开始时间，`up` 是控制服务读取成功标记。`resources` 表示逻辑准入，`default_admission` 表示默认请求及物理成本，`ct_cpu` 和 `ct_memory` 表示控制环境观测，`pending_operations` 列出原生结果仍不确定的 journal 项。

持久状态事务应保持短小，原生探测在事务和锁外执行；rqlite 切换后通过线性一致读取比较探测前后的 epoch。只有新鲜度明确可接受的原生观测才可采用较弱的一致性。epoch 只在写事务提交时改变，成功读取或无修改事务不能改变它。Deployd 必须保留 `observation_consistent=false` 和警告，不能将混合快照呈现为权威结果。读取、解析或验证失败必须暴露相应来源失败；`null`、缺失或未验证的限制、内存压力和 CPU PSI 都不能转换为零。

Deployd 将 `admin-metrics` 和 `admin-status` 作为独立采样通道。缓慢或不可用的 `admin-status` 请求不得擦除有效容量指标；失败的清单观测必须移除或失效任务行，不能静默保留过期清单。容量采样失败必须独立报告，不能被新完成的清单采样掩盖。各来源的时间戳与健康诊断必须与汇总的 `agent_vm_up` 区分。

`admin-status` 使用 keyset pagination，默认每页 25 条，最多 100 条；原生探测最多并发 8 个任务，每任务超时为 1 秒，每页超时为 5 秒，并输出 `partial`、`unknown`、`observation_consistent`、`probed`、`verified` 和 `timed_out`。未知或 `native_missing` 的任务仍未验证，不能显示为健康。全局 `agent_vm_task_states` 必须来自成功且一致的全量 `admin-metrics.task_states`，不能从分页清单推导；各已知状态在零任务时输出 `0`，容量观测失败时不能伪造零值。清单中的任务明细仍由清单采样负责。

Prometheus 只拉取 Deployd 的 `/metrics`，部署方通过 `job` 标识该唯一目标。槽位级序列必须带 `slot` 标签；部署级序列不能因为双槽位存活而重复。Grafana 只通过 Prometheus 查询，不读取 Deployd SQLite、槽位快照、Caddy 日志或 Unix socket。

文件系统指标是控制环境内独立的只读观测，与 Incus 数据池的磁盘准入分开。槽位在完成容量快照时采样 `/`、`/var/lib/incus/images` 和已配置的镜像上传暂存目录，Deployd 从该完成快照输出带 `slot` 与 `mountpoint` 的指标；`GET /metrics` 不触发新的文件系统探测。逐个验证路径和字节计数，失败路径输出 `agent_vm_filesystem_up=0` 并省略容量值；不能从来宾 rootfs、数据池用量或 PVE 指标推测这些容量。

采用已批准的超额预留部署配置时，`agent_vm_cpu_startup_pressure_threshold_percent` 为 `50`；这是组合启动保护阈值，不是逻辑 CPU 上限，公共指标仍支持其他部署配置。

`admin-metrics.reclaim_required=null` 表示磁盘准入未验证，该次采样必须省略 `agent_vm_admission_reclaim_required`，不能解释为无需回收。

磁盘数据必须区分两个领域：`disk_admission.guest_allocation` 是受管容器稀疏 rootfs 的分配量，`disk_admission.physical_admission` 是控制环境文件系统相对于低水位的已用量、预留量和余量。原生磁盘准入指标只映射物理领域，不能将稀疏 rootfs 配额总和与物理容量比较。容器数量准入位于 `instances.admission`；没有权威数量上限或 Incus 返回 `-1` 时，上限和余量为 `null`。

默认容器数量估算是派生观测，不是控制器限制。Deployd 从已验证的默认请求和物理成本计算 `logical_vm_slots`：取逻辑 CPU、逻辑内存和物理磁盘余量比值的最小值并向下取整；再应用控制环境可见 CPU 和启动压力保护，得到 `startable_vm_slots`。输入缺失、矛盾或未验证时省略估算，不得伪造零值。

## 看板合同

看板必须回答下一个受管容器还剩多少资源；包速率 `pps` 不得乘以 `8`，以免错误转换单位。

`admin-metrics.up=true` 要求读取成功，且初始 Incus 版本与实例探测成功。就绪、准入和压力分别表达，因此镜像不可用或准入被拒绝时可以出现 `up=true`、`admission_ready=false`。`observed_at` 保留最终有界采样窗口的开始时间，不能替换为完成时间使缓慢或过期数据显得新鲜。

provisioning 看板默认配置 `refresh: "auto"`，保留可选刷新间隔；页面重新加载后显示 `Auto selected`。

容量视图 `Agent VM / Capacity & Admission` 使用 `admin-metrics`，清单与操作视图 `Agent VM / Inventory & Operations` 使用 `admin-status`。两者只读，必须报告采样失败和新鲜度，不能保留已失效的任务行；另有独立 Deployd 看板展示两个槽位的角色和部署事务。

容量视图展示控制环境根目录与镜像暂存目录的已用和可用容量、数据池总量/已用/空闲、原生准入的分配/预留/余量/上限、默认请求及成本、任务状态数量和默认容器逻辑/可启动数量估算。来宾面板保持 `Guest rootfs allocation` 名称，并与物理数据池面板分开，避免把配额总量当成实际磁盘用量。

CPU 视图区分逻辑预留组成、控制环境可见容量、`some.avg10`、`full.avg10`、启动阈值和阻塞状态；内存视图区分逻辑预留与可见容量、`memory.current`、`MemAvailable`、压力和 OOM。清单按 `active`、`preparing`、`closing`、`deleting`、`failed`、`cached` 和 `absent` 筛选，缓存行默认折叠。CPU 使用 vCPU，内存和磁盘使用 GiB，lease 到期时间使用 Unix seconds。

权威上限使用水平虚线，包括数据池总量、低/目标水位、原生资源上限及配置的来宾警告/严重阈值。来源缺失、未验证或为 `-1` 时不绘制相应线，不用零值或猜测值替代。

看板同时展示准入、回收、drain、分阶段操作结果、镜像解包失败、警告、运行时间、新鲜度、当前任务和待处理/失败清理数量，用于解释数据池仍有余量时的创建失败。清单资源列按 `task_id|state` 联结，避免混合不同生命周期状态。网络 TX/RX 的包速率直接使用 `pps`；乘以 `8` 既不能正确表示包数，也不能得到 bytes per second。

## Provisioning 边界

公共仓库维护[脱敏、参数化的看板模板与校验器](../../../../deploy/observability/README.md)。Grafana 数据库不是配置权威。公共模板不包含部署地址、凭据、真实 datasource、任务名、监听路径或使用方导出的 JSON。

部署仓库通过显式参数生成 JSON、provisioning、Prometheus 拉取和 Deployd 监听配置。生成前必须替换 organization ID、datasource UID、controller job、看板 UID/标题/文件夹和容器内路径。生成结果写入版本目录，并原子替换稳定的 `current` 符号链接；provisioning YAML 原子替换。原生查询通过 `controller_job` 变量选择目标，CPU 容量来自 `agent_vm_admission_cpu_capacity_vcpu`，资源上限线只来自权威指标。

部署方的 `obs` 配置中，`grafana/dashboards/incus/incus-agent-vm.json` 由 `grafana/provisioning/dashboards/incus.yml` 加载，Grafana 只读挂载相关路径。恢复出的运行副本只可回写其所属部署仓库，记录所有者、校验和及来源提交后再审查；不能复制到公共仓库或替换公共模板。

只有 provisioning 配置源包含修改、唯一拉取目标输出约定指标、Grafana 已加载配置，且根目录、镜像暂存、物理数据池、来宾分配和原生准入查询均返回有效新鲜数据时，才能认定观测集成完成。没有任务时明细列表可以为空，但全局任务统计必须明确为零；缺失上限不能当作面板故障或伪造为零。

## 验证

部署前使用生成的 JSON 和 provisioning 运行公共校验器。验证 `refresh: "auto"`、页面 `Auto selected`、物理与来宾磁盘标题分离、包速率表达式不含 `* 8`，以及每条上限线均有权威指标或配置依据。通过 Prometheus datasource 查询各指标族，确认缺失或 `-1` 的上限不会变成零值或虚构线。

## 参考

- [原生 Agent VM 规范](../SPEC.md)
- [Agent VM 运维手册](../OPERATIONS.md)
- [Agent VM 仅内存预留 admission ADR](../../../adr/0029-agent-vm-memory-reservation-only-admission.md)
- [Agent VM overcommit 的 CPU 启动压力 ADR](../../../adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md)
- [Agent VM 容量与清单观测 ADR](../../../adr/0033-agent-vm-capacity-and-inventory-observability.md)

## 维护故障与准入

`doctor.ready` 和 `admin-metrics.admission_ready` 必须反映持久 reconcile 故障；默认准入原因包含 `maintenance_unhealthy`，不得仅因为控制进程可响应或 startup 曾完成就报告可准入。scheduler 的内部启动就绪与维护健康分离，故障期间继续有界修复，读取、取回、续租和释放仍受原有合同保护。故障只有在 pending 与 task 两个扫描均完成无失败周期后才清除，不能由无关页的一次成功覆盖。
