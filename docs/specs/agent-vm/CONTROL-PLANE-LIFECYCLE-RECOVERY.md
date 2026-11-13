# Agent VM 控制面生命周期与恢复设计

本文件是 Agent VM session lifecycle、controller lease、native worker、operation journal 和 recovery 之间关系的权威设计。长期行为合同由本文件和 [Agent VM Spec](SPEC.md) 共同定义；CLI 输出由 [CLI 合同](contracts/cli.md) 定义；运行步骤由 [运维手册](OPERATIONS.md) 定义。

## 设计目标

控制面必须在 lease 丢失、进程重启、native worker 超时、传输超时和重复请求后仍能区分已确认成功、已确认失败、未产生副作用和结果未知。结果未知时，系统必须保留原始 operation identity 和恢复证据，禁止用新 operation ID 重放可能已经发生的 native effect。

控制面是 session、task、generation、lease、reservation、port ownership 和 operation journal 的唯一权威。CLI 的本地 journal 只能保存请求和恢复线索，不能覆盖控制面的当前状态。

## 核心实体

| 实体 | 约束 |
| --- | --- |
| `session_id` | 选择唯一受管 session，不等于用户身份。 |
| `task_id` | 标识受管 native container，生命周期内保持稳定。 |
| `generation` | 标识 session 的一次生命周期代次，只能由控制面推进。 |
| `lease` | 授权当前 generation 使用 native resource；没有有效 lease 不能开始新的 native mutation。 |
| `operation_id` | 标识一次幂等请求；结果未知时必须沿用原值。 |
| `worker evidence` | 保存 worker state、PID、process group、start time、native ownership 和 cleanup 结果。 |

## 生命周期状态机

```text
cached ── acquire ──> preparing ── native verified ──> active
   ▲                      │                              │
   │                      │ uncertain                    │ lease expires/release
   │                      ▼                              ▼
   └──────── reconciled ◄─ unknown ◄──── stopping ◄──── closing
                         │
                         └─ safe failure / cleanup-only
```

`preparing` 只表示控制面已经接受 operation，不表示 native resource 已经可用。`active` 必须同时有有效 lease、匹配 generation、已确认 native ownership 和可用 guest。`unknown` 表示结果不能由 durable state 和 native evidence 证明，不能被解释为 success 或 failure。`reconciled` 只有在原 operation identity、generation、worker evidence 和 native cleanup 都通过检查后才能写入。

## Lease 丢失规则

1. Native mutation 开始前必须持有当前 controller lease、session lock、generation 和 admission fence。
2. Mutation 期间 lease 丢失时，控制面必须停止继续发起 native mutation，并将 operation 保持为 `pending` 或 `unknown`。
3. Lease 丢失不能推进 generation，不能创建替代 operation，不能直接返回 success。
4. 控制面必须保存 lease loss phase、原始 request digest、worker identity、native ownership 和最后一次可验证状态。
5. 只有在 lease 已清除、目标 task/generation/operation 精确匹配且 native cleanup 证据完整时，才允许执行 scoped reconciliation。
6. 新的 controller lease 只能在 admission、scheduler、pending operation 和 native worker 状态满足 controller recovery contract 后授予。

## Operation journal 状态

| 状态 | 含义 | 允许动作 |
| --- | --- | --- |
| `pending` | 已持久化 intent，native effect 尚未被证明完成。 | 查询、原 operation recovery、只读 preview。 |
| `running` | native worker 已开始，副作用可能已经发生。 | 查询、等待或沿原 operation 做 reconciliation。 |
| `success` | request result、generation、ownership 和 native result 均已验证。 | 幂等 replay、正常后续生命周期操作。 |
| `error` | 控制面确认没有未完成的 native effect，或失败发生在 native barrier 之前。 | 按 error contract 处理。 |
| `unknown` | 无法证明 native effect 是否发生。 | 保留 evidence；禁止新 operation replay。 |
| `reconciled` | 原 operation 已完成安全收敛，结果与 cleanup 证据已持久化。 | 允许后续新 generation 操作。 |

`historical_unknown` 只能表示已保留但尚未完成对账的历史 operation，不能作为当前 session 健康或新 acquire 成功的证据。

## Generation 与本地 journal 对账

控制面返回的 `generation`、`state`、`lease_expires_at` 和 `pending_operations` 优先于客户端本地缓存。客户端发现 local generation 小于 server generation 时，必须标记 local journal stale，保留原始文件供审计，但不得据此发送旧 generation mutation。

客户端发现 server 已处于 `cached`、`effective_lease=false`、`guest_available=false` 且没有 pending operation 时，可以将本地旧 pending 记录标记为 superseded；不能把它改写成 success，也不能删除原始 evidence。

## Recovery 准入

`admin-reconcile-session` 必须同时匹配 `session_id`、`task_id`、正整数 `generation`、权威 `pending_operation_id`、request digest 和 worker evidence。以下任一条件成立时必须拒绝 apply：lease 未到期、generation 不匹配、operation 不存在或不匹配、worker 已产生 effect、native ownership 不确定、存在同 session 的其他 pending intent、task 已经进入 `cached` 或已经完成其他终态转换。

Preview 必须纯只读，不创建 operation intent、local journal、worker intent 或 mutation audit。Apply 必须使用独立 operation ID、持有维护与 session fence，并沿原有 cleanup worker 路径完成证据闭合。

## 错误与诊断合同

所有 mutation uncertainty 至少返回 `operation_id`、`session_id`、`task_id`、`generation`、`state`、`lease_expires_at`、`native_status`、`worker`、`outcome_verified`、`recovery_blocker` 和 `next_action`。字段缺失、旧 peer 或 transport timeout 必须标记 `diagnosis_completeness=partial`，不能默认解释为安全成功。

`lease_lost_during_mutation`、`native_effect_unknown`、`lease_unexpired`、`generation_mismatch` 和 `task_already_reconciled` 是不同 blocker，客户端不得把它们合并成同一个 retry 建议。

## 验证要求

实现必须覆盖 lease 丢失前后、worker 未启动、worker 已启动、native cleanup 成功、cleanup 超时、重复 operation、旧 peer、local/server generation 不一致和 task 已 cached 等场景。每个场景都必须证明 journal outcome、native ownership、lease、generation 和 CLI diagnosis 的最终关系。

相关实现和操作证据见 [维护恢复方案](../../solutions/agent-vm/lease-loss-during-mutation.md)、[维护进展 ADR](../../adr/0047-agent-vm-maintenance-progress-and-recovery.md) 和 [CLI 合同](contracts/cli.md)。
