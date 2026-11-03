---
status: accepted
---

# Deployd 与蓝绿事务

本文是 Architecture Decision Record，只记录 Deployd 的决策背景、技术取舍与被否决方案。
规范设计和长期要求以 [Agent VM SPEC](../specs/agent-vm/SPEC.md) 的 `REQ-AGENT-VM-031` 及其关联合同为准；本文不替代该 SPEC。

Agent VM 的蓝绿部署需要在同一控制环境内持续观察两个并行槽位，并协调候选验证、控制器主权转移、活动路由切换、旧槽位排空、回滚和 daemon 重启恢复。
我们采用位于蓝绿槽位之外的 root-owned Go Deployd（`agent-vm-deployd`）作为唯一部署事务编排者，并提供使用受保护 Unix socket 的 `agent-vm-deploy` CLI；它不接收公网流量，也不成为业务状态或 rqlite 的第二权威。

## 决策

- `agent-vm-deployd.service` 是稳定的宿主级部署控制器，不随蓝绿槽位切换。
  蓝色和绿色槽位只提供受保护的 readiness、controller-role、traffic-role、请求计数和 native-operation 观测/控制合同。
- deployd 在受保护的 Unix socket 之外，提供仅限内部管理网络访问的只读 `GET /metrics` endpoint。
  Prometheus 直接拉取该 endpoint，使用 `job="agent-vm-deployd"`；Grafana 只查询 Prometheus，不读取 deployd 数据库或槽位 snapshot。
  该 endpoint 不经过 Caddy，不承载业务 API，也不触发新的采样或部署变更。
- deployd 使用独立的本机 SQLite 数据库存储部署事务、阶段事件、失败证据和最新观测快照；它不打开业务 `state.sqlite3`，不写 rqlite，也不保存业务资源状态。
  SQLite 在这里用于事务原子性、并发只读、索引查询和守护进程崩溃恢复；rqlite 可用性不决定 deployd 是否能记录本机部署故障。
- deployd 的事实来源是 `active` 符号链接、release manifest、systemd unit 状态、两个槽位 Unix socket、Go control API 返回的状态纪元/operation/native-worker 状态，以及槽位实际产生的 traffic counters。
  SQLite 只记录这些事实和状态机，不替代事实来源。
- 槽位的流量角色与控制器主权分离。
  候选可以以 `standby` 运行；只有持有同机 process-held controller lease 的槽位才能运行 scheduler、reconciliation、age GC 和 scheduler-driven native mutation。
  切换期间两个进程可以同时存活，旧槽位必须先进入 `retiring` 并排空，再允许停止。
- 正常事务顺序是 stage、standby validation、controller handoff、traffic enable、原子切换 `active`、旧槽位 drain、quiescence、finalize。
  任何 readiness、state capability、epoch、worker、请求排空或主权证据不确定时都 fail closed。

## 后果

- 部署工具不再通过 SSH 远程调用另一个部署主机，也不依赖一次性 shell helper 才能观察切换过程；CLI 是 daemon 的本机操作面。
- deployd 必须有独立的 systemd、受保护 Unix socket、部署 SQLite schema、事务恢复和权限测试；其自身升级不属于蓝绿槽位切换，避免控制器在切换过程中替换自己。
- 现有业务 state/rqlite 合同、protocol 4、generation、operation journal 和 unknown-outcome 语义保持不变。
  蓝绿激活要求两个槽位使用同一共享状态权威；状态迁移仍由 Go state service 负责，不由 deployd 导入数据。
- 蓝绿事务只切换服务槽位和 `active` 路由选择，不迁移、恢复、替换或重建 rqlite；状态后端迁移和灾难恢复必须由独立的、管理员授权的状态流程执行。
- 观测必须区分路由选择、进程存活、controller leadership、traffic draining、请求 in-flight 和 native worker quiescence；`active` 指针或 Caddy access log 单独不能证明实际 served-flow 已排空。

## 否决的替代方案

- **把 deployd 放进蓝绿槽位**：切换时会替换正在协调切换的控制器，导致事务恢复、自更新和版本兼容边界不清晰。
- **让两个槽位都运行完整 scheduler**：共享状态锁不能替代唯一 controller ownership，可能产生重复 reconcile、GC 或 native mutation。
- **让 Caddy 或 `active` 指针承担排空语义**：指针只表达路由选择，已有连接和请求仍可能由旧槽位完成。
- **把部署事务写入业务 rqlite**：部署控制器会与它正在协调的状态后端形成不必要耦合，且部署事务不属于业务状态；deployd 只写独立本机数据库。
- **使用 JSONL 作为权威事务状态**：它适合导出审计，但需要自行实现阶段原子性、并发查询、索引和崩溃恢复；部署状态机直接使用 SQLite 事务更可靠。
