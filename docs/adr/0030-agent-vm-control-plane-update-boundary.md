---
status: accepted
---

# Agent VM 控制面更新边界

Agent VM 控制面仍以运维者拥有的控制环境为权威，因为 Incus 访问、持久生命周期状态、租约代数、native 映射、容量准入和 systemd 维护必须由一个权威统一协调。
本机 `agent-vm` 客户端仍是版本化 SSH 请求客户端，不得取得 Incus/PVE 管理权，也不得成为第二个控制面。

控制面发布、更新和回滚属于运维者拥有的部署工具或流程，不属于公共 `agent-vm` CLI。
远端一次性控制进程仍是 SSH 请求和 systemd timer 共用的规范执行产物；发布必须显式执行并可审计。
无人值守的控制面更新默认关闭，也不属于核心协议。

对于目标蓝绿部署，运维者使用槽位外 Go `agent-vm-deployd` 服务及其本机 `agent-vm-deploy` CLI。
本历史边界中描述的 SSH/wrapper 流程仍是兼容传输；当前事务阶段、双槽位观测、控制器主权和流量排空由[ADR 0045](0045-agent-vm-deployment-controller.md) 定义。

控制面发布是一个逻辑兼容单元，包含本机客户端、远端控制脚本、systemd unit 资源和发布元数据。
它排除部署配置、凭据、连接材料、状态数据库和 WAL/journal、锁、Incus 容器数据及 guest image。
发布使用配置好的控制 SSH 入口、运维者专用身份或最小范围的 root-owned 部署 wrapper；它不增加 sshd、gateway、PVE 访问路径或 Agent VM guest 权限。

远端控制脚本或 unit 更新必须在安装前进入 admission drain。
Drain 会阻止新的 acquire 和 restart 准入，但不会终止在途请求，也不会关闭 expiry、reconciliation 或 cache-cleanup timer；发布会等待 pending operation 和 task transition 收敛，原子替换产物，验证健康状态和 timer state，并且只有检查成功后才能退出 drain。
仅更新本机客户端不需要 drain。

兼容性按 operation 评估，而不是在不同 release 之间做全局承诺。
Release manifest 为每个 operation 声明 protocol、request/response schema、语义兼容性和支持方向。
兼容 operation 可以跨不同 release revision 继续使用，并返回结构化兼容警告；不支持的 action、不兼容的 schema 或语义变化、无效 checksum 或不兼容 protocol 必须在产生任何 state 或 native effect 前拒绝。
兼容演进必须保持既有 protocol 合同（当前为 protocol 4）；破坏性演进可以要求新 protocol，也可以合理地要求更新。

发布事务在 drain 前暂存并验证选定的 release，在不强制终止的情况下等待在途和 pending operation 收敛，然后原子激活产物并验证健康状态、reconciliation 和 timer state。
激活前失败保持当前 release 不变。
激活后健康检查失败时允许向上一个已验证 release 执行一次有界 rollback；rollback 失败时保持 admission drain，绝不改写 state、generation 或不确定 operation。
至少保留当前和上一个已验证 release 及其 manifest 和 checksum。

本边界接受 SSH 和控制环境可用性这一依赖，以换取单一事实来源、有界故障处理和最小权限面。
它同时确保私有部署路径、凭据、清单和状态留在公共仓库之外。
