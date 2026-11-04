Warning: truncated output (original token count: 27831)
Total output lines: 864

# Agent VM 原生 Incus 主题规范

## 上下文与范围

本主题定义在已配置 Incus 主机上管理 Linux 容器的一次性客户端和控制进程。公共仓库记录生命周期合同并提供实现；部署通过显式配置提供自己的 project、容器镜像、网络、地址池、存储路径、状态数据库和控制地址。

本设计排除公共 broker、任意 HTTP 网关、客户端轮询、隐式 keepalive、共享主机工作区、导出归档，以及租约到期后仍然有效的保证。目标控制环境的每个蓝绿槽位包含一个受保护的常驻 Go 服务；其内部 UDS API 和 scheduler 是实现边界，不是客户端面。内置管理台和直接 Agent API 通过项目 Caddy 入口作为该活动服务的目标面，两者都不得暴露任意内部控制路由。当前检出仍提供既有 protocol-4/UDS 和 SSH 兼容路径；目标 OIDC、Caddy、浏览器 API、直接 Agent API 和 rqlite 拓扑不代表当前已有实现或已部署 listener。

部署拓扑分层：本地 Codex 客户端访问已配置的 Agent VM 控制 endpoint；控制环境运行在已观测的 CT109 LXC 中；控制环境拥有按 session 管理的 Incus system container；CT109 以下的物理或虚拟来源不属于已验证的公共边界。控制 endpoint 是入口路径，不是底层主机或其重建权威的证据。具体地址和实时 inventory 保留在受保护的部署记录中。

## 会话与传输

带 session 的请求携带 `session_id`；无 session 的只读 `admin-metrics` 请求省略它，并拒绝同时出现的 `session_id` 和 `operation_id`。对于带 session 的 action，CLI 优先从显式 `--session-id`，其次从 `AGENT_VM_SESSION_ID` 选择 session。缺少该值时，在任何远程写入前返回 `missing_session`。Session identity 用于选择受管资源，不是认证，也不是用户与资源的关系。客户端通过配置的控制路径发送一个有版本的 JSON 请求，并输出一个 JSON 响应；诊断信息写入 stderr。Format-1 客户端视图使用旧 Bun control entry。Format-2 客户端视图选择固定的 `legacy-bun` 或 `go-request` 传输；`go-request` 保留 SSH 兼容适配器，同时由远程 Go request process 通过受保护的 control UDS 发送相同请求体。

客户端和控制端都必须使用 protocol 4。Mutation 包含 operation ID 和 lease generation。控制端持久化请求与结果，拒绝不兼容或冲突的重试，绝不自动重放不确定的 native mutation。目标直接 HTTPS 传输是 `POST /agent/v1/control`，使用不透明 Bearer Token 和相同的 protocol-4 body；目标统一服务还提供独立版本化的浏览器合同，见[管理 API](contracts/management-api.yaml)和 [Token API](contracts/token-api.md)。增加这些目标 API 不改变 CLI protocol 4；本规范出现这些内容，也不代表当前检出已经提供对应 listener。

## 生命周期

每个 session 最多拥有一个受管 container slot，包括 preparing、stopping、deleting 和 cached 状态。每个 task 记录创建 guest 时固定的 container-image fingerprint。只有 fingerprint 仍与当前固定镜像匹配时，Acquire 才能创建或复用停止 cache；过期 cache 在产生 native effect 前返回 `cache_stale`，操作员必须先显式销毁它，新 Acquire 才能从当前镜像创建 guest。Acquire 记录新 generation，并在一个有界的单调时钟预算内等待容器运行且 IPv4 就绪。如果无关的 lease-expiry cleanup 短暂暴露 transitional native instance，Acquire 在独立的 30 秒 admission-convergence 预算内等待，轮询间隔不得短于 250 毫秒，并保持同一 operation pending；只有该准确的 transitional 条件允许重试，其他所有未经验证的状态都必须失败关闭。Lease 到期和 release 撤销访问，移除仅属于该 task/session 的映射，并在 slot 可复用前停止 guest。停止容器的 root filesystem 可以作为一次性 cache 保留，但不提供恢复保证。

新建 guest 时，native initialization request 必须写入 managed、task、session 和 generation identity metadata。初始化失败或结果不确定，不授权认领已有的未标记 instance。只有确认 instance 缺失且同 session 的 native worker 已验证清空后，恢复流程才可以回收失败初始化；上下文相关的 native not-found result 必须标识请求的 instance 和 project，不能把不确定或无关的查询失败当作不存在的证据；identity、generation 或 native query 不确定时，task 和 reservation 必须保留。清理已存在的 instance 也必须在 native mutation 前等待同 session 未解决的 worker。缓存 guest 必须携带当前 generation；已验证的上一 generation guest 在 generation 差异安全解决前仍保留 reservation。每次 Acquire 都必须在 native work 前记录持久 task identity 和 generation，使恢复流程能够区分重建的 session 与较早 operation。已验证的独立 Acquire record 可以在其他 task 仍阻塞时完成 reconciliation，但不得重新开放 admission。活动 Acquire 不能由 orphan recovery 完成；绑定的 pending Acquire 必须匹配该 identity 和 generation。Task 正在关闭时，恢复流程保留已验证的 pending destroy intent，并遵守相同的 worker 和 identity 校验。没有绑定的旧请求必须匹配当前或紧邻的上一 generation。只有在同 session 的所有 pending operation 都同样处于 transition 前状态时，旧的 starting Acquire 才能在 transition 前完成；不过，已完成的旧 generation Acquire 仍独立保持 pending，并且其缓存 task 必须保留已验证的 request generation、identity 和 state，或者不存在 task/generation tombstone 且已确认 native instance 缺失。之前被阻塞的 maintenance reconciliation 可以在 transitional task 清除且其 worker 已验证不存在后重试一次。被 supersede 的 Acquire、resize 或 restart 只有在以下条件全部满足后才可停止阻塞 admission：较晚的同 session operation 已通过 journal 顺序和时间戳确认发生在保存的 worker 完成之后；当前 cached task 已验证为 stopped 且 identity 明确；所有保存的 worker identity 已确认不存在；control service cgroup 和 Incus 后台 operation 已静止。原请求和 worker evidence 仍须保留，重试返回明确的历史未知结果，不得重放原 native operation。标记为 error 完成的旧 worker record，只有在 identity 和 process group 都验证后，恢复流程才能将其视为 exited。

Renewal 必须显式且有界。Acquire、status、SSH、执行和传输都不续租。Generation 变化或 lease 到期会拒绝迟到的 readiness 和 native result。持久状态、锁和 timer 使重复请求幂等，同时不隐藏部分清理。

Native process observation 必须容忍竞态：如果被跟踪进程在 `/proc` 枚举和 identity 读取之间退出，控制器将其视为已消失并继续正常的有界清理路径。没有 process-group ID 的 Linux worker identity 保持未验证，并继续阻塞 reconciliation。Process-group discovery 必须受 worker PID start time 保护；root 消失后，只能保留在该 root identity 存在时观测到的成员，或已观测成员的后代；意外的同 group 成员会使状态保持未验证。已正常 reap 且确认 group 为空的 root 可以接受；仍然存在或无法验证的已观测 group 必须继续保持未验证。有界清理在一次空扫描后执行最终观测，并在成功前确认 kernel process group 已不存在。Root 消失时，清理在声明 worker 消失前仍必须检查其 process group，包括在 procfs snapshot 后执行 kernel group-presence probe。Group 清理以 root pidfd 作为 identity anchor；每次非原子 process-table scan 后重新检查 root identity；先向已验证成员发送 signal，再处理 root；新观测到的无 root 后代只能通过已有的已验证成员纳入。第一次无 root 扫描绝不认领没有 anchor 的数字 process group；复用的数字 PID 或 process group 绝不能成为 ownership claim。只有 zombie 的 snapshot 也必须刷新并经过 kernel group-presence probe；只有 identity 已验证的 root 或成员可以被 reap，因此必须同时得到 procfs 和 kernel evidence 才能判定 group 不再存活。Native worker start 在 admission 前以带有明确 unattempted marker 的 `starting` 状态写入 journal，并为每次 admission 记录一次 start attempt；带有显式 unattempted marker 且没有 identity 的 inactive worker 可以恢复为 exited，而没有该 marker 的旧 record 仍保持未验证；同一 operation 的每次后续 start 都必须重新激活这个持久屏障，确认 spawn 前失败时记录 `exited`，之后才允许重试。嵌套 maintenance recovery 只能绕过活动 coordinator 的 unattempted preparation marker；已经尝试或带有 identity 的 worker 仍然阻塞。已尝试但未写入 journal 的 start 仍保持阻塞。Native worker 完成并带 error 时，必须继续以 running 状态记录在 journal 中，使 startup reconciliation 不能把 ownership 不确定当作完成；在 identity reconciliation 完成前，禁止该 operation 或 session 下的后续 native worker start。只有在同 session 的全部 worker journal entry 都验证清除后，缺失的 native instance 才能最终完成 destroy，其中包括 destroy worker 自身以及已经记录 attempted worker 的 pending Acquire journal。不存在 task 的 pending destroy 必须同时具备已确认的 native absence、终态 worker record，以及 task 或保留 tombstone 中相同的 generation；缺少 native 访问、存在活动请求、worker 未验证或 generation 不确定时，仍保持 pending。Reconciliation 验证 identity 已退出后，可恢复的 maintenance operation 才能重试。没有 delegated cgroup tracking 时，隔离的 process group 也是 reparented descendant 的 ownership 边界。其他 process-table read error 都必须作为失败处理。

## 访问与隔离

部署配置提供 control address 和主机 port range。Proxy NAT 使用已配置的主机地址、租用的 port 和固定 guest address。SSH 必须使用显式 identity 和 known-host 文件，并保持 host-key verification。受管 Incus project 必须允许所需 proxy device，配置的 ACL 必须在 admission 前证明 peer 与 host 隔离。Mapping cleanup 时必须删除准确的 conntrack entry。

## 容量与扩缩容

容量观测仅限于已配置的 Incus pool filesystem，以及控制环境可见的 CPU 和内存边界。不支持外部存储探针和基础设施管理器检查。CPU 和 memory allocation ceiling 使用分别配置的 overcommit ratio；停止 cache 只消耗磁盘。出现容量观测缺失或无效、超过 logical reservation ceiling、磁盘 admission 失败、处于 drain，或新的 native boot 将在已验证的严重 CT-local CPU scheduler pressure 下运行时，必须拒绝新需求。批准的 overcommit deployment profile 将 `cpu_startup_pressure_full_avg10_percent` 设置为 `50`；CPU startup guard 只在 projected CPU allocation 超过 CT-visible CPU capacity 且已验证的 `full.avg10` 达到阈值时生效。它不改变 logical CPU ceiling，也不停止有效 lease。CT memory headroom 偏低在 reservation-only memory admission 下属于观测和告警；它不减少 logical remaining memory，也不拒绝配置 ceiling 内的请求。Lease 查询、retrieval、renewal 和 release 在 pressure 期间仍可用。

Rootfs allocation change 必须显式允许 restart，且只能增长。Container rootfs growth 是独立的 post-check：它校验 Incus root device 和容器可见 rootfs capacity 是否达到请求值；如果 rootfs 已经达到请求容量的至少 95%，视为幂等成功；如果选定的 storage/profile 无法提供这些证据，必须失败关闭。只有 Incus root device 和容器 rootfs capacity 都验证后，旧 reservation 才能释放。对已记录的 native balloon timeout 只允许一次有界 convergence retry；不得使用轮询循环，也不得第三次尝试。部分 native 或 rootfs growth change 必须保守保留 reservation，并返回结构化 recovery state。

## 回收与操作

手动 garbage collection 默认是 dry-run，只选择已停止、没有有效 lease 且确认完成访问清理的受管 cache。Age timer 和 capacity admission 可以使用不同的有界选择策略，但任何一个都不得删除有效 lease、transition、resize target 或未知资源。基础设施操作需要单独的明确授权，并使用同样的配置路径和 identity 检查。

## 控制面发布与兼容性

Go control service 仍是 Incus operation、持久生命周期状态、lease generation、capacity admission、native mapping、观测 snapshot 和 maintenance 的唯一权威。其进程内 scheduler 负责 lease expiry、startup compensation、有界 retry 和 cache age-GC；systemd 只启动和重启服务，不是持久 scheduler。本地 `agent-vm` CLI 仍是有版本的 request client；publication、update 和 rollback 是操作员拥有的部署动作，不是公共 CLI 生命周期命令。默认关闭无人值守的 control-plane update。

Control-plane release 是一个逻辑兼容单元，包含本地 client、兼容 bridge 或 Go control binary、systemd unit asset、固定的 Caddy-facing service asset 和 release manifest。Production format 3 记录一个产品 SemVer、准确的 `source_revision`、target/toolchain、artifact checksum 和 mode、protocol 4，以及每个 supported operation 在各方向上的 request/response/semantic compatibility。启用管理能力的包使用 format 4，增加声明式持久状态 read/write capability、OIDC/Token/API compatibility 和内嵌管理 asset。Format 1 到 3 仍是只读迁移和必要的回滚输入，不是新的管理能力 production output。部署配置、凭据、连接材料、状态数据库和 WAL/journal、锁、Incus container data 以及 guest image 不属于 release。

兼容 operation 可以在不同 release revision 之间继续运行，并返回结构化 warning。兼容性不能从能否解析推断：unsupported action、不兼容 schema 或语义变化、无效 checksum 或不兼容 protocol，都必须在任何持久化或 native effect 前失败。兼容演进必须保留既有 protocol contract；破坏性演进可以要求新 protocol 和显式 client update。

管理状态使用版本化 capability marker。只有当 release 声明支持读写数据库中存在的每个 marker 时，才可以针对该数据库激活。Format 1 到 3 表示只支持管理前状态。不兼容的 package rollback 必须在 pointer activation 或 service start 前拒绝；绝不能降级数据库来制造旧包兼容的假象。

## 管理台与 API

管理台管理已有的 Agent session VM、受管镜像、容量、operation 历史、审计和固定系统维护 action。它不提供手动创建 VM、通用 Incus、主机/PVE、shell 或 Web terminal 面。它包含五个页面：Overview、VMs、Images、Operations & Audit 和 System；首个认证视图是 Overview。

目标浏览器 API 是同源 `/api/v1`，独立于 protocol 4。目标必须使用只接受 `code_challenge_method=S256` 的 OIDC Authorization Code + PKCE、准确的 issuer/subject identity、安全浏览器 session、准确 Origin 校验和 session-bound CSRF protection。目标直接 Agent API 是 `/agent/v1/control`，要求不透明 Bearer Token，绝不接受浏览器 Cookie。任何必要的 OIDC、Token、origin 或 resource-limit 配置不完整时，目标 listener 保持关闭。[管理合同](contracts/management-api.md)、[Token API 合同](contracts/token-api.md)、[Agent API 合同](contracts/agent-api.md)和 [OpenAPI 文档](contracts/management-api.yaml)定义目标 wire shape 和部署输入；当前实现证据仍仅限于已有兼容面。

UI 支持 `zh-CN` 和 `en`。已保存的偏好优先于浏览器语言，英文是回退。所有界面和可访问文案都必须本地化。标识符、fingerprint、稳定错误码和用户填写的 reason 保持原值。API 返回与 locale 无关的值，保留观测新鲜度，并将未知测量表达为 unknown 而不是零。

唯一管理员使用浏览器 session，机器 Token 是独立的 Bearer credential。每个 Agent request 都重新校验 Token expiry/revocation、scope、目标 session、generation 和 native state；机器 Token 不需要预绑定 session，可以获取任意允许的目标。管理员 mutation 使用 UUID operation identity；已有资源的 mutation 使用 resource version，VM action 还携带准确 session generation。可选 operator reason 有界，提供时写入审计，但任何 operation 都不要求 reason。敏感变更使用绑定预览的 confirmation token，统一服务在产生 effect 前重新校验全部前提。Operation 和 audit API 保留当前管理员 actor、无管理员权限的 typed rejected-request actor，或显式未知历史 actor，以及 target、可选 reason、phase 和 outcome。不确定结果仍可查询，绝不使用新的 operation identity 自动重放。

镜像导入仅限配置的可信来源和大小有界的本地 Incus container package。完整性和 Agent VM 适用性是分开的校验。除非管理员选择了已验证替代品，否则选中的 default 必须通过验证；已有配置的 default 作为可见的 legacy-compatibility default 保留。已有 task 和 cache 继续保留其固定的 image fingerprint。权威组件串行化 default selection 和引用变更；default 只影响新 session，default、active、cached、releasing、pending-creation 或 verification 任一引用都会阻止删除。

首个管理状态迁移采用增量和事务方式。它保留 task、generation、lease、port、reservation 和 protocol-4 journal。SQLite 在一次 drain 且验证通过的一次性导入让 rqlite 成为共享权威之前仍是唯一 writer；禁止双写、共享 SQLite 文件访问和数据库降级。关键读取使用 linearizable consistency，跨行变更使用一个 rqlite transaction。State-capability marker、legacy-default seeding、installer preflight 和整包 rollback 规则见[管理合同](contracts/management-api.md)、[状态迁移 ADR](../../adr/0044-agent-vm-state-migration-to-rqlite.md)和[发布合同](contracts/release.md)。这是目标迁移合同，不代表当前检出已经执行该迁移。

Remote control-script 和 unit publication 必须使用配置的 control SSH entry、操作员专用 identity 或范围严格限定的 root-owned deployment wrapper、私有 staging 区和原子激活。不得增加 sshd、HTTP gateway、PVE path 或 Agent VM guest permission。远程 publication 必须在更新本地 client 前完成激活和验证；只更新本地 client 不需要 drain。

Remote publication 必须在激活前进入 admission drain，阻止新的 acquisition 和 restart admission，保留 expiry/reconciliation/cache-cleanup timer 可用，并允许在途请求完成，不得强制 kill。它必须等待 pending operation 和 task transition 收敛，验证 health、reconciliation 和 timer 状态，并且只有在通过当前 release transaction 的 release-scoped session 成功验证后才退出 drain；浏览器 maintenance request 不得重新开放 admission。首个具备管理能力的 release 必须能够接管前一个 protocol-4 controller 留下的 ownerless drain，但不同 release 或浏览器请求不得替换活动的 release owner。尚未越过 native-effect barrier 的 queued management work 仍可 retry，但在 drain 期间必须被持久 fence，禁止启动。激活前失败时保持当前 release 不变。激活后 health failure 允许对上一个已验证 release 执行一次有界 rollback；rollback 失败时继续保持 admission drain，不得改写 state、generation 或不确定 operation。至少保留当前和上一个已验证 release 及其 manifest 和 checksum。
# 原生 Incus Agent VM

本文是 Agent VM 主题的规范设计文档和长期需求合同。
状态后端迁移、恢复材料保留和蓝绿边界以 `REQ-AGENT-VM-028` 及其关联合同为准，部署控制器边界以 `REQ-AGENT-VM-031` 及其关联合同为准。
相关 ADR 只记录决策背景、技术取舍和被否决方案，不替代本 SPEC，也不是本主题的设计规范来源。

## Context and Scope

本文定义在已配置 Incus 宿主上管理 Linux 容器所需的客户端和控制进程。
公共仓库记录生命周期合同并提供实现；
部署通过显式配置提供自己的 Incus project、容器镜像、网络、地址池、存储路径、状态数据库和控制地址。

设计排除公共 broker、任意 HTTP 网关、客户端轮询、隐式 keepalive、共享宿主工作区、导出归档，以及跨 lease expiry 仍然有效的保证。
目标控制环境在每个蓝绿槽位运行一个受保护的 Go 常驻服务，并在槽位外运行一个 Go Deployd（`agent-vm-deployd`）；
它们的内部 UDS API 都是实现边界，不是公共客户端入口。
槽位服务拥有业务生命周期状态和 native operation；
Deployd 只拥有部署事务、槽位观测以及控制器/流量协调。
普通蓝绿事务永远不写业务状态或 rqlite，只有首次部署的固定 `migrate` 事务可以一次性完成 legacy SQLite 到 rqlite 的导入。
内置管理控制台和 direct Agent API 是 active service 通过项目 Caddy 入口提供的目标入口，二者都不得暴露任意内部控制路由。
当前 checkout 已实现 protocol-4/UDS、双槽位 Go 服务、槽位外 deployd 和 SSH 兼容路径；
具体 OIDC、Caddy、浏览器 API、direct Agent API、地址、证书和 rqlite 运行拓扑仍由部署配置提供，不能从仓库实现推断为已部署。

部署拓扑是分层的：本机 Codex client 访问已配置的 Agent VM control endpoint；
control environment 运行在已观测的 CT109 LXC 内；
control environment 拥有按 session 管理的 Incus system containers；
CT109 下方的物理或虚拟来源不属于已验证的公共边界。
control endpoint 只是进入路径，不是底层宿主机或其重建权限的证据。
具体地址和 live inventory 保存在受保护的部署记录中。

## Session 与 transport

按 session 作用域的 request 携带 `session_id`；
无 session 的只读 `admin-metrics` request 不携带它，并拒绝 `session_id` 和 `operation_id`。
对于按 session 作用域的 action，CLI 优先从显式 `--session-id`，其次从 `AGENT_VM_SESSION_ID` 选择 session。
缺少该值时，任何 remote write 发生前都返回 `missing_session`。
Session identity 用于选择 managed resource，不是 authentication，也不是 user-resource relation。
Client 通过 configured control path 发送一个带版本的 JSON request，并输出一个 JSON response；
diagnostics 写入 stderr。
Format-1 client view 使用 legacy Bun control entry。
Format-2 client view 选择固定的 `legacy-bun` 或 `go-request` transport；
`go-request` 保留 SSH 作为 compatibility adapter，同时远端 Go request process 通过受保护的 control UDS 发送相同 body。

Client 和 control 都必须使用 Protocol 4。
Mutation 包含 operation ID 和 lease generation。
Control 持久化 request 与 outcome，拒绝不兼容或冲突的 retry，并且绝不自动 replay uncertain native mutation。
目标 direct HTTPS transport 是 `POST /agent/v1/control`，使用 opaque Bearer Token 和相同的 protocol-4 body；
目标 unified service 还通过[管理 API](contracts/management-api.yaml)和[Token API](contracts/token-api.md)提供独立版本的 browser contract。
加入这些目标 API 不会改变 CLI protocol 4；
本规范出现这些内容也不表示当前 checkout 已提供这些服务。

## 生命周期

每个 session 至多拥有一个 managed-container slot，包括 preparing、stopping、deleting 和 cached 状态。
每个 task 记录用于创建 guest 的 pinned container-image fingerprint。
只有 fingerprint 仍匹配当前 pinned image 时，Acquire 才会创建或复用 stopped cache；
stale cache 会在 native effect 前返回 `cache_stale`，运维者必须显式 destroy 它，之后新的 acquire 才能使用当前 image 创建 guest。
Acquire 记录新的 generation，并在一个有界的 monotonic budget 内等待 container 进入 running 且 IPv4-ready。
如果无关的 lease-expiry cleanup 短暂暴露 transitional native instance，acquire 会在单独的、有界 30 秒 admission-convergence budget 内等待，每次 polling 间隔不少于 250 毫秒，并保持同一个 operation pending；
只有这个精确的 transitional condition 可 retry，其他所有未验证状态都必须 fail closed。
Lease expiry 和 release 会撤销 access，只移除属于该 task/session 的 mappings，并在 slot 可复用前停止 guest。
Stopped container root filesystem 可以作为 disposable cache 保留，但不提供 recovery guarantee。

新创建的 guest 会在 native initialization request 中接收 managed、task、session 和 generation identity metadata。
Initialization 失败或结果 uncertain 时，不得据此认领已有的未标记 instance。
只有在确认 instance 缺失且验证同 session 的 native workers 已清空后，recovery 才能回收失败的 initialization；
contextual native not-found result 必须标识 requested instance 和 project，uncertain 或无关的 query failure 不能证明资源不存在；
identity、generation 或 native query 不确定时，必须保留 task 和 reservation。
清理仍存在的 instance 之前，也必须等待未解决的同 session workers。
Cached guest 必须携带当前 generation；
已验证的 previous-generation guest 要保留 reservation，直到该 generation discrepancy 可以安全 reconcile。
每次 acquire 都要在 native work 前记录 durable task identity 和 generation，使 recovery 能区分 recreated session 与 older operation。
已验证的独立 acquire records 可以在其他 task 仍 blocked 时 reconcile，但不得重新开放 admission。
Active acquire 不能由 orphan recovery finalize；
bound pending acquire 必须匹配该 identity 和 generation。
当 task 正在关闭时，recovery 在通过相同 worker 和 identity checks 后保留 verified pending destroy intent。
Legacy unbound requests 要求匹配当前或紧接之前的 generation。
Legacy starting acquire 只有在同 session 的所有 pending operations 同样处于 pre-transition 状态时才可在 transition 前 finalize；
已完成的 older-generation acquire 仍独立保持 pending，并且必须满足以下条件之一：其 cached task 保留 request generation 且 identity/state 已验证，或者不存在 task/generation tombstone 且已确认 native instance 缺失。
先前被阻塞的 maintenance reconciliation 只有在 transitional tasks 清空且其 worker 已验证不存在后才可 retry 一次。
Superseded acquire、resize 或 restart 只有在后续成功的同 session operation 满足以下条件后才能停止阻塞 admission：journal order 和 timestamps 证明该 operation 发生在保存的 worker 完成之后；
当前 cached task 已验证为 stopped 且 identity 正确；
所有保存的 worker identities 均已确认不存在；
control service cgroup 和 Incus background operations 均已 quiescent。
Original request 和 worker evidence 必须继续记录，retry 返回明确的 unknown historical outcome，不得 replay original native operation。
标记为 finished with an error 的 legacy worker records，只有在 identity 和 process-group 验证通过后，recovery 才能将其视为 exited。

Renewal 必须显式且有界。
Acquire、status、SSH、execution 和 transfer 都不会 renew lease。
Generation 变化或 lease 过期会拒绝迟到的 readiness 和 native results。
Durable state、locks 和 timers 让重复 request 保持幂等，同时不会隐藏 partial cleanup。

Native process observation 必须容忍 race：如果 tracked process 在 `/proc` enumeration 与 identity reads 之间退出，controller 将其视为 gone，并继续正常的有界 cleanup path。
没有 process-group ID 的 Linux worker identities 保持 unverified，并继续阻塞 reconciliation。
Process-group discovery 仍由 worker PID start time fence 保护；
root 消失后，只能保留 root identity 存在时观测到的 members 或已观测 member 的 descendants，unexpected same-group member 会让 state 保持 unverified。
正常 reaped 且确认 group 为空的 root 可以接受；
仍存在或无法验证的 observed group 会保持 state unverified。
有界 cleanup 在 empty scan 后执行 final observation，并在成功前确认 kernel process-group absence。
Worker root 消失时，cleanup 仍必须在声明 worker gone 前检查其 process group，包括在 procfs snapshot 后执行 kernel group-presence check。
Group cleanup 以 root pidfd 作为 identity anchor；
每次 non-atomic process-table scan 后重新检查 root identity，在 root 前 signal 已验证的 members，并且只有通过已验证 member 才能接纳新观测到的 root-less descendants。
第一次 root-less scan 绝不认领没有 anchor 的 numeric process group；
复用的 numeric PID 或 process group 绝不构成 ownership claim。
Zombie-only snapshots 仍要刷新并经过 kernel group-presence probe；
只有 identity-verified root 或 members 才能 reap，因此只有 procfs 和 kernel evidence 一致表明 group 不再 live 时，才能认定其 non-live。
Native worker start 在 admission 前以 `starting` journal，并记录显式的 unattempted marker；
每次 admission 只记录一次 start attempt。
带显式 unattempted marker 且没有 identity 的 inactive workers 可以恢复为 exited；
没有该 marker 的 legacy records 保持 unverified。
后续同 operation 的每次 start 都重新 armed 该 durable barrier；
已确认的 pre-spawn failure 记录为 `exited`，使 operation 可以 retry。
Nested maintenance recovery 只能绕过 active coordinator 的 unattempted preparation marker；
已经 attempted 或带 identity 的 worker 仍然阻塞。
Attempted 但未 journal 的 start 仍保持 blocked。
带 error 的 native worker completion 仍以 running 状态 journal，使 startup reconciliation 不能把 uncertain ownership 当作 finished；
在 identity reconcile 前，禁止该 operation 或其 session 下的后续 native worker starts。
只有在所有同 session worker journal entries（包括 destroy worker 本身，以及已经记录 attempted worker 的 pending acquire journals）都验证清空后，missing native instances 才能 finalize destroy。
Absent task 的 pending destroy 要求确认 native absence、terminal worker record，以及 task 或 retained tombstone 中的相同 generation；
missing native access、active request、unverified worker 或 generation uncertainty 都会让它继续 pending。
Reconciliation 验证 identity 已 exited 后，可 retry recoverable maintenance operations。
没有 delegated cgroup tracking 时，isolated process group 也构成 reparented descendants 的 ownership boundary。
其他 process-table read errors 仍然是 failure。

## 访问与隔离

Deployment configuration 提供 control address 和 host port range。
Proxy NAT 使用 configured host address、leased port 和 fixed guest address。
SSH 必须使用 explicit identity 和 known-host files，并保持 host-key verification enabled。
Managed Incus project 必须允许所需的 proxy devices；
configured ACL 必须在 admission 前证明 peer isolation 和 host isolation。
Mapping cleanup 时移除精确的 conntrack entries。

## 容量与调整

Capacity observation 仅限于 configured Incus pool filesystem，以及 control environment 可见的 CPU 和 memory boundaries。
不支持 external storage probes 或 infrastructure-manager inspection。
CPU 和 memory allocation ceilings 使用分别配置的 overselling ratios；
stopped caches 只消耗 disk。
当 capacity measurements 缺失或无效、超过 logical reservation ceiling、disk admission 失败、处于 draining，或新的 native boot 会在已验证的严重 CT-local CPU scheduler pressure 下运行时，拒绝新的 demand。
批准的 overcommit deployment profile 将 `cpu_startup_pressure_full_avg10_percent` 设为 `50`；
CPU startup guard 仅在 projected CPU allocation 超过 CT-visible CPU capacity 且已验证的 `full.avg10` 达到该 threshold 时生效。
它不会改变 logical CPU ceiling，也不会停止 effective lease。
Reservation-only memory admission 下的 low CT memory headroom 只是 observation 和 warning；
它不会减少 logical remaining memory，也不会拒绝 logical ceiling 内的 request。
Pressure 期间仍可执行 existing lease queries、retrieval、renewal 和 release。

Rootfs allocation change 要求显式 restart allowance，并且只能 grow。
Container rootfs growth 是独立的 post-check：它根据 requested allocation 验证 Incus root device 和 container-visible rootfs capacity；
如果 rootfs 已达到 requested capacity 的至少 95%，则视为幂等成功；
如果 selected storage/profile 无法提供这些 evidence，则 fail closed。
只有在 Incus root device 和 container rootfs capacity 都验证通过后，才释放 old reservation。
对于文档化的 native balloon timeout，允许一次有界 convergence retry；
不存在 polling loop 或第三次尝试。
部分 native 或 rootfs growth change 必须保留 conservative reservations 和 structured recovery state。

## 回收与运维操作

Manual garbage collection 默认是 dry-run，并且只选择没有 effective lease 且 access cleanup 已确认的 stopped managed caches。
Age timer 和 capacity admission 可以使用不同的有界 selection policies，但都不能删除 valid lease、transition、resize target 或 unknown resource。
Infrastructure operations 要求单独的显式授权，并使用相同的 configured paths 和 identity checks。

## 控制面发布与兼容性

Go control service 继续是 Incus operations、durable lifecycle state、lease generations、capacity admission、native mappings、observation snapshots 和 maintenance 的唯一 authority。
其 in-process scheduler 负责 lease expiry、startup compensation、bounded retries 和 cache age-GC；
systemd 只负责启动和重启 service，不是 durable scheduler。
Scheduled reconcile 和 age-GC 使用独立的 single-flight lane、additive `maintenance_cursor` meta key、每轮最多 16 个 task 或 2 秒，以及 2/4/8/16/32/60 秒的 bounded retry backoff。
Cursor 只有在 task/native journal commit 已 durable 后才前移；
失败 round 记录 phase 和 error，但不前移失败 task。
历史 `timer-reconcile` 和 `timer-age-gc` journal rows 仍可读取和恢复，但 scheduler tick 不创建新的 timer operation。
本机 `agent-vm` CLI 仍是带版本的 request client；
publication、update 和 rollback 属于 operator-owned deployment actions，不是 public CLI lifecycle commands。
Unattended control-plane updates 默认关闭。

Control-plane release 是一个 logical compatibility unit，包含 local client、compatibility bridge 或 Go control binary、systemd unit assets、固定的 Caddy-facing service assets 和 release manifest。
Production format 3 记录一个 product SemVer、精确的 `source_revision`、target/toolchain、artifact checksums 和 modes、protocol 4，以及每个支持方向上的 per-operation request/response/semantic compatibility。
启用管理功能的 package 使用 format 4，新增声明的 durable state read/write capabilities、OIDC/Token/API compatibility 和内置 management assets。
Formats 1 到 3 仍作为只读迁移输入和必要的 rollback inputs；
它们不是新的 management-enabled production output。
Deployment configuration、credentials、connection material、state database 及 WAL/journals、locks、Incus container data 和 guest images 不属于 release。

Compatible operations 可以跨不同 release revisions 继续使用，并返回 structured warning。
不能根据 parseability 推断 compatibility：unsupported action、incompatible schema 或 semantic change、invalid checksum、或 incompatible protocol MUST 在任何 durable 或 native effect 之前失败。
兼容演进必须保留已建立的 protocol contract；
breaking evolution 可能需要新 protocol 和显式 client update。

Management state 使用带版本的 capability markers。
Release 只有声明支持 read/write 当前存在的每一个 marker 时，才能对 database activation。
Formats 1 到 3 仅表示支持 pre-management state。
Incompatible package rollback 必须在 pointer activation 或 service start 前拒绝；
不得降级 database 来制造旧 package 兼容的假象。

## 管理控制台与 API

Console 管理 existing Agent session VMs、managed images、capacity、operation history、audit 和固定的 system-maintenance actions。
它不提供 manual VM creation、generic Incus、host/PVE、shell 或 Web terminal surface。
它包含五个页面：Overview、VMs、Images、Operations & Audit 和 System。
首次认证后的 view 是 Overview。

目标 browser API 是 same-origin `/api/v1`，独立于 protocol 4。
该目标必须只使用 OIDC Authorization Code + PKCE 和 `code_challenge_method=S256`，并执行 exact issuer/subject identity、secure browser sessions、exact Origin validation 和 session-bound CSRF protection。
目标 direct Agent API 是 `/agent/v1/control`，要求 opaque Bearer Token，绝不接受 browser Cookie。
如果必需的 OIDC、Token、origin 或 resource-limit configuration 不完整，target listener 必须保持 disabled。
[管理合同](contracts/management-api.md)、[Token API 合同](contracts/token-api.md)、[Agent API 合同](contracts/agent-api.md)和[OpenAPI 文档](contracts/management-api.yaml)定义目标 wire shapes 与 deployment inputs；
当前实现证据仍限于已有 compatibility surfaces。

UI 支持 `zh-CN` 和 `en`。
Saved preference 优先于 browser language；
English 是 fallback。
所有 interface 和 accessible copy 都必须本地化。
Identifiers、fingerprints、stable error codes 和 user-authored reasons 保持不变。
API 返回 locale-neutral values，保留 observation freshness，并将 unknown measurements 表示为 unknown，而不是 zero。

唯一 administrator 使用 browser session，machine Tokens 则是独立的 Bearer credentials。
每个 Agent request 都重新检查 Token expiry/revocation、scope、target session、generation 和 native state；
machine Token 不要求 pre-bound session，并可以 acquire 任意允许的 target。
Administrator mutations 使用 UUID operation identity；
对 existing resources 的 mutation 使用 resource version，VM actions 还携带 exact session generation。
Operator reason 是可选的，提供时必须有界并记录到 audit，但任何 operation 都不要求它。
Sensitive changes 使用 preview-bound confirmation token，unified service 在产生 effect 前重新验证所有 preconditions。
Operation 和 audit APIs 保留 current administrator actor、没有 administrator authority 的 typed rejected-request actor，或明确的 unknown historical actor，以及 target、optional reason、phase 和 outcome。
Uncertain result 必须可 query，且不得使用新的 operation identity 自动 replay。

Image import 仅限于 configured trusted sources 和有大小上限的 local Incus container packages。
Integrity 与 Agent VM suitabil…7831 tokens truncated…slots 的显式 snapshot estimates。
Inventory view MUST 覆盖所有 managed task states，使用有界字段，删除 stale rows，并避免暴露 raw errors、credentials 和 deployment paths。
缺失或为 `-1` 的 native slot limit MUST 保持缺失；
derived slot estimate MUST NOT 标记为 authoritative slot limit。

`agent-vm-deployd` 的 `/metrics` endpoint MUST 将 `admin-metrics` 和 `admin-status` 调度为独立的 observation lanes。
Pending 或 failed inventory read MUST NOT 移除最新有效的 capacity metric families；
pending 或 failed capacity read MUST NOT 保留失败 inventory scrape 中的 task rows。
Retained source snapshot MUST 保留其原始 observation timestamp，并暴露 source-specific health 或 freshness failure；
deployd MUST NOT 使用伪造的 zeroes 替换 unavailable values。
Prometheus MUST 只抓取 deployd 的 `/metrics` endpoint，并使用 `job="agent-vm-deployd"`；
槽位级 metric series MUST 带 `slot` label，以支持切换期间两个槽位同时存活。
蓝色和绿色槽位 MUST 使用各自独立的 snapshot 目录；
共享 snapshot 目录 MUST NOT 用于蓝绿运行。
`admin-status` MUST use keyset pagination with a default page size of 25 and a maximum of 100, probe no more than 8 tasks concurrently, bound each task probe to 1 second and the page to 5 seconds, and expose `partial`、`unknown`、`observation_consistent`、`probed`、`verified` 和 `timed_out` without treating unknown native state as healthy。

这些规则的固定合同名称是 independent observation lanes、pending or failed inventory read、latest valid capacity metric families、retained source snapshot 和 original observation timestamp；
它们对应上面的中文语义，不是额外的英文操作说明。

### REQ-AGENT-VM-020 - 认证管理边界

每个蓝绿槽位 MUST 运行一个 Go/Gin Agent VM 服务，提供固定静态 Web 资源、OIDC 回调、同源管理 API 和 `/agent/v1/control`；
业务边界 MUST NOT 增加独立 Web 进程、Forward Auth 身份标头、公共管理 UDS、路由器、Web/BFF、公共 native 生命周期服务或公共 TCP 绕过路径。
独立的运维者专用 deployd UDS 位于业务管理 API 之外，MUST 对 Caddy 和浏览器不可访问。
Caddy MUST 只执行 TLS 和固定路径的 active-slot 路由。
统一服务 MUST 执行受限的方法/路径/ 输入、OIDC/session/Token 授权、Origin/CSRF、target-session、generation 和 journal 检查；
必需的 OIDC、origin、Token、upload 或 staging configuration 不完整时，MUST 保持 listener disabled。

### REQ-AGENT-VM-021 - 管理 API 与本地化界面

Management console MUST 只暴露 OpenAPI contract 定义的 same-origin `/api/v1` surface。
它 MUST 拒绝 unknown fields 和 unsupported actions，使用 bounded cursor pagination，保留 exact byte 和 generation values，返回 structured field-whitelisted errors，并暴露 source freshness 和 unknown values，不得把它们强制转换为 healthy zeroes。
UI MUST 支持 `zh-CN` 和 `en`，优先使用 saved choice，其次 browser language，最后 English fallback；
accessible names、confirmations 和 errors MUST 翻译，technical identifiers 和 user-entered reasons 保持不变。

### REQ-AGENT-VM-022 - 管理操作与审计恢复

Existing management resource 的每次 mutation MUST 携带 UUID operation ID 和 current target version；
每次 durable create 或 Token issuance MUST 携带 UUID operation ID 和 canonical retry identity；
VM actions 还 MUST 携带 expected generation。
Request MAY 包含有界的 non-empty reason，但缺少 reason MUST 被接受并表示为 absent。
Administrator-sensitive submission MUST 使用绑定 authenticated subject、exact target、action、parameters 和 target version 的 confirmation token；
authority MUST 在产生 effects 前重新检查 authorization 和 state。
Token creation 是 durable-create exception，不要求单独 preview。
Identical operation retries MUST 解析到同一个 durable operation；
conflicting reuse MUST failure。
Token create 在 durable operation 和 metadata layer 上必须幂等：第一笔 transaction 原子消费 one-time plaintext disclosure latch，只有该 response 可以携带 bearer，后续或并发 retry 返回相同的 `token_id`/metadata，不返回 plaintext。
Uncertain native outcomes MUST 保持可 query，且 MUST NOT 自动 replay。
Audit records MUST 保留 current administrator actor、没有 administrator authority 的 typed rejected-request actor，或带 original raw value 的明确 unknown historical actor，并保留 optional reason、target、phase 和 outcome；
不得暴露 raw command diagnostics、credentials 或 private paths。

### REQ-AGENT-VM-023 - Managed image 安全与 session pinning

Managed images MUST 使用 immutable fingerprints、allowlisted source identities、有界的 local package uploads、独立的 integrity 和 suitability results，以及 explicit reference counts。
Configured legacy default MUST 在迁移期间保留，并在替换前显示为 unverified；
只有 fully verified image 才能通过 management API 被选中。
Default selection MUST 只影响 new sessions。
Existing tasks 和 caches MUST 保留 exact fingerprint。
Default、active、cached、releasing、pending-creation 和 verification references MUST 阻止 deletion；
reference checks 必须与 image operations 串行化。

### REQ-AGENT-VM-024 - 管理状态与回滚兼容性

Management schema changes MUST 是 additive、transactional，并由 versioned state-capability markers 记录。
Migration MUST 保留 task identity、generations、leases、ports、reservations 和 existing protocol-4 journal state。
Package 只有声明支持 read/write state database 中每个 capability marker 时才能 activation。
没有 capability declaration 的 package MUST 仅视为支持 pre-management state。
Incompatible rollback MUST 在改变 active package pointers 或启动 target 前被拒绝；
MUST NOT 降级或重建 databases 来强行兼容。
Protocol 4 和 existing CLI request/response behavior MUST 保持不变。

### REQ-AGENT-VM-025 - OIDC principal 与 browser session identity

Browser login MUST 只使用 OIDC Authorization Code + PKCE 和 `code_challenge_method=S256`，并使用 one-time `state`、`nonce`、protected code verifier、exact redirect URI、短时 browser-bound login cookie 和 bounded expiry。
Service MUST 在创建 session 前验证 configured discovery issuer、exact ID Token `iss`、`aud`/`azp`、signature/JWKS、`exp`、`iat` policy、authorization-code exchange 和 nonce。
Service 与 proxy access logs MUST 删除 callback query strings 和 `Referer`；
application logs MUST NOT 保留 OIDC code、state、nonce 或 verifier values。
Durable principal MUST 是精确且区分大小写的 `(issuer, subject)` pair；
只有 configured pair 是 administrator actor，email、display name、group claims 和 forwarded identity headers MUST NOT 建立 authorization。
Replayed、expired、cross-browser 或 unverified callback MUST 被拒绝；
non-allowlisted login MUST NOT 创建 session 或 administrator capability。

### REQ-AGENT-VM-026 - Machine Token 与 Agent API

Sole administrator MUST 是唯一允许 issue、list、read 和 revoke opaque machine Tokens 的 browser actor。
Token metadata MUST 包含 opaque `resource_version`、scopes、expiry 和 revocation state，但 MUST NOT 包含 user-owner field 或 pre-bound session set；
plaintext MUST 只返回一次，service 只存储 validation digest 和 one-time disclosure state。
Token creation MUST 要求 operation ID 和 canonical request identity；
requested `agent:operate` scope MUST 同时显式包含 `agent:read` scope，不允许 implicit scope inheritance。
成功的 `token_create` operation MUST 通过 safe metadata 和 durable plaintext disclosure state 包含 Token ID。
Retry 或 concurrent request MUST 返回 durable operation 的 `token_id`、metadata、`resource_version` 和 disclosure state，不得重新签发 plaintext；
响应丢失后，administrator MUST query original operation、确认 Token ID、显式 revoke 该 Token，并使用新的 operation ID 创建 replacement。
Token revocation MUST 要求 current preview `target_version` 和 single-use confirmation token，在 acknowledgment 前 commit，推进 `resource_version`；
Agent authorization MUST 通过 linearizable read 观察到 revocation。
`POST /agent/v1/control` MUST 只接受 valid Bearer Token 和 existing protocol-4 body，并且每个 request 都 MUST 重新检查 Token expiry/revocation、scope、target session、generation、operation ID 和 request digest。
带 allowed scope 的 valid Token MAY acquire 任意 target session；
MUST NOT 要求预先绑定该 session。
Sessionless administrative observation MUST 要求显式 `agent:observe_admin` scope。

### REQ-AGENT-VM-027 - Administrator 敏感操作与审计

Administrator capabilities MUST 只根据 configured exact `(issuer, subject)` pair 解析，MUST NOT 从 email 或 group claims 推断。
Sensitive actions MUST 要求 explicit capability、impact preview，以及绑定 actor、exact target、parameters、target version 和 generation 的 one-time confirmation token；
optional non-empty bounded reason 在提供时写入 audit，但不是必需项。
Unified service MUST 在 native effects 前重新检查 authorization 和 state。
Accepted、rejected、succeeded、failed 和 outcome-unknown attempts MUST 可审计，并保留 current issuer/subject actor、没有 administrator authority 的 typed `unauthenticated` 或 `unauthorized` rejected-request actor，或明确的 unknown historical actor，以及 optional reason、operation ID、target、phase、result 和 safe error code；
不得包含 Token values、OIDC codes、credentials、raw command output 或 private paths。
Historical task 和 native records MUST 根据 durable task identity、generation、lease、journal 和 native identity reconcile；
不允许 user mapping 或 first-login claim。

### REQ-AGENT-VM-028 - 保守的 SQLite-to-rqlite 迁移与原子并发

首次部署 MUST 由发布器自动调用槽位外 Deployd 的 `POST /v1/migrate`，不得把旧 SQLite 的停止、snapshot、导入或验证留给人工前置步骤。
Deployd MUST 将这次迁移作为单一 durable transaction 持久化，并在结果不明确时返回 transaction ID、进入 `needs_recovery`、保持 legacy 服务停止或 fenced；
发布器只可对该 transaction 自动执行一次 `retry-migration`。

SQLite MUST 在 state-write fence 拒绝新的 browser/API/Agent durable mutations、暂停 scheduler/reconciliation/timer durable writes、排空 in-flight transactions 和 native workers，并要求每个 native worker 在其 original operation ID 下以 durable result 完成，同时验证 process-group、PID-identity 和 Incus-operation quiescence；
在此之前继续作为 sole writer。
Pending/unknown native result、active worker 或 unverified quiescence MUST 阻止 final export；
migration MUST NOT 只记录它然后继续。
Migration MUST 保留 protocol 4、schema、generation、`state_epoch`、operation journal、audit、session、Token 和 capability semantics；
MUST NOT dual-write、从两个 slots 打开 shared SQLite file、reverse-sync guessed state、downgrade schema 或为 rollback recreate state。
Cutover 后，rqlite MUST 是 sole shared authority；
critical administrator-authorization、lease、generation、journal、Token-revocation、capability 和 epoch reads MUST 使用 linearizable consistency，cross-row compare-and-act changes MUST 使用一个带 conditional updates 或 unique constraints 的 rqlite transaction。
如果需要 rqlite recovery，verified backup MUST 记录不早于 last trusted commit 的 coverage boundary，且该 boundary 之后的 backup tail MUST 与 trusted append-only journal 和 native-effect evidence reconcile；
仅 schema、digest、epoch 或 journal checks 不能作为 coverage proof。
如果无法证明 tail 或任意 post-cutover write，service MUST 保持 fenced，等待 administrator-authorized journal/native reconciliation。
只有证据证明 post-cutover 没有任何 writes 和 effects 时，pre-cutover SQLite snapshot 才可采用。
Native unknown outcomes MUST 保持 pending，MUST NOT 使用新的 operation ID replay。

状态恢复材料 MUST 使用有界保留：最多三个已验证恢复点、至少保留最后两个恢复点，超过三十天的恢复点进入清理候选，同一控制环境内全部恢复材料的总大小硬上限为 1 GiB。
1 GiB MUST 按实际文件大小计算，不得预分配，也不得设置恢复材料体积下限。
迁移源 SQLite snapshot 在迁移完成并通过新的 rqlite restore check 后 MUST 保留七天。
只有在没有 pending/unknown operation 或未解决的 `needs_recovery`、对应保留期到期，并且清理后仍保留两个恢复点时，材料才可以删除。
达到容量预算且没有可删除材料时，系统 MUST 返回 `backup_storage_exhausted`，拒绝新的备份或状态迁移，不得删除受保护恢复点。
状态数据库中的终态操作和审计历史 MUST 默认保留三十天后回收；
需要长期保留的审计 MUST 先归档到状态库之外，不能通过扩大恢复材料预算替代历史记录回收。

### REQ-AGENT-VM-029 - 蓝绿兼容窗口

Release contract MUST 至少保留 current 和 previous verified package 及 fixed static resources，覆盖一小时的 API/state compatibility。
每个 release MUST 声明 protocol、request/response/semantic windows、OIDC/Token/management API compatibility 和 state capability read/write sets。
只有 target readiness 和 shared-state checks 通过后，active link 才能原子改变；
Caddy MUST NOT replay writes。
无法 read/write 每个现有 capability 的 slot 或 rollback target MUST 在 activation 前被拒绝；
超出 compatibility window 的 browser asset MUST 收到显式 refresh result，不得静默使用不匹配的 API 或 state semantics。

### REQ-AGENT-VM-030 - 安全 Web Demo 边界

仓库 MAY 提供复用 management console 五个页面、route suffixes 和 API-shaped view model 的静态 Web Demo，但 Demo MUST 使用显式 demo build 和 in-memory transport。
Demo bootstrap MUST 在 console rendering 前完成，并且如果失败 MUST fail closed，不能 fallback 到真实 `/api/v1` transport。
Demo reads 和 mutations MUST NOT 调用真实 API、读取或上传 user files、访问 deployment credentials，或持久化 simulated results。
Share links MAY 只包含文档化的 `demo` 和 `d_*` scenario settings；
reasons、file metadata、credentials 和其他 sensitive values MUST 排除。
Demo MUST 保持明显的 synthetic 属性，MUST NOT 作为 authenticated production management boundary 已部署的证据。

Standalone Demo MUST 使用 origin root 作为 entry、assets、page routes 和 share links 的根路径。
Static hosting MUST 对 unknown page paths 返回 Demo entry，以支持 direct navigation 和 refresh。
原 `/demo/` route 不是 supported compatibility path。

Demo deployment pipeline MUST 为 current revision 构建单一 static artifact，并且只能从 trusted main-branch runs 将该 artifact 部署到现有 EdgeOne Makers project。
Pull request runs MUST 只 build，不得 deploy 或访问 deployment credentials。
Deployment credentials MUST 保留在 hosting repository 的 secret store；
local 或 manual artifact upload 不是 authorized deployment path。

Demo Inspector MUST 遵循[Web Demo Inspector UX/UI 合同](contracts/web-demo-inspector.md)：其 structure、responsive layout、position-menu semantics、explicit floating drag handle、keyboard movement alternative、focus behavior、touch targets 和 visual evidence requirements 都属于 Demo contract。
单击 position control MUST 应用 viewport default，MUST NOT 打开 menu；
right-click、long-press 和 `Shift+F10` MAY 打开 menu。
Floating Inspector MUST 暴露可见的 `Drag`/`拖动` handle，并且 MUST 让 panel 保持在 viewport 内，不得将 coordinates 持久化到 share URL。

### REQ-AGENT-VM-031 - 槽位外部署控制器

部署控制器 MUST 作为稳定的 Go 服务运行在蓝绿槽位外，并且在任一槽位处于 staged、active、retiring 或 rollback 时保持可用。
它 MUST 向运维 CLI 暴露受保护的本机 Unix socket，并向 Prometheus 暴露仅限内部管理网络访问的只读 `GET /metrics` endpoint。
该 endpoint MUST NOT 成为公共 TCP/HTTP 路由或业务 API；
deployd 仍 MUST NOT 获取 Incus/PVE 管理权限或拥有业务状态。
普通蓝绿事务 MUST NOT 写入 rqlite 或访问业务状态数据库；
首次部署的固定 `migrate` 事务是一次性例外，必须受固定 source path、target URL、systemd unit 和阶段状态机约束。
它的独立 SQLite 数据库 MUST 保存部署事务标识、阶段事件、失败证据和最新观测快照，但 MUST NOT 保存 session、lease、generation、operation journal 或业务资源状态。
它 MUST 区分 selected 槽位、进程健康、就绪状态、控制器主权、流量角色、在途请求和 native-worker quiescence，并包含来源新鲜度与来源信息。
`/metrics` MUST 返回最近一次已完成的观测快照，不得在 scrape 请求中触发部署变更、槽位采样或 native effect。
Prometheus scrape target 的 job 名称 MUST 为 `agent-vm-deployd`；
deployd 输出的槽位级指标 MUST 带 `slot="blue"|"green"`，不得因双槽位同时存活而产生重复时间序列。
切换期间两个槽位进程 MAY 同时存活，但恰好一个槽位 MUST 持有运行 scheduler、reconciliation、age GC 和 scheduler-driven native mutation 所需的控制器主权。
槽位在取得已认证 role 前 MUST NOT 运行 startup reconciliation 或这些后台 mutation loop，失去 role 时 MUST fence 它们。
缺失或矛盾的观测 MUST fail closed，并让事务保持可恢复，而不是当成成功。

## 同步与取回

TypeScript/Bun synchronization helper 将 tracked、unignored worktree files 传输到自己的 guest source directory，并且只原子替换 helper-owned content。
Git selection、manifests 和 native filesystem access 保留 byte paths、symlink targets、modes 和 hashes；
不支持的 local root-path representation 必须显式失败。
传输前拒绝 submodules、symlink parents、reserved ownership filenames 和 unsafe names。
Guest transaction 是受 managed image runtime/native prerequisites 和 external execution deadline 保护的 in-memory Bun action；
缺少 Bun 或 atomic capability 时 fail closed。
Native locks、exclusive preparation、baseline identity comparison、fsync 和 atomic directory exchange 在冲突和失败中保留 current source 与 adjacent results。
Uncertain commit 保留 source marker 和 precise recovery record，并返回 exit 75，而不是推断 rollback。
[CLI 合同](contracts/cli.md#source-synchronization-transaction)定义 execution bounds、ownership 和 recovery。
Export 直接向 caller stream tar archive，不创建 server-side archive。

## 验证

- `VER-AGENT-VM-010` 覆盖：`REQ-AGENT-VM-010` 通过[操作运行手册](OPERATIONS.md)的 topology/provenance 章节、已接受的[拓扑与恢复 ADR](../../adr/0028-agent-vm-topology-recovery-boundary.md)和 `bin/test_agent_vm_documentation.py`。
- `VER-AGENT-VM-011` 覆盖：`REQ-AGENT-VM-011` 通过[操作运行手册](OPERATIONS.md)的恢复流程、现有 native reconciliation 与 operation-journal behavior test，以及 `bin/test_agent_vm_documentation.py`。
- `VER-AGENT-VM-012` 覆盖：`REQ-AGENT-VM-012` 通过[操作运行手册](OPERATIONS.md)的 sparse-disk 和 failure-domain 章节、现有 capacity/reclamation test，以及 `bin/test_agent_vm_documentation.py`。
- `VER-AGENT-VM-013` 覆盖：`REQ-AGENT-VM-013` 通过[观测合同](contracts/observability.md)、[容量与故障 dashboard 方案](../../solutions/agent-vm/observability-dashboard.md)和 `bin/test_agent_vm_documentation.py`。
- `VER-AGENT-VM-014` 覆盖：`REQ-AGENT-VM-014` 通过[控制面更新 ADR](../../adr/0030-agent-vm-control-plane-update-boundary.md)、[CLI 合同](contracts/cli.md)、[部署边界](../../../skills/agent-vm-ops/references/deployment.md)和公共边界检查。
- `VER-AGENT-VM-015` 覆盖：`REQ-AGENT-VM-015` 通过[CLI 合同](contracts/cli.md)的兼容性章节、[发布合同](contracts/release.md)、[部署边界](../../../skills/agent-vm-ops/references/deployment.md)中的 release-manifest 规则，以及对支持和拒绝 operation 的兼容行为测试。
- `VER-AGENT-VM-016` 覆盖：`REQ-AGENT-VM-016` 通过[发布合同](contracts/release.md)、[操作运行手册](OPERATIONS.md)的 publication transaction 和 recovery 章节、[控制面更新 ADR](../../adr/0030-agent-vm-control-plane-update-boundary.md)，以及 staged activation、drain、rollback 和 failure-state behavior test。
- `VER-AGENT-VM-017` 覆盖：`REQ-AGENT-VM-017` 通过[发布合同](contracts/release.md)、[部署边界](../../../skills/agent-vm-ops/references/deployment.md)、[agent-vm-ops Skill](../../../skills/agent-vm-ops/SKILL.md)、公共边界检查和 release manifest permission test。
- `VER-AGENT-VM-018` 覆盖：`REQ-AGENT-VM-018` 通过[操作运行手册](OPERATIONS.md)的 CPU startup-pressure 章节、[CLI 合同](contracts/cli.md)、[观测合同](contracts/observability.md)和确定性的 CPU pressure admission test。
- `VER-AGENT-VM-019` 覆盖：`REQ-AGENT-VM-019` 通过[观测合同](contracts/observability.md)的 capacity 和 inventory 章节、[容量与 inventory 观测 ADR](../../adr/0033-agent-vm-capacity-and-inventory-observability.md)、[dashboard 方案](../../solutions/agent-vm/observability-dashboard.md)和公共 dashboard validator。
- `VER-AGENT-VM-020` 覆盖：`REQ-AGENT-VM-020` 通过[管理合同](contracts/management-api.md)、[统一服务安全 ADR](../../adr/0043-agent-vm-unified-service-security-boundary.md)，以及 fixed-route、static-entry/API separation、service-boundary 和 configuration check。现有检查只覆盖旧的 Forward Auth/management UDS boundary；目标 OIDC/unified-service behavior 仍属于计划验证。
- `VER-AGENT-VM-021` 覆盖：`REQ-AGENT-VM-021` 通过[OpenAPI 合同](contracts/management-api.yaml)、其共享示例、[PRODUCT.md](../../../PRODUCT.md)、[DESIGN.md](../../../DESIGN.md)，以及 `bun run typecheck`、`bun run test:web` 和 `bun run test:e2e:auth` 对未认证、登录失败、已认证和 session 过期 UI state 的双语/API 合同检查。
- `VER-AGENT-VM-022` 覆盖：`REQ-AGENT-VM-022` 通过[管理合同](contracts/management-api.md)、OpenAPI operation/error schema，以及 operation recovery 和 audit check。现有检查只覆盖旧管理行为；目标 OIDC、管理员 capability 和机器 Token behavior 仍属于计划验证。
- `VER-AGENT-VM-023` 覆盖：`REQ-AGENT-VM-023` 通过[管理合同](contracts/management-api.md)、[session-pinned image ADR](../../adr/0041-agent-vm-session-pinned-image-identity.md)、CLI 合同和 image-reference concurrency check。
- `VER-AGENT-VM-024` 覆盖：`REQ-AGENT-VM-024` 通过[管理合同](contracts/management-api.md)、[发布合同](contracts/release.md)、[CLI 合同](contracts/cli.md)，以及 migration 和 package-compatibility check。
- 以下条目属于计划中的目标验证，不是当前实现证据；在对应目标面和测试存在前保持 pending。
- `VER-AGENT-VM-025` 覆盖：`REQ-AGENT-VM-025` 通过[管理合同](contracts/management-api.md)的 OIDC 与 principal 章节、[统一服务安全 ADR](../../adr/0043-agent-vm-unified-service-security-boundary.md)，以及 `go test ./internal/webadmin -run 'OIDC|Auth|Session'`、`go test ./internal/state -run 'OIDC|Migration'` 和 `bun run test:e2e:auth` 对 `return_to` allowlist、一次性 companion state、安全失败回跳、browser binding、session establishment 和 SPA session-loss recovery 的验证。
- `VER-AGENT-VM-026` 覆盖：`REQ-AGENT-VM-026` 通过[Token API 合同](contracts/token-api.md)、[Agent API 合同](contracts/agent-api.md)、OpenAPI Token path 和旧 protocol-4 request test。该项属于计划目标验证，在目标面和测试存在前保持 pending。
- `VER-AGENT-VM-027` 覆盖：`REQ-AGENT-VM-027` 通过[管理合同](contracts/management-api.md)的敏感操作与审计章节、OpenAPI confirmation/error schema，以及 administrator authorization 和 audit check。该项属于计划目标验证，在目标面和测试存在前保持 pending。
- `VER-AGENT-VM-028` 覆盖：`REQ-AGENT-VM-028` 通过[状态迁移 ADR](../../adr/0044-agent-vm-state-migration-to-rqlite.md)、[管理合同](contracts/management-api.md)与[发布合同](contracts/release.md)的迁移章节，以及 `go test -race ./internal/state` 对 companion-table ordering、local/remote schema bootstrap、并发 consume、重启恢复和既有 operation ID 语义的验证。其余发布阶段门禁仍按发布合同执行。
- `VER-AGENT-VM-029` 覆盖：`REQ-AGENT-VM-029` 通过[系统架构](../../architecture/system.md)的蓝绿章节、[发布合同](contracts/release.md)，以及 active-link、compatibility-window 和 rollback check。该项属于计划目标验证，在目标面和测试存在前保持 pending。

- `VER-AGENT-VM-030` 覆盖：`REQ-AGENT-VM-030` 通过[Web Demo 决策备忘录](../../proposals/agent-vm-web-demo.md)、[Web Demo Inspector UX/UI 合同](contracts/web-demo-inspector.md)、Demo runtime/API isolation test、页面级 browser suite、static build 和现有 management API boundary check。

## 视觉证据

管理台 Web surface 已从本地 `ui_demo` source 获得确认的视觉证据。
这些截图使用 mock data，不连接 backend，不包含 login、credential 或 live deployment。
它们覆盖 Token 管理、pending revocation-preview recovery，以及 Token 创建、确认对话框和镜像导入中的 512 字符 reason limit。
Reason-limit 截图表明，过长输入仍可编辑，直到修正前提交保持禁用。
Pending-preview 截图延迟 mock response，以展示 recovery control。
移动端截图使用 `393x852` CSS-pixel viewport；桌面端截图展示完整渲染页面。
认证入口截图使用受控的 session/API problem response，不连接身份提供方或真实管理服务；它们覆盖直接访问后的公开主页、脱敏延迟状态、OIDC 登录入口和 session loss 后卸载后台壳的独立认证错误页。
验证映射的机器可读形式使用 ASCII 冒号，以保持仓库检查工具兼容：

- `VER-AGENT-VM-010` 覆盖: `REQ-AGENT-VM-010`
- `VER-AGENT-VM-011` 覆盖: `REQ-AGENT-VM-011`
- `VER-AGENT-VM-012` 覆盖: `REQ-AGENT-VM-012`
- `VER-AGENT-VM-013` 覆盖: `REQ-AGENT-VM-013`
- `VER-AGENT-VM-014` 覆盖: `REQ-AGENT-VM-014`
- `VER-AGENT-VM-015` 覆盖: `REQ-AGENT-VM-015`
- `VER-AGENT-VM-016` 覆盖: `REQ-AGENT-VM-016`
- `VER-AGENT-VM-017` 覆盖: `REQ-AGENT-VM-017`
- `VER-AGENT-VM-018` 覆盖: `REQ-AGENT-VM-018`
- `VER-AGENT-VM-019` 覆盖: `REQ-AGENT-VM-019`
- `VER-AGENT-VM-020` 覆盖: `REQ-AGENT-VM-020`
- `VER-AGENT-VM-021` 覆盖: `REQ-AGENT-VM-021`
- `VER-AGENT-VM-022` 覆盖: `REQ-AGENT-VM-022`
- `VER-AGENT-VM-023` 覆盖: `REQ-AGENT-VM-023`
- `VER-AGENT-VM-024` 覆盖: `REQ-AGENT-VM-024`
- `VER-AGENT-VM-025` 覆盖: `REQ-AGENT-VM-025`
- `VER-AGENT-VM-026` 覆盖: `REQ-AGENT-VM-026`
- `VER-AGENT-VM-027` 覆盖: `REQ-AGENT-VM-027`
- `VER-AGENT-VM-028` 覆盖: `REQ-AGENT-VM-028`
- `VER-AGENT-VM-029` 覆盖: `REQ-AGENT-VM-029`
- `VER-AGENT-VM-030` 覆盖: `REQ-AGENT-VM-030`
- `VER-AGENT-VM-031` covers: `REQ-AGENT-VM-031`

- `VER-AGENT-VM-010` 覆盖：`REQ-AGENT-VM-010`，依据[运维手册](OPERATIONS.md)的 topology/provenance sections、已接受的[拓扑与恢复 ADR](../../adr/0028-agent-vm-topology-recovery-boundary.md)以及 `bin/test_agent_vm_documentation.py`。
- `VER-AGENT-VM-011` 覆盖：`REQ-AGENT-VM-011`，依据[运维手册](OPERATIONS.md)中的 recovery procedure、现有 native reconciliation 和 operation-journal behavior tests，以及 `bin/test_agent_vm_documentation.py`。
- `VER-AGENT-VM-012` 覆盖：`REQ-AGENT-VM-012`，依据[运维手册](OPERATIONS.md)中的 sparse-disk 和 failure-domain sections、现有 capacity/reclamation tests，以及 `bin/test_agent_vm_documentation.py`。
- `VER-AGENT-VM-013` 覆盖：`REQ-AGENT-VM-013`，依据[观测合同](contracts/observability.md)、[容量与故障 dashboard 方案](../../solutions/agent-vm/observability-dashboard.md)以及 `bin/test_agent_vm_documentation.py`。
- `VER-AGENT-VM-014` 覆盖：`REQ-AGENT-VM-014`，依据[控制面更新 ADR](../../adr/0030-agent-vm-control-plane-update-boundary.md)、[CLI 合同](contracts/cli.md)、[部署边界](../../../skills/agent-vm-ops/references/deployment.md)以及 public boundary checks。
- `VER-AGENT-VM-015` 覆盖：`REQ-AGENT-VM-015`，依据[CLI 合同](contracts/cli.md)的 compatibility section、[发布合同](contracts/release.md)、[部署边界](../../../skills/agent-vm-ops/references/deployment.md)中的 release-manifest rules，以及针对 supported/rejected operations 的 compatibility behavior tests。
- `VER-AGENT-VM-016` 覆盖：`REQ-AGENT-VM-016`，依据[发布合同](contracts/release.md)、[运维手册](OPERATIONS.md)中的 publication transaction 和 recovery sections、[控制面更新 ADR](../../adr/0030-agent-vm-control-plane-update-boundary.md)，以及 staged activation、drain、rollback 和 failure-state behavior tests。
- `VER-AGENT-VM-017` 覆盖：`REQ-AGENT-VM-017`，依据[发布合同](contracts/release.md)、[部署边界](../../../skills/agent-vm-ops/references/deployment.md)、[agent-vm-ops Skill](../../../skills/agent-vm-ops/SKILL.md)、public boundary checks 以及 release manifest permission tests。
- `VER-AGENT-VM-018` 覆盖：`REQ-AGENT-VM-018`，依据[运维手册](OPERATIONS.md)中的 CPU startup-pressure section、[CLI 合同](contracts/cli.md)、[观测合同](contracts/observability.md)以及 deterministic CPU pressure admission tests。
- `VER-AGENT-VM-019` 覆盖：`REQ-AGENT-VM-019`，依据[观测合同](contracts/observability.md)中的 capacity 和 inventory sections、[容量与清单观测 ADR](../../adr/0033-agent-vm-capacity-and-inventory-observability.md)、[dashboard 方案](../../solutions/agent-vm/observability-dashboard.md)以及 public dashboard validator。
- `VER-AGENT-VM-020` 覆盖：`REQ-AGENT-VM-020`，依据[管理合同](contracts/management-api.md)、[统一服务安全 ADR](../../adr/0043-agent-vm-unified-service-security-boundary.md)以及 fixed-route、service-boundary 和 configuration checks。
  现有 checks 只覆盖 legacy Forward Auth/management UDS boundary；
  target OIDC/unified-service behavior 仍属于 planned validation。
- `VER-AGENT-VM-021` 覆盖：`REQ-AGENT-VM-021`，依据[OpenAPI 合同](contracts/management-api.yaml)、shared examples、[PRODUCT.md](../../../PRODUCT.md)、[DESIGN.md](../../../DESIGN.md)以及 bilingual/API contract checks。
- `VER-AGENT-VM-022` 覆盖：`REQ-AGENT-VM-022`，依据[管理合同](contracts/management-api.md)、OpenAPI operation/error schemas 以及 operation recovery 和 audit checks。
  现有 checks 只覆盖 legacy management behavior；
  target OIDC、administrator capability 和 machine-Token behavior 仍属于 planned validation。
- `VER-AGENT-VM-023` 覆盖：`REQ-AGENT-VM-023`，依据[管理合同](contracts/management-api.md)、[session-pinned image ADR](../../adr/0041-agent-vm-session-pinned-image-identity.md)、CLI contract 和 image-reference concurrency checks。
- `VER-AGENT-VM-024` 覆盖：`REQ-AGENT-VM-024`，依据[管理合同](contracts/management-api.md)、[发布合同](contracts/release.md)、[CLI 合同](contracts/cli.md)以及 migration 和 package-compatibility checks。
- 以下条目属于 planned target validation，不是 current implementation evidence；
  在对应 target surfaces 和 tests 存在前保持 pending。
- `VER-AGENT-VM-025` 覆盖：`REQ-AGENT-VM-025`，依据[管理合同](contracts/management-api.md)的 OIDC 和 principal sections、[统一服务安全 ADR](../../adr/0043-agent-vm-unified-service-security-boundary.md)以及 callback/session validation checks。
  该项属于 planned target validation，在 target surface 和 tests 存在前保持 pending。
- `VER-AGENT-VM-026` 覆盖：`REQ-AGENT-VM-026`，依据[Token API 合同](contracts/token-api.md)、[Agent API 合同](contracts/agent-api.md)、OpenAPI Token paths 和 legacy protocol-4 request tests。
  该项属于 planned target validation，在 target surface 和 tests 存在前保持 pending。
- `VER-AGENT-VM-027` 覆盖：`REQ-AGENT-VM-027`，依据[管理合同](contracts/management-api.md)的 sensitive-action 和 audit sections、OpenAPI confirmation/error schemas 以及 administrator authorization 和 audit checks。
  该项属于 planned target validation，在 target surface 和 tests 存在前保持 pending。
- `VER-AGENT-VM-028` 覆盖：`REQ-AGENT-VM-028`，依据本 SPEC 的状态迁移与恢复合同、[管理合同](contracts/management-api.md)、[发布合同](contracts/release.md)中的 migration sections，以及 transaction/concurrency/recovery checks；[state migration ADR](../../adr/0044-agent-vm-state-migration-to-rqlite.md)只提供决策背景。
  该项属于 planned target validation，在 target surface 和 tests 存在前保持 pending。
- `VER-AGENT-VM-029` 覆盖：`REQ-AGENT-VM-029`，依据[system architecture](../../architecture/system.md)的 blue-green sections、[发布合同](contracts/release.md)以及 active-link、compatibility-window 和 rollback checks。
  该项属于 planned target validation，在 target surface 和 tests 存在前保持 pending。

- `VER-AGENT-VM-030` 覆盖：`REQ-AGENT-VM-030`，依据[Web Demo decision memo](../../proposals/agent-vm-web-demo.md)、[Web Demo Inspector UX/UI 合同](contracts/web-demo-inspector.md)、Demo runtime/API isolation tests、page-level browser suite、static build 以及现有 management API boundary checks。
- `VER-AGENT-VM-031` 覆盖: `REQ-AGENT-VM-031`：验证本 SPEC 的部署控制器边界、[部署控制器合同](contracts/deployment-controller.md)、[发布合同中的部署控制器章节](contracts/release.md#deployment-controller-boundary)、[部署观测合同](contracts/observability.md#deployment-control-observation)、[蓝绿部署资源](../../deployment/blue-green.md)，以及隔离的 Go/systemd/双槽位事务和恢复检查；[部署控制器 ADR](../../adr/0045-agent-vm-deployment-controller.md)只提供决策背景。
  当前实现证据包括 `agent-vm-deployd`/`agent-vm-deploy`、controller lease fencing、独立部署 SQLite、双槽位事务/恢复测试、Prometheus exposition 测试、发布清单测试和 Agent VM 隔离验收脚本；
  真实部署地址、证书、配置和长期 Prometheus/Grafana 实例仍属于部署方验证。

## 视觉证据

管理台 Web 界面已通过本地 `ui_demo` source 取得 visual evidence。
Captures 使用 mock data，不包含 backend、login、credentials 或 live deployment。
它们覆盖 Token management、pending revocation-preview recovery，以及 Token creation、confirmation dialogs 和 image import 中的 512-character reason limit。
Reason-limit captures 表明 overlong input 仍可编辑，直到修正前 submission 保持 disabled。
Pending-preview capture 延迟 mock response，以展示 recovery controls。
Mobile captures 使用 `393x852` CSS-pixel viewport；
desktop capture 展示完整的 rendered page surface。

- [桌面 System 页面](assets/agent-vm-web-system-desktop.png)
- [移动端 System 页面](assets/agent-vm-web-system-mobile-list.png)
- [移动端撤销预览对话框](assets/agent-vm-web-system-mobile-revoke-dialog.png)
- [移动端 pending 撤销预览](assets/agent-vm-web-system-mobile-preview-pending.png)
- [移动端确认 reason limit](assets/agent-vm-web-system-mobile-dialog-reason-limit.png)
- [移动端 Token 创建 reason limit](assets/agent-vm-web-system-mobile-create-reason-limit.png)
- [移动端镜像导入 reason limit](assets/agent-vm-web-images-mobile-import-reason-limit.png)
- [桌面端公开主页](assets/agent-vm-public-home-desktop.png)
- [移动端公开主页](assets/agent-vm-public-home-mobile.png)
- [桌面端 session loss 独立认证错误页](assets/agent-vm-auth-expired-desktop.png)

这里的 owner confirmation 只适用于此处展示的渲染 UI，不代表端到端 backend integration 或 deployment readiness。
Owner confirmation 仅适用于此处展示的 rendered UI，不表示 end-to-end backend integration 或 deployment readiness 已得到确认。

## 相关 ADR

- [Agent VM 拓扑与恢复边界](../../adr/0028-agent-vm-topology-recovery-boundary.md)
- [Agent VM 仅按内存 reservation admission](../../adr/0029-agent-vm-memory-reservation-only-admission.md)
- [Agent VM 控制面更新边界](../../adr/0030-agent-vm-control-plane-update-boundary.md)
- [Agent VM overcommit 的 CPU 启动压力](../../adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md)
- [Agent VM 容量与 inventory 观测](../../adr/0033-agent-vm-capacity-and-inventory-observability.md)
- [Agent VM Go 常驻控制运行时](../../adr/0037-agent-vm-go-control-runtime.md)
- [原生 Incus Agent 容器生命周期](../../adr/0038-native-incus-agent-container-lifecycle.md)
- [Agent VM session-pinned image identity](../../adr/0041-agent-vm-session-pinned-image-identity.md)
- [Agent VM 统一服务安全边界](../../adr/0043-agent-vm-unified-service-security-boundary.md)
- [Agent VM SQLite 到 rqlite 状态迁移](../../adr/0044-agent-vm-state-migration-to-rqlite.md)

## 视觉证据

- [Web Demo 容量场景，桌面端](assets/web-demo-capacity-desktop.png)
- [Web Demo 空场景，移动端 393×852 CSS px](assets/web-demo-empty-mobile.png)
- [带面板级拖动手柄的 Web Demo 浮动 Inspector](assets/web-demo-inspector-panel-handle.png)

## 相关资料
- [Agent VM 仅内存预留准入](../../adr/0029-agent-vm-memory-reservation-only-admission.md)
- [Agent VM 控制面更新边界](../../adr/0030-agent-vm-control-plane-update-boundary.md)
- [Agent VM overcommit 的 CPU 启动压力](../../adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md)
- [Agent VM 容量与清单观测](../../adr/0033-agent-vm-capacity-and-inventory-observability.md)
- [Agent VM Go 常驻控制运行时](../../adr/0037-agent-vm-go-control-runtime.md)
- [Native Incus Agent 容器生命周期](../../adr/0038-native-incus-agent-container-lifecycle.md)
- [Agent VM session 固定镜像身份](../../adr/0041-agent-vm-session-pinned-image-identity.md)
- [Agent VM 统一服务安全边界](../../adr/0043-agent-vm-unified-service-security-boundary.md)
- [Agent VM SQLite 到 rqlite 状态迁移](../../adr/0044-agent-vm-state-migration-to-rqlite.md)
- [Agent VM 部署控制器](../../adr/0045-agent-vm-deployment-controller.md)

## 视觉证据补充

- [Web Demo 容量场景，桌面](assets/web-demo-capacity-desktop.png)
- [Web Demo 空状态场景，移动端 393×852 CSS px](assets/web-demo-empty-mobile.png)
- [Web Demo 浮动 Inspector 与面板级拖动手柄](assets/web-demo-inspector-panel-handle.png)

## 相关材料

- [CLI 合同](contracts/cli.md)
- [控制面发布合同](contracts/release.md)
- [管理 API](contracts/management-api.yaml)
- [管理实现合同](contracts/management-api.md)
- [Token API](contracts/token-api.md)
- [Agent API](contracts/agent-api.md)
- [Web Demo Inspector UX/UI 合同](contracts/web-demo-inspector.md)
- [回收设计](RECLAMATION-DESIGN.md)
- [保留设计](RETENTION-DESIGN.md)
- [观测合同](contracts/observability.md)
