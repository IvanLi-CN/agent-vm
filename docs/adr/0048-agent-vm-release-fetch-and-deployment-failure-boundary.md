---
status: accepted
---

# Agent VM 发布读取与部署故障边界

本文记录目标控制环境读取 GitHub Release、准备候选产物和提交蓝绿部署事务时的安全边界；它与 [Agent VM 控制面发布合同](../specs/agent-vm/contracts/release.md)、[CLI 合同](../specs/agent-vm/contracts/cli.md) 和 [Deployd 与蓝绿事务 ADR](0045-agent-vm-deployment-controller.md) 一起定义该路径。

## 背景

目标主机上的 `agent-vm-deploy update` 需要先查询 GitHub Release 元数据，再下载与目标架构匹配的 bundle、provenance 和 checksum，最后交给 Deployd 执行蓝绿事务。

现有 release lookup 使用匿名 GitHub API 请求，非 200 响应只保留 HTTP 状态码，无法区分凭据缺失、仓库权限、匿名限流和网络故障；下载失败后也缺少明确的 staging 清理与下一步合同。

一次人工绕过标准事务的直接 slot 替换还证明了另一个边界：程序文件可以已经运行，但 deployd 仍可能没有 process pin、slot socket link 或 controller lease。运行中的新版本不能因此被视为已部署成功。

## 决策

- 目标主机可以匿名读取公开 release；PAT 是可选的 release read credential，不是所有环境的硬前置。
- 如果配置 PAT，凭据必须只存在于目标主机的 root-only 0600 文件中；它只能读取本仓库 release metadata 和 assets，不能出现在 CLI 参数、SSH 命令、日志、JSON 响应、release bundle 或 deployd 常驻环境中。
- releasefetch 必须先完成 release metadata lookup，再下载和校验产物；不得用固定 download URL、旧 bundle 或其他猜测路径绕过 metadata、immutable、provenance、checksum、source revision 和 target architecture 校验。
- 匿名请求在非限流条件下必须支持成功；匿名 API 限流时必须停止并返回 `release_rate_limited`，不能把限流解释成版本不存在。PAT 可以作为降低匿名限流风险的推荐配置，但不能掩盖错误。
- release lookup 和下载错误使用稳定的结构化字段：`code`、`phase`、`http_status`、`retryable`、`reason`、`next_action`、`request_id` 和 `retry_after_seconds`。人类输出使用同一原因和下一步的有界摘要；不输出完整响应、凭据或私有路径。
- 网络错误、5xx 和明确的 429 采用有界重试；401/403 权限拒绝不盲目重试，匿名限流只按有效的 `Retry-After` 或 reset 信息等待，最长等待受安全上限约束。
- 下载在事务专属 staging 目录中进行。下载、解包、manifest、provenance、checksum、目标架构和容量校验全部成功后，候选才允许进入 Deployd；失败时只清理本次未提交的 staging，并保留版本、source revision、失败阶段和容量证据。
- `update` 只能提交 Deployd 的 stage、verify 和 activate 事务。它不得直接替换 slot、复制二进制、编辑 `active`、授予 controller lease 或恢复 traffic；任何标准事务失败都必须停在可诊断状态。
- Deployd 对 controller pin 失败必须披露具体 blocker，包括 `process_identity_mismatch`、`systemd_unit_mismatch`、`socket_link_missing`、`release_manifest_mismatch`、`controller_lease_unavailable`、`active_slot_mismatch` 和 `observation_stale`。
- 外部直接修改 slot 的状态默认保持 fenced。受限 repair 必须是独立的、显式授权的事务，绑定原 transaction ID，并重新验证 slot、manifest、进程、systemd、socket、generation、controller 和 traffic evidence；系统不得自动 adoption。

## 失败分类

| 阶段 | 错误码 | 含义 | 默认动作 |
| --- | --- | --- | --- |
| `release_metadata_lookup` | `release_access_denied` | 凭据缺失、凭据无权访问或发布读取被拒绝 | 停止并提示配置或轮换 release read credential |
| `release_metadata_lookup` | `release_rate_limited` | GitHub API 明确报告匿名或认证限流 | 按 reset/Retry-After 等待或提示使用合规凭据 |
| `release_metadata_lookup` | `release_not_found` | 版本或 tag 不存在，且响应不是权限/限流拒绝 | 停止并提示核对版本 |
| `release_metadata_lookup` | `release_network_error` | DNS、连接、TLS、超时或 5xx | 有界重试，耗尽后停止 |
| `release_validation` | `release_identity_mismatch` | immutable、source revision、manifest、provenance 或 checksum 不匹配 | 删除本次 staging 并停止 |
| `staging_preflight` | `staging_capacity_low` | 下载或回滚所需空间无法证明 | 不下载，报告容量证据并停止 |
| `deployd_transaction` | `deployment_transaction_rejected` | Deployd 拒绝 stage、verify 或 activate | 保留 transaction/phase 证据，禁止直接替换 slot |
| `deployd_observation` | `controller_pin_blocked` | 运行时身份或角色证据不能证明 | 保持 fencing，报告具体 blocker |

## 验收

单元和 mock contract tests 必须覆盖匿名成功、PAT 成功、401、403 权限拒绝、403 匿名限流、429、5xx、网络中断、响应截断、malformed JSON、容量不足、staging 清理和 Deployd 拒绝。

目标主机 live smoke 必须在无 PAT 条件下从目标主机完成 metadata、immutable、bundle、provenance、checksum、source revision 和 target architecture 校验；rate-limit 窗口内的失败只能记录为 `rate_limited`，不得替代成功证据。另一次 smoke 使用 PAT 验证受限 API 路径，但不得把 PAT 路径当作匿名路径的替代验收。

部署验收必须证明候选通过 Deployd stage/verify/activate，两个槽位证据完整，唯一 controller lease、traffic、active link、ready、pending operations 和 transaction finalize 均收敛。直接 slot 替换、手工 `active` 切换或仅看到新版本进程运行都不构成部署成功。

## 后果与取舍

匿名模式降低首次配置成本，但受到 GitHub 公共 API 配额约束；生产控制环境推荐配置最小权限 PAT。强制所有环境配置 PAT 会提高可靠性，却违反“PAT 可选”的部署边界，因此不采用。

不提供固定 download URL fallback 会在 metadata API 故障时降低可用性，但保留 immutable release 和 provenance 约束，避免在无法证明发布身份时部署未知产物。

不自动 adoption 会让外部修改后的环境需要人工 repair，但避免 deployd 把未知进程或未验证 socket 误认成受控 controller；这是 controller lease 安全边界的必要成本。
