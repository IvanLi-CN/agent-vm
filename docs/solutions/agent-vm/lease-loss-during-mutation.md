---
title: Agent VM mutation 期间 lease 丢失的诊断与恢复
module: agent-vm
problem_type: lifecycle-recovery
component: control-plane
tags: [lease, operation-journal, generation, native-worker, recovery]
status: active
related_specs: [docs/specs/agent-vm/CONTROL-PLANE-LIFECYCLE-RECOVERY.md, docs/specs/agent-vm/SPEC.md]
---

## 现象

`acquire` 返回 `state_unverified` 或 `startup_reconcile_required`，detail 中出现 `controller-lease`、`outcome_verified=false` 或 `resume-preparing`。随后 status 可能显示 `preparing`、`cached`、`Stopped` 或 lease deadline，不能只根据单个字段判断是否可以重试。

## 只读诊断顺序

1. 运行 `doctor`，确认 control plane、release、admission 和 draining 状态。
2. 运行目标 session 的 `status`，记录 session、task、generation、state、lease、native status、guest availability 和 last error。
3. 运行 `admin status`，确认目标 task 的 native observation 是否 verified，并检查其他 pending worker。
4. 运行 `admin metrics`，确认 `pending_operations`、`in_flight_operations`、`historical_unknown_operations` 和 `task_states`。
5. 使用权威的 task、generation 和 pending operation ID 执行 `admin-reconcile-session` preview；不得猜测 operation ID。

## 决策表

| 观察结果 | 结论 | 下一步 |
| --- | --- | --- |
| 有效 lease 仍未到期 | 不能恢复 | 等待 `lease_expires_at`，保持只读。 |
| lease 已清除，operation 精确匹配且 worker 未产生 effect | 可计划恢复 | 先审查 preview，再由授权运维者决定 apply。 |
| worker 已启动或 native ownership 不确定 | 结果未知 | 保持 pending/unknown，禁止 acquire replay。 |
| task 已为 `cached`、无 lease、无 pending operation | 已安全收敛 | 不需要 reconcile；后续可由负责人决定重新 acquire。 |
| generation 或 operation ID 不匹配 | 拒绝恢复 | 重新读取权威 status 和 journal，不能使用旧 ID 猜测。 |

## 禁止事项

不得用新 operation ID 重放原 acquire，不得手工删除 journal，不得直接操作 Incus container 或 proxy，不得把 `Stopped` 直接解释成 cleanup 成功，也不得把本地 client journal 当作控制面当前状态。

## 完成证据

恢复完成必须同时具备：目标 operation 已有 durable terminal/reconciled result、session generation 已明确、lease 与 reservation 状态一致、native ownership 已确认、worker/process group 已静止、端口与 proxy ownership 已清理、status 与 admin metrics 观察一致。

## 相关设计

- [Agent VM 控制面生命周期与恢复设计](../../specs/agent-vm/CONTROL-PLANE-LIFECYCLE-RECOVERY.md)
- [Agent VM 运维手册](../../specs/agent-vm/OPERATIONS.md)
- [Agent VM CLI 合同](../../specs/agent-vm/contracts/cli.md)
