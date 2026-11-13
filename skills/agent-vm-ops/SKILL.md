---
name: agent-vm-ops
description: 在明确的基础设施任务中维护已配置的 native Incus Agent VM 安装、镜像、timer、容量准入和精确 reconciliation。日常代码执行使用 agent-vm。
---

# Agent VM 运维

本 Skill 仅用于明确授权的基础设施任务。
日常命令和 admin command 共用已配置的管理访问；`session_id` 是 ownership metadata，不是 credential boundary。
CLI 见[命令合同](../../docs/specs/agent-vm/contracts/cli.md)；生命周期和容量策略由 [SPEC](../../docs/specs/agent-vm/SPEC.md) 维护。

进行控制面重建或恢复时，遵循[拓扑与恢复运行手册](../../docs/specs/agent-vm/OPERATIONS.md)。
该手册区分已配置的 control entry、CT109、managed Incus system container 和任何未经验证的下层宿主机；它不授权宿主机维护，也不授权猜测 rebuild path。

## 检查与维护

先运行 `agent-vm doctor`、`agent-vm admin status`、`agent-vm admin session list` 和 `ssh -G <configured-control-alias>`。
Alias 和 address 必须来自明确的运维配置；日常 VM SSH 使用 task connection information。
Mutation 前检查精确的 candidate revision/checksum、component version、native unit、Incus project/pool/network/profile 和 CT-local filesystem capacity。

计划维护时进入 `agent-vm admin drain enter`，检查 drain status，等待 effective lease 结束，并确认 cleanup 已关闭 NAT/flow 且停止 container。
Drain 拒绝新的 lease 和 restart admission，允许在线 retrieval/release，不会静默延长 deadline。
已经越过短 admission fence 的 restart 可以在不持有 global lock 的情况下完成。
强制中断需要单独的明确授权，并列出受影响的 resource。

检查完整的控制环境 package transaction 和所需的 native dependency（包括 conntrack-tools）；不得用局部升级替代。
升级后检查 native Incus/systemd/OpenSSH 行为。
本 Skill 不授权外部宿主机生命周期变更、自动重启控制环境或大范围 cleanup。

只部署来自同一 revision 的正式候选版本：包括[Go 控制服务](control/cmd/agent-vm-control/main.go)、槽位外的 deployd 服务、[format-2 bridge](scripts/agent_vm_control_bridge.ts)、完整客户端包和[systemd 资源](assets/systemd/)。
路径和验证遵循[部署参考](references/deployment.md)，Incus project/profile policy 遵循[初始化参考](references/initialization.md)，native OS 和 guest prerequisite 遵循[运行时合同](references/control-runtime.md)。
Format-1 release 可以保留 `agent_vm_control.ts`；format-2 client 通过既有 SSH entry 调用固定的 Go `request` adapter，adapter 使用受保护 UDS。
修改 client 前先 build、verify 和 publish matching manifest。
八个客户端文件和依赖哈希 sidecar 必须作为一个可恢复 package 安装；更新受保护的连接材料时不得同步任何 consumer runtime home。
每个槽位保持一个 Go 业务控制权威，并通过 deployd 只向一个槽位授予 scheduler/native-mutation role；不得让 legacy Bun authority 并发运行。
只有 deployd 报告新鲜的槽位健康状态、控制器主权、流量排空、startup reconciliation、expiry/GC 和 storage admission 证据后，才能退出 drain。

控制面发布是运维者负责的人工 release operation，入口是目标服务器上的 `agent-vm-deploy update`，不是公共 `agent-vm` 生命周期 command。Agent 只通过已配置的 SSH alias 提交版本和 source revision；release 下载、目标架构选择、provenance/checksum 校验和 blue-green transaction 都在目标服务器上完成。不得从 Agent 工作站下载 release、使用 `scp`/`rsync` 上传 release bundle，或依据 Agent 工作站架构选择 native package。
遵循[部署参考](references/deployment.md)和[发布合同](../../docs/specs/agent-vm/contracts/release.md)中的 release manifest、兼容性、staging、drain、原子 activation、rollback 和权限规则；兼容 action 可以跨 release revision 继续使用并返回结构化 warning，不兼容 action 必须在产生 native effect 前失败。
自动更新默认保持关闭。

`agent-vm` skill 中的 `upload`、`download`、`export` 和源码同步只服务于 guest/source 工作流；不得把这些通用传输能力用于发布或部署。正常线上更新使用 `ssh <configured-control-alias> agent-vm-deploy update --version <semver> --source-revision <sha>`，低层 `stage`、`verify`、`activate` 和 `recover` 仅用于事务诊断和受限恢复。

Client/control 使用 protocol 4，并增加显式的 live memory-only adjustment；不兼容的旧 peer 必须在产生 effect 前失败。
升级前保留 database、generation 和 journal。
Go service 最多只对已保存的、没有 native worker effect 的 pending drain transition 执行一次同 operation recovery；pending native worker identity 通过 reconciliation 处理，不 replay 不确定 action；证据缺失或仍不确定时保持 admission closed，等待显式 operator reconciliation。
Drain 期间，使用受保护的 recovery copy 原子安装匹配的 candidate file/config。
验证已接受的 aggregate CPU/RAM 和 VM-count quota 缺失状态，同时保留 container-only、无关限制和 per-VM limit。
日常请求遇到 project configuration 冲突时，control 应报告冲突而不是修改配置。
健康检查失败时保持 drain；恢复期间 quota 冲突应报告，不能删除 cache。

## 镜像与精确清理

准备 Debian 13 container image，包含 OpenSSH、Bun、Python、rsync、Docker/Compose 和 growpart，验证 container startup，并固定已接受的 container-image fingerprint。
Bun 执行一次性 guest disk 和 source action；source staging 前验证 native FFI/locking/atomic rename、GNU `timeout`、Git 和 rsync。
通用 image 中的 Python 不提供 client/source fallback。
Container rootfs growth 还要求 Incus storage/profile capability 能够执行并验证请求的 limit；选定 backend 无法提供该边界时必须 fail closed。
项目专用 toolchain 不应放入通用 image（Keep project-specific toolchains out of the generic image）；镜像 capability 必须单独维护和验证。
VM image 必须拒绝；managed pool 只接受 container image。
保留 non-ephemeral instance，并设置 `boot.autostart=false`。

先检查 `agent-vm admin reconcile` 和 `agent-vm admin gc` 的 dry-run。
只有在维护任务的精确范围内才能使用 `--apply`。
删除前重新检查 managed identity、lease generation、state 和 native resource。
Manual GC 选择所有已停止、没有有效 lease 且已确认访问清理完成的 managed cache，不受 age 限制。
查看 preview 后只应用已授权范围。
Native age timer 独立选择至少 idle 24 小时的 cache；admission 触发的 disk reclaim 按 age 从旧到新选择 eligible cache，并在空间足够后停止。
没有最小 cache 保留时间。
绝不处理有效 lease、transition object、当前 resize target、未知 resource 或 revocation backup。
不得全局 flush conntrack。

只能在 CT 的 Incus pool filesystem 内检查实际 disk allocation 和当前 free space。
PVE storage inspection 和外部 capacity probe 不属于本 Skill；不得请求 PVE address 或 access。
缺少本机测量时必须阻止 admission。
CPU/RAM 使用独立 budget。
Pressure 不得停止 effective lease。
报告真实 I/O/ENOSPC 或 partial resize/cleanup failure，并保留 non-active record 以便有界 retry。

将 non-oversold base budget 保持在已验证的 CT-visible capacity 内。
配置至少为 1 的有限 ratio；部署策略使用 32768-MiB CT-visible memory budget 和 2x logical memory ratio，得到 65536-MiB logical memory ceiling。
这些是逻辑 allocation ceiling，可以超过 CT-visible capacity；只有 base 必须适配。
Allocation ceiling 计入 occupied/transition record 的 conservative max(current,target) reservation，并排除 stopped cache。
Memory observation 只使用可见 CT root cgroup 和已验证的 LXCfs meminfo；unlimited cgroup memory 要求 LXCfs。
Capacity metric 无效或缺失时拒绝新的 admission，同时 query 暴露结构化错误，已有 lease operation 仍可用。
当可见有效 capacity 的 10% 与 1 GiB 中的较大值以下时，报告 memory pressure 并告警，但不得阻止逻辑上可用的 memory reservation，也不得抢占 guest。
不得自动增加 base、检查 ancestor、添加 balancer，或通过压迫生产 memory 验证 overselling。

CPU PSI 是 startup-schedulability 观测，不是逻辑 CPU admission 的替代品。
当 projected CPU 超过 CT-visible CPU capacity，且已验证的 `full.avg10` 达到 `cpu_startup_pressure_full_avg10_percent` 时，必须在改变 native state 前以结构化 `resource_pressure` 拒绝新的 acquire/resume/restart startup。
不得停止 effective lease、降低 logical CPU remaining，或阻止 read、renew、release 或 live memory-only resize；缺失 PSI 必须保持为明确的 observation failure。

Dashboard 集成遵循[观测合同](../../docs/specs/agent-vm/contracts/observability.md)。
CT root 和 image-staging filesystem 必须与 physical pool 和 guest rootfs allocation panel 分开。
Native admission limit 来自 Go service 导出的 snapshot；未设置的 Incus project quota 不是 admission limit。
服务 HTTP API 是受保护的控制边界，不是公共 metrics broker；relay 只读取已有 snapshot。

故障测试使用 ownership 明确的临时 container 和隔离 storage。
不得填满生产 disk、修改生产时钟或影响无关 flow。
报告 candidate identity、command/result、server-time observation、storage fact 和剩余 blocker；Incus daemon health 单独不能证明完整 service acceptance。
