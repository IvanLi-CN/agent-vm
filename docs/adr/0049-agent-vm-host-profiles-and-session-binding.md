---
status: accepted
---

# Agent VM 宿主配置与 Session 绑定

## 背景

当前 Agent VM 客户端通过 `AGENT_VM_CONTROL_HOST` 等低层环境变量选择控制端点，两个 Skill 没有统一的宿主别名、默认宿主或项目偏好模型。多台控制宿主并存时，依赖手工 IP 或 SSH alias 容易把请求发到错误的 Agent VM。

## 决策

- 在 Agent 所在机器使用 `~/.config/agent-vm/config.toml` 保存宿主 profile、全局默认和项目绑定。
- `agent-vm` 与 `agent-vm-ops` 共用 alias、transport 和解析优先级，并提供统一 `--host <alias>`。
- 解析优先级为命令行、`AGENT_VM_HOST`、项目绑定、全局默认；无默认时 fail closed。
- `acquire` 后将宿主 alias 与控制端点身份绑定到 session，后续操作不得漂移。
- HTTPS 和 SSH 是宿主 profile 的显式 transport；连接失败不自动切换宿主或 transport。
- 旧低层环境变量继续兼容，但只作为未命名宿主的临时覆盖，不自动持久化。

## 取舍

本机配置降低了提示词和命令的重复输入，也让 Agent 能在多个宿主间明确切换。配置增加了需要维护的用户状态，因此解析结果必须在执行前展示，敏感值必须保持在独立凭据存储中。Session 绑定牺牲了默认值修改后的自动迁移，但避免了跨宿主误操作，优先保证资源边界和可审计性。
