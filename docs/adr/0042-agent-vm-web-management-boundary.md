---
status: superseded
superseded_by: 0043-agent-vm-unified-service-security-boundary.md
---

# Agent VM 内置管理台的交付与权限边界

本 ADR 记录曾经接受、现已被 [ADR 0043](0043-agent-vm-unified-service-security-boundary.md) 替代的管理台边界。它保留为决策历史，不再是当前实现或部署合同。

历史决策曾选择在同一产品包中运行独立的非 root Web 进程，由它接收外部认证后的浏览器请求，再通过专用管理 UDS 调用唯一 Go control authority。该边界曾试图隔离浏览器解析与高权限 native 控制，但与蓝绿完整槽位、共享认证事务和共享 rqlite 状态合同不再一致。

当前不应从本文件推导独立 Web 进程、Forward Auth、管理 UDS 或第二个路由权威。统一服务的安全取舍、单一管理员 OIDC、机器 Token、管理员审计和 Caddy 边界以 ADR 0043 以及相应 Spec/API 合同为准。

历史上的管理员动作、原因、确认和审计要求仍由管理 API 合同保留，但其当前服务边界已经迁移到 ADR 0043。

历史的产品范围和研究依据仍保留在提案文档；新增管理行为以 Agent VM Spec 和管理 API 合同为准。
