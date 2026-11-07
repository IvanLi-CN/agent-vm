# 部署验收

完整验收必须在本次任务专属的干净 Linux/systemd 环境执行，使用候选 SHA 对应的产品包与部署资料，不使用生产 VM。已有静态检查和测试槽位脚本不能替代真实 Incus 生命周期验收。

## 安装与恢复

按[首次安装步骤](install.md)执行只读预检，保存退出码和报告，确认系统文件、systemd 状态和数据库均未变化。显式安装后保存原事务 ID、四个服务的状态、Deployd 组合观测和 rqlite 权威证明。用相同输入再次安装，必须不覆盖配置、不生成另一个初始化事务，也不重启有效槽位。

分别在文件校验前、初始化返回后和 active 切换后注入中断。重试必须核验原阶段；不确定的首次启动请求不得自动重放。active 缺失时用原事务执行显式 `resume-bootstrap`，active 存在时按原事务执行 `finalize`。重启 Deployd 后，旧数据库记录不能直接恢复 controller role。

## VM 基本功能

在专属服务上，通过受保护的管理员管理页签发仅供本次验收使用的 Agent Token，配置本次客户端，然后以唯一 session ID 执行以下动作。身份同意由操作者本人完成，Token 不写入验收日志。

```sh
bun skills/agent-vm/scripts/agent_vm.ts doctor
bun skills/agent-vm/scripts/agent_vm.ts acquire --session-id deployment-acceptance
bun skills/agent-vm/scripts/agent_vm.ts exec --session-id deployment-acceptance -- sh -lc 'printf "vm-exec-ok\n"'
bun skills/agent-vm/scripts/agent_vm.ts renew --session-id deployment-acceptance
bun skills/agent-vm/scripts/agent_vm.ts release --session-id deployment-acceptance
bun skills/agent-vm/scripts/agent_vm.ts status --session-id deployment-acceptance
```

确认释放后无有效租约、端口映射关闭，且实例进入 stopped/cache 状态。仅对本次专属测试实例，在明确允许丢弃其数据的隔离验收中执行 `destroy`，确认 Incus 实例与残留映射均已删除。不能对用户实例执行这些测试。

## 蓝绿与观测

以另一个有效候选包执行 `stage`、`verify`、`activate`，记录原事务、两槽进程身份、controller term、active 链接、旧槽 drain 和 quiescence。切换期间必须能同时观测两槽，且最多一个槽持有有效 controller lease。验证一次有界 rollback、Deployd 重启与不确定阶段 fencing；普通切换前后业务权威必须是同一 rqlite，不得执行数据库替换。

按[观测接入](observability.md)启动本次专属 Prometheus/Grafana，验证只有一个 Deployd target，容量和清单来自活动槽位，角色面板显示两个槽位。生成真实 VM 任务后检查容量与任务序列；缺失数据必须定位来源，不能作为成功证据。跨主机示例必须分别验证有效客户端证书成功、缺少或错误证书失败、ACL 外来源失败、Caddy 不提供 `/metrics`。

验收记录应绑定候选 SHA、运行环境身份、命令、退出码、事务 ID 与时间范围。标注静态检查、测试适配器和完整实证的区别；任何未运行或失败项都不能填写为通过。

## 生命周期恢复回归

控制服务候选必须覆盖旧 pending journal 与缓存释放收尾；不得用清空数据库或放宽 worker 围栏通过验证。`go test ./internal/lifecycle` 包含跨页维护公平性、错误 binding、缓存释放拒绝条件和维护故障准入测试。

真实 Incus 回归使用会话自有 Linux 来宾中的独立嵌套项目。`AGENT_VM_SCOPED_INCUS_PROJECT` 必须以 `scoped-reconcile-test-` 开头，三个 fixture `instance-target`、`instance-expired-peer`、`instance-active-peer` 必须实际 Running，并具有测试规定的受管 session/task/generation 和 proxy。未提供项目时测试会跳过，跳过不算原生验收通过。运行 `TestScopedReconcileIsolatedIncus` 验证精确恢复；重新建立相同 Running fixture 后设置 `AGENT_VM_SCOPED_INCUS_MODE=maintenance` 验证普通 scheduler 在未知历史 journal 下仍清理过期对象、保留有效租约并核验 cached release。两种模式不能复用前一模式已经停止的初始实例状态。
