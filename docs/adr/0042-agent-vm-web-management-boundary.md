---
status: accepted
---

# Agent VM 内置管理台的交付与权限边界

Agent VM 增加常驻内置管理台，以 React、TypeScript、Vite 和 shadcn/ui 构建操作面，静态资源嵌入 Go 产品二进制，与控制服务共享产品版本及完整包回滚。管理范围限定为 Agent VM 受管镜像和已有 Agent 会话 VM，不提供人工创建 VM 或通用 Incus 管理入口。

同一二进制中的独立 Web 进程以专用非 root 身份运行，通过外部统一认证与管理员授权接收浏览器请求，再经专用管理 UDS 调用唯一 Go control authority。Web 进程不打开 SQLite、不获得通用 Incus 权限、不使用或放宽现有 root-only control socket；专用入口校验 peer 身份，只允许固定管理路由、动作和字段，最终会话归属、generation、租约、准入和 journal 检查仍由 authority 执行。

这个选择增加一个进程、systemd unit 和受限内部请求合同，换取浏览器处理与高权限 native 控制的隔离。直接在现有 root authority 中增加 Web listener 更简单，但会将浏览器与外部认证的处理放进高权限进程；独立部署前后端则增加产品版本与回滚协调成本。

管理员可以主动干预有效租约中的 VM，但必须展示受影响会话、填写原因、二次确认并记录审计；请求提交后 authority 重新校验状态。管理响应采用字段白名单和结构化错误，不直接透传内部 control/snapshot 的原始诊断。

这是已确认但尚未实施的目标边界。具体可信代理或 OIDC 接入、受限 socket 权限、镜像校验隔离、增量状态迁移与安装包合同需要在实施时验证。完整产品范围和替代方案见[管理台研究备忘录](../proposals/agent-vm-web-console.md)；会话镜像身份规则见 [ADR 0041](0041-agent-vm-session-pinned-image-identity.md)。
