# Agent VM 蓝绿部署

本文是 Agent VM 蓝绿部署的实施与运维主文档。它定义槽位布局、Deployd、一次性状态迁移、Prometheus 观测、失败恢复和清理规则；长期需求与 API 字段分别由 [Agent VM SPEC](../specs/agent-vm/SPEC.md) 及其合同约束。

这些资源只描述目标安装和隔离验收，不包含生产地址、凭据、证书私钥或业务数据。生产首次部署必须由发布器完成完整的一键激活流程，不能把状态迁移拆成发布后的人工前置步骤。

## 固定组件

权威的机器可读组件清单是 [`deployment-components.json`](../../skills/agent-vm-ops/assets/blue-green/deployment-components.json)。只允许使用清单中的官方发布地址，并在安装前验证固定校验和。

| 组件 | 版本 | 运行时职责 | 支持的产物 |
| --- | --- | --- | --- |
| Caddy | 2.11.7 | HTTPS 边缘代理 | 已验证 SHA-512 的 Linux amd64 和 arm64 压缩包 |
| rqlite | 10.5.2 | 共享业务状态权威 | 已验证 SHA-256 的 Linux amd64 和 arm64 压缩包 |

自动下载和自动安装已关闭。运维者必须先验证固定校验和，才能把二进制文件放置到 `/usr/local/bin/caddy` 或 `/usr/local/bin/rqlited`。

## 资源布局

`skills/agent-vm-ops/assets/systemd/` 中的 unit 使用以下布局：

```text
/etc/agent-vm-native/
  Caddyfile
  caddy.env
  rqlite/auth.json
  tls/caddy/{server.crt,server.key}
  tls/rqlite/{server.crt,server.key}
  deployd.json
  slots/{blue,green}/config.json
/opt/agent-vm-native/
  release-manifest.json
  bin/{agent-vm-deployd,agent-vm-deploy}
/var/lib/agent-vm-native/
  active -> slots/{blue,green}
  slots/{blue,green}/bin/agent-vm-control
  slots/{blue,green}/release-manifest.json
  deployment/deployment.sqlite3
  migration/.rqlite-cutover-complete
  rqlite/
/run/agent-vm-native/{blue,green}/app.sock
/run/agent-vm-native/{blue,green}/metrics/
/run/agent-vm-native/deployd.sock
```

`agent-vm-native@.service` 为每个槽位启动一个 Go 服务，并使用该槽位目录中的 manifest。两个槽位可以同时存活并接受只读观测，但没有有效 controller lease 的槽位不得运行 startup reconcile、timer reconcile、age GC 或任何 scheduler-driven native mutation。

`agent-vm-deployd.service` 位于两个槽位之外，是稳定的宿主级部署控制器。它不通过 `Wants=` 或 `After=` 拉起蓝、绿槽位；槽位只能由部署事务在正确阶段显式启动。这样首次状态迁移可以先停止旧 SQLite writer，再启动新槽位，避免迁移期间出现第三个 scheduler 或新的业务 writer。

启动 unit 前，使用随附的 `sysusers.d` 和 `tmpfiles.d` 条目运行 `systemd-sysusers` 与 `systemd-tmpfiles --create`。这些条目创建专用的 rqlite 用户和非特权 `caddy` 用户；`active` 不由 tmpfiles 创建，因为它必须是原子符号链接。

首次从旧单实例切换时，如果 `active` 尚不存在但旧 `state.sqlite3` 存在，发布器会在迁移成功后自动创建 `/var/lib/agent-vm-native/active -> slots/blue`。如果已经完成迁移而 `active` 缺失、断裂或指向蓝绿以外的路径，发布器拒绝猜测并保持失败围栏。

## Deployd 和观测

Deployd 是常驻服务进程，`agent-vm-deploy` 是它的本机 CLI。CLI 通过 `/run/agent-vm-native/deployd.sock` 提交事务和读取状态；Prometheus 不访问这个 socket，也不访问 Deployd SQLite 或槽位 snapshot。

Deployd 另外提供只读的 `GET /metrics`。默认监听回环地址；跨主机监听必须使用相互认证的 TLS 和网络访问控制。该监听不经过 Caddy，不提供业务 API，不触发采样或部署变更。

Prometheus 只配置一个 Deployd target：

```yaml
scrape_configs:
  - job_name: agent-vm-deployd
    metrics_path: /metrics
    static_configs:
      - targets: ["<部署方提供的 Deployd 指标地址>"]
```

Grafana 只查询 Prometheus。核心指标包括 `agent_vm_deployd_snapshot_fresh`、`agent_vm_deployd_slot_selected`、`agent_vm_deployd_slot_ready`、`agent_vm_deployd_slot_controller_owned`、`agent_vm_deployd_transaction_phase`、`agent_vm_deployd_recovery_required`、`agent_vm_deployd_observation_up` 和 `agent_vm_deployd_observation_fresh`。

所有槽位指标都带 `slot="blue|green"`；观测来源指标带 `source`。槽位内部的 `capacity`、`inventory` 和 `health` snapshot 分别写入 `/run/agent-vm-native/blue/metrics` 与 `/run/agent-vm-native/green/metrics`，这些目录只供 Deployd 采样，不能注册为 Prometheus 的第二个 target。

切换期间两个槽位可以同时活跃，但 Deployd 只能授予一个槽位 controller lease。Prometheus 通过 `slot_controller_owned` 验证唯一主权，通过 `slot_in_flight_requests`、`slot_pending_operations` 和 native worker 指标判断旧槽位是否已收敛。

## rqlite 配置

将 `rqlite-auth.json.example` 复制为受保护的 `/etc/agent-vm-native/rqlite/auth.json`，再由部署方填入 secret。rqlite unit 将 HTTP 和 Raft 绑定到回环地址，并启用 HTTPS 与 HTTP authentication。

每个槽位的 state URL、认证信息和 CA 信任路径必须由部署配置提供，不能写入 release manifest、公开 dashboard 或仓库测试 fixture。Deployd 迁移使用 `deployd.json` 中的 `migration.rqlite_url` 和 `migration.rqlite_unit`，不读取业务 SQLite 之外的隐藏配置。

## 一键首次部署

首次部署的输入是已经安装好的旧 `agent-vm-native.service`、旧 `/var/lib/agent-vm-native/state.sqlite3`、rqlite 配置、两个槽位配置和已验证的 format-4 发布包。发布器执行 `stage`、`verify`、`activate` 时，按以下顺序自动完成：

1. 校验发布包和两个槽位目录，但不启动新槽位。
2. 启用并重启 `agent-vm-deployd.service`，确认 Deployd socket 可用。
3. 调用 Deployd 的 `POST /v1/migrate`，由 Deployd 启动 rqlite、让旧服务进入写围栏、等待请求和 native worker 收敛、停止旧服务、验证 SQLite writer 已关闭、创建一致 snapshot、导入并验证 rqlite。
4. 迁移成功后写入 `.rqlite-cutover-complete`，再创建初始 `active` 链接、停止并禁用旧单实例、启动蓝绿槽位。
5. 重新观测两个槽位和共享状态，只有验证通过才报告激活成功。

发布器不会让槽位先于迁移启动，也不会把 `agent-vm-deploy migrate` 留给人工执行。直接使用 CLI 时，等价入口是：

```sh
sudo agent-vm-deploy migrate --timeout 9000 --json
```

如果 Deployd 返回带事务 ID 的 `state_unverified`，发布器会查询该事务；只有状态为 `needs_recovery` 时，才自动提交一次 `recover --action retry-migration`。重试会复用已验证的迁移 snapshot，并且在 rqlite 已经精确等于源 snapshot 时把丢失响应视为幂等成功；目标不一致时继续围栏，不猜测导入结果。

迁移完成标记使用格式 2，经过原子写入、文件同步和目录同步，并携带迁移事务 ID、源摘要、目标 rqlite 摘要、状态纪元和 snapshot 保留截止时间。Deployd 只有在标记、当前 rqlite 目标摘要和仍然保留的唯一 finalized 迁移事务三者一致时，才允许重复执行 `migrate` 返回已完成事务；任何一项缺失、不一致或目标已被重置，都保持围栏，不会再次猜测或替换业务数据库。

## 一次性状态迁移

状态迁移只在首次从旧 SQLite 状态切换到 rqlite 时发生，不属于普通蓝绿 `stage`、`activate` 或 `rollback`。正常蓝绿切换不迁移、恢复、替换或重建业务数据库。

迁移由一个 Deployd durable transaction 串行记录，阶段为 `migration_prepare`、`migration_drain`、`migration_quiescence`、`migration_stop`、`migration_source_check`、`migration_import` 和 `migration_finalize`。每个阶段转换都写入阶段事件和有限证据，事务 ID 会在失败响应中返回。

迁移的实际顺序固定为：先启动并确认 rqlite，再让旧服务进入 state-write fence；随后轮询 `drain-status` 和 `admin-metrics`，确认没有 pending、unknown、in-flight 或未收敛的 transition task；然后停止旧服务，立即取得旧状态目录的 `authority.lock` 并在整个检查、snapshot 和导入阶段保持持有，检查 systemd 状态和 `/proc` 文件描述符，确认没有进程打开 SQLite 主文件、WAL 或 SHM 文件。旧服务在这把锁释放前重新启动会使迁移失败闭锁，不会继续启动槽位。

只有 writer 检查通过后，Deployd 才使用 SQLite 自己的 `VACUUM INTO` 创建一致 snapshot，并记录 source hash、大小、state epoch、fence ID 和验证时间。单个源文件允许的最大大小是 1 GiB；没有人为设定的体积下限，空的小数据库也必须按实际大小处理。

导入由 Go state adapter 完成。目标 rqlite 必须为空或处于受支持的 pristine schema；导入使用一次 rqlite request-level transaction，完成后重新读取目标并比较完整 snapshot，同时生成目标摘要。源 SQLite 在迁移完成前不修改，迁移成功后才删除旧主文件及其 WAL/SHM，snapshot 按保留截止时间作为有限期恢复材料保留。

任一 fence、排空、停止、writer 检查、snapshot、导入、验证或状态持久化步骤失败，Deployd 都将旧服务保持停止或围栏状态，事务进入 `needs_recovery`，并拒绝启动蓝绿槽位。Deployd 重启时不会根据旧状态重建 controller role；它先重新执行 legacy fence，再等待显式恢复动作或发布器的受限 `retry-migration`。

## 普通蓝绿切换

普通切换只处理 release、槽位角色、流量、`active` 链接和 native quiescence：

```sh
sudo agent-vm-deploy stage --slot green --bundle <已验证的发布包>
sudo agent-vm-deploy verify --transaction <transaction-id>
sudo agent-vm-deploy activate --transaction <transaction-id>
sudo agent-vm-deploy status --transaction <transaction-id> --json
```

Deployd 启动候选槽位，验证 manifest、socket、就绪状态、状态能力和共享状态纪元；然后授予候选 controller lease，确认 readiness 后启用 traffic role，原子替换 `active`，让旧槽位进入 retiring 并排空。Deployd 必须重新观测旧槽位已停止、目标槽位仍持有唯一 controller lease 且继续接收流量，才能进入 finalize。

切换前失败保持当前槽位和 `active` 不变。切换后健康或 quiescence 失败只允许向上一个已验证槽位执行一次有界 rollback；结果不明确时保持 `needs_recovery` 并对 admission 和槽位主权执行 fencing，不能改写 state、generation 或 uncertain operation。

`agent_vm_cutover.sh --dry-run` 只用于验证隔离布局和 active-link guard，不是蓝绿事务权威，也不能证明 controller 交接、流量排空或 native quiescence。

## 恢复材料和清理

Deployd SQLite 与业务状态分离。Deployd 的终态事务、阶段事件和失败证据默认保留三十天；活动事务和 `needs_recovery` 事务不会清理。

一次性迁移 snapshot 位于 `/var/lib/agent-vm-native/migration/`，文件名以 `migration-` 开头、以 `.sqlite3` 结尾。迁移正在进行或需要恢复时，Deployd 不删除这些文件；迁移完成后从 finalized 时间开始保留七天，Deployd 每小时按完成标记中的保留截止时间清理过期 snapshot。唯一 finalized 迁移事务和目标摘要永久保留一条，不会随普通三十天历史清理删除，因此清理 snapshot 不会让完成状态失去可验证证据。

清理按实际文件逐个处理，不预分配空间，不设体积下限，也不允许单个迁移源超过 1 GiB。rqlite 的业务 backup/snapshot 不由蓝绿 Deployd 创建或清理，必须由业务状态后端自己的保留策略管理；不能把 rqlite backup 混入蓝绿槽位切换。

如果恢复材料目录无法检查、文件不是普通文件或清理失败，Deployd 记录告警但不删除不确定目标。任何仍被活动迁移事务引用的材料都必须保留。

## 隔离验收

重型验收必须在当前会话拥有的 Agent VM 中运行，不在开发机上运行：

```sh
AGENT_VM_REAL_SYSTEMD=1 bash skills/agent-vm-ops/scripts/agent_vm_deployment_acceptance.sh
```

验收必须证明两个槽位可以同时观测、同一时刻只有一个 controller lease、Deployd 只有一个 Prometheus pull 出口、迁移失败保持 `needs_recovery`、Deployd 重启不会猜测角色、普通切换能够执行有限 rollback，并且迁移 snapshot 会按保留期清理。

真实 Agent VM 验收还必须启动真实 `agent-vm-control` 槽位进程，复验 PID、启动令牌、scheduler fencing、active switch、旧槽位 quiescence、Deployd 重启恢复和 Prometheus 指标；静态检查不能替代这些运行证据。
