# 快速开始

本页给第一次使用 Agent VM 的用户一条最短路径：准备客户端、选择控制主机、获取会话、连接来宾并在结束后释放租约。命令的完整参数和退出行为以[客户端 CLI 合同](../specs/agent-vm/contracts/cli.md)为准。

## 准备客户端

Agent VM 客户端由 Bun 执行，并按功能使用 OpenSSH、rsync 或 Git。先确认客户端包和发布清单来自同一份受信任发布，再执行 `agent-vm version` 检查本地协议版本；发布身份、校验和及回滚边界见[产品发布与部署合同](../specs/agent-vm/contracts/release.md)。

不要把控制服务地址、Token、私钥或真实清单写入项目文档。连接材料应由运维者通过受保护配置提供，客户端只保存完成连接和恢复所需的本地状态。

## 初始化连接

```sh
agent-vm config init
agent-vm config host add <alias>
agent-vm config host set-default <alias>
agent-vm config list
agent-vm doctor
```

`config host add` 选择 SSH 或 HTTPS 控制传输，并引用对应认证材料；`doctor` 只检查控制主机和组件健康，不会创建会话。项目绑定可用 `agent-vm config project bind` 完成，显式 `--host` 和 `--session-id` 始终优先于环境变量和默认值。

## 获取并使用会话

```sh
agent-vm --session-id <session> acquire --cpu 2 --memory 4GiB --disk 20GiB
agent-vm --session-id <session> status
agent-vm --session-id <session> ssh
```

`acquire` 创建或复用所选会话的受管容器，并返回任务、代次、租约和连接所需的结构化结果。资源请求、镜像绑定和准入判断由控制端负责；客户端不直接访问 Incus、PVE 或状态数据库。

连接来宾前先使用 `ssh-info` 验证当前任务的端点、host key 和会话代次；控制 Token 不等同于任意来宾命令授权。文件传输使用 `upload`、`download`、`export` 或 `sync`，这些命令不会隐式延长租约。

## 结束会话

```sh
agent-vm --session-id <session> renew
agent-vm --session-id <session> release
```

租约即将到期时显式执行 `renew`；可用 `--lease 30m` 到 `6h` 指定增加量。需要跨长时间任务保留时执行 `pin --reason "<理由>"`，取消保留使用 `unpin`，之后会按首租默认时长重新计算截止时间。`release` 会撤销访问并停止来宾，可能保留合格缓存；它不是普通关机，也不保证缓存永久保留，且保留中的 VM 必须先 unpin。需要彻底删除时使用 `destroy`，并先确认数据已导出。

## 结果不确定时

请求超时、浏览器或 SSH 连接中断不代表原生动作失败。保留原来的 `operation_id`、`generation` 和会话标识，再通过 `status` 或带 `--explain` 的命令查询结果；不要换一个操作 ID 重做同一动作。生命周期、租约和恢复语义见[控制面生命周期与恢复设计](../specs/agent-vm/CONTROL-PLANE-LIFECYCLE-RECOVERY.md)。

遇到连接、准入或释放问题时，先看[连接指南](connections.md)和[故障排查](troubleshooting.md)，再按[运维手册](../specs/agent-vm/OPERATIONS.md)执行需要授权的恢复步骤。
