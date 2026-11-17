# 维护 drain 与恢复

本手册说明现有控制器的 drain ownership 行为。它适用于已明确授权的控制环境维护，不授予发布、配置迁移、终止有效租约或外部宿主操作权限。正常部署的 drain 由发布器或 Deployd 持有；存在活动或待恢复发布事务时，使用原事务的恢复流程，不能另建维护会话抢占。

## 进入维护前

从受保护部署配置确认控制 alias、匹配的客户端和服务版本。保存 `doctor`、`admin status`、`admin session list`、`admin drain status` 及控制环境内 `/opt/agent-vm-native/bin/agent-vm-deploy status --json` 的结果。要求观测新鲜一致，核对活动槽位、controller/traffic 角色、进行中事务与原 drain 状态；已有 drain 的 owner 或来源不明时停止，不能假定是自己的暂停。

当前控制器的 `drain-exit` 要求请求 `session_id` 以 `release-` 开头，并与保存的 `drain_owner` 一致。普通会话可能成功执行 `drain-enter`，却在退出时收到 `only the active release transaction may exit admission drain`。因此获授权的独立维护从进入时就使用唯一的 `release-maintenance-<unique-id>` 会话，并在本次维护全程保存该 ID。这个字符串是操作 ownership metadata，不替代运维认证、授权或健康检查；不要复用其他发布器的 session ID。

下面命令在配置了正式客户端的操作端执行。先将变量设为已核实的控制 alias 和本次唯一 ID；示例不是自动生成发布事务，也不调用 `stage`、`activate` 或 `update`：

```sh
control_alias='<configured-control-alias>'
maintenance_session='release-maintenance-<unique-id>'
bun skills/agent-vm/scripts/agent_vm.ts --host "$control_alias" \
  --session-id "$maintenance_session" admin drain enter
bun skills/agent-vm/scripts/agent_vm.ts --host "$control_alias" \
  --session-id "$maintenance_session" admin drain status
bun skills/agent-vm/scripts/agent_vm.ts --host "$control_alias" \
  --session-id "$maintenance_session" admin status
```

检查结构化响应 `ok=true` 和 `draining=true`，不能仅凭进程退出码判断成功。Drain 阻止新 acquisition 和 restart admission，保留读取及安全释放路径；它不会强制停止有效租约。等待 effective lease、pending transition 和 native worker 收敛后才能开始已授权修改；不能以强制停止用户实例代替等待。

## 恢复准入

先验证本次配置或回退结果、服务身份、CT-local 容量、scheduler/expiry/reconciliation 健康与 dry-run 对账。`admin status` 必须完整且一致；`partial`、`unknown`、超时或 epoch 变化都不构成通过证据，先检查原因并重新取得有界、完整观测。确认没有其他发布事务接管后，以同一个维护会话退出：

```sh
bun skills/agent-vm/scripts/agent_vm.ts --host "$control_alias" \
  --session-id "$maintenance_session" admin drain exit
bun skills/agent-vm/scripts/agent_vm.ts --host "$control_alias" doctor
bun skills/agent-vm/scripts/agent_vm.ts --host "$control_alias" \
  --session-id "$maintenance_session" admin status
```

必须看到退出响应 `ok=true`、`draining=false`，并在控制环境内复查 Deployd 的新鲜一致观测：预期活动槽位就绪、拥有唯一 controller、`traffic_accepting=true`、`draining=false`，没有未处理的 native worker 或 pending operation。不要因 control 进程健康就宣称业务准入已经恢复。任何退出或健康检查失败都保留原会话、operation ID 和证据，不能直接改数据库、删除 journal 或通过重启服务清除 fence。

## 普通维护会话无法退出时

先核对原会话、进入操作和当前发布状态。仅在运维记录证明这是本次任务创建的普通维护 drain、没有活动或待恢复发布事务、没有其他 owner 竞争且健康条件满足时，获授权运维者才可创建本次唯一 `release-maintenance-<unique-id>` 恢复会话。先用该会话执行 `admin drain enter`，通过控制器现有路径接管普通维护 drain；确认成功后，再以同一会话执行 `admin drain exit` 并完成上述复查。

控制器允许 `release-*` 会话接管普通维护或旧版无 owner 的 drain，但不允许替换另一个 `release-*` owner。出现 `operation_conflict`、owner 不明或不确定的在途 mutation 时保持暂停，使用原 owner 的部署/恢复流程；不得尝试不同 session ID 来绕过冲突。历史进入或退出请求结果不确定时，保留原 operation identity 并按[运行恢复手册](../../docs/specs/agent-vm/OPERATIONS.md)对账。
