# 日常工作流

本页按一次真实工作会话组织常见操作。它只解释使用顺序和风险边界；字段、错误码和兼容范围仍以[CLI 合同](../specs/agent-vm/contracts/cli.md)、[控制协议](../specs/agent-vm/contracts/control-protocol.md)和[Agent VM Spec](../specs/agent-vm/SPEC.md)为准。

## 获取、连接和同步

1. 使用 `config list` 确认控制主机和项目绑定，使用 `doctor` 检查健康与准入。
2. 使用 `acquire` 请求 CPU、内存和磁盘，记录返回的 session、task、generation、lease 和 operation 身份。
3. 使用 `status` 和 `ssh-info` 确认实例、租约、来宾可用性和 host key，再进入来宾。
4. 使用 `sync` 同步工具拥有的源码目录，或使用 `upload`、`download`、`export` 处理明确的文件传输。
5. 使用 `port add` 暴露已知服务，使用 `port list` 检查映射，结束后使用 `port remove` 清理不再需要的入口。

客户端是一次性请求工具，不负责后台续租或常驻保活。同步不会替用户判断租约结束后的数据可恢复性；在释放前导出需要保留的结果。

## 续租、调整和重启

`renew` 在配置上限内延长租约；`resize` 调整 CPU、内存或磁盘，在线调整和允许重启的调整必须使用不同的选项；`restart` 只在明确知道中断影响时执行。变更前后都应查询 `status`，确认 generation 和 operation 结果。

```sh
agent-vm --session-id <session> renew --explain
agent-vm --session-id <session> resize --memory 8GiB --allow-restart
agent-vm --session-id <session> restart
agent-vm --session-id <session> status
```

默认诊断是有界的状态摘要；`--explain` 才增加指标和只读恢复预览。`--apply` 只适用于明确授权的 `admin reconcile`、`admin gc` 或精确会话恢复操作，不是普通会话变更的通用开关。

## 释放和重建

`release` 会撤销访问、停止来宾并按条件保留缓存；`destroy` 会删除指定会话的运行环境和相关缓存。默认镜像切换只影响新会话，已有 VM 和缓存继续绑定原来的不可变 fingerprint。需要换镜像时必须明确销毁并重建，不能用 alias 隐式覆盖旧身份。

释放前先导出数据并移除不再需要的端口，释放后不要假设缓存一定存在。缓存可以被回收；[镜像指南](images.md)解释默认镜像、引用和安全删除的关系。

## 结果不确定和恢复

传输超时、客户端退出或控制服务重启可能留下 `operation_outcome_unknown`。保留原 operation ID 和 generation，先查询同一身份的状态和 operation journal；不要用新 ID 重放 acquire、release、resize 或 restart。只有控制端明确证明原生动作不可能发生时，才能将结果标为已验证失败。

需要维护权限时，先进入 admission drain，再按[运维手册](../specs/agent-vm/OPERATIONS.md)和[维护部署指南](../../deploy/docs/maintenance.md)的步骤操作。管理台的预览、确认和审计语义与 CLI 共用同一控制权威，见[管理台 API 合同](../specs/agent-vm/contracts/management-api.md)。

## 维护者工作流

维护者通常按“概览和观测 → 定位会话 → 预览影响 → 明确确认 → 查询结果和审计”的顺序处理问题。`admin status` 回答任务清单，`admin metrics` 回答容量和健康；缺失、过期、未验证或 `null` 不能当作零值或正常。

```sh
agent-vm admin status
agent-vm admin metrics
agent-vm admin drain status
agent-vm admin reconcile --explain
```

管理动作不会替代部署控制器，也不会获得宿主机、PVE 或任意 Incus 运维权限。必要时使用[部署运维指南](troubleshooting.md)定位边界，再进入受保护的部署流程。
