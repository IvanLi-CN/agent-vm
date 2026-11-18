# 连接与认证

Agent VM 将控制面连接、来宾 SSH 连接和文件传输分开处理。控制面负责会话、租约、资源与操作结果；来宾连接只消费当前任务的可信端点和 host key。客户端职责和依赖边界见[客户端设计与审查](../specs/agent-vm/CLIENT.md)。

## 选择控制传输

主机配置可以选择 SSH 或 HTTPS。SSH 传输调用远端固定的 Go request adapter，再由适配器连接受保护的 control socket；HTTPS 传输使用经 Caddy 转发的 Agent API 和不透明 Bearer Token。两种传输共享 protocol 4、请求身份和 operation journal，不形成两套生命周期权威。

```text
agent-vm client -> SSH request adapter -> protected control socket -> Go service
agent-vm client -> HTTPS /agent/v1/control -> Go service
```

HTTPS 控制请求不依赖本机 OpenSSH；来宾 SSH、`ssh`、`exec`、`upload`、`download` 和 `sync` 仍需要相应的 OpenSSH、rsync 或来宾工具。控制 Token 不应被当作来宾登录凭据使用。

## 配置和身份

客户端配置默认位于 `~/.config/agent-vm/config.toml`，凭据通过环境变量或外部文件引用。主机选择顺序为显式 `--host`、`AGENT_VM_HOST`、项目绑定和 `default_host`；会话选择顺序为显式 `--session-id`、`AGENT_VM_SESSION_ID` 和命令所需的本地状态。

主机配置应明确控制地址、传输、认证材料、可选的 socket 路径和项目标识。不要从 Git `origin` 猜测控制目标，也不要把一个主机的 socket、私钥、host key 或恢复日志带到另一个主机。

## 来宾 SSH

`ssh-info --format json` 返回当前任务的连接材料和代次，`ssh-info --format ssh-config` 可生成受限的 SSH 配置片段。客户端必须严格校验 host key，并在目标、session 或 generation 改变时重新获取材料。

```sh
agent-vm --session-id <session> ssh-info --format ssh-config
agent-vm --session-id <session> ssh -- uname -a
agent-vm --session-id <session> exec -- sh -lc 'pwd'
```

`exec --` 和 `ssh --` 后的参数直接交给目标命令；需要 shell 语法时显式写出 `sh -lc`。来宾 SSH 的授权范围不等于控制面 Token 的权限范围。

## 文件传输与网络入口

`upload`、`download`、`export` 和 `sync` 复用当前会话的可信来宾连接，不自动续租。`port add` 只接受明确的名称、guest port、协议和可选 host port，控制端负责分配、验证和回收映射；`port list` 与 `port remove` 用于查询和移除已命名映射。

```sh
agent-vm --session-id <session> upload ./dist /workspace/dist
agent-vm --session-id <session> download /workspace/result ./result
agent-vm --session-id <session> port add web --guest-port 8080 --protocol tcp
agent-vm --session-id <session> port list
```

端口、文件和 SSH 操作都受租约、generation 和资源状态约束。出现 `stale_target`、`operation_outcome_unknown` 或 host key 错误时，不要在没有核对状态的情况下重试。

## 相关合同

- [客户端 CLI](../specs/agent-vm/contracts/cli.md)定义命令、参数、输出和退出行为。
- [控制协议](../specs/agent-vm/contracts/control-protocol.md)定义请求身份、响应匹配、兼容和恢复。
- [Agent API](../specs/agent-vm/contracts/agent-api.md)和[Token API](../specs/agent-vm/contracts/token-api.md)定义 HTTPS 入口与机器 Token。
- [发布合同](../specs/agent-vm/contracts/release.md)定义客户端和控制服务的版本、清单与校验。
