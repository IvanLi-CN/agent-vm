# Agent VM 宿主选择需求

## 目标

`agent-vm` 和 `agent-vm-ops` 必须使用同一套本机宿主配置，让 Agent 可以用稳定别名选择 105、109 等控制宿主，并能设置用户全局默认宿主和项目级默认宿主。

## 配置位置与内容

用户配置位于 `~/.config/agent-vm/config.toml`，目录权限为 `0700`，文件权限为 `0600`。配置保存宿主别名、连接方式、控制端点、CA/known-hosts 路径和凭据引用；不得保存 token、密码或私钥明文。

推荐结构如下：

```toml
default_host = "105"

[hosts.105]
transport = "https"
control_host = "https://agent-vm-15.ivanli.cc"
ca_file = "~/.config/agent-vm/certs/105-ca.crt"
token_env = "AGENT_VM_105_TOKEN"

[hosts.109]
transport = "ssh"
control_host = "agent-vm-109"
ssh_config = "~/.ssh/config"

[projects."github.com/IvanLi-CN/agent-vm"]
host = "105"
```

## 选择优先级

宿主解析顺序固定为：命令行 `--host`、环境变量 `AGENT_VM_HOST`、当前项目绑定、用户全局 `default_host`。没有有效默认时返回结构化 `host_selection_required`，不得根据 IP 可达性、最近使用记录或其它启发式猜测宿主。

两个 Skill 都接受相同的自然语言约定：“使用 Agent VM 宿主 105”。命令层提供统一的 `--host <alias>`。执行前的 `doctor`、`status` 和 mutation 输出必须显示解析出的 alias、transport 和脱敏 endpoint。

## Session 边界

`acquire` 解析宿主一次，并将 `host_alias` 和控制端点身份写入本地 session 状态。后续 `status`、`renew`、`release`、`ssh` 和 `exec` 始终使用该绑定。改变默认宿主不会迁移已有 session；不同宿主必须使用不同的 `session-id`。

## 连接方式

连接方式绑定在宿主配置中。HTTPS 使用 `token_env` 或 `token_file` 以及可选 CA；SSH 使用 `ssh_config`、identity file 和 known-hosts。凭据缺失或连接失败时直接报告该宿主的错误，不自动切换 transport 或其它宿主。

现有 `AGENT_VM_CONTROL_HOST`、`AGENT_VM_CONTROL_TOKEN`、`AGENT_VM_CONTROL_CA`、`AGENT_VM_REMOTE_REQUEST` 和 `AGENT_VM_REMOTE_SOCKET` 保持兼容，但只作为未命名宿主的临时低层覆盖。它们不得自动写回配置文件，`doctor` 必须提示兼容模式。

## 项目绑定

项目绑定使用规范化 Git remote 作为主键，当前工作树的子目录继承仓库根目录绑定。非 Git 目录不使用项目绑定；不同 remote 的 fork 不继承原项目绑定。项目绑定只选择宿主，不覆盖显式 `--host` 或连接凭据参数。

## 配置命令

实现统一的配置入口：`config init`、`config list`、`config host add`、`config host set-default` 和 `config project bind`。首次使用可以创建空配置结构，但不得猜测宿主或默认值。`config list` 和 `doctor` 只显示凭据引用及 `present/missing` 状态，不显示敏感值。

删除仍被全局默认或项目绑定引用的宿主必须先解除引用。删除宿主配置不删除证书、私钥、known-hosts 或其它外部凭据文件。

## 两个 Skill 的边界

`agent-vm` 和 `agent-vm-ops` 共用宿主配置和解析库。普通执行可以使用解析出的默认宿主；涉及控制面 mutation 的 `agent-vm-ops` 必须在执行记录中保留解析出的 alias、transport 和目标身份，不得把一个宿主的默认配置用于另一个宿主。

## 验收标准

- 使用 `--host 105` 和 `--host 109` 时，两个 Skill 都连接到对应宿主。
- 项目默认优先于全局默认，显式 `--host` 优先于所有默认。
- session 创建后修改默认值，原 session 仍使用原宿主。
- 缺少默认宿主时不执行控制请求，并返回 `host_selection_required`。
- HTTPS 与 SSH 配置分别验证成功，凭据缺失不会跨宿主或跨 transport 降级。
- 旧环境变量继续可用，但会被 `doctor` 标记为兼容模式。
- 配置权限、敏感值脱敏、项目 remote 规范化和删除引用保护均有测试覆盖。
