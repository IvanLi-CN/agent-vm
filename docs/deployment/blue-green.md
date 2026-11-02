# Agent VM 蓝绿部署资源

这些文件定义 native Go 控制服务和[Deployd 合同](../specs/agent-vm/contracts/deployment-controller.md)描述的槽位外 Deployd 边界。
它们是目标安装资源和隔离验收检查，不执行生产部署，不包含部署清单，也不保存凭据。
父服务集成仍必须在业务 SQLite 状态导入 rqlite 前提供真实的迁移闸门。

## 固定组件

权威的机器可读元数据是 [`deployment-components.json`](../../skills/agent-vm-ops/assets/blue-green/deployment-components.json)。
只允许使用该文件中的官方 GitHub release URL。

| 组件 | 版本 | 运行时职责 | 支持的产物 |
| --- | --- | --- | --- |
| Caddy | 2.11.7 | HTTPS edge proxy | 已验证 SHA-512 的 Linux amd64 和 arm64 tarball |
| rqlite | 10.5.2 | 单节点状态权威 | 已验证 SHA-256 的 Linux amd64 和 arm64 tarball |

自动安装已关闭。
运维者必须先验证固定 checksum，才能把任一二进制文件放置到 `/usr/local/bin/caddy` 或 `/usr/local/bin/rqlited`。

## 资源布局

`skills/agent-vm-ops/assets/systemd/` 中的 systemd unit 使用以下布局：

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
  bin/agent-vm-deployd
/var/lib/agent-vm-native/
  active -> slots/{blue,green}
  slots/{blue,green}/bin/agent-vm-control
  slots/{blue,green}/release-manifest.json
  slots/{blue,green}/app.sock -> /run/agent-vm-native/{blue,green}/app.sock
  deployment/deployment.sqlite3
  rqlite/
/run/agent-vm-native/{blue,green}/app.sock
/run/agent-vm-native/{blue,green}/metrics/
/run/agent-vm-native/deployd.sock
```

`agent-vm-native@.service` 为每个槽位启动一个 Go 服务，并把该槽位目录中的 manifest 作为该服务的校验输入。
Deployd 将候选 format-2 bundle 中经过完整清单校验的文件原子暂存到对应的 `/var/lib/agent-vm-native/slots/<slot>` 目录，然后才启动该槽位。
服务继续使用现有 Incus 控制边界和 `agent-vm-web` socket 组。
`agent-vm-deployd.service` 是槽位之外、由 root 拥有的宿主级控制器；它不是边缘代理、业务 API 或 rqlite 写入者。
Deployd unit 不会因为自身重启而自动拉起槽位；槽位的启动、停止和切换由
Deployd 在已验证的事务阶段中显式协调。
Deployd 还从部署配置读取一个仅供 Prometheus 访问的内部管理监听地址，并在该地址提供只读 `GET /metrics`。
该监听不经过 Caddy，不接收公网业务流量，也不替代 deployd 的 Unix socket。
`agent-vm-caddy.service` 是唯一的边缘代理服务单元。
已退役的 `agent-vm-web.service` 不属于蓝绿路径。

启动 unit 前，使用随附的 `sysusers.d` 和 `tmpfiles.d` 条目运行 `systemd-sysusers` 与 `systemd-tmpfiles --create`。
这些条目会创建专用的 rqlite 用户和非特权 `caddy` 用户。
`active` 路径不会由 tmpfiles 创建，因为它必须是原子符号链接，而不是目录。

## Deployd

将 `agent-vm-deployd` 安装为稳定的宿主级 systemd 服务，并使用受保护的 `/run/agent-vm-native/deployd.sock`。
配套的 `agent-vm-deploy` CLI 是唯一面向运维者的事务客户端，提供只读 `status`、`plan`、`verify`，以及授权的 `stage`、`activate`、`rollback` 和 `recover`。
Deployd 在另一个内部管理监听上提供 `GET /metrics`，供 Prometheus 以 pull 方式采集。
Prometheus 的 scrape job 使用 `agent-vm-deployd`；Grafana 只通过 Prometheus 查询，不访问 deployd socket、槽位 socket、SQLite 或 snapshot 文件。
该监听不暴露公共业务 TCP 端点，不写业务状态或 rqlite，也不替代 Caddy。

Deployd 观测原子 `active` 链接、两个槽位的 systemd 服务单元和 socket、槽位就绪状态/状态能力、控制器主权、流量角色和在途计数器、native worker/pending-operation 状态以及 release manifest。

槽位进程启动时默认保持 `traffic_accepting=false`；只有 Deployd 在完成新鲜观测并授予 controller role 后，才能为当前 `active` 槽位启用流量角色。
Deployd 重启或槽位进程重启后必须重新完成这条恢复路径，不能根据旧的本地状态直接恢复流量或 controller role。
它的独立 SQLite 数据库保存事务阶段、阶段事件、失败证据和最新观测快照，不保存每次轮询，也不成为运行时来源的权威。
Deployd 的 `/metrics` 只输出最近一次已完成的组合快照，不在 scrape 请求中触发采样或部署变更。
所有槽位级指标都必须带 `slot` 标签；切换期间两个槽位可以同时存活，但最终 Prometheus target 只有 deployd 这一份出口。
蓝色和绿色槽位必须把内部 `capacity`、`inventory` 和 `health` snapshot 写入各自的 `/run/agent-vm-native/{slot}/metrics` 目录。
不得让两个槽位共用一个 metrics 目录，否则旧槽位和候选槽位可能相互覆盖 snapshot。
切换期间两个槽位进程可以同时活跃；deployd 一次只授予一个槽位运行 scheduler 和 scheduler-driven native mutation 的权限。
槽位必须先取得该角色，才能运行 startup reconciliation 或启用 timer/reconcile/GC 循环；角色丢失时必须先 fence 这些循环。
之后 deployd 排空旧槽位，确认在途请求、pending operation、native worker 和 scheduler work 都已收敛，再停止旧槽位。
只有重新观测到旧槽位已经停止、目标槽位仍保持 controller role 和 traffic role，事务才能进入 finalize。

Prometheus 只配置一个 `agent-vm-deployd` scrape job，目标是部署配置提供的 Deployd `/metrics` 地址。
Grafana 只通过 Prometheus 查询，不读取 Deployd socket、部署 SQLite、槽位 snapshot 或 Caddy 日志。
切换期间的核心查询是 `agent_vm_deployd_snapshot_fresh`、`agent_vm_deployd_slot_selected`、`agent_vm_deployd_slot_ready`、`agent_vm_deployd_slot_controller_owned`、`agent_vm_deployd_transaction_phase` 和 `agent_vm_deployd_recovery_required`。
来源故障通过 `agent_vm_deployd_observation_up` 与 `agent_vm_deployd_observation_fresh` 区分；前者表示来源结果，后者表示结果是否仍然新鲜。
旧槽位是否能够停止，则结合 `agent_vm_deployd_slot_in_flight_requests`、`agent_vm_deployd_slot_pending_operations` 和对应的 `slot` 标签判断。
槽位 metrics 目录只供 Deployd 采样，不能作为第二个 Prometheus target。

## CA 与 TLS

在空的、由运维者拥有的目录中创建部署专用 CA：

```sh
bash skills/agent-vm-ops/scripts/generate_deployment_ca.sh \
  --output-dir /var/lib/agent-vm-native/bootstrap-ca \
  --hostname <operator-provided-hostname>
```

将 `ca.crt`、Caddy leaf certificate/key pair 和 rqlite leaf certificate/key pair 安装到 unit 指定的路径。
所有私钥必须放在仓库之外，每个 leaf key 分配给对应的服务用户（`caddy` 或 `agent-vm-rqlite`），权限使用 `0600`。
只有在 Go 控制服务必须验证 rqlite HTTPS endpoint 时，才把 CA 安装到宿主机 trust store。
部署 hostname 是运维输入，按设计不出现在本仓库中。

Caddy 示例会关闭 admin API 和自动签发证书，从日志中移除 query value 与敏感 header，在 edge 阻断 `/internal/v1/*`，并把公共请求转发到活动 Unix socket。
它不会添加 identity header，也不会关闭 upstream TLS verification。

## rqlite 边界

将 `rqlite-auth.json.example` 复制到受保护的 `/etc/agent-vm-native/rqlite/auth.json` 路径，并用运维者管理的 secret 替换两个占位符。
Control user 只能使用 state adapter 所需的 SQL 权限。
Backup user 不能执行 SQL。
rqlite unit 将 HTTP 和 raft 绑定到 loopback，并启用 HTTPS 与 HTTP authentication。

在 client transport 已配置为信任部署 CA 后，每个槽位配置中的 state URL 可以使用运维者管理的 HTTPS URL 和凭据。
不得把该 URL、凭据或 CA 私钥写入 release manifest 或源文件。

## 受控蓝绿切换

蓝绿事务只负责服务槽位、控制器主权、流量角色、`active` 链接和 native quiescence。
它不执行业务状态迁移、rqlite backup restore、业务数据库替换或状态后端 endpoint 切换；这些属于独立的状态后端迁移或灾难恢复流程。

部署事务通过 deployd 执行，不能用 shell 辅助脚本直接替换 `active`：

```sh
sudo agent-vm-deploy stage --slot green --bundle <verified-bundle>
sudo agent-vm-deploy verify --transaction <transaction-id>
sudo agent-vm-deploy activate --transaction <transaction-id>
sudo agent-vm-deploy status --transaction <transaction-id> --json
```

Deployd 启动候选槽位，验证 release manifest、socket、就绪状态、状态能力和共享状态纪元，将控制器主权交给候选槽位，启用其流量角色，原子替换受保护的 `active` 链接，并将旧槽位置为 `retiring`。
随后等待流量排空、pending operation、native worker 和 scheduler work 达到可验证的收敛状态，停止旧槽位，并重新观测停止结果后再 finalize。
切换前检查失败时保持当前槽位和 active 链接不变。
切换后失败时允许通过 deployd 向上一个兼容槽位执行一次有界 rollback；结果不明确时保持 `needs_recovery` 并 fence admission。

Deployd 重启后的 recovery finalize 也必须遵守同一顺序：先撤销旧槽位角色并停止旧槽位，再重新授予目标槽位角色并恢复流量。
任一撤销、停止或观测步骤失败，都保持 `needs_recovery` 并对两个槽位执行 hard-fence。

`agent_vm_cutover.sh --dry-run` 仍可用于隔离布局和 active-link guard，但它不是蓝绿事务权威，不能用它证明控制器交接、流量排空或 native quiescence。

## 状态迁移与恢复

本节描述一次性的状态后端迁移和独立的状态恢复门禁，不属于蓝绿 `stage`、`activate` 或 `rollback`。
蓝绿事务不替换业务数据库；迁移或灾难恢复期间必须先让所有槽位保持 fenced，再由独立的状态流程处理。

迁移是单向且由证据门控的 operation：

1. Service coordinator 使用 `state_write_fenced`、`workers_quiescent` 和 `sqlite_writers_closed` 记录签名的规范证据 payload，同时记录精确的 source hash/size、state epoch/generation、trusted commit、fence identity 和短有效期。
   私钥由 service coordinator 持有；该 helper 只接受运维者安装的公钥。
2. 运维者对仓库外的普通 SQLite 文件运行 `agent_vm_state_migration.ts preflight`。
   该命令执行 integrity check，记录 schema/table count，拒绝未解决的 management operation，并写出 mode-0600 snapshot 和 manifest。
3. Go state service 负责最终的 rqlite import；在接受新的权威前，必须验证 schema、`state_epoch`、generation 单调性、有效 VM row 和 operation journal。
4. 在该 coordinator 和 deployment fence 存在前，导出的 Go migration 入口保持 fail-closed。
   不得使用 shell、Python、rqlite CLI 或直接 HTTP request 导入 row。

使用运维者提供的证据执行 preflight 示例：

```sh
bun skills/agent-vm-ops/scripts/agent_vm_state_migration.ts preflight \
  --source /var/lib/agent-vm-native/state.sqlite3 \
  --evidence /var/lib/agent-vm-native/migration/fence-evidence.json \
  --evidence-public-key /etc/agent-vm-native/migration/fence.pub \
  --output-dir /var/lib/agent-vm-native/migration/snapshot
```

restore check 只有在 backup hash 与 manifest 和单独签名的 tail-evidence file 匹配、backup 证明 SQLite writer 已关闭，并且 tail 证明 trusted-commit、state-epoch/generation、fence 和 coverage-boundary 精确匹配时，才接受 snapshot。
它会生成 authorization record，但绝不写入 rqlite。
如果 tail 无法验证，保持服务 fenced，并从 rqlite 权威或运维者批准的恢复流程执行 reconciliation。

### 恢复材料的删除

迁移源 SQLite snapshot/export、rqlite backup/snapshot 和 deployd `deployment.sqlite3` recovery copy（如有）都不属于蓝绿槽位产物。
蓝绿 `stage`、`activate` 和 `rollback` 不会因为切换成功而删除它们。

恢复材料默认最多保留三个已验证恢复点，超过三十天的恢复点进入清理候选，总大小硬上限为 1 GiB。
1 GiB 按实际文件大小计算，不是预分配空间，也不存在体积下限。
最后两个可验证恢复点始终保留；迁移源 SQLite snapshot 在迁移完成并通过新的 rqlite restore check 后保留七天。

只有在状态后端已恢复、隔离 restore check 已通过、没有 pending/unknown operation 或未解决的 `needs_recovery`、迁移源保留期已到期，并且清理后仍保留两个可验证恢复点时，材料才可以进入 `purge_eligible`。
状态迁移或灾难恢复流程的 retention worker 在每次成功备份后和每小时执行清理；该 worker 不属于蓝绿 Deployd，也不读取 Deployd socket。
删除必须针对准确的材料 ID 并写入审计；预算仍不足时报告 `backup_storage_exhausted`，拒绝新的备份或状态迁移，不删除受保护恢复点。
状态数据库中的终态操作和审计历史默认保留三十天后回收，不能依靠放大 1 GiB 预算掩盖数据库自身的无界增长。

## 隔离验收

在当前会话拥有的 Agent VM 中运行以下命令，不要在开发机上运行：

```sh
AGENT_VM_REAL_SYSTEMD=1 bash skills/agent-vm-ops/scripts/agent_vm_deployment_acceptance.sh
```

该检查执行固定的 amd64 产物，并验证固定的 arm64 rqlite checksum，但不执行异构架构二进制文件。
它还会验证 Caddyfile、Caddy/rqlite/deployd 和双槽位 systemd 合同，创建临时 CA，启动仅监听回环地址且启用认证 TLS 的 rqlite 节点，运行 deployd 切换状态机和旧版 active-link guard，并证明签名迁移闸门拒绝与恢复准入。
临时数据库、凭据、证书、日志、签名密钥、公钥和证据会在退出时删除，除非在隔离 VM 内显式设置 `KEEP_PROOF_DIR=1`。
