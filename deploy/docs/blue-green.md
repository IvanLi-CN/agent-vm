# Agent VM 蓝绿部署

本文是 Agent VM 蓝绿部署的实施与运维主文档。它定义槽位布局、Deployd、一次性状态迁移、Prometheus 观测、失败恢复和清理规则；长期需求与 API 字段分别由 [Agent VM SPEC](../../docs/specs/agent-vm/SPEC.md) 及其合同约束。

这些资源只描述目标安装和隔离验收，不包含生产地址、凭据、证书私钥或业务数据。生产首次部署必须由发布器完成完整的一键激活流程，不能把状态迁移拆成发布后的人工前置步骤。

## 固定组件

权威的机器可读组件清单是 [`deployment-components.json`](../assets/blue-green/deployment-components.json)。只允许使用清单中的官方发布地址，并在安装前验证固定校验和。

| 组件 | 版本 | 运行时职责 | 支持的产物 |
| --- | --- | --- | --- |
| Caddy | 2.11.7 | HTTPS 边缘代理 | 已验证 SHA-512 的 Linux amd64 和 arm64 压缩包 |
| rqlite | 10.5.2 | 共享业务状态权威 | 已验证 SHA-256 的 Linux amd64 和 arm64 压缩包 |

默认预检不会下载或安装组件。全新安装显式使用 `agent-vm-deploy install --apply` 后，安装器按固定清单下载并验证组件，再安装到 `/usr/local/bin/caddy` 和 `/usr/local/bin/rqlited`；操作步骤见[首次安装](install.md)。

## 资源布局

`deploy/assets/systemd/` 中的 unit 使用以下布局：

```text
/etc/agent-vm-native/
  Caddyfile
  caddy.env
  rqlite/auth.json
  backup/{auth.json,ca.crt}
  tls/caddy/{server.crt,server.key}
  tls/rqlite/{server.crt,server.key}
  deployd.json
  slots/{blue,green}/config.json
/opt/agent-vm-native/
  release-manifest.json
  bin/{agent-vm-server,agent-vm-deploy}
/var/lib/agent-vm-native/
  active -> slots/{blue,green}
  slots/{blue,green}/bin/agent-vm-server
  slots/{blue,green}/release-manifest.json
  deployment/deployment.sqlite3
  migration/.rqlite-cutover-complete
  rqlite/
/run/agent-vm-native/{blue,green}/app.sock
/run/agent-vm-native/{blue,green}/metrics/
/run/agent-vm-native/deployd.sock
```

`agent-vm-native@.service` 为每个槽位启动一个 Go 服务，并使用该槽位目录中的 manifest。两个槽位可以同时存活并接受只读观测，但没有有效 controller lease 的槽位不得运行 startup reconcile、timer reconcile、age GC 或任何 scheduler-driven native mutation。

`agent-vm-deployd.service` 位于两个槽位之外，是稳定的宿主级部署控制器，不通过 `Wants=` 拉起蓝、绿槽位。槽位由部署事务显式启动，或由已启用的 systemd 开机配置启动；`After=` 只约束已经排队的启动任务，不会创建槽位启动任务。槽位的 `ExecStartPost` 使用 `agent-vm-deploy wait-process` 等待只读进程接口，接受尚无 controller lease 的 `process_ready=true`，不等待业务 readiness；冷启动时 Deployd 在这一步完成后才检查槽位，避免误停尚未创建 socket 的进程。首次迁移仍先停止旧 SQLite writer，再由事务启动新槽位，不会自动启动额外 writer。

全新安装保留 `/var/lib/agent-vm-native/active/app.sock` 作为 Caddy 和请求适配器的入口。公开的 `active` 链接由 root 持有，指向 `routing/active`；部署事务在 `routing` 内原子更新槽位选择。历史安装的直接链接仍可识别。专用部署账户下由 systemd 持有 `/run/agent-vm-native/deployd.sock`，部署服务重启不会替换监听路径，root 运维 CLI 继续使用原路径。

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

槽位观测通过 Incus 的只读 `/1.0/metrics?project=…` 获取该项目的容器 CPU、内存、磁盘和网络指标；采样超时或解析失败时输出 `agent_vm_incus_metrics_up=0`，不伪造零值，也不影响 controller lease。Deployd 仅从活动槽位的一份新鲜快照输出共享的 `incus_*` 指标，保留 Incus 原有标签，不添加槽位标签；槽位自身的 `agent_vm_*` 指标仍区分 `slot`，因此 Prometheus 仍只有一个 target，切换时不会重复统计容器。

Grafana 只查询 Prometheus。核心指标包括 `agent_vm_deployd_snapshot_fresh`、`agent_vm_deployd_slot_selected`、`agent_vm_deployd_slot_ready`、`agent_vm_deployd_slot_controller_owned`、`agent_vm_deployd_transaction_phase`、`agent_vm_deployd_recovery_required`、`agent_vm_deployd_observation_up` 和 `agent_vm_deployd_observation_fresh`。

所有槽位指标都带 `slot="blue|green"`；观测来源指标带 `source`。槽位内部的 `capacity`、`inventory` 和 `health` snapshot 分别写入 `/run/agent-vm-native/blue/metrics` 与 `/run/agent-vm-native/green/metrics`，这些目录只供 Deployd 采样，不能注册为 Prometheus 的第二个 target。

切换期间两个槽位可以同时活跃，但 Deployd 只能授予一个槽位 controller lease。Prometheus 通过 `slot_controller_owned` 验证唯一主权，通过 `slot_in_flight_requests`、`slot_pending_operations` 和 native worker 指标判断旧槽位是否已收敛。

## rqlite 配置

将 `rqlite-auth.json.example` 复制为受保护的 `/etc/agent-vm-native/rqlite/auth.json`，再由部署方填入 secret。rqlite unit 将 HTTP 和 Raft 绑定到回环地址，并启用 HTTPS 与 HTTP authentication。

恢复点客户端只读取 `migration.rqlite_backup_auth_file` 和 `migration.rqlite_backup_ca_file`；独立凭据文件只包含 `agent-vm-backup` 的密码，由安装器从认证输入导出。备份账号必须有 `backup` 和 `query`，不能有 `execute`，且不能与业务账号复用密码。旧 SQLite 导入仍使用其受保护的原有认证配置。

每个槽位的 state URL、认证信息和 CA 信任路径必须由部署配置提供，不能写入 release manifest、公开 dashboard 或仓库测试 fixture。Deployd 迁移使用 `deployd.json` 中的 `migration.rqlite_url` 和 `migration.rqlite_unit`，不读取业务 SQLite 之外的隐藏配置。

## 首次安装与已有环境

全新环境使用 Go CLI 的 `install`，按[首次安装步骤](install.md)准备受保护输入，先只读预检，再显式加 `--apply` 安装完整服务栈。安装器通过业务状态层直接初始化空 rqlite，不创建临时业务 SQLite。已有蓝绿安装只核验并指向本手册的升级入口；安装器不覆盖配置、不自动排空或重启有效槽位。

只有确实存在旧业务 SQLite 的环境才委托既有发布封装及 Deployd 的 `migrate` 事务，具体发布器输入和操作见[兼容发布说明](legacy-release.md)。`migrate` 不负责安装完整服务栈，不能代替 `install`，也不属于普通蓝绿升级。迁移结果不确定时查询原事务并按迁移恢复合同处理，不能删除记录后重试初始化。

## 一次性状态迁移

状态迁移只在首次从旧 SQLite 状态切换到 rqlite 时发生，不属于普通蓝绿 `stage`、`activate` 或 `rollback`。正常蓝绿切换不迁移、恢复、替换或重建业务数据库。

迁移由一个 Deployd durable transaction 串行记录，阶段为 `migration_prepare`、`migration_drain`、`migration_quiescence`、`migration_stop`、`migration_source_check`、`migration_import` 和 `migration_finalize`。每个阶段转换都写入阶段事件和有限证据，事务 ID 会在失败响应中返回。

迁移的实际顺序固定为：先启动并确认 rqlite，再让旧服务进入 state-write fence；随后轮询 `drain-status` 和 `admin-metrics`，确认没有 pending、unknown、in-flight 或未收敛的 transition task；然后停止旧服务，立即取得旧状态目录的 `authority.lock` 并在整个检查、snapshot 和导入阶段保持持有，检查 systemd 状态和 `/proc` 文件描述符，确认没有进程打开 SQLite 主文件、WAL 或 SHM 文件。旧服务的 Bun 入口在创建 SQLite store 前也必须取得同一把排他的 `authority.lock`，因此它在这把锁被 Deployd 持有时不能重新打开数据库。旧服务在这把锁释放前重新启动会使迁移失败闭锁，不会继续启动槽位。

迁移开始前，Deployd 会为仍作为 SQLite writer 的 legacy singleton 临时授予蓝槽位 controller lease，然后才执行 `drain-enter`。这个 lease 只用于通过旧服务已有的写入围栏，不授予流量角色，也不把 legacy singleton 纳入正常的双槽位运行；迁移停止 legacy singleton 后，该 lease 随进程消失，发布器才会启动蓝、绿两个 rqlite 槽位。全新安装不走该路径，也不创建 legacy SQLite。

只有 writer 检查通过后，Deployd 才使用 SQLite 自己的 `VACUUM INTO` 创建一致 snapshot，并记录 source hash、大小、state epoch、fence ID 和验证时间。迁移预算按 SQLite 主库、WAL、SHM 和恢复目录中已登记的 snapshot 文件累计计算，累计上限为 1 GiB；没有人为设定的体积下限，空的小数据库也必须按实际大小处理。snapshot 生成后还会再次检查实际大小，超限材料会被清理并拒绝迁移。

导入由 Go state adapter 完成。目标 rqlite 必须为空或处于受支持的 pristine schema；导入使用一次 rqlite request-level transaction，完成后重新读取目标并比较完整 snapshot，同时生成目标摘要。源 SQLite 在迁移完成前不修改，迁移成功后才删除旧主文件及其 WAL/SHM，snapshot 按保留截止时间作为有限期恢复材料保留。

任一 fence、排空、停止、writer 检查、snapshot、导入、验证或状态持久化步骤失败，Deployd 都将旧服务保持停止或围栏状态，事务进入 `needs_recovery`，并拒绝启动蓝绿槽位。Deployd 重启时不会根据旧状态重建 controller role；它先重新执行 legacy fence，再等待显式恢复动作或发布器的受限 `retry-migration`。自动重试的额度先记录为 `in_progress`，因此 Deployd 在实际导入前退出时可以恢复同一个逻辑尝试；只有导入明确失败后才标记为 `exhausted`。

## 普通蓝绿切换

账户迁移会在准备候选槽时先启用 root 专用 SSH 请求入口 `/run/agent-vm-native/control.sock`。该入口按活跃槽的真实 UID 转发，所以迁移前的 root 槽、新账户槽和回退后的历史槽使用同一 protocol 4。固定请求适配器及历史包无需改写。若 SSH 连接资料曾显式使用 `/var/lib/agent-vm-native/active/app.sock`，须在新入口已就绪、现役槽仍在服务时将 `remote_socket` 改为 `/run/agent-vm-native/control.sock`，并用普通 `status` 请求核验；只看 `doctor` 或部署健康检查不能证明业务请求权限正确。保留原连接资料用于维护恢复，不放宽槽位对 root 或部署账户的业务授权。

转发入口不更改 operation ID、generation 或请求正文；读取或传输失败时不重发。客户端继续按原 operation ID 查询不确定的变更结果。回退验证必须包含 root SSH 普通业务请求，而非仅以服务账户直接访问槽位。

目标宿主须用 `agent-vm-deploy` 专用账户运行部署服务，并为两个槽位配置 `agent-vm-server` 专用账户。旧安装仍以 root 运行时，普通 `update` 会在获取产物或修改槽位前拒绝。不要用底层阶段命令绕过账户迁移，也不要在旧槽仍以 root 运行时把该槽的程序目录交给常驻账户写入。

对已有安装，可使用同一正式版本的 `agent-vm-deploy install --config <输入> --bundle <产品包> --materials <资料目录> --check-account-migration` 进行只读预检。预检返回候选包摘要、现役槽位和上一版本源码修订；它要求现役槽健康且独占控制权、没有未完成部署事务、现场程序与清单一致，并且候选沿用同一 rqlite 和 Incus 环境。预检不能与 `--apply` 一起使用，通过预检也不能替代旧版在专用账户下的隔离回退验证。

候选槽的配置输入须与该槽宿主上已安装的配置逐字节一致；账户迁移只调整服务账户的读取权限。预检还核对输入中的专用备份身份与宿主已安装的 rqlite 认证配置一致，并用提供的 TLS 证书对现役 rqlite 执行一次线性一致的只读查询。这一步不创建恢复点，也不变更 rqlite 权限。

账户迁移为两槽配置私有挂载，使任一槽运行旧版时都能使用同一个共享锁目录。预检要求两个槽的 Incus 存储路径及已有锁目录均由 root 保护，路径中不能有符号链接。部署不授予主服务对 Incus 存储池的写权限。

只有在隔离 Linux 主机上用相同产物完成账户迁移、连续登录和业务读写检查，并证明原版本能以专用账户回退后，才将该次验收的身份记录作为宿主维护输入。记录是 root 持有的 JSON 文件，`schema_version` 为 1；`previous_source_revision`、`previous_release_id`、`candidate_source_revision`、`candidate_bundle_sha256`、`materials_manifest_sha256` 分别绑定验收使用的版本和产物。不要用生产预检结果代替隔离验收。

```sh
agent-vm-deploy install --config <输入> --bundle <正式产品包> \
  --materials <资料目录> --apply --account-migration-proof <验收记录>
```

命令先核对正式发布和验收记录，再使用现有蓝绿事务准备并激活候选槽。命令返回事务 ID 后如果报错，先用 `agent-vm-deploy transaction --transaction <ID>` 检查原事务和当前槽位，避免重新发起切换。只有部署控制服务已用专用账户接管并确认新槽持有原状态权威，命令才报告迁移完成。

候选包的 `deployment_contract` major 在 stage 前由部署控制服务预检；缺失字段按 legacy v1 兼容，major 不兼容时不创建事务、不启动候选。普通更新和后续事务操作都使用部署控制服务对应的 CLI：全新安装使用 `/opt/agent-vm-native/bin/agent-vm-deploy`；完成旧安装账户迁移后使用 `/opt/agent-vm-native/maintenance/agent-vm-deploy`。以下命令在控制宿主执行，先按安装方式设置 `deploy_cli`。`agent-vm-deploy update` 是完整蓝绿 release transaction，并自行协调 stage、controller handoff 与 drain；不得在它之前另行进入维护 drain。若已有独立维护 drain owner，必须先由该原 owner 安全退出且保持 traffic 关闭，否则 release transaction 会发生 ownership conflict。默认由控制宿主下载并核对当前架构的正式发布包：

```sh
deploy_cli=/opt/agent-vm-native/bin/agent-vm-deploy
# 完成旧安装账户迁移后，改用：
# deploy_cli=/opt/agent-vm-native/maintenance/agent-vm-deploy
sudo "$deploy_cli" update --version '<semver>' --source-revision '<40-char-source-sha>'
```

需要指定文件时，在控制宿主的受保护目录放置正式完整包，并给同一命令添加 `--bundle /protected/path/release.tar`。若同时使用 `--server-file /protected/path/agent-vm-server`，该文件须与包内产物字节一致。两种输入都核对正式发布摘要、版本、源码和架构。使用[发布合同](../../docs/specs/agent-vm/contracts/release.md)中的恢复点门禁；收到 `backup_storage_exhausted` 时现役槽继续服务，不绕过门禁切换或回退。

恢复或诊断时仍可直接查询原事务并执行受限阶段命令：

```sh
sudo "$deploy_cli" stage --slot green --bundle '<已验证的发布包>'
sudo "$deploy_cli" verify --transaction '<transaction-id>'
sudo "$deploy_cli" activate --transaction '<transaction-id>'
sudo "$deploy_cli" transaction --transaction '<transaction-id>' --json
```

Deployd 启动候选槽位，验证 manifest、socket、就绪状态、状态能力和阶段证据绑定的 `state_authority`；然后授予候选 controller lease，确认 readiness 后启用 traffic role，原子替换 `active`，让旧槽位进入 retiring 并排空。Deployd 必须重新观测旧槽位已停止、目标槽位仍持有唯一 controller lease 且继续接收流量，才能进入 finalize。

切换在发布新的 `active` 链接前，必须先成功启用所选槽位的 systemd 开机启动。冷启动可以同时启动已启用的两个槽位，但它们均从 fencing 状态启动；Deployd 重新观测产物、进程和共享状态后，才向当前 active 槽位授予新 lease，不能使用旧部署库记录恢复角色。开机启动配置不授予 controller 或 traffic。

切换前失败保持当前槽位和 `active` 不变。切换后健康或 quiescence 失败只允许向上一个已验证槽位执行一次有界 rollback；结果不明确时保持 `needs_recovery` 并对 admission 和槽位主权执行 fencing，不能改写 state、generation 或 uncertain operation。

查询原事务，确认存在已验证的上一槽位且尚未消耗回滚额度后，可以显式执行一次有界回滚；回滚只切换发布和角色，不回退业务数据库：

```sh
sudo "$deploy_cli" transaction --transaction '<原事务ID>' --json
sudo "$deploy_cli" rollback --transaction '<原事务ID>'
sudo "$deploy_cli" status --json
```

回滚先在后台启动上一版本，现役槽继续接流。上一版本能接手后，部署控制服务交接唯一控制权和流量，再排空并停止原现役进程。上一版本首启加两次重试均失败，且现役槽仍经确认持有唯一控制权并正常接流时，本次回滚以 `rollback_previous_unavailable` 结束，现役槽继续服务。启动次数写入原部署事务；部署控制服务重启不会清零，也不会尝试更早的版本。

事务进入 `needs_recovery` 时，普通 `activate` 或 `rollback` 不代替恢复动作。保留原 ID，选择回到已验证上一槽位的恢复：

```sh
sudo "$deploy_cli" recover --transaction '<原事务ID>' --action rollback
sudo "$deploy_cli" transaction --transaction '<原事务ID>' --json
```

只有 active 已切到原候选且候选产物、状态权威、进程与旧槽静止证据均可重新验证时，才可选择 `recover --transaction <原事务ID> --action finalize` 完成原切换。active 尚不存在的首次初始化使用[首次安装恢复](install.md)中的 `resume-bootstrap`，不能互换。证据不足时保持 fencing，不得手工改写业务状态或部署事务。

如果原 transaction 仍在 `stage` phase 且已有 `stage_effect_started` 与可验证 recovery point，可用受限 `retry-stage` 复用原 transaction/bundle/recovery point；该动作只重新 stage 并 verify，成功后停在 `controller_handoff`，不恢复 snapshot、不切换 active、不授予 traffic。确认原 transaction 为 active/controller_handoff 后，再显式 activate 同一 transaction ID：

```sh
sudo "$deploy_cli" recover --transaction '<原事务ID>' --action retry-stage
sudo "$deploy_cli" transaction --transaction '<原事务ID>' --json
sudo "$deploy_cli" activate --transaction '<原事务ID>'
```

如果上一槽位的旧 release 已经无法读取当前数据库能力集合、重新启动必然失败，原事务只能用受限 forward 恢复继续：确认当前 `active` 槽位已停止且两个槽位都不持有 controller 或 traffic role 后，用同一 transaction ID 和正式完整包执行 `complete-forward`。该动作会重新 stage、verify 原目标槽位，授予唯一 controller，切换 `active`，然后 finalize 原事务；它不恢复业务数据库，也不接受其他目标槽位。

```sh
sudo "$deploy_cli" recover --transaction '<原事务ID>' --action complete-forward --bundle /protected/path/release.tar
sudo "$deploy_cli" transaction --transaction '<原事务ID>' --json
```

若活动槽位仍有另一 `release-*` drain owner，先由该原 owner 使用保留当前 traffic role 的路径退出；不得以新 transaction 接管该 owner。`retry-stage` 的前置观测、恢复点复用和 fencing 约束见[Deployd 合同](../../docs/specs/agent-vm/contracts/deployment-controller.md#恢复与验证)。

如果 maintenance drain 尚未退出，原 owner 可通过固定 root request UDS 使用其同一 session 发送 drain-exit，并显式保留 traffic role；不得以普通 drain-exit 重新开放一个尚无 controller 的槽位：

```sh
printf '%s\n' '{"protocol":4,"action":"drain-exit","session_id":"<原release-maintenance-session>","operation_id":"drain-exit-preserve-<唯一ID>","generation":0,"preserve_traffic":true}' | \
  sudo /opt/agent-vm-native/bin/agent-vm-server request --socket /run/agent-vm-native/control.sock
```

确认响应 `ok=true`、`draining=false`，并重新读取 status 确认 `traffic_accepting=false`、`controller_slot=none` 后，才可执行 `retry-stage`。

`agent_vm_cutover.sh --dry-run` 只用于验证隔离布局和 active-link guard，不是蓝绿事务权威，也不能证明 controller 交接、流量排空或 native quiescence.

## 恢复材料和清理

Deployd SQLite 与业务状态分离。Deployd 的终态事务、阶段事件和失败证据默认保留三十天；活动事务和 `needs_recovery` 事务不会清理。

一次性迁移 snapshot 位于 `/var/lib/agent-vm-native/migration/`，文件名以 `migration-` 开头、以 `.sqlite3` 结尾。迁移正在进行或需要恢复时，Deployd 不删除这些文件；迁移完成标记先于 legacy SQLite 清理持久化，清理动作中途退出时会复用标记绑定的 rqlite 目标，不会重复导入或重新采样；迁移完成后从 finalized 时间开始保留七天，Deployd 每小时按完成标记中的保留截止时间清理过期 snapshot。唯一 finalized 迁移事务和目标摘要永久保留一条，不会随普通三十天历史清理删除，因此清理 snapshot 不会让完成状态失去可验证证据。Deployd 在创建 snapshot 前检查目录总预算和文件系统可用空间；临时 snapshot 失败会自动删除，不会留下占用预算的半成品。

清理按实际文件逐个处理，不预分配空间，不设体积下限，也不允许单个迁移源超过 1 GiB。rqlite 的业务 backup/snapshot 不由蓝绿 Deployd 创建或清理，必须由业务状态后端自己的保留策略管理；不能把 rqlite backup 混入蓝绿槽位切换。

如果恢复材料目录无法检查、文件不是普通文件或清理失败，Deployd 记录告警但不删除不确定目标。任何仍被活动迁移事务引用的材料都必须保留。

## 隔离验收

重型验收必须在当前会话拥有的 Agent VM 中运行，不在开发机上运行：

```sh
AGENT_VM_REAL_SYSTEMD=1 bash deploy/scripts/agent_vm_deployment_acceptance.sh
```

验收必须证明两个槽位可以同时观测、同一时刻只有一个 controller lease、Deployd 只有一个 Prometheus pull 出口、迁移失败保持 `needs_recovery`、Deployd 重启不会猜测角色、普通切换能够执行有限 rollback，并且迁移 snapshot 会按保留期清理。

真实 Agent VM 验收还必须启动真实 `agent-vm-server` 槽位进程，复验 PID、启动令牌、scheduler fencing、active switch、旧槽位 quiescence、Deployd 重启恢复和 Prometheus 指标；静态检查不能替代这些运行证据。

蓝绿校验使用去除凭据后计算的 `state_authority` 后端配置指纹和 `state_capabilities`，并在激活、恢复及回滚时复核。`state_epoch` 随业务写入和维护游标变化，仅用于单次快照一致性及诊断，不能要求两个槽位在不同采样时刻的值相等。配置指纹不能证明后端数据库未被替换；正常蓝绿流程禁止恢复、替换或重建业务数据库，也不会执行此类操作。缺少新验证证据的旧部署事务保持拒绝，不根据旧 epoch 猜测后端身份。

普通维护采集业务快照期间，Deployd 通过独立角色通道续租已在当前进程内绑定的同一份有效 controller lease；每次核验 active 链接、systemd 进程身份和另一槽位无 controller。它不从持久化快照重新授予租约，不改变租约身份，不延长业务观测的新鲜期；业务探测失败仍关闭流量准入。`status` 等只读请求不触发续租。角色通道失败、进程变化或租约过期仍立即 fencing。
