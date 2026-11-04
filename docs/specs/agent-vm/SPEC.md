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

## 需求

### REQ-AGENT-VM-010 - 分层来源

公共合同必须区分 Codex client、配置的 control endpoint、CT109 LXC、Incus system container 和未知的底层 hosting source。不得把配置地址、CT identifier 或 Incus identity 变成对下一层的断言，并且必须将私有部署地址和 inventory 保留在仓库之外。

### REQ-AGENT-VM-011 - 重建与恢复封锁

恢复必须保留状态数据库和 WAL/journal、operation 与 audit record、generation、配置、受保护的 recovery copy、native timer asset 和 Incus container data。它必须在 drain 下恢复匹配组件，依据 native identity 和 generation 对持久 record 做 reconciliation，并在 state、provenance、capacity 或 native identity 缺失或不一致时保持 admission 关闭。不得发明 operation ID、重置 generation、重放不确定的 native effect 或执行宽泛清理。

### REQ-AGENT-VM-012 - 稀疏磁盘故障边界

磁盘 admission 必须核算配置的 CT-local Incus pool filesystem、request cost、watermark 和持久 reservation，但不得声称为每个配置的 container rootfs limit 都预留了完整物理空间。控制面状态和 container data 必须视为分离的逻辑故障域；ENOSPC 或 I/O failure 必须保守保留状态，并拒绝不安全的新 admission，不得驱逐有效 lease。

### REQ-AGENT-VM-013 - 观测来源

运维 dashboard 必须区分 CT filesystem capacity、临时 image staging、物理 Incus pool usage、guest rootfs allocation 和 native admission state。不得从未设置的 Incus project quota 推断权威 native limit，也不得为缺失或 `-1` 的来源画 limit line。Dashboard 合同必须暴露 failure phase、image-unpack failure、admission state 和 scrape freshness，使 ENOSPC 边界无需访问 host manager 也能识别。公共仓库必须提供经过脱敏、参数化的 dashboard baseline 和验证路径；部署专属渲染文件、datasource identity、controller job 和 collector path 归部署所有。

### REQ-AGENT-VM-014 - 控制面更新权威

控制面 publication、update 和 rollback 必须保持为部署方拥有的操作。公共 `agent-vm` CLI 必须保持为 lifecycle/request client，不得获得 Incus/PVE authority，不得暴露公共 self-update command，也不得成为第二控制面。Go `serve` process 必须保持唯一在线控制权威；旧 Bun control process 只允许作为经过验证的兼容 release 或 forwarding bridge 使用，不得与 live state 并发运行。

### REQ-AGENT-VM-015 - 方法级兼容性

Release manifest 必须为每个 supported operation 声明 protocol、request/response schema、semantic compatibility 和 direction。兼容 operation 可以跨 release revision 继续运行，并返回结构化 warning。不支持的 action、不兼容的 schema 或语义变化、无效 artifact checksum 或不兼容 protocol，必须在任何持久化或 native effect 前拒绝。

### REQ-AGENT-VM-016 - 发布事务与回滚

Publication 必须在 drain 前 staging 并验证明确的 release，在远程激活前进入 drain，保留在途 operation 和 maintenance timer，等待 pending operation 与 task transition 收敛，原子激活 artifact，并在重新开放 admission 前验证 health、reconciliation 和 timer state。激活前失败必须保持当前 release 不变。激活后的 health failure 可以对上一个已验证 release 执行一次有界 rollback；rollback 失败时必须继续保持 admission drain，不得改写 state、generation 或不确定 operation。

### REQ-AGENT-VM-017 - 发布范围与权限边界

Control-plane release 必须包含本地 client、remote control script、systemd asset、release metadata 和准确的 artifact checksum，同时排除部署配置、凭据、连接材料、状态数据库、锁、Incus container data 和 guest image。Publication 必须使用配置的 control entry、操作员专用 authority 或范围严格限定的 root-owned deployment authority，不得增加 PVE、主机、guest、额外 SSH、HTTP 或 gateway 访问。默认必须关闭无人值守的 control-plane update。

### REQ-AGENT-VM-018 - CPU 启动压力边界

Controller 必须把 CT-local CPU PSI 与 logical CPU reservation 分开观测。当新的 acquire、cached resume、允许 restart 的 resize 或显式 restart 会使 CPU allocation 预计超过 CT-visible CPU capacity，且已验证的 `full.avg10` 达到配置的 startup-pressure threshold 时，必须在停止或启动已有 guest 前，以结构化 `resource_pressure` evidence 拒绝 native boot。该 guard 不得改变 logical CPU allocation ceiling，不得停止有效 lease，也不得阻止 read、renewal、retrieval、release 或 live memory-only resize；缺失 CPU PSI 必须保持为明确的观测失败，不得伪造为零。

### REQ-AGENT-VM-019 - 容量与清单观测

只读观测合同必须区分 logical reservation composition、CT-visible capacity、runtime pressure、physical disk headroom、native admission state 和逐 task lifecycle state。它必须暴露 `some.avg10`、`full.avg10`、配置的 startup threshold 和 startup-blocked state，不得把 PSI 当作 CPU utilization 或 logical remaining capacity。批准的 overcommit deployment profile 必须将 full-pressure threshold 配置为 50%，同时保留 compound projected-capacity gate。

部署观测面必须分别提供 capacity/admission 和 inventory/operations 视图。Capacity 视图必须在有权威依据时展示 allocated、reserved、allocated-and-reserved、remaining、limit 和 CT-visible capacity，并展示 task-state count，以及 logical slot 和当前可启动 default-container slot 的明确 snapshot estimate。Inventory 视图必须覆盖所有受管 task state，使用有界字段，移除 stale row，并避免原始错误、凭据和部署路径。缺失或为 `-1` 的 native slot limit 必须保持缺失；derived slot estimate 不得标记为权威 slot limit。

Deployment scrape bridge 必须将 `admin-metrics` 和 `admin-status` 调度为独立观测 lane。Inventory read pending 或失败时，不得移除最近有效的 capacity metric family；capacity read pending 或失败时，不得保留失败 inventory scrape 中的 task row。保留的 source snapshot 必须使用其原始观测时间戳，并暴露按来源的 health 或 freshness failure；bridge 不得用伪造的零替换不可用值。

### REQ-AGENT-VM-020 - 认证管理边界

每个蓝绿槽位必须运行一个 Go/Gin Agent VM service，提供固定静态 Web asset、OIDC callback、同源 management API 和 `/agent/v1/control`；设计不得增加独立 Web process、Forward Auth identity header、management UDS、router、Web/BFF、native service 或 public TCP bypass。Caddy 只能执行 TLS 和固定路径的活动槽位路由。统一服务必须执行有界的 method/path/input、OIDC/session/Token authorization、Origin/CSRF、target-session、generation 和 journal check；任何必要的 OIDC、origin、Token、upload 或 staging 配置不完整时，必须保持 listener 关闭。没有 browser session 时可以提供固定 static asset 和 allowlisted management page deep link，但不得包含管理数据，也不得作为已认证证据。

### REQ-AGENT-VM-021 - 管理 API 与本地化界面

管理台只能暴露 OpenAPI 合同定义的同源 `/api/v1` 面。它必须拒绝未知字段和不支持的 action，使用有界 cursor pagination，保留准确的 byte 和 generation 值，返回结构化的字段白名单错误，并暴露 source freshness 和 unknown value，不得将其转换成健康的零。UI 必须通过 `/api/v1/session` 启动 authentication gate，之后才能展示管理员 actor、capability 或受管资源。直接访问没有有效会话时，UI 必须把入口收敛到公开主页 `/`，并可通过无需认证的 `/api/v1/public/status` 展示脱敏、按固定间隔更新的基础状态；该公开响应不得包含管理员身份、capability、容量或压力明细，且不得被当作管理授权或实时事实。公开主页必须提供直接发起 OIDC 的登录 action。收到 `authentication_required` 时，必须显示本地化登录状态和明确的登录 action，不得形成无界 retry loop。OIDC 回跳失败时回到公开主页并显示通用失败提示。已有管理会话发生 session loss 时，UI 必须卸载后台普通 layout，保留当前管理页面路径，显示独立 auth-error layout；该 layout 不得渲染后台导航、后台数据或管理员 actor，并在顶栏提供直接发起 OIDC 的登录 action。任何 management API 返回 `401 authentication_required` 或 `csrf_invalid` 时，UI 必须使本地 session 失效，暂停 query 和 mutation，等待明确的重新认证；成功后重新 bootstrap session 并加载页面 data。不得自动 replay mutation；任何可能已提交的 mutation 必须使用原 operation ID reconciliation。UI 必须支持 `zh-CN` 和 `en`，优先使用已保存选择，其次是浏览器语言，最后回退英文；accessible name、confirmation 和 error 必须翻译，technical identifier 和用户填写的 reason 保持不变。

### REQ-AGENT-VM-022 - 受管操作与审计恢复

已有 management resource 的每次 mutation 必须携带 UUID operation ID 和当前 target version；每个 durable create 或 Token issuance 必须携带 UUID operation ID 和 canonical retry identity；VM action 还必须携带 expected generation。Request 可以包含有界的非空 reason，但缺少 reason 必须被接受并表示为 absent。管理员敏感提交必须使用绑定 authenticated subject、准确 target、action、参数和 target version 的 confirmation token；authority 必须在产生 effect 前重新校验 authorization 和 state；Token creation 是 durable-create exception，不需要单独 preview。相同 operation retry 必须解析为同一 durable operation；冲突复用必须失败。不确定的 native outcome 必须保持可查询，不得自动 replay。Audit record 必须保留当前管理员 actor、没有管理员 authority 的 typed rejected-request actor，或带原始值的显式未知历史 actor，以及可选 reason、target、phase 和 outcome；不得暴露原始 command diagnostic、凭据或私有路径。

### REQ-AGENT-VM-023 - 受管镜像安全与会话固定

受管镜像必须使用不可变 fingerprint、allowlisted source identity、有界的本地 package upload、独立的完整性与适用性结果，以及明确的 reference count。配置的 legacy default 必须在迁移期间保留并显示为未验证，只有完整验证的镜像才能通过 management API 选择。Default selection 只影响新 session。已有 task 和 cache 必须保留准确的 fingerprint。Default、active、cached、releasing、pending-creation 和 verification reference 都必须阻止删除，引用检查必须与镜像 operation 串行化。

### REQ-AGENT-VM-024 - 管理状态与回滚兼容性

管理 schema 变更必须是增量、事务性的，并记录到版本化 state-capability marker。Migration 必须保留 task identity、generation、lease、port、reservation 和既有 protocol-4 journal state。只有声明支持读写状态数据库中每个 capability marker 的 package 才可激活。没有 capability declaration 的 package 必须视为只支持管理前 state。不兼容 rollback 必须在变更 active package pointer 或启动 target 前拒绝；不得降级或重建 database 来强制兼容。Protocol 4 和既有 CLI request/response behavior 必须保持不变。

### REQ-AGENT-VM-025 - OIDC 主体与浏览器会话身份

浏览器登录必须使用 OIDC Authorization Code + PKCE，且只允许 `code_challenge_method=S256`，并使用一次性 `state`、`nonce`、受保护 code verifier、准确 redirect URI、短时 browser-bound login cookie 和有界 expiry。服务必须在创建 session 前校验配置的 discovery issuer、准确的 ID Token `iss`、`aud`/`azp`、signature/JWKS、`exp`、`iat` policy、authorization-code exchange 和 nonce。Service 和 proxy access log 必须省略 callback query string 和 `Referer`，application log 不得保留 OIDC code、state、nonce 或 verifier。持久主体必须是大小写敏感的 `(issuer, subject)` 二元组；只有配置的二元组才是管理员 actor，email、display name、group claim 和 forwarded identity header 不得建立 authorization。重放、过期、跨浏览器或未经验证的 callback 必须拒绝；不在 allowlist 的登录不得创建 session 或 administrator capability。可选 `return_to` 必须校验为同源管理页面 `/`、`/vms`、`/images`、`/operations` 或 `/system` 之一，保存到一次性 transaction，且只用于成功登录后的导航；缺少 `return_to` 时必须解析为 `/`。Provider denial、identity rejection 或 callback failure 不得创建 session，并必须跳转到 `/?auth=failed`；frontend 必须消费并清除该标记，不暴露 provider diagnostic。

### REQ-AGENT-VM-026 - 机器 Token 与 Agent API

唯一管理员必须是唯一允许通过浏览器签发、列出、读取和撤销不透明 machine Token 的 actor。Token metadata 必须包含不透明的 `resource_version`、scope、expiry 和 revocation state，但不得包含 user-owner field 或预绑定 session set；明文只能返回一次，服务只保存 validation digest 和一次性 disclosure state。Token creation 必须要求 operation ID 和 canonical request identity；请求 `agent:operate` scope 时必须同时显式包含 `agent:read` scope，不得隐式继承。成功的 `token_create` operation 必须通过安全 metadata 包含 Token ID 和持久 plaintext disclosure state。Retry 或并发 request 必须返回 durable operation 的 `token_id`、metadata、`resource_version` 和 disclosure state，不得重新签发明文；响应丢失后，管理员必须查询原 operation，确认 Token ID，显式撤销该 Token，再用新的 operation ID 创建替代品。Token revocation 必须要求当前 preview 的 `target_version` 和一次性 confirmation token，在 acknowledgment 前 commit，推进 `resource_version`，并且 Agent authorization 必须以 linearizable read 观察到 revocation。`POST /agent/v1/control` 只能接受有效 Bearer Token 和既有 protocol-4 body，每个 request 都必须重新校验 Token expiry/revocation、scope、目标 session、generation、operation ID 和 request digest。具有允许 scope 的有效 Token 可以获取任意目标 session，不要求预绑定该 session。无 session 的 administrative observation 必须要求显式 `agent:observe_admin` scope。

### REQ-AGENT-VM-027 - 管理员敏感操作与审计

Administrator capability 只能根据配置的准确 `(issuer, subject)` 二元组解析，不得从 email 或 group claim 推导。敏感 action 必须要求显式 capability、影响 preview 和绑定 actor、准确 target、参数、target version 与 generation 的一次性 confirmation token；提供时审计有界非空 reason，但不要求 reason。统一服务必须在 native effect 前重新校验 authorization 和 state。Accepted、rejected、succeeded、failed 和 outcome-unknown attempt 都必须可审计，actor 使用当前 issuer/subject、没有管理员 authority 的 typed `unauthenticated` 或 `unauthorized` rejected-request actor，或者显式未知历史 actor，并记录可选 reason、operation ID、target、phase、result 和安全错误码；不得记录 Token value、OIDC code、凭据、原始 command output 或私有路径。历史 task 和 native record 必须依据 durable task identity、generation、lease、journal 和 native identity reconciliation；不得建立 user mapping，也不得依据首次登录推断身份。

### REQ-AGENT-VM-028 - 保守 SQLite 到 rqlite 迁移与原子并发

在 state-write fence 拒绝新的 browser/API/Agent durable mutation、暂停 scheduler/reconciliation/timer durable write、排空在途 transaction 和 native worker，并要求每个 native worker 在原 operation ID 下以持久结果结束且 process group、PID identity 和 Incus operation 已验证静止之前，SQLite 必须保持唯一 writer。Pending/unknown native result、active worker 或未经验证的 quiescence 都必须阻止最终 export；migration 不能只记录后继续。Migration 必须保留 protocol 4、schema、generation、`state_epoch`、operation journal、audit、session、Token 和 capability 语义；不得双写、从两个槽位打开共享 SQLite 文件、反向同步猜测的 state、降级 schema 或为 rollback 重建 state。切换后 rqlite 必须是唯一共享权威；关键的 administrator-authorization、lease、generation、journal、Token-revocation、capability 和 epoch read 必须使用 linearizable consistency，跨行 compare-and-act change 必须使用一个 rqlite transaction，并通过条件更新或 unique constraint 实现。如果需要 rqlite recovery，经过验证的 backup 必须记录不早于最后可信提交的 coverage boundary，boundary 后的 backup tail 必须使用可信 append-only journal 和 native-effect evidence reconciliation；schema、digest、epoch 或 journal 检查本身不得当作覆盖证明。如果 tail 或切换后任意写入无法证明，服务必须继续 fenced，等待管理员授权的 journal/native reconciliation。只有证据证明切换后零写入和零 effect 时，cutover 前 SQLite snapshot 才可采用。Native unknown outcome 必须保持 pending，不能使用新的 operation ID replay。

### REQ-AGENT-VM-029 - 蓝绿兼容窗口

Release contract 必须至少保留当前和上一个已验证 package 及固定 static resource，维持一小时的 API/state compatibility。每个 release 必须声明 protocol、request/response/semantic window、OIDC/Token/management API compatibility，以及 state capability read/write set。只有目标 readiness 和 shared-state check 通过后，active link 才能原子改变；Caddy 不得 replay write。无法读写所有现存 capability 的 slot 或 rollback target，必须在 activation 前拒绝；超出 compatibility window 的 browser asset 必须收到明确 refresh result，不得静默使用不匹配的 API 或 state semantic。

### REQ-AGENT-VM-030 - 安全 Web Demo 边界

仓库可以提供静态 Web Demo，复用管理台的五个页面、route suffix 和 API-shaped view model，但 Demo 必须使用显式 demo build 和 in-memory transport。Demo bootstrap 必须在 console render 前完成；失败关闭时不得退回真实 `/api/v1` transport。Demo read 和 mutation 不得调用真实 API、读取或上传用户文件、访问部署凭据或持久化模拟结果。Share link 只可以包含记录在案的 `demo` 和 `d_*` scenario setting；不得包含 reason、文件 metadata、凭据或其他敏感值。Demo 必须始终明显是合成体验，不得作为已部署认证生产边界的证据。

独立 Demo 必须使用 origin root 作为 entry、asset、page route 和 share link 的根。静态 hosting 必须为未知 page path 提供 Demo entry，以支持直接导航和刷新。旧的 `/demo/` route 不是支持的兼容路径。

Demo deployment pipeline 必须为当前 revision 构建一个静态 artifact，并且只允许来自可信 main branch run 的流程将其部署到现有 EdgeOne Makers project。Pull request run 必须在不部署、不访问 deployment credential 的情况下完成构建。Deployment credential 必须保存在 hosting repository 的 secret store 中；本地或手动 artifact upload 不是授权的部署路径。

Demo Inspector 必须遵守 [Web Demo Inspector UX/UI 合同](contracts/web-demo-inspector.md)：其结构、响应式布局、position-menu 语义、明确的浮动 drag handle、键盘移动替代方案、focus 行为、touch target 和视觉证据要求都属于 Demo 合同。单击 position control 必须应用 viewport default，不得打开菜单；右键、长按和 `Shift+F10` 可以打开菜单。浮动 Inspector 必须显示可见的 `Drag`/`拖动` handle，并保持面板在 viewport 内，不得把坐标持久化到 share URL。

## 同步与取回

TypeScript/Bun synchronization helper 将 tracked、unignored worktree file 传输到自己的 guest source directory，并原子替换仅由 helper 拥有的内容。Git selection、manifest 和 native filesystem access 保留 byte path、symlink target、mode 和 hash；无法安全表示本地 root path 时必须明确失败。它在 transfer 前拒绝 submodule、symlink parent、保留 ownership filename 和不安全 name。Guest transaction 是受 managed image runtime/native prerequisite 和外部 execution deadline 保护的内存中 Bun action；缺少 Bun 或 atomic capability 时失败关闭。Native lock、exclusive preparation、baseline identity comparison、fsync 和 atomic directory exchange 在冲突和失败中保护当前 source 及相邻 result。不确定的 commit 保留 source marker 和准确 recovery record，并返回 exit 75，而不是推断 rollback。[CLI 合同](contracts/cli.md#source-synchronization-transaction)定义执行边界、ownership 和 recovery。Export 直接向 caller 输出 tar stream，不创建服务端归档。

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
