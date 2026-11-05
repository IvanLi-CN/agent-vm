---
status: accepted
---

# ADR 0046: Agent VM 失败通知通道与启用边界

Agent VM 已有可信默认分支 sidecar，可以构建失败工作流的 metadata-only context，但在 transport 选择前必须 fail closed。我们选择由 Oidrune 通过 GitHub OIDC 将失败事件交给 operator-controlled Telegram destination，并把通道选择、外部配置、真实验证和生产启用明确分开。

## 决策

- Oidrune GitHub OIDC Relay 是本项目唯一的失败通知 transport；不使用 GitHub 默认邮件、隐式 webhook 或调用方保存的 Telegram/Webhook 静态凭据。
- `Notify failed workflow` 和 `Notify failed release` 继续由可信默认分支触发，只消费失败运行的 metadata；通知器和 notification smoke 自身继续排除，防止递归通知。
- 调用方使用完整 commit SHA 固定的 Oidrune reusable workflow，授予最小读取权限和 `id-token: write`，使用成对的 `gateway_url` 与 `oidc_audience`；调用方不传递 `secrets:`。
- Oidrune 返回 `202 Accepted` 表示 durable handoff，不表示 Telegram delivery completion；gateway 失败使 notification job 失败，但不得重新构建、重新发布或重新部署。
- 同一 `repository:workflow:run_id:attempt` 必须幂等；重跑产生新的 attempt，但仍绑定原始失败运行，不改变原产品发布 identity。
- 通知 transport 的状态按 `selected`、`configured`、`verified`、`enabled` 分层；真实 smoke 成功前不得标记为 `enabled`，正式 Release 也不得因设计确认而提前启用。
- 发布 workflow 和 recovery workflow 不再依赖 `RELEASE_PLATFORM_TOKEN`；GitHub 内置 token 负责产品发布所需的最小 API 操作，管理员级 branch protection/ruleset audit 属于独立 operator audit。

## 后果

调用方仓库不保存 Telegram 或 Oidrune 静态凭据，Oidrune operator 负责 gateway、OIDC admission、来源 allowlist、queue、retry、DLQ 和 Telegram destination。仓库仍必须提供规范化失败上下文、workflow inventory、幂等键和手动 smoke 入口，并保留通知结果与原工作流结果的独立性。

在外部 gateway URL、OIDC audience、来源 allowlist、目标配置和真实 smoke 证据完成前，仓库只能记录已接受的 transport 决策，不能宣称通知已启用、正式发布已完成或 Telegram 已送达。

## 否决的替代方案

- **`RELEASE_PLATFORM_TOKEN` 或 Telegram 静态 Secret**：扩大调用方权限和凭据泄露面，且不是 Manual version release delivery 的必要条件。
- **GitHub 默认邮件、现有 webhook 或人工查看**：没有项目选择证据、完整上下文和可验证的独立交付合同。
- **把通知发送嵌入 Release 主流程**：会把通知失败错误地耦合为发布失败，并破坏发布、部署和通知的独立恢复边界。
