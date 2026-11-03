# Agent VM 拓扑与恢复运行手册

本文是 Native Incus Agent VM 合同的公共运维边界。
它记录部署证据实际建立的拓扑，以及现有客户端和控制合同能够执行的恢复操作。
它不是宿主机清单，也不声称知道观测到的 CT 下方的物理或虚拟来源。

本手册描述目标蓝绿部署控制器，同时保留蓝绿部署前的 SSH/SQLite 恢复基线。
目标部署使用槽位外 Go `agent-vm-deployd` 服务、统一 Agent VM 服务、稳定的项目 Caddy 和共享 rqlite，具体见[系统架构](../../architecture/system.md)、[部署控制器 ADR](../../adr/0045-agent-vm-deployment-controller.md) 以及[ADR 0044](../../adr/0044-agent-vm-state-migration-to-rqlite.md)。
Deployd SQLite 是独立的部署状态；不得把它作为业务状态打开，也不得在迁移后用它替代 rqlite。
业务状态完成一次性迁移后，不得把受保护的业务 SQLite 文件作为第二个活动权威恢复或打开；应按 ADR 0044 执行排空、导出、导入和验证。
状态后端迁移和灾难恢复与蓝绿服务切换是两条独立流程；蓝绿发布不替换业务数据库，也不把恢复目标直接提升为活动状态后端。

## 部署控制器恢复

`agent-vm-deployd.service` 是位于两个蓝绿槽位之外的稳定宿主级服务。
它的受保护本机 socket 提供 `agent-vm-deploy status --json` 和部署事务。
`/var/lib/agent-vm-native/deployment/deployment.sqlite3` 只包含事务阶段、阶段事件、失败证据和最新观测快照，不包含 session、lease、generation、operation journal 或 rqlite 业务数据。

Deployd 重启后，先验证服务身份、socket 所有权和数据库权限，再恢复最近的事务阶段，并重新读取 active 链接、两个 systemd 服务单元、两个槽位 socket、就绪状态、能力、控制器主权、流量计数器、pending operation 和 native-worker 状态。
阶段在重启前被记录过，不代表它已经完成。
如果任一来源过期、矛盾或不可用，deployd 必须报告 `needs_recovery`，阻止新的部署转换，并保留事务证据。
不得手工替换 `active`、停止槽位、获取控制器主权，或用 shell command 重放阶段。

正常的只读诊断流程是：

1. 执行 `systemctl status agent-vm-deployd.service`，检查受保护 socket。
2. 执行 `agent-vm-deploy status --json`，记录 transaction ID、phase、新鲜度和来源信息。
3. 对比 `active`、两个槽位服务单元状态、槽位角色/就绪响应，以及 native pending-operation/worker 证据。
4. 只有证据满足发布合同和观测合同后，才能 resume、rollback 或将事务标记为管理员恢复。

## 业务状态后端灾难恢复（不属于蓝绿切换）

本节只处理 rqlite 状态后端故障或数据损坏，不是蓝绿部署阶段，也不是 `agent-vm-deploy activate` 或 `rollback` 的子步骤。
rqlite 迁移完成后，它是唯一的活动状态权威；正常蓝绿切换只切换 Agent VM 服务槽位和受保护的 `active` 链接，不迁移、恢复、替换或重建业务数据库。
如果 rqlite 不可用、其数据未通过 schema/capability 或 digest 验证，或其 journal 无法验证，必须保持 state-write fence 和 native admission closed，同时保留对 effective lease 的只读访问；不得把旧 SQLite export 当作当前状态。
灾难恢复首先把已验证的 rqlite backup/snapshot 恢复到隔离的 recovery target；该目标只用于验证 coverage 和 tail，不得直接成为活动状态后端，也不得在此过程中替换 `active` 或切换蓝绿槽位。
Backup 必须记录不早于最后可信提交的 coverage boundary；随后用可信的 append-only journal 和 native effect evidence 对该 boundary 之后的 backup tail 执行 reconciliation。
单独检查 `state_epoch`、generation、operation journal 顺序、Token 撤销、audit record 和 native-effect 仍不足以证明 backup coverage。
只有 boundary 和 tail 都得到证明，并且经过独立的管理员授权状态后端恢复流程，才能恢复共享服务；该流程不属于蓝绿发布事务。

只有受保护证据证明 rqlite 状态后端启用后没有发生任何持久写入、epoch advance、lease change 或 native effect 时，迁移前 SQLite snapshot 才可接受。
如果可能发生过状态后端启用后的写入，就必须使用已验证且具有可证明 coverage boundary 和 tail 的 rqlite recovery source。
如果 coverage 或 tail 无法证明，必须保持 fenced，等待管理员授权的 journal/native inventory reconciliation；不得并行打开 stale SQLite、猜测新的 generation、从日志重建状态，或删除记录来强行达到 readiness。

### 恢复材料保留与删除

迁移源 SQLite snapshot/export、rqlite backup/snapshot、deployd `deployment.sqlite3` recovery copy（如有）、backup tail、manifest、tail-evidence 和 recovery authorization record 都是恢复材料。
蓝绿 `stage`、`activate` 和 `rollback` 只处理服务槽位与流量，不创建、替换或删除状态后端备份。

恢复材料的默认上限是三个已验证恢复点、三十天候选年龄和 1 GiB 总硬上限。
1 GiB 是实际占用上限，不是预分配空间，也不存在体积下限；小数据库按实际大小保留。
最后两个可验证恢复点始终受保护，迁移源 SQLite snapshot 在迁移完成并通过新的 rqlite restore check 后保留七天。

迁移或恢复事务未进入 `finalized` 前禁止删除任何恢复材料。
进入 `finalized` 后，只有在新的隔离恢复验证完成、没有 pending/unknown operation 或未解决的 `needs_recovery`、迁移源保留期已到期，并且清理后仍保留两个可验证恢复点时，材料才可进入 `purge_eligible`。

状态迁移或灾难恢复流程的 retention worker 在每次成功备份后和每小时执行清理；蓝绿 Deployd 不读取、创建或删除这些业务恢复材料。
它按数量、年龄或总字节预算任一超限清理最旧的可删除材料。
清理后仍无空间时，报告 `backup_storage_exhausted` 并拒绝新的备份或状态迁移，不删除受保护恢复点，也不影响正常蓝绿切换。

该上限还要求状态数据库自身有界。
`operations`、`audit`、`management_operations` 和 `management_audit` 中已完成且不再被恢复流程引用的历史记录，默认保留三十天后回收；pending/unknown operation、活动 operation、当前租约、generation、Token 撤销和恢复所需审计不得回收。
需要长期保留的审计必须先归档到状态库之外，不能通过放大备份预算解决。

删除迁移源 SQLite snapshot 前，还必须已经存在当前和上一个可验证的 rqlite 恢复点，并由运维者针对准确的材料 ID 执行带审计的 purge。
清理失败不改变活动槽位、`active` 链接、state authority 或事务状态；禁止使用目录级 cleanup 强行删除材料。
临时 preflight/acceptance 目录可以按其自身生命周期清理，但一旦文件登记为恢复材料，就遵守同一套删除门禁。

## 拓扑与来源

| 层 | 已建立的职责 | 边界 |
| --- | --- | --- |
| Codex client | 本机 `agent-vm` CLI 选择 session，记录本机 operation journal，并通过配置的 SSH 发送一个 protocol-4 request。 | 客户端可以识别其配置的 control entry，但不能推断或重建该入口背后的机器。 |
| Agent VM control endpoint | 配置的 SSH alias/address 到达一次性 control process 及其规范配置、持久状态、lock 和 native timer 资源。 | 这是 control entrypoint，不是下层宿主机、LXC owner、storage provider 或 rebuild authority 的证明。运维记录中的具体私有 address 保留在公共仓库之外。 |
| CT109 LXC | 部署证据证明 CT109 是包含控制环境以及本部署使用的 Incus client/daemon 边界的 LXC。 | CT109 是部署事实，不声称知道创建、运行或能够重建它的机器。 |
| CT109 内的 Incus container | 每个 session 最多拥有一个 non-ephemeral Incus system container，通过 native proxy NAT 和明确的 lease/generation 到达。 | Container 的 stopped cache 和 guest data 在逻辑上独立于 control database，但公共合同不承诺独立物理 disk 或独立 storage failure domain。 |
| 下层 hosting source | 下层 hosting source 未知；没有公共或已验证证据识别 CT109 下方的物理/虚拟机、hypervisor、infrastructure manager 或 storage system。 | 不得用猜测的 PVE、Incus host、CT host、physical disk 或 provisioning API 替代缺失证据。 |

拓扑合同中的固定层名是 `Incus container inside CT109` 和 `Underlying hosting source`；`Unknown underlying hosting source` 表示下层来源没有证据。
Configured control endpoint 是入口事实，`not proof of the underlying host`。

已验证的部署记录包含匹配的 revision、控制文件/配置的受保护副本，以及持久 state/journal 材料。
它们还记录生命周期证据没有建立宿主机重启和下层 storage behavior。
Control SSH failure（包括 host-unavailable diagnostic）只能证明配置的入口在该次观测时不可用；不能识别缺失的层级，也不能授权 rebuild command。

公共仓库按设计不保存已解析的 address、私有 filesystem path、凭据、实时 inventory 和部署专用 checksum，这些内容保存在受保护的运维记录中。
即使配置的 address 和 CT identifier 出现在同一份私有记录中，也必须将它们视为两个独立事实。

## 恢复边界

恢复分为控制面恢复、state/native reconciliation 和重新开放 admission。
宿主机或 CT rebuild 超出本仓库权限：只有在 owner-authorized infrastructure procedure 提供 control target identity、rebuild authority、storage/pool disposition，以及证明该 CT 和其 Incus data 属于目标部署的证据后，才能开始。

### 替换前保留

替换文件或重启恢复后的 control process 前，必须从控制环境及其受保护 backup 中保留以下内容：

- 已配置的 state database、WAL/journal file、operation record、audit record、task row、lease generation、resource reservation、last-error detail，以及父目录的 ownership 和 mode。
- Deployd 部署数据库、事务事件、失败证据、最新观测快照、socket 所有权，以及父目录所有权和 mode。
  必须与业务状态数据库分开，不能合并两套恢复流程。
- 规范 configuration、已安装的 client/control/systemd revision 和 checksum、timer directory、受保护 recovery copy、SSH identity/known-host 材料，以及精确配置的 project/profile/image/network/ACL/pool/state path。
- Incus pool 和所有 owned container data，包括 stopped sparse cache；control database 不包含 guest filesystem，不能作为它的替代品。
- lock directory 及其 ownership/mode。
  Active lock file 属于 process state，只有恢复后的 process 获得 exclusive access 后才能重新创建；删除 durable database 或 operation journal 不是 lock-recovery step。

Native outcome 不确定时，不得 rotate、compact、convert 或 rewrite pending operation record 或 generation。
不得仅因为 control record 缺失或不完整，就删除 Incus instance、port mapping、timer、cache 或 conntrack entry。

### 恢复匹配的控制组件

1. 如果 control endpoint 不可用，在 provenance boundary 停止，并取得 owner-authorized CT/rebuild evidence。
   不得尝试其他 address，不得通过假定的 host 进入 CT109，也不得使用猜测的 infrastructure API。
2. 目标 control environment 可用后，在接触已安装的 client、一次性 control script、configuration 或 native unit 前保持 admission drained。
   如果无法证明 drain state，保持 control service stopped，并将 admission 视为 closed。
3. 原子恢复一个已批准 release manifest 的 client/control/unit artifact 和规范 configuration，并保持受保护的 ownership 和 mode。
   原位恢复已有 state database 和 journal；不得在旧 database 旁初始化新的空 database，也不得为了适配旧 record 而降级 protocol。
4. 恢复或验证 native expiry/reconciliation unit 及其 timer directory。
   不能仅凭 unit file 声称完成恢复：仍必须 reconciliation state database、native instance、mapping 和 generation metadata。
5. 恢复或验证 `agent-vm-deployd.service`、其受保护 socket 和独立部署数据库。
   允许部署转换前，必须重新观测两个槽位。
6. 执行只读健康和清单检查：`agent-vm doctor`、`agent-vm admin status`、`agent-vm admin session list`、`agent-vm admin reconcile` 和 `agent-vm-deploy status --json`。
   除非显式提供 `--apply`，reconcile command 都是 dry-run。

常驻 scheduler 将 reconcile 与 cache age-GC 保持为独立的 single-flight lane。
每轮最多处理 16 个 task 或运行 2 秒，并在 additive `maintenance_cursor` meta
record 中保存 restart cursor 和 retry backoff。缺少 cursor 时任务立即到期。
不得删除或手工修改 cursor 来强制 cleanup；恢复时必须与 state database 和
operation journal 一起保留。历史 timer operation rows 仍可检查和恢复，但正常
scheduler tick 不再创建新的 timer rows。

### Reconcile pending operation 与 generation

本机 client journal 和 control `operations` table 是 mutation 的 identity record。
Transport timeout 或 `outcome: pending` 表示 native effect 不确定，绝不是成功信号。

- 将原始 `operation_id`、request payload 和 generation 一起保留。
  绝不创建替代 ID、修改 generation，或把不确定的 native effect 作为新 intent replay。
- 在提交任何其他操作前，检查 `status`、`admin status`、`admin session list` 和 dry-run reconciliation result。
  Completed 或 error record 只有在使用原始 operation ID 和完全相同 request 时才能 replay；pending record 必须保持 blocked，直到 native state 明确。
- 对每个 durable task 对比其 session、task identity、generation、state、lease deadline、resource reservation、native instance ownership、native status、port 和 timer token。
  Generation mismatch、record 缺失、foreign identity 或无法执行 inspection 都是 stop condition。
- 将 `native_missing` 视为有界 observation。它不是 lifecycle state，也不足以单独删除 task：terminal absence 还必须有匹配的 destroy journal、generation tombstone、worker quiescence 和精确 absence proof。包含 `partial`、`unknown` 或 `observation_consistent=false` 的 page 保持 unverified。
- 只有 dry-run 确认 ownership、generation、lease state 以及 native cleanup/rearm action 全部正确时，才能应用 `agent-vm admin reconcile --apply`。
  确认其 native instance 和 port 均不存在后，reconciliation 可以验证并修复 resource/reservation state malformed 或不一致且非零的 `absent` record；也可以关闭 incomplete cleanup 或 rearm 当前 generation。
  不得制造 generation，也不得重新打开 stale mapping。
- 按现有合同保持 effective lease 可用于 status、retrieval、renewal 和 release。
  带 stopped 或未经验证 guest 的 effective lease 属于 unavailable；不得推断为 OOM，也不得自动 restart。

### 重新开放 admission

只有在目标 control environment 观测到以下全部条件后，才能恢复 admission：

- 已批准的 release manifest、兼容的 client/control artifact、规范 configuration、database/journal、ownership/mode 和 native unit 存在且可读。
- `doctor` 报告 component health，control database 可写，已配置的 CT-local Incus pool filesystem 可测量，并且 image/project/network/ACL policy 已验证。
- 每个 task 和 port record 要么已 reconciliation 到已确认的 native state，要么明确保持 blocked；不得静默忽略 pending operation、generation mismatch、unknown resource、partial resize 或 cleanup transition。
- 已恢复 generation 的必需 timer 和 expiry/reconciliation behavior 已验证，不能用猜测的 host 或 storage provenance 替代缺失证据。

只有这样，获授权的运维者才能运行 `agent-vm admin drain exit`，并立即重复 `agent-vm doctor` 和 `agent-vm admin status`。
任何检查失败都必须保持 admission drained。
`admin gc --apply`、`destroy`、cache deletion 或大范围 port/conntrack cleanup 都不能替代 state recovery。

### 状态缺失必须停止（Missing state is a hard stop）

如果 state database、operation journal、generation record、受保护 configuration 或权威 native inventory 缺失、损坏或不一致，停止控制面并保留剩余数据（Missing state is a hard stop）。
不得猜测 ownership（Do not guess ownership）、重置 generation、创建新的 state database、重新 acquire session、删除 cache 或全局清理 mapping；不得删除 Incus instance（Do not delete an Incus instance）。

继续操作所需的最低证据包括：control entry 与 CT109 之间的 owner-authorized mapping、匹配的 state/config backup、已安装 revision/checksum、目标 Incus project/pool，以及能够证明 task ownership 和 generation 的 native inventory。
缺少这些证据时，物理/虚拟宿主来源和 rebuild path 都是 blocker；正确结果应是 incident report，而不是 cleanup 或 admission。

## 控制面发布与兼容性

当前蓝绿发布是由运维者通过本机 `agent-vm-deploy` CLI 和槽位外 deployd 执行的人工操作。
Release manifest 覆盖客户端、槽位二进制文件、deployd/systemd 资源、来源 revision、精确校验和、协议，以及每个操作的请求/响应/语义兼容性。
它排除部署配置、凭据、连接材料、业务和部署状态数据库、WAL/journal、lock、Incus 容器数据和 guest image。
SSH 仍是旧版兼容传输，不是当前部署事务的权威。

正常事务依次为 `stage`、`standby_validation`、`controller_handoff`、`traffic_enable`、`active_switch`、`drain_old`、`quiescence` 和 `finalize`。
这些阶段只处理服务产物、槽位角色、流量和 native worker 收敛；不执行状态迁移、rqlite backup restore、业务数据库替换或状态后端 endpoint 切换。
候选槽位在当前槽位保持 active 时启动并验证。
交接期间两个槽位进程可以存活，但只能有一个槽位持有控制器主权。
排空会停止旧槽位的新流量，并等待在途请求、pending operation、native worker 和 scheduler work 收敛；不能强制终止它们。

旧客户端只有在 manifest 声明动作兼容时才能继续使用该动作，response 会携带结构化兼容警告。
未知 action、不兼容的 protocol/schema/semantic 或未验证产物必须在产生副作用前失败。
激活前失败保持当前槽位和 active 链接不变。
激活后健康或 quiescence 失败允许通过 deployd 向上一个已验证槽位执行一次有界 rollback；rollback 失败时保持部署和业务 admission fenced，不能改写 state、generation 或 uncertain operation。

恢复 finalize 不是对旧阶段记录的盲目重放。
Deployd 必须重新确认目标槽位、撤销旧槽位的 controller 和 traffic role，完成排空与 quiescence，并确认旧槽位已经停止；之后才能重新授予目标槽位 role、恢复流量并持久化 finalize。
撤销、停止或观测失败时，事务保持 `needs_recovery`，两个槽位都必须 hard-fence。

## 稀疏磁盘与故障域

Agent VM disk admission 不会为每个 container 预留完整请求的 rootfs allocation（does not reserve the full requested rootfs allocation）。
Incus container root filesystem 可以是 sparse。
Admission 使用配置的 CT-local Incus pool filesystem 测得的 free space、配置的 low/target watermark、request cost 和 durable reservation；stopped cache 会消耗 disk，但不消耗 CPU 或 memory allocation budget。
因此 rootfs limit 是 allocation limit，不承诺物理上已经预留对应 capacity。

这里的固定合同名称是 `configured CT-local Incus pool filesystem` 和 `separate logical failure domains`；前者指配置指定且可测量的 CT-local pool filesystem，后者指 control state 与 container data 的逻辑隔离，不宣称物理磁盘隔离。

控制面由 state database、WAL/journal、operation/audit record、规范 configuration、lock 和 native timer asset 组成。
Container data 由 Incus instance metadata、sparse backing storage、guest filesystem 和 stopped cache 组成。
它们是不同的逻辑故障域和 ownership/recovery domain，但不承诺部署使用不同的物理 disk 或独立的底层 storage。

CT-local Incus pool 的 ENOSPC 或 I/O failure 可能阻止 sparse growth、创建新 container、恢复 cache 或扩展 guest filesystem。
Disk admission 在测量缺失、无效或低于配置的 disk threshold 时必须拒绝新 demand；不得驱逐已有 effective lease。
CT memory pressure 单独报告，不拒绝逻辑上可用的 memory reservation。
Partial native change 在 inspection 和显式 recovery 或 release 前，保留其 conservative reservation 和 error detail。

CPU overselling 有独立的 startup-schedulability boundary。
批准的 overcommit deployment policy 将 `cpu_startup_pressure_full_avg10_percent` 设置为 `50`；只有 projected CPU 超过 CT-visible CPU capacity 且已验证的 CPU PSI `full.avg10` 达到该 threshold 时，才以 `resource_pressure` 拒绝新的 boot。
该检查发生在 restart 停止已有 guest 之前，不改变 logical CPU limit，也不停止 effective lease。
`some.avg10` 和 `full.avg10` 是 pressure observation，不是 CPU utilization 或 allocation count。
PSI sample 缺失时报告 observation failure，绝不能转换为零。
公共示例仍可配置；在验证已安装配置前，不得把它当作现网部署使用特定 threshold 的证据。

Control-state filesystem 的 ENOSPC 或 I/O failure 更为根本：如果无法持久记录，就不能信任 operation identity、generation、cleanup 和 timer decision。
保持 admission closed，停止依赖 durable state 的 mutation 和 cleanup，保留 database/journal，并在 reconciliation 前恢复可写的 control storage。
不能仅因 Incus daemon 有响应，就推断 container data 安全；existing effective lease must not be evicted。

Guest-level ENOSPC 是独立观测（Guest-level ENOSPC is a separate observation）：guest command 可能失败，而控制面和 CT pool 仍然健康。
保留 command error 和 lease state，并使用正常 retrieval/release 规则；不得把 guest ENOSPC 报告为 CT filesystem 或下层宿主机已满的证明。

Container rootfs growth 在 rootfs mutation 后是独立的 fail-closed boundary（separate fail-closed boundary after the container rootfs mutation）。
改变 guest 前记录 capability 和 topology evidence（Before changing the guest, record capability and topology evidence），并要求 Incus storage/profile path 能够执行和验证请求的 rootfs limit（Incus storage/profile path that can enforce and verify the requested rootfs limit）；root filesystem 已达到请求 rootfs capacity 至少 95% 时视为幂等成功（already at least 95% of the requested rootfs capacity is an idempotent success）。
只有分类为 busy 或 stale-filesystem 的 failure 才能获得一次有界 reread 和 settle retry（one bounded reread and settle retry）。
Guest command diagnostic 只保留 stdout prefix 和 stderr tail，并在 structured error 中保留原始 guest phase、command、return code、存在时的 truncation flag、command timeout 时的 process-group cleanup evidence（process-group cleanup evidence on command timeout）、capacity evidence 以及有界 attempt/retry chain（bounded attempt/retry chain）。

如果 acquire 无法验证 guest root growth，不得保留 active lease 或 SSH mapping。
Cleanup 将 record 留在正常可 retry 的 cached 或 cleanup transition，并记录 `acquire_partial` detail；重试同一 operation 前先检查 guest 和 durable state。

本合同的任何部分都不能阻止所有 ENOSPC condition，也不为已过期 lease 提供 recovery guarantee。
它只定义在 capacity 或 provenance 无法验证时的安全 admission 和停止行为。

## 容量与清单观测

使用 `agent-vm admin metrics` 作为 logical admission、CT-visible capacity、pressure、disk readiness、operation outcome、image-unpack failure、warning、新鲜度和 task-state count 的聚合来源。
使用 `agent-vm admin status` 作为 per-task state、native status、guest availability、resource、lease expiry、SSH mapping 和 structured last-error code/phase 的有界清单来源。
两个 action 都是只读的；`agent-vm-deployd` 的 Prometheus scrape 不得修改 lease 或 native state。

容量视图必须分别展示 CPU、memory 和 disk 的 `allocated`、`reserved`、`allocated_and_reserved`、逻辑 `remaining` 和逻辑 `limit`，并同时展示 CT-visible capacity 和 runtime pressure。
它还必须展示 policy default request/cost，以及两个 snapshot estimate：logical default-container slots 和 current startable default-container slots。
第一个值根据已验证的 CPU、memory 和 physical-disk headroom ratio 的最小值计算；第二个值应用当前 CT CPU capacity 和 startup-pressure gate 计算。
输入未验证时省略对应估算，绝不能称为 native maximum 或 safe maximum。

清单视图覆盖 active、preparing、closing、deleting、failed、cached 和 absent row。
先排序 active 和 transitional work，再排序 failed cleanup，最后排序 cached/absent；cached row 默认折叠，并提供 state filter。
保留 identifier、state、有界 resource、lease/mapping state 和 error code/phase，但省略 raw error text、凭据和部署 filesystem path。
每次成功 scrape 后删除 stale per-task series；任一只读来源无法读取或解析时，暴露 `up=0` 和 freshness failure。

## 相关合同

- [Native Incus Agent VM 规格](SPEC.md)
- [Agent VM CLI 合同](contracts/cli.md)
- [Native 部署边界](../../../skills/agent-vm-ops/references/deployment.md)
- [Agent VM 拓扑与恢复边界](../../adr/0028-agent-vm-topology-recovery-boundary.md)
- [Agent VM 控制面更新边界](../../adr/0030-agent-vm-control-plane-update-boundary.md)
- [Agent VM 部署控制器](../../adr/0045-agent-vm-deployment-controller.md)
- [Agent VM 观测合同](contracts/observability.md)
- [部署控制器合同](contracts/deployment-controller.md)
