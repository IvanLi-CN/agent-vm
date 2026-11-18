# 故障排查

先判断问题属于客户端配置、控制面连接、会话生命周期、部署运行还是镜像来源，再按对应合同排查。不要把超时当作失败，不要在没有核对 operation、generation 和目标身份时重做会产生副作用的动作。

## 按症状定位

| 症状 | 先检查 | 继续阅读 |
| --- | --- | --- |
| `missing_session` 或主机选择错误 | 显式 `--host`、`--session-id`、项目绑定和配置文件 | [CLI 身份与配置](../specs/agent-vm/contracts/cli.md) |
| `doctor` 失败或无法连接 | 传输类型、控制地址、Token/SSH 材料、socket 权限和协议兼容 | [连接与认证](connections.md)、[控制协议](../specs/agent-vm/contracts/control-protocol.md) |
| acquire 被拒绝 | `admission_ready`、容量、磁盘准入、压力、drain 和镜像适用性 | [Agent VM Spec](../specs/agent-vm/SPEC.md)、[观测合同](../specs/agent-vm/contracts/observability.md) |
| SSH 或文件传输失败 | 当前任务的 host key、generation、来宾可用性、租约和端口映射 | [连接与认证](connections.md) |
| 操作超时或 `operation_outcome_unknown` | 保留原 operation ID，查询同一 session/generation 的状态和日志 | [控制面生命周期与恢复](../specs/agent-vm/CONTROL-PLANE-LIFECYCLE-RECOVERY.md) |
| release 后数据或缓存不见 | 是否已导出、是否满足缓存保留条件、是否发生回收 | [日常工作流](workflows.md)、[回收设计](../specs/agent-vm/RECLAMATION-DESIGN.md) |
| 镜像不可用或恢复被阻止 | fingerprint、退役状态、manifest、适用性和现有引用 | [镜像管理](images.md) |
| 部署切换或服务恢复失败 | deployd 阶段、active 链接、槽位 readiness、controller role 和业务状态后端 | [部署运维手册](../specs/agent-vm/OPERATIONS.md)、[维护指南](../../deploy/docs/maintenance.md) |
| 指标缺失或数据过期 | `observed_at`、来源健康、新鲜度和 `observation_consistent` | [观测指南](../../deploy/docs/observability.md)、[观测合同](../specs/agent-vm/contracts/observability.md) |

## 连接问题

先执行不产生变更的 `version`、`config list` 和 `doctor`。确认实际使用的 host、transport、项目标识和认证材料，不要凭 Git remote 或旧配置猜目标。HTTPS 控制请求不需要本机 OpenSSH；来宾 SSH 和文件同步则需要各自的工具。

如果 host key、session 或 generation 不匹配，停止后续写入并重新获取 `ssh-info`。如果控制端返回 `stale_target`，以诊断中的 lease、generation 和 retry condition 为准，不能覆盖另一个有效租约。

## 生命周期和未知结果

控制服务或客户端重启后，先查询原 operation，再判断是否需要受保护恢复。`admin reconcile`、`admin gc` 和精确会话恢复默认只预览，只有明确授权的 `--apply` 才会创建变更意图。未知结果的历史记录可以被封存，但不应被删除或重放。

## 部署和镜像问题

部署问题不要通过手工替换 `active`、停槽位、打开业务 SQLite 或重放 shell 命令解决。按[部署安装](../../deploy/docs/install.md)、[蓝绿流程](../../deploy/docs/blue-green.md)、[维护](../../deploy/docs/maintenance.md)和[部署验收](../../deploy/docs/acceptance.md)检查受保护的事务与服务身份。

镜像问题先验证锁、manifest、checksum 和 readiness，再检查 catalog、默认引用、活跃任务和缓存。失败时保留现有默认和旧会话，记录可复现的来源修订与 fingerprint；真实库存和凭据只保留在受保护运维记录中。

## 仍无法定位时

收集命令、时间、脱敏后的 session/task/generation/operation 身份、结构化错误、来源新鲜度和已执行的只读检查。不要附加 Token、私钥、host 地址、数据库、原始部署路径或完整生产观测文件。需要进一步恢复时，将这些事实交给有权限的维护者，并以[运维手册](../specs/agent-vm/OPERATIONS.md)的边界为准。
