# ADR 0037：Go 常驻控制运行时

## 状态

已接受，用于 Agent VM 控制面迁移。

本 ADR 中的 SQLite、受保护 UDS 和 SSH 细节描述蓝绿部署前的控制运行时基线。
蓝绿服务边界和迁移后的共享状态权威由 ADR 0043 与 ADR 0044 定义；部署事务归属和槽位协调由 ADR 0045 单独定义。
这些决定不改变本文记录的生命周期、journal 或 protocol-4 不变量。

## 背景

现有 Agent VM 控制路径是每次请求启动一次的 Bun 进程。
它在同一个执行模型中负责 SQLite 状态、Incus 生命周期操作、维护调度和观测采样。
引入常驻父服务时不得创建第二个 writer、改变 operation identity，或让 expiry 依赖外部触发器。

迁移还必须保留当前 SSH 客户端合同，同时允许以后移除 SSH。
因此 transport adapter 必须可替换，不能把 protocol validation、lifecycle policy、journal handling 或 cleanup 移入客户端。

## 决策

控制环境在每个槽位使用一个 Go 1.27.3 二进制文件，作为唯一常驻的业务控制权威。
该二进制文件提供 `serve`、`request` 和 `native-worker` 子命令。
`serve` 拥有 SQLite 写连接、生命周期状态机、admission、native-worker 监督、观测快照和进程内 expiry/cache scheduler。
在蓝绿目标中，scheduler-driven mutation 只有持有 deployd 授予的 controller role 的槽位才能启用；另一个槽位可以继续存活以提供 readiness 和流量处理，但不能成为第二个业务 scheduler。
`request` 子命令是服务的有界一次性 HTTP 客户端，不打开 SQLite，也不直接调用 Incus。

槽位外 `agent-vm-deployd` 服务不是替代 `serve` 的进程。
它通过独立的受保护边界拥有部署事务，并观测槽位/systemd/流量证据，具体见 ADR 0045。

Gin v1 处理固定的内部 HTTP 路由。
服务监听受保护的 Unix domain socket，并验证 peer credentials。
SQLite 仍是现有数据库，通过 `database/sql` 和 `modernc.org/sqlite` 访问；SQL 保持显式，不引入 ORM。
Linux 专用的 locking、peer credentials、subreaper 和 pidfd 处理使用 `x/sys/unix`；native command 使用 `os/exec`，结构化日志使用 `slog`。

首个 release 保留 SSH 作为兼容 transport。
本机 Bun 客户端对 format-1 sidecar 选择 `legacy-bun`，对 format-2 sidecar 选择 `go-request`。
后者由 SSH 启动固定的 Go `request` adapter，通过 UDS 上的 HTTP 发送原始 protocol-4 body。
Adapter 不持有控制状态，服务拥有唯一的活动权威。
未来的 HTTPS 或直接管理 UDS 的客户端可以替换该 adapter，而无需改变 protocol-4 body、operation identity、SQLite schema、生命周期服务或 scheduler。

Format 2 声明目标 OS/architecture、Go toolchain、不可变产物 checksum、固定客户端 transport enum，以及由常驻 scheduler 退役的旧 systemd unit。
支持窗口内，Format 1 对旧版 Bun release 仍然有效。

## 后果

服务必须在 systemd 下启动后才能准入新工作，并且必须在报告 ready 前 reconcile 过期 lease、pending operation 和常驻 worker。
HTTP 或 SSH 请求路径不可用时，expiry、retry 和 age-GC 仍继续运行。
旧 Bun controller 只能作为已验证的兼容 release 保留，不得与 Go 服务并发操作 live database。

迁移会扩大 native-process 和部署测试范围。
在宣布 cutover 可合并前，必须具备 Linux process-tree、systemd、SQLite recovery 和隔离 Incus 证据。
激活失败时允许回滚一次到不可变的 format-1 bridge bundle 或上一个已验证的 format-2 release，但不得降迁持久状态。

## 否决的替代方案

- 让 Bun 客户端成为第二个生命周期权威虽然可以保留 transport 形状，但会重复 state 和 cleanup 决策。
- 使用 ORM 会隐藏现有 journal 合同中的 transaction 和 numeric identity 细节。
- 在同一变更中引入 Incus Go SDK 会扩大 native 控制边界，并让 rollback 依赖两个独立集成。
- 永久要求 SSH 会把未来的直接 HTTPS/UDS transport 变成业务逻辑迁移，而不是 adapter 替换。
