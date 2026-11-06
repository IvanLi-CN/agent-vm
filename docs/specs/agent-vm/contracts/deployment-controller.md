# Deployd 合同

本文定义 Deployd（进程名 `agent-vm-deployd`）及其运维 CLI `agent-vm-deploy` 的目标合同。Deployd 负责部署协调和部署观测，但不是第二个 Agent VM 生命周期权威、业务状态后端或公共业务 HTTP 服务。发布包提供该服务、CLI 和槽位模板 unit。format-4 发布器负责把已验证的控制二进制和 manifest 安装到蓝、绿两个槽位。实际配置、状态后端凭据和运行目录仍由部署方提供。

## 边界

`agent-vm-deployd.service` 是位于蓝、绿槽位之外的稳定宿主级服务。它在槽位切换期间保持不变，并且一次只负责一个部署事务。CLI 通过 `/run/agent-vm-native/deployd.sock` 连接。Caddy、浏览器、Agent Token、公共 `agent-vm` CLI 和槽位业务 API，都不能访问这个 socket。

全新安装由 Go 安装器与 Deployd 的首次安装事务创建并验证受保护的槽位目录，并在初始化、角色及就绪验证完成后创建 `/var/lib/agent-vm-native/active -> slots/blue`。已有部署必须验证当前 `active` 指向 blue 或 green，不能覆盖断裂或指向未知目标的链接。

Deployd 可以读取或协调以下部署事实：

- 受保护的 `active` 符号链接和 release manifest；
- 两个槽位的 systemd unit 和 Unix socket；
- 槽位的就绪状态、状态能力、状态纪元和 release identity；
- 槽位的 `controller` 角色、`traffic` 角色、在途请求数和请求计数器；
- 槽位的 `admin-metrics`、`admin-status`、`doctor`、native-worker 和 pending-operation 观测结果。

Deployd 的普通蓝绿事务 MUST NOT 写入业务状态数据库、调用 rqlite、修改 Incus 资源、签发 Agent Token 或提供业务/API 路由。槽位 Go 服务仍是生命周期状态、操作日志、租约、代数、准入和 native effect 的唯一权威。普通蓝绿事务只协调 release、槽位角色、流量排空和 native quiescence。它 MUST NOT 执行状态迁移、rqlite backup restore、业务数据库替换或状态后端 endpoint 切换。首次部署的 `migrate` 事务是唯一例外：它由 Deployd 受保护地协调一次 legacy SQLite 到 rqlite 的切换，读取旧状态、写入 rqlite、验证结果并持久化完成标记。该例外不改变 Deployd 的业务状态权威边界；迁移完成后，rqlite 是唯一业务状态权威，Deployd 只保存迁移事务和有限的迁移 snapshot。

## Deployd 与观测入口

Deployd 是常驻的部署服务进程。`agent-vm-deploy` 是它的运维 CLI 客户端。CLI 负责提交部署变更和读取组合状态，不是 Prometheus 的采集器。

Deployd 同时提供两个相互独立的本机入口：

- `/run/agent-vm-native/deployd.sock` 供 `agent-vm-deploy` 执行受保护的读取和变更操作；
- 部署配置指定的内部 HTTP 监听地址上的 `GET /metrics`，供 Prometheus 拉取只读指标。

`/metrics` 监听必须绑定到部署配置指定的 loopback 或私有管理地址，不能挂到 Caddy，不能成为公共业务路由。Prometheus 跨主机拉取时，部署必须通过私有网络访问控制并使用相互认证的 TLS。具体地址、端口和证书属于部署配置，不写入本仓库。该监听只提供 Prometheus exposition format，不提供写操作、事务操作、业务 API 或任意文件读取。

Prometheus 只配置一个指向 Deployd 的 scrape job，推荐名称为 `agent-vm-deployd`；保留既有 job 名称时，必须同步 dashboard 的 `controller_job` 绑定。Grafana 只查询 Prometheus，不直接访问 deployd socket、槽位 socket、SQLite 数据库或 snapshot 文件。

Prometheus 的部署配置只创建一个 Deployd target，结构等价于：

```yaml
scrape_configs:
  - job_name: agent-vm-deployd
    metrics_path: /metrics
    static_configs:
      - targets: ["<deployment-managed-deployd-metrics-address>"]
```

`<deployment-managed-deployd-metrics-address>` 是部署系统提供的地址，不属于仓库内容。不得为两个槽位的 socket、textfile 目录或 JSON snapshot 另建 scrape target。

Grafana 面板通过 Prometheus 查询 Deployd 输出，例如：

- `agent_vm_deployd_slot_ready{slot=~"blue|green"}`：比较两个槽位的就绪状态；
- `agent_vm_deployd_slot_controller_owned{slot=~"blue|green"}`：确认唯一 controller lease；
- `agent_vm_deployd_slot_selected{slot=~"blue|green"}`：确认受保护的 `active` 指针；
- `agent_vm_deployd_transaction_phase` 与 `agent_vm_deployd_recovery_required`：显示事务阶段和恢复阻塞；
- `agent_vm_deployd_observation_up`：按来源识别观测 lane 故障；
- `agent_vm_deployd_slot_in_flight_requests` 与 `agent_vm_deployd_slot_pending_operations`：判断旧槽位是否可以 quiescence。

面板必须保留 `slot` 和 `source` 维度，不能把蓝、绿槽位聚合成无法区分来源的单一值。

`deployd.json` 必须为 metrics listener 提供部署配置。至少包含 `metrics_listen`。当地址不是 loopback 时，还必须包含用于校验 Prometheus client certificate 的 CA、服务端 certificate 和 private key 路径。loopback listener 可以使用本机访问控制；私有管理地址必须使用 mTLS 和网络 ACL。这些路径、地址和端口是部署输入，不能写入 release manifest、公共 dashboard 或本仓库的测试 fixture。

`/metrics` 请求只读取 deployd 最近一次完成的观测快照，不在 HTTP 请求中触发槽位采样、部署变更或 native effect。Deployd 的后台采样分别读取两个槽位的 `admin-metrics`、`admin-status`、 `doctor`、native-worker 和 pending-operation 观测，并结合 `active` 符号链接、systemd、角色合同和流量计数器生成最终快照。来源至少包括 `active_link`、`systemd`、`slot_contracts`、 `native_observation` 和 `inventory_observation`。systemd 身份校验失败必须能与槽位健康合同失败分别观测。容量与清单采样必须保持独立的 observation lane；一条 lane 失败不能擦除另一条 lane 的最新有效指标。

槽位指标必须带 `slot="blue"` 或 `slot="green"` 标签。切换期间两个槽位可以同时存活，deployd 不能把两个槽位同名的 series 直接合并、覆盖或重复暴露。部署级指标由 deployd 统一输出，使用 `agent_vm_deployd_` 前缀。槽位业务指标保留既有 `agent_vm_` 名称，但由 deployd 加入槽位维度，并只输出一份最终结果。缺失、矛盾或过期的来源保持为省略、`unknown` 或独立的 freshness failure，不能伪造为健康的零值。因此，在途请求、pending operation 和 native worker 计数器没有有效采样时，Deployd 必须省略对应 series。零值只表示已经成功采样并确认数量为零。

至少必须稳定提供以下部署级指标：

| 指标 | 类型 | 语义 |
| --- | --- | --- |
| `agent_vm_deployd_up` | gauge | deployd 进程和 `/metrics` 监听是否可用；Prometheus 无法连接时仍由 Prometheus 自身的 target `up` 表示抓取失败。 |
| `agent_vm_deployd_snapshot_fresh` | gauge | 组合观测是否仍在 Deployd 的 freshness bound 内。 |
| `agent_vm_deployd_observation_up` | gauge | 按 `source` 表示某个运行时来源最近一次观测是否成功。 |
| `agent_vm_deployd_observation_fresh` | gauge | 按 `source` 表示该来源的最近一次观测是否仍在 freshness bound 内。 |
| `agent_vm_deployd_observation_timestamp_seconds` | gauge | 按 `source` 表示来源快照的实际采样时间。 |
| `agent_vm_deployd_slot_process_up` | gauge | 按 `slot` 表示槽位进程是否存活且身份可验证。 |
| `agent_vm_deployd_slot_ready` | gauge | 按 `slot` 表示槽位是否通过当前 release 的 readiness 合同。 |
| `agent_vm_deployd_slot_selected` | gauge | 按 `slot` 表示 `active` 当前是否指向该槽位。 |
| `agent_vm_deployd_slot_controller_owned` | gauge | 按 `slot` 表示该槽位是否持有唯一 controller role。 |
| `agent_vm_deployd_slot_traffic_accepting` | gauge | 按 `slot` 表示该槽位是否接受新流量。 |
| `agent_vm_deployd_slot_in_flight_requests` | gauge | 按 `slot` 表示已观测的在途请求数。 |
| `agent_vm_deployd_transaction_phase` | gauge | 按 `phase` 表示当前事务阶段；同一时刻只允许一个阶段为 `1`。 |

部署必须为 Prometheus 配置一个指向 deployd `/metrics` 的 scrape target。槽位生成的 textfile 或 JSON snapshot 只能作为 deployd 的内部采样材料，不能同时作为 Prometheus 的第二个 scrape target。两个槽位必须把内部 snapshot 写入不同的目录，例如 `/run/agent-vm-native/blue/metrics` 和 `/run/agent-vm-native/green/metrics`。共享的 `/run/agent-vm-native/metrics` 不能作为蓝绿槽位的共同输出目录。deployd 必须按槽位读取，并在最终 series 中保留 `slot` label。

## 角色与不变量

部署状态必须区分以下角色：

| 角色 | 含义 | 证据来源 |
| --- | --- | --- |
| `selected` | 受保护的 `active` 符号链接指向的槽位，接收新请求。 | 原子符号链接目标和 release manifest。 |
| `process` | 槽位进程存活，且 PID/启动身份可验证。 | `systemctl show` 和 socket identity。 |
| `standby` | 槽位已通过就绪验证，但尚未成为新流量目标。 | 槽位就绪状态和 traffic-role 合同。 |
| `traffic` | 槽位是否接受新流量，以及当前在途工作量。 | 受保护的槽位计数器；Caddy 日志只能作为辅助证据。 |
| `controller` | 槽位是否可以运行 scheduler、reconciliation、age GC 和 scheduler-driven native mutation。 | Deployd 授予的进程持有型角色租约和槽位角色响应。 |
| `native` | 槽位 worker 和 pending operation 是否已经得到可验证的收敛结论。 | 槽位观测和 operation journal 响应。 |

在暂存、交接和排空期间，两个槽位进程 MAY 同时存活。同一时刻最多只能有一个槽位持有 `controller` leadership。在有界交接期间，`selected` 槽位 MAY 与控制器槽位不同。仅凭进程存活、`active` 指针或 readiness 成功响应，MUST NOT 认定流量已经排空或 native 已经 quiescent。

槽位 Go 服务 MUST 在运行 startup reconciliation，或启用 scheduler、timer reconciliation、age-GC 和 scheduler-driven native-mutation 循环前，先获取并认证 deployd 授予的 controller role。standby 或 retiring 槽位可以提供受保护的就绪状态和只读观测，但没有该 role 时 MUST NOT 执行 `StartupReconcile`、`timer-reconcile`、 `timer-age-gc` 或任何等价的后台 native mutation。管理台的镜像导入、镜像选择或删除、VM action、maintenance action 以及 management operation recovery 同样属于 controller-gated mutation；没有该 role 时只能提供管理查询，不能 claim operation、调用 native backend 或写入终态 journal。role 丢失时，必须先 fence 这些循环，槽位才能进入 retiring 或 standby。controller role 发生变化时，正在执行的 native mutation 必须收到取消信号。

支持续租的槽位在 health 中明确声明 `controller_lease_renewal=true`。Deployd 必须先确认该能力；未声明的旧槽位继续使用原有的更高 term 授权路径，不能向其发送同 term 请求而引发周期性拒绝和 hard-fence。旧路径仍有角色更换会中断在途作业的历史限制，因此升级须保持 drain，在新槽位完成验证前不得宣称长操作续租验收通过。对于明确声明续租能力的槽位，持续协调不得通过每次更换 term、lease ID 或 nonce 来续租，否则会取消正在进行的 native mutation。同一 Deployd 进程只能根据新鲜运行时观测延长自己持有的有效 lease 的 expiry，必须保持 slot、term、lease ID、generation、nonce、owner PID 和进程启动标识不变，且不得缩短 expiry。槽位接受这种续租时不触发角色变更通知；真正的撤销、过期、身份变化和 owner 退出仍立即 fencing。已过期或已撤销的 lease 不得复活；Deployd 重启后必须重新观测并分配更高 term，不能从持久化快照恢复身份。缺少进程启动标识时不允许复用旧身份。

首次授予后的就绪等待最多六十秒；等待期间只能通过已验证进程的角色通道延长同一有效 lease，通道失败或身份变化时立即停止并 fencing。持续协调复用本轮完成并持久化的观测更新指标，不为指标发布再次串行采集一遍业务状态；已就绪且身份未变的控制器续租后无需重复等待启动协调。

业务槽位的后台容量、清单和健康采样复用同一次只读状态快照，每十秒完成一轮，单轮最多八秒；不能用三条独立循环重复同步远端状态。部署事务停在 standby validation 或尚未执行的 controller handoff 时，只能续租当前 Deployd 进程已经验证、尚未过期的 previous/active lease，不能因为等待运维者执行下一命令而撤销权限，也不能为过期角色分配新身份。不确定阶段、恢复事务和进程身份不符仍须 fencing。

当清单观测本身新鲜且一致，但部分任务的原生状态尚未确认时，Deployd 必须区分“观测不完整”和“观测矛盾”。只有其他来源、进程身份、状态纪元和能力均有效，并且活动槽位已经确认 admission drain 时，才允许重新授予唯一 controller lease，使 startup reconciliation 和显式维护能够收敛失败记录。此时 `controller_recovery_only=true`、组合观测仍为 `consistent=false`，业务流量保持关闭，不能用于 stage、activate 或 rollback 的就绪证明。清单不完整且未确认 drain 时不得重新授予 controller。若清单只是部分原生状态未确认、各观测来源仍新鲜且相互一致，活动槽位仍独占持有同一 Deployd 进程的有效 lease，并明确支持原身份续租，则允许只延长该 lease 的 expiry，使正在进行的生命周期操作完成；不得分配新身份、恢复已过期 lease 或等待业务 ready 来决定是否撤销现有权限。此时两个槽位的 traffic admission 均保持关闭，组合观测仍不构成部署就绪证明；清单完整且槽位 ready 后才恢复当前活动槽位的流量。除上述一致但不完整的观测与下述业务快照不一致时的原身份续约外，任何来源缺失、过期、矛盾时仍必须 hard-fence。即使清单完整，drain 期间也必须保留维护所需 lease，并保持 traffic disabled；不能因为启用流量被 drain 拒绝而反复撤销 controller lease。

对于旧版本遗留的 `destroy`，日志明确为 `starting`、`start_attempted=true` 时，发布恢复仅接受两种进程身份：PID、进程组和启动标识全部缺失，或三者完整有效。身份部分缺失、非正整数、无效启动标识必须拒绝。完整身份还必须确认原进程和进程组已不存在；不能向可能已复用的 PID 发送信号。发布器必须验证候选声明 `attempted_destroy_start=1` 和发布排空要求；仅声明旧版无身份恢复能力的候选不能处理完整身份记录。符合这些条件才允许核验原生 worker 的静默状态。此原始恢复对同一 operation 最多执行一次，已有恢复证据时不得覆盖原证据；除下述旧身份携带修复外，不得再次自动解除未知启动屏障。业务控制服务必须持有有效 controller、发布归属的 admission drain，以及跨进程 drain、maintenance 和 session 锁；当前不得存在活动操作。原请求、摘要、operation ID、session 和 generation 必须匹配日志，任务必须仍为同 generation 的 `deleting`。静默检查必须逐一验证旧单服务和蓝绿槽位的 systemd 身份、`KillMode=control-group`、`Delegate=no`、实际 cgroup 和其中的进程，禁止残留子进程，并在 Incus 全项目后台操作为空后重新核验相同进程边界。静默验证只能将 worker 记为已退出，并在同一事务中将原 worker、验证依据和 `unknown` 结果保存到操作日志和既有审计记录；兼容旧版本终结日志时也不能丢失审计证据；原销毁仍为 pending，不能据此声明成功。随后由现有 Incus 清理流程继续原 operation ID，重新核验实例和映射清理结果后才能终结日志。缺失、矛盾、活动操作、角色失效或验证失败时保持 pending 和排空。

原生命令确认退出后，后续命令登记新的 `starting` 屏障时必须清除上一命令的 PID、启动标识和进程组；`exited` 与无清理错误的 `finished` 都可以进入这个新尝试，仍未确认退出的 worker 身份不可替换。旧版本若已将原恢复中的身份错误携带到新屏障，仅允许一次专门修复：当前 worker 必须恰含完整旧身份、`starting`、`start_attempted=true` 和晚于原恢复观察时间的 `prepared_at`，三项身份必须与原 `unknown` 静默恢复证据完全一致，且不得已有专门修复证据。修复仍执行上述角色、发布排空、请求与任务身份和原生静默检查，并重新证明原进程及进程组不存在；在同一事务中将 worker 记为 `exited`、追加独立修复证据和审计，保留原证据和 pending 结果。随后只能由原 Incus 清理流程继续原 operation ID，确认实际资源已删除后才写入成功。其他新身份、时间矛盾、缺失证据或再次受阻都必须保持 fenced，不能重复自动修复。

发布封装器只对上述形态且无在途操作的有界 `destroy` 列表提供修复产物安装入口。已验证的候选 binary 必须通过只读 `recovery-capabilities` 声明对应恢复能力；旧候选、其他动作或包含任何不完整进程身份的记录不能使用该入口。安装事务必须先持久化待恢复项，停止 Deployd 和全部业务 unit，核验 MainPID 与 cgroup 均为空、Incus 全项目后台操作为空，再安装并启动完整产品。封装器不修改业务数据库；新业务控制器负责再次核验发布归属和原日志身份，恢复被拒绝时继续排空，不能将安装完成等同于生命周期恢复完成。普通蓝绿转换仍要求既有的严格 quiescence 证明。

如果底层 effect 不能确认已经停止，生命周期或管理操作都必须保持 pending/unknown，不能写入 success。槽位启动后的管理 operation recovery 必须在 controller role 授予后执行，不能在 role 尚未建立时由 standby 自行恢复。除上述一致但不完整清单和下述业务快照不一致时的原身份续约外，当 Deployd 无法确认观测、部署状态或角色通道时，必须先对两个槽位执行本地 hard-fence：清除槽位的 controller role 和 traffic role。如果角色 socket 不可用，则停止对应 systemd 单元作为兜底。hard-fence 不依据旧状态重新授予角色，也不分配新的持久化 term。后续恢复必须重新观测，并使用更高的持久化 term 授予新 role。

槽位进程启动时默认不得接受新流量。只有 Deployd 在新鲜观测通过后授予 controller role，并重新观测到槽位 ready，才能启用当前 `active` 槽位的 traffic role。Deployd 或槽位重启后必须重新观测、授予 role 并启用流量，不能依据旧状态直接恢复。

Deployd 在完成旧槽位 quiescence 后退出共享 drain 时，`preserve_traffic=true` 必须保留调用槽位当前的 traffic role：已单独启用的目标槽位保持启用，仍被关闭的槽位保持关闭。该参数不能强制关闭目标槽位，也不能自动开放 standby。槽位健康响应中的 term 和 generation 必须按整数无损读取，经过观测与续租后仍须匹配原 lease 身份。

槽位的只读 `admin-metrics` 必须在 native startup、restart 和 cleanup 持有排空边界锁期间继续响应，不能让 controller lease 的续租观测等待同一个长操作锁。发布封存只在该锁可以立即取得、活动操作为零、当前仍由发布事务排空且状态纪元与已完成观测一致时执行；否则跳过本次封存，保留排空并等待后续观测，不能阻塞采样或提前声明 quiescence。

controller 下的维护循环必须按 lease deadline 判断 `active` 和 `preparing` 的到期资格。`preparing` 不表示 effective lease，但未来的 deadline 仍必须保留，不能提前创建到期清理日志。无效 deadline 必须报告状态未验证。旧版已终结的 `timer-expire` 只有请求身份和摘要匹配、明确成功返回 `expired=false`，且 worker 明确为 `starting`、`start_attempted=false` 时，才允许保留原日志并创建绑定当前已到期 deadline 的确定性新清理意图；pending、失败、身份冲突、已尝试启动或证据缺失均不能借此重试或改写。

正常生命周期和维护游标写入可能在容量与清单采样期间推进 state epoch。业务观测不一致时，单次协调最多重新采样三次，失败样本不能授权新的 controller 身份或启用流量。若三次仍未收敛，但当前 active 槽位的 `active` 链接、systemd 进程和角色通道均有新鲜证据，只有一个 controller，且它仍持有同一 Deployd 进程的有效 lease、完整身份与初次采样相同，则关闭两个槽位的 traffic admission，并只续约这份现存 lease，允许在途操作和清理继续。业务快照仍报告不一致；只有取得完整、新鲜且一致的观测后才能恢复流量。租约已经过期、身份变化、出现第二个 controller、角色来源缺失或过期、关闭 traffic 失败、观测持久化失败或续约失败时，仍立即 fencing。不得从旧快照或事务记录重建授权。部署事务也必须在每次新鲜一致的阶段观测后续约同一进程仍持有的候选 lease，不能让多阶段检查耗尽初始角色期限。

控制器持有排他的宿主级协调锁。槽位 controller role 是进程持有型的；deployd 或持有 role 的槽位失去已认证角色通道后，该 role 立即失效。deployd 重启会使所有尚未验证的交接失效，并要求重新观测后才能授予新的 role。不能根据过期的 SQLite 行重建 role。

维护 CLI 和浏览器均可进入 admission drain。发布事务以 `release-` 开头的 `session_id` 显式接管尚未归属于发布事务的排空状态，包括维护 CLI 会话和浏览器操作；接管只改变排空归属，必须保持 admission 与 traffic 关闭。另一项发布事务、维护 CLI 或浏览器都不能替换已有发布事务的归属。只有当前发布事务能退出 drain，旧维护会话不能在交接后重新开放准入；退出前仍须完成发布健康与收敛检查。

历史版本留下的发布 `drain-enter` 若因归属冲突保留为 pending，恢复必须沿用原 operation ID。只有原请求摘要与日志身份匹配、generation 为零、不含 native worker、已经排空且当前归属允许交接时，才允许在已有一次恢复尝试后再执行一次有持久化标记的有界恢复；不能接管其他发布事务或重新打开 admission。归属冲突在产生副作用前被拒绝时必须记为确定失败，不能再留下不确定操作。发布封装器只有在活动槽位确认已排空、没有在途操作，且 pending 项全部为身份有效、不含 worker 的发布 `drain-enter` 时，才允许安装已验证的修复产物并交由新控制器恢复原记录；其他 pending 操作仍按原安全检查拒绝或验证，不得借此绕过原生操作收敛，也不得为该检查打开业务数据库。

## 事务状态机

持久化事务阶段为：

```text
stage
  -> standby_validation
  -> controller_handoff
  -> traffic_enable
  -> active_switch
  -> drain_old
  -> quiescence
  -> finalize
```

`rollback` 是回到上一个已验证槽位的有界事务路径。当阶段效果或证据不明确时进入 `needs_recovery`，包括守护进程在不可幂等边界期间重启的情况。该状态必须保持阻塞，直到运维者提供事务标识且 deployd 重新观测所有必需来源。任何阶段都不能跳过缺失的就绪状态、能力、纪元、主权、流量、在途请求或 native-worker 证据。

各阶段的语义是：

| 阶段 | 进入下一阶段前的必需证据 |
| --- | --- |
| `stage` | Bundle 标识、产物精确 checksum、目标槽位和暂存路径已验证。 |
| `standby_validation` | 候选进程、socket、release identity、就绪状态、共享状态能力和状态纪元兼容。 |
| `controller_handoff` | 旧 controller role 已释放或 fenced；候选槽位持有已认证的进程持有型角色租约。 |
| `traffic_enable` | 候选槽位接受新流量并提供新鲜计数器；直到切换边界前，旧槽位仍保持 selected。 |
| `active_switch` | 同一文件系统上的 `active` 替换成功，重新观测到的目标与事务一致。 |
| `drain_old` | 旧槽位拒绝新流量，同时保留在途请求和 native 观测能力。 |
| `quiescence` | 在途请求、pending operation、native worker 和 scheduler work 都有已知的 terminal/converged 证据。 |
| `finalize` | 候选槽位保持健康；旧槽位已停止且不再持有 controller 或 traffic role；旧 release 仍保留为兼容 rollback 目标；事务结果已持久化。 |

## 观测响应

`agent-vm-deploy status --json` 返回有界响应，结构如下。缺失或矛盾的证据必须返回 `null` 或 `unknown`，并携带来源的新鲜度，不能伪造健康值：

```json
{
  "transaction_id": "<uuid>",
  "phase": "drain_old",
  "observed_at": "<rfc3339>",
  "active_slot": "blue",
  "controller_slot": "green",
  "slots": {
    "blue": {
      "process": "running",
      "readiness": "ready",
      "traffic_role": "retiring",
      "in_flight": 2,
      "native_quiescence": "pending",
      "source_observed_at": "<rfc3339>"
    },
    "green": {
      "process": "running",
      "readiness": "ready",
      "traffic_role": "accepting",
      "in_flight": 0,
      "native_quiescence": "quiescent",
      "source_observed_at": "<rfc3339>"
    }
  },
  "sources": {
    "active_link": "fresh",
    "systemd": "fresh",
    "slot_contracts": "fresh",
    "native_observation": "fresh"
  },
  "outcome": "pending"
}
```

`observed_at` 和每个来源的 `source_observed_at` 必须是不晚于当前时间的有效 RFC3339 时间戳。Deployd 必须拒绝未来时间戳，不能用未来时间延长 freshness window。`admin-metrics` 结果必须明确给出 `observation_consistent=true` 才能作为一致业务观测；缺失或为 false 时不能授予新 controller 身份或启用流量。现存 lease 的续约只能使用上述独立、新鲜的进程和唯一主权证据，不能把不一致的业务样本解释为健康。

确切 wire schema 仍由 deployd CLI contract 版本化。响应 MUST 标识 transaction 和 release，区分 selected/process/traffic/controller/native 角色，保留各来源时间戳，并暴露 failure code 和 phase。不得包含原始命令输出、凭据或私有部署路径。Caddy access log 可以辅助证明请求完成，但不能作为在途请求或 quiescence 判断的唯一来源。

## 部署 SQLite

Deployd 使用独立的本机 SQLite 数据库，路径为 `/var/lib/agent-vm-native/deployment/deployment.sqlite3`。选择它是因为部署事务需要阶段转换的原子性、并发运维读取、可索引的事务历史和守护进程崩溃恢复。它不是业务 `state.sqlite3`，不是 rqlite，也不承载业务状态；但它必须记录一次性 `migrate` 事务的阶段、失败证据和恢复所需的引用，才能在 Deployd 重启后继续执行有界恢复。

最小逻辑 schema 如下：

| 表 | 内容 | 保留/权威语义 |
| --- | --- | --- |
| `deployment_transactions` | 事务 ID、kind、来源/目标/上一个 release、目标槽位、phase、outcome 和 timestamp。 | 每个 deployment/rollback/recovery transaction 一行；对 deployd workflow phase 具有权威性。 |
| `deployment_phase_events` | 单调递增的事件序列、phase、transition、source、timestamp、actor 和有界结构化 evidence digest。 | 只追加的事务历史；不包含原始命令输出或凭据。 |
| `deployment_failure_evidence` | Failure code、phase、source、观测时间、有界 evidence payload 和 digest。payload 中的 `cause` 必须经过限长、控制字符清理和私有路径脱敏。 | 保留到事务 finalize 且 retention policy 允许清理为止；写入时必须拒绝与 payload 不匹配的 digest。 |
| `deployment_observations` | 每个事务最新的有界组合快照、来源时间戳、新鲜度和 digest。 | 每个事务只保留最新快照；不是轮询日志，也不能覆盖运行时事实。 |

已 `finalize` 且不再被恢复流程引用的 `deployment_transactions`、`deployment_phase_events` 和 `deployment_failure_evidence` 默认保留三十天后回收。活动事务、`needs_recovery` 事务、未解决的 phase、pending/unknown operation 和恢复所需证据不得回收。Deployd 只管理 `/var/lib/agent-vm-native/migration/` 下由首次迁移创建的 `migration-*.sqlite3` snapshot；它不创建或清理 rqlite 的业务 backup/snapshot。迁移 snapshot 的单文件上限是 1 GiB，没有体积下限，迁移完成后保留七天，由 Deployd 的维护循环逐个删除到期且未被活动事务引用的文件。

数据库 MUST 由 deployd 服务账号保护，使用有界 transaction lock 和与所选 SQLite 模式匹配的 fsync/atomic commit 语义，并拒绝没有明确兼容声明的 schema/version 变化。数据库故障会阻止新的阶段转换，但不能让 deployd 声称槽位已停止、已排空或已激活。数据库恢复可写后，服务必须保留事务的可恢复性并重新观测运行时状态。

## 本机 API 与授权

Unix-socket API 提供只读的 `status`、`transaction` 和 `plan` 操作，以及需要授权的 `verify`、`stage`、`activate`、`rollback`、`recover` 和一次性的 `migrate` 操作。每个变更请求都要携带或创建 transaction ID、release/bundle 标识、明确的目标槽位，并在提供时携带操作原因。Socket 检查 peer credentials，服务在产生文件系统、systemd 或 slot-role effect 前完成运维授权。不存在任意 shell、path、SQL 或 Incus passthrough。`migrate` 不是通用 rqlite passthrough；它只接受固定的首次迁移流程，不能由调用方提交任意 SQL、URL、文件路径或 systemd unit。

CLI 永远不能直接编辑 `active`。响应丢失时，CLI 返回 deployd transaction ID 和 status；不能创建新事务来猜测 activation 或 rollback 是否发生。

## 首次状态迁移

首次部署不能要求运维者先手工迁移状态。发布器安装并验证 Deployd 后，自动调用 `POST /v1/migrate`；Deployd 在同一个 durable transaction 中启动 rqlite、让旧服务进入写围栏、等待请求和 native worker 收敛、停止旧服务、取得旧状态目录的 `authority.lock` 并保持到迁移完成、确认没有 SQLite writer、创建一致 snapshot、导入并验证 rqlite，最后写入 `.rqlite-cutover-complete`。旧服务的 Bun 入口必须在创建 SQLite store 前取得同一把排他的 `authority.lock`；如果旧服务在停止后重新启动并竞争这把锁，Deployd 只会闭锁，不会把竞态当成迁移成功。

已有完成标记且其绑定的 Deployd 事务已经 finalized 时，完整产品升级只验证该既有迁移结果，不再调用 `migrate`，也不要求旧 SQLite 服务或旧数据库文件存在。缺失、冲突或未 finalized 的证据不能作为跳过迁移的依据，仍按原有的恢复围栏和有界重试规则处理。

迁移事务的阶段依次为 `migration_prepare`、`migration_drain`、`migration_quiescence`、`migration_stop`、`migration_source_check`、`migration_import` 和 `migration_finalize`。迁移成功后发布器才创建初始 `active -> slots/blue` 链接并启动蓝、绿槽位；普通 `stage`、`activate` 和 `rollback` 不再执行这条路径，也不替换业务数据库。

任何步骤的结果不明确都必须保持 legacy 服务停止或围栏，并把事务置为 `needs_recovery`。发布器的固定 activation fence 在迁移恢复完成前持续存在；Deployd 重启也不能根据旧状态猜测迁移成功，必须重新验证停止状态、writer 状态和目标 rqlite 内容。自动 `recover --action retry-migration` 的次数由迁移事务中的持久计数限制为一次，失败后仍保持 `needs_recovery`，不能通过重新启动发布器绕过限制。

如果 Deployd 在创建源 snapshot 之前重启，且迁移事务仍处于 `migration_stop` 或 `migration_source_check`、没有 `source_verified` 证据，恢复流程可以在重新确认 legacy 已停止和 writer 已关闭后继续创建这一次 snapshot。只要已经存在源摘要证据，恢复流程就必须复用原 snapshot；snapshot 缺失、被替换或摘要不一致都必须闭锁，禁止重新采样 legacy 数据库。完成标记必须在删除 legacy SQLite 及其 WAL/SHM 文件之前持久化；如果进程在这两个动作之间退出，恢复流程必须验证同一 transaction、源摘要和 rqlite 目标摘要，复用已完成的导入并继续删除和收口，不能再次导入或重新采样。迁移已完成但初始 `active` 链接或槽位启动失败时，后续发布只需验证完成标记、finalized 事务和 rqlite 目标，不得再次要求 legacy SQLite 文件存在。迁移恢复的自动重试是一个可恢复的逻辑尝试：额度提交后如果 Deployd 在实际导入前退出，重启可以继续这一次尝试；只有导入返回明确失败后才耗尽额度，不能因进程退出窗口把尚未执行的恢复永久锁死。

## 恢复与验证

启动时，deployd 加载最近事务，验证数据库 schema，并重新观测每个运行时来源。只有再次观测到对应运行时证据，记录的 phase 才能视为完成。如果无法证明该 phase，deployd 进入 `needs_recovery`，保持部署和受影响的 admission fenced，并保留证据。只有运维者确认事务标识且必需来源新鲜、相互一致后，恢复流程才能 resume、rollback 或 finalize。执行 recovery finalize 时，deployd 必须先确认目标槽位仍然是已验证的 active 槽位，撤销旧槽位的 controller role 和 traffic role，完成 drain/quiescence，并停止旧槽位。只有重新观测到旧槽位已经停止、目标槽位重新持有 controller role 且可以接受流量后，才能把事务写成 `finalize`。撤销、停止或重新观测任一步失败，都必须保持 `needs_recovery`，并对两个槽位执行 hard-fence，不能猜测恢复已经完成。

目标验证覆盖 Go unit 和 socket permissions、SQLite migration 和 crash recovery、重复协调器防护、双槽位进程存活、唯一 controller leadership、候选槽位验证、原子 active switch、traffic drain、在途请求完成、native-worker quiescence、有界 rollback，以及守护进程在每个不可幂等阶段重启时的行为。验证在当前会话拥有的 Agent VM 中运行，不修改开发机或现网部署。

## 全新安装事务

`agent-vm-deploy install --config <受保护输入> --bundle <产品包> --materials <同修订资料目录>` 默认只执行只读预检，不创建安装记录、锁、解包目录或系统资源；显式 `--apply` 才执行本机安装。安装输入只引用受保护的配置与证书文件，不内联凭据。安装器固定目标路径，拒绝替换不匹配的已有文件；已有蓝绿安装只核验就绪并指向升级入口。

全新安装使用受保护 UDS 的 `POST /v1/bootstrap`，请求包含安装器先行持久化的 `transaction_id=install-<32位小写十六进制>` 和已验证产品包的绝对路径。Deployd 必须先确认 active 链接和旧业务 SQLite 均不存在、两个已安装槽位 unit 均已停止，再创建部署事务。Deployd 通过已验证 blue 槽位的 `agent-vm-control initialize-state` 委托业务状态层直接初始化空 rqlite，不生成 SQLite 迁移源。该入口不授予角色，也不启动 scheduler。

Deployd 随后验证初始 standby 的 release、状态能力和进程身份，分配持久化 fencing term，通过既有角色通道授予 blue 唯一 controller lease，观测 ready 后开启 traffic 并原子切换 active。最终新鲜一致观测通过后才终结事务，安装器最后启动 Caddy。同一安装事务的重复请求只允许返回已完成结果；其他阶段不得自动重放。

初始化或切换结果不确定时必须保留原事务并 fencing。active 尚不存在时，只能显式使用 `recover --transaction <原ID> --action resume-bootstrap`：首先再次确认没有 active 或旧 SQLite，停止槽位，在业务状态层验证现有 rqlite 的受支持 schema 且没有 VM task 或生命周期 operation 后恢复初始化，不替换或清空数据库。active 已存在时拒绝该动作，使用既有 `finalize` 恢复合同重新验证原候选。安装记录只保存阶段、修订、摘要和原事务 ID，不作为角色授权来源。

首次安装事务在慢速组合观测期间，通过独立的角色通道核验已授予 controller 的进程身份和完整 lease 身份，并只延长仍有效的同一 lease。进程变化、lease 到期、身份不匹配或通道失败均终止续期并进入原事务恢复，不能分配新 term 来掩盖丢失。事务结束或请求取消会停止该续期。UDS 保留 30 秒请求读取预算，响应写入上限为 9000 秒，以覆盖既有迁移事务的最长等待；metrics 监听的短超时不变。

槽位启动后的进程身份等待预算为 60 秒，与槽位 systemd unit 的启动预算一致；期间仍须验证实际 PID、进程启动标识及 health 合同，超时进入失败或恢复，不能仅凭 systemd 已启动判定成功。

运行时副作用失败后的证据记录与双槽位 fencing 使用独立的 30 秒收尾预算。请求取消或超时不能取消这段收尾；收尾只保存不确定结果并撤销角色，不能继续激活或重放安装。

`recover finalize` 在重新授予 controller lease 前要求目标进程就绪、观测新鲜一致且发布身份和状态能力已验证，不要求依赖 controller role 的业务就绪。授予新 lease 后必须完成 startup reconciliation 并验证业务就绪，才能开放 traffic；fenced 槽位的业务未就绪不能形成恢复授权的循环依赖。

恢复路径重新验证原事务包及目标实际进程后，在业务就绪和最终组合观测期间使用与首次安装相同的 lease 身份连续性检查。blue 和 green 均适用；任何失败先停止事务内续租，再保存证据并 fencing，不能因事务锁阻止常规协调而让恢复中的 lease 自行耗尽。

槽位通过受保护 UDS 的只读 `GET /internal/v1/deployment-observation` 一次返回 capacity、inventory 和 health。三类观测在同一个业务状态只读事务中生成；rqlite 权威同步一次后，进程内镜像在读取期间保持同一视图。该路径不授予角色、不封存 drain、不写业务状态，也不对外暴露指标。八秒内无法完成时整组失败，不能拼接部分成功的结果。Deployd 仍验证每类结果、时间戳、状态纪元和完整性，再发布已完成快照；旧槽位明确返回 404 或 405 时保留原采样兼容路径，其他错误不能降级为成功。

### 状态后端与共享原生指标的验证

槽位健康观测提供 `state_authority`，其值为去除凭据后的业务后端配置指纹；它不是数据库内容标识。Deployd 验证候选时比较双方指纹与状态能力，并将指纹写入部署阶段证据；激活、恢复和回滚继续绑定该证据。`state_epoch` 仅证明单次快照内的一致性，不能作为跨时刻后端身份。没有指纹的旧阶段证据不得授权角色转换。正常蓝绿部署仍禁止数据库替换。

Incus 项目级共享 `incus_*` 指标由槽位只读采样，Deployd 只输出活动槽位的新鲜一份，保留原生标签且不附加 `slot`；槽位级 `agent_vm_*` 指标继续附加 `slot`。`agent_vm_incus_metrics_up` 标明各槽位该采样是否成功，失败或过期不得补造容器指标，Prometheus 不增加第二个 target。

回滚遇到原 Deployd 的未完成 `drain-exit` 时，仅在两个槽位的 controller 与 traffic 均已 fencing、没有在途操作、全部待处理记录都是 Deployd 的零代 metadata drain-exit 时，才可通过业务 API 重放原 operation ID。业务服务再次校验原请求摘要、不可变身份、无 native worker 标记及 drain 所有权，保留恢复尝试证据；随后重新进入 drain 并证明 quiescence。未知记录、native mutation 或其他所有者仍保持 `needs_recovery`，Deployd 不直接修改业务数据库。drain 控制请求使用既有 240 秒控制请求预算；健康、角色与指标请求仍使用短超时。

激活自授予候选 controller 起，直到旧槽静止、停止和最终验证结束，必须持续通过角色通道核验并续期同一 lease；补偿回滚或 fencing 前先停止续期。旧槽静止计数复用组合只读观测的 capacity lane，仅在明确不支持该端点时兼容旧查询，不能因请求失败降级。只读 status 不持有角色写入锁，指标发布独立读取最近完成的持久化快照；正常活动事务不能被误判为恢复失败，只有明确 `needs_recovery` 或恢复标记才强制不一致。所有快照仍保留原始新鲜度边界，发布器不延长时间戳、不执行观测或角色授权。

只读 status 返回本次观测但不覆盖用于指标发布的 latest 记录，避免并发的旧观测覆盖较新部署证据。激活请求取消后，不继续补偿切换；使用独立的三十秒有界清理上下文记录不确定结果并 fencing，保留原事务等待显式恢复。
