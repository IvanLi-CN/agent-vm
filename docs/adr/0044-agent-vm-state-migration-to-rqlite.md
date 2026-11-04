---
status: accepted
---

# Agent VM SQLite 到 rqlite 的保守状态迁移

本文是 Architecture Decision Record，只记录一次性状态后端迁移和独立恢复边界的决策背景、技术取舍与被否决方案。
规范设计和长期要求以 [Agent VM SPEC](../specs/agent-vm/SPEC.md) 的 `REQ-AGENT-VM-028` 及其关联合同为准；
本文不替代该 SPEC。
本文不定义蓝绿服务槽位切换，但规定首次蓝绿部署必须如何自动完成状态后端切换。
蓝绿 `stage`、`activate` 和 `rollback` 不替换业务数据库；
它们要求两个槽位使用同一个已经验证的状态权威。

## 背景

现有 Agent VM 使用 `database/sql`、`modernc.org/sqlite`、protocol 4、generation、`state_epoch` 和 operation journal。
蓝绿服务需要两个版本共享同一份状态，但不能让两个槽位直接打开同一个 SQLite 文件，也不能通过双写制造两个状态权威。
rqlite 提供服务化的 SQLite 状态入口，但它的事务和读一致性语义与本地 `BEGIN IMMEDIATE` 不同，不能把旧 SQL 调用原样包在 HTTP 请求中。

## 决策

首次部署由槽位外的 `agent-vm-deployd` 通过 `POST /v1/migrate` 自动执行本 ADR 的一次性迁移流程。
发布器不得要求运维者先手工导出、导入或停止旧服务；
这些动作由 Deployd 以一个可恢复的 durable transaction 串行完成。
普通蓝绿 `stage`、`activate` 和 `rollback` 不调用该 API，也不触碰业务数据库。

迁移分为明确的单一权威阶段：

1. **SQLite 基线**：迁移前保持当前 SQLite schema、protocol-4 请求/响应、generation、`state_epoch`、journal、audit 和 unknown-outcome 语义。
   现有服务继续是唯一写入者。
2. **排空和导出**：迁移开始前进入 state-write fence，停止新的浏览器/API/Agent durable mutation 和 native admission，暂停 scheduler、reconciliation、timer 的 durable writes。
   已在途请求和 native worker 必须以原 operation ID 完成 durable result，并通过 worker PID 启动身份、进程组以及已知 Incus operation identity 验证每个 worker 已停止且静默；
   pending/unknown 结果、仍可运行的 worker 或无法证明 quiescence 的 native effect 都是迁移阻塞条件，不能只记录后继续。
   只有所有在途 state transaction 和 native worker 收敛、全部 SQLite 写入者关闭后，才能记录最终 `state_epoch`，只允许读和备份，再生成一致的 SQLite 备份与导出，并记录 schema、capability、行数、主键范围、generation/journal 摘要和受保护密钥版本。
3. **一次性导入**：目标 rqlite 先通过 schema/capability 预检，再使用 `POST /db/execute?transaction` 的单请求事务导入数据。
   不能在请求 SQL 中自行发送 `BEGIN`、`COMMIT`、`ROLLBACK`、`SAVEPOINT` 或 `RELEASE`；
   多条语句必须由 rqlite 的事务请求包住，任何失败都不能发布部分迁移。
4. **验证和切换**：对 rqlite 做 schema、capability、行数、摘要、generation、journal 顺序、Token/会话摘要和审计校验；
   审计 actor 必须保留可证明的管理员 `(issuer, subject)`、不授予管理员权限的 typed rejected-request actor，或无法证明的历史字符串作为原值保留的显式未知 actor，不能猜测或伪造 OIDC 身份。
   启动一个目标服务做只读/readiness 验证，再绑定蓝绿活动槽位。
   验证失败保持 SQLite 备份和旧服务不变，不写回或猜测修复 rqlite。
5. **迁移后**：rqlite 成为唯一共享状态权威，蓝绿服务均通过受保护 API 读写；
   SQLite 文件只作为受保护的恢复材料，不再由运行服务打开。
   不得双写、实时双主、从 rqlite 反向重建 SQLite，或通过删表/降级 schema 让旧包“兼容”。

迁移后的 rqlite 故障恢复也必须保持单一权威。
服务发现 rqlite 不可用、数据损坏或摘要不一致时，立即保持 state-write fence、停止新的 native admission，并保留可读的有效租约；
不得把 stale SQLite 当作现行数据库。
恢复源优先使用 cutover 后已验证的 rqlite backup/snapshot，但内部 schema、摘要、epoch 和 journal 校验不足以证明备份覆盖故障前最后可信提交。
恢复前必须记录并验证 backup coverage boundary 位于最后可信提交之后或其上，并用可信 backup tail、append-only journal 或 native-effect evidence 补齐备份之后的提交区间；
Token/session revocation 和 native effect 不能因恢复旧快照而回退。
覆盖范围或 tail 缺失无法证明时，服务必须继续 fenced，进行 administrator-authorized journal/native inventory reconciliation。
只有证据证明 cutover 后没有任何 durable write、epoch advance、lease change 或 native effect 时，才允许重新导入原始 pre-cutover SQLite snapshot；
禁止猜测重建、并行打开旧 SQLite 或通过删表恢复。

### 有界备份保留与删除门禁

迁移源 SQLite snapshot/export、迁移后的 rqlite backup/snapshot、backup tail、manifest、tail-evidence 和 recovery authorization record 都属于恢复材料。
普通蓝绿 `stage`、`activate` 和 `rollback` 不创建、替换或删除这些材料；
首次 `migrate` 只创建自己的迁移源 snapshot，不创建 rqlite backup。

恢复材料使用有界保留策略，默认参数固定如下：

- `minimum_verified_points = 2`：至少保留当前和上一个可验证的 rqlite 恢复点。
- `maximum_verified_points = 3`：超过三个恢复点时，优先删除最旧且已通过删除门禁的恢复点。
- `maximum_age = 30d`：超过三十天的恢复点进入清理候选，但不能因此删除最后两个恢复点。
- `recovery_max_bytes = 1 GiB`：这是状态后端恢复材料的总硬上限；
  部署控制面另外对每个自动创建的迁移源 snapshot 执行 1 GiB 文件上限。
  它不是预分配空间，也不存在体积下限。
- `migration_source_hold = 7d`：迁移源 SQLite snapshot 在迁移完成后至少保留七天，七天从迁移事务 `finalized` 且目标 rqlite 完整 snapshot 摘要验证通过时开始计算；
  完成标记永久保存目标摘要，snapshot 到期删除后仍可验证目标是否被重置。
- `state_history_retention = 30d`：终态 operation 和 audit history 在不再被恢复流程引用且完成外部归档（如需要）后，保留三十天。

恢复材料只有同时满足以下条件，才进入 `purge_eligible`：

- 活动 rqlite 已通过 schema、capability、摘要、`state_epoch`、generation、journal、Token 撤销、lease 和 native-effect 校验。
- 一个新的 rqlite backup/snapshot 已在隔离 recovery target 中通过 restore check，并且它的 coverage boundary 和 backup tail 可以覆盖最后可信提交。
- 没有 pending/unknown operation、未完成的 reconciliation、活动 recovery transaction 或未解决的 `needs_recovery`。
- 对迁移源 SQLite snapshot，迁移事务已 `finalized` 且目标 rqlite 完整 snapshot 摘要已写入完成标记后，七天恢复保留期已经到期。
- 清理后仍至少保留两个可验证的 rqlite 恢复点。
- 运维者针对准确的材料 ID 执行带审计记录的 purge；
  不得使用目录级或全局 cleanup 代替精确删除。

业务状态后端的 retention worker 在每次成功创建恢复点后运行一次，并按“超过最大数量、超过最大年龄或超过总字节预算”的任一条件选择最旧候选，但永远跳过保留期未到、仍被事务引用、或属于最后两个恢复点的材料。
Deployd 每小时只清理自己创建的 `migration-*.sqlite3` snapshot，且不会删除 rqlite backup；
Deployd 永久保留唯一 finalized 迁移事务、完成标记和目标摘要。

生成新恢复点前必须先清理可删除材料并检查 `recovery_max_bytes`。
如果清理后仍没有空间，必须拒绝新的备份或迁移操作并报告 `backup_storage_exhausted`，不能删除受保护恢复点，也不能继续写入导致磁盘无界增长。
这个容量失败不改变正常蓝绿槽位，但会阻止依赖新恢复点的状态迁移或灾难恢复流程。

1 GiB 预算按实际文件大小计算，不为小数据库预留固定空间，也不因为预算存在就把小备份填充到某个最小体积。
实现必须在创建新恢复点前测量数据库、WAL/临时文件、manifest、tail-evidence 和已有恢复材料的实际大小；
候选恢复点连同临时写入空间超过 1 GiB 时，必须在发布前失败。

该上限成立的前提是状态数据库自身不会无限增长。
`operations`、`audit`、`management_operations` 和 `management_audit` 中已完成且不再被恢复流程引用的历史记录，必须按 `state_history_retention` 回收；
pending/unknown operation、活动 operation、当前租约、generation、Token 撤销和恢复所需审计不得回收。
若部署需要超过该保留期的审计，必须先归档到状态库之外，再从状态库删除；
不能通过放大恢复材料预算来掩盖历史记录无界增长。

临时 preflight/acceptance 工作目录可以在进程退出时清理，但只要其中的文件被登记为恢复材料，就必须遵守上述有界保留和删除门禁。

关键管理员授权、会话、Token 撤销、租约、generation、operation journal、状态 capability、`state_epoch` 和 compare-and-act 读取使用 linearizable consistency；
native observation 前后的 epoch 比较也使用 linearizable consistency。
只有明确允许延迟的观测值可以选择更弱的一致性，并且必须保留 freshness/observed-at。
跨行变化使用同一个 rqlite transaction；
条件更新和唯一约束负责 operation ID、request digest、resource version 和 generation 的原子检查。

native Incus 操作在数据库事务外执行时，事务先创建带 slot、worker identity、generation 和 operation ID 的 pending journal；
native 结果通过同一身份条件提交。
提交响应丢失、rqlite 不可用或 native 结果不明时保留 pending/unknown 状态，阻止冲突操作，不创建新 operation ID 重放。
迁移 fence 的最终导出门禁还必须证明这些 pending/unknown worker 已有终态 durable result 且 process group、PID 身份和 Incus operation identity 均已静默；
任何未收敛或可继续产生 native effect 的 worker 都必须使迁移停止在 SQLite。

## 后果

- 首次迁移需要短暂 drain 和可验证的回退点，牺牲一次切换窗口的可用性换取单一状态权威。
- rqlite 单节点共享状态不等于高可用；
  rqlite、Caddy 和两个 Agent VM 槽位的故障域仍需分别观测。
- 迁移后的服务必须把旧的本地锁、generation 和 journal 语义映射到跨槽位并发合同，不能用异步 queued write 代替关键事务。
- 兼容 release 必须声明同时读写当前数据库 capability；
  旧包不能通过启动检查后再发现自己无法处理新状态。

## 否决的替代方案

- **SQLite 与 rqlite 双写**：网络、提交顺序或槽位故障会产生分叉状态，无法安全决定哪一份 operation journal 真实。
- **让两个槽位直接打开共享 SQLite 文件**：文件锁和进程故障不提供跨服务的发布、读取一致性和恢复边界，也违反 rqlite 共享状态目标。
- **在 rqlite HTTP 请求中手动发送 BEGIN/COMMIT**：rqlite 的事务边界由请求参数管理，混用未定义的 SQL 事务控制会破坏迁移合同。
- **为了回滚而降级或重建数据库**：会丢失 generation、租约、Token 撤销、审计或未知操作证据；
  回滚只能选择声明完整读写 capability 的版本。
