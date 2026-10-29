---
status: accepted
---

# Agent VM SQLite 到 rqlite 的保守状态迁移

## Context

现有 Agent VM 使用 `database/sql`、`modernc.org/sqlite`、protocol 4、generation、`state_epoch` 和 operation journal。蓝绿服务需要两个版本共享同一份状态，但不能让两个槽位直接打开同一个 SQLite 文件，也不能通过双写制造两个状态权威。rqlite 提供服务化的 SQLite 状态入口，但它的事务和读一致性语义与本地 `BEGIN IMMEDIATE` 不同，不能把旧 SQL 调用原样包在 HTTP 请求中。

## Decision

迁移分为明确的单一权威阶段：

1. **SQLite 基线**：迁移前保持当前 SQLite schema、protocol-4 请求/响应、generation、`state_epoch`、journal、audit 和 unknown-outcome 语义。现有服务继续是唯一写入者。
2. **排空和导出**：迁移开始前进入 drain，停止新的 native admission，允许查询和安全收敛；在一致性边界生成 SQLite 备份与导出，并记录 schema、capability、行数、主键范围、generation/journal 摘要和受保护密钥版本。
3. **一次性导入**：目标 rqlite 先通过 schema/capability 预检，再使用 `POST /db/execute?transaction` 的单请求事务导入数据。不能在请求 SQL 中自行发送 `BEGIN`、`COMMIT`、`ROLLBACK`、`SAVEPOINT` 或 `RELEASE`；多条语句必须由 rqlite 的事务请求包住，任何失败都不能发布部分迁移。
4. **验证和切换**：对 rqlite 做 schema、capability、行数、摘要、generation、journal 顺序、Token/会话摘要和审计校验；启动一个目标服务做只读/readiness 验证，再绑定蓝绿活动槽位。验证失败保持 SQLite 备份和旧服务不变，不写回或猜测修复 rqlite。
5. **迁移后**：rqlite 成为唯一共享状态权威，蓝绿服务均通过受保护 API 读写；SQLite 文件只作为受保护的恢复材料，不再由运行服务打开。不得双写、实时双主、从 rqlite 反向重建 SQLite，或通过删表/降级 schema 让旧包“兼容”。

关键授权、主体 ownership、会话、Token 撤销、租约、generation、operation journal、状态 capability 和 compare-and-act 读取使用 linearizable consistency。只有明确允许延迟的只读观测可以选择更弱的一致性，并且必须保留 freshness/observed-at。跨行变化使用同一个 rqlite transaction；条件更新和唯一约束负责 operation ID、request digest、resource version 和 generation 的原子检查。

native Incus 操作在数据库事务外执行时，事务先创建带 slot、worker identity、generation 和 operation ID 的 pending journal；native 结果通过同一身份条件提交。提交响应丢失、rqlite 不可用或 native 结果不明时保留 pending/unknown 状态，阻止冲突操作，不创建新 operation ID 重放。

## Consequences

- 首次迁移需要短暂 drain 和可验证的回退点，牺牲一次切换窗口的可用性换取单一状态权威。
- rqlite 单节点共享状态不等于高可用；rqlite、Caddy 和两个 Agent VM 槽位的故障域仍需分别观测。
- 迁移后的服务必须把旧的本地锁、generation 和 journal 语义映射到跨槽位并发合同，不能用异步 queued write 代替关键事务。
- 兼容 release 必须声明同时读写当前数据库 capability；旧包不能通过启动检查后再发现自己无法处理新状态。

## Rejected alternatives

- **SQLite 与 rqlite 双写**：网络、提交顺序或槽位故障会产生分叉状态，无法安全决定哪一份 operation journal 真实。
- **让两个槽位直接打开共享 SQLite 文件**：文件锁和进程故障不提供跨服务的发布、读取一致性和恢复边界，也违反 rqlite 共享状态目标。
- **在 rqlite HTTP 请求中手动发送 BEGIN/COMMIT**：rqlite 的事务边界由请求参数管理，混用未定义的 SQL 事务控制会破坏迁移合同。
- **为了回滚而降级或重建数据库**：会丢失 generation、租约、Token 撤销、审计或未知操作证据；回滚只能选择声明完整读写 capability 的版本。
