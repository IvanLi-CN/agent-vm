---
status: accepted
---

# Agent VM 传输入口与控制权威分离

Go 父级服务迁移首轮保留现有 SSH 调用入口，通过一次性 Control request adapter 把请求交给 Gin HTTP API；后续允许客户端以 HTTPS 直连同一服务并退出 SSH 兼容。
这是为了在替换远端控制实现时保留既有客户端调用方式，同时避免将 SSH 环境、会话或进程变成生命周期业务的依赖。

Control authority 独占租约、generation、operation journal 和生命周期决策；请求适配器仅转换传输和输出，不拥有第二套控制策略。
HTTP handler 与业务服务通过已校验的请求和调用身份衔接，认证与传输留在入口边界；后续替换为 HTTPS 时补齐 TLS、客户端传输和调用方授权，复用业务与持久状态合同。
相比首轮立即更换客户端传输，这个选择增加了临时适配层，因此实现必须验证不依赖 SSH 的 HTTP handler 调用路径，并按明确的客户端支持窗口退出兼容。

本 ADR 记录已确认的目标边界，实施细节见[Go 父级服务设计与技术选型](../proposals/agent-vm-go-parent-service.md)。
现行 [CLI 合同](../specs/agent-vm/contracts/cli.md) 与[控制更新边界](0030-agent-vm-control-plane-update-boundary.md)描述的执行方式随实际迁移同步更新；到期清理的职责边界另见 [ADR 0035](0035-agent-vm-built-in-expiry-cleanup.md)。
