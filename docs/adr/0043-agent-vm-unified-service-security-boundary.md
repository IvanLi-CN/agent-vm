---
status: accepted
supersedes: 0042-agent-vm-web-management-boundary.md
---

# Agent VM 统一服务安全边界与身份模型

## Context

蓝绿部署要求蓝色和绿色都是可独立启动、可验证、可切换的完整 Agent VM 服务，并且共享 OIDC 回调事务、浏览器会话、Token 摘要、生命周期状态和 operation journal。ADR 0042 选择独立非 root Web 进程、Forward Auth 和专用管理 UDS；这会把一个产品槽位拆成两套发布单元，也会让认证事务、状态迁移、回滚和管理 API 依赖额外的跨进程协议。

Agent VM 的 Go 服务已经是唯一的 Incus、生命周期、scheduler 和持久状态权威。新增一个浏览器控制权威会增加第二套输入过滤、错误映射、发布资产和恢复边界。另一方面，把浏览器处理放入当前高权限服务会扩大 Web 攻击面，不能被描述成与非 root Web 进程相同的隔离。

## Decision

每个蓝绿槽位只运行一个 Go 1.27.3/Gin Agent VM 服务进程。该服务同时提供固定的静态资源、同源 `/api/v1` 浏览器 API、OIDC Authorization Code + PKCE 回调、Token API、`/agent/v1/control` 和现有控制权威；它继续拥有 SQLite 迁移前状态或 rqlite 迁移后状态、Incus 调用、scheduler、generation 和 journal。不得引入独立路由器、常驻 Web/BFF、native service 或发布 wrapper。

Caddy 是唯一项目专用的外部入口。它负责 TLS、固定路径和活动槽位转发，只连接已验证的活动 Unix socket；它不执行用户登录、不接受或注入身份头、不实现资源授权，也不提供任意路径代理。服务端不接受浏览器提供的 `X-Forwarded-User` 或其他代理身份头。

浏览器认证使用第三方 OIDC 的 Authorization Code Flow + PKCE，且只接受 `code_challenge_method=S256`；`plain` 或其他方法必须拒绝。服务端在共享状态中记录一次性 `state`、`nonce`、受保护的 code verifier、回调 URI、过期时间和消费状态，并绑定短时 `Secure`、`HttpOnly`、`SameSite=Lax` login-binding cookie 的摘要；回调必须同时匹配两者，并在创建会话前清除 cookie。回调验证 issuer、subject、audience、签名、有效期、state、nonce、TLS 和 redirect URI。服务端和 Caddy 访问日志省略回调 query string 和 `Referer`，应用日志不保留 code、state、nonce 或 verifier。稳定身份严格是大小写敏感的 `(issuer, subject)`，不使用邮箱、显示名或 group claim。只有与配置的唯一管理员 allowlist 完全匹配的 OIDC 结果才能创建管理员会话并获得配置的 capability；其他主体必须拒绝，不能从登录本身、邮箱或 group claim 推导能力。

唯一管理员的 allowlist 和审计 actor 按完整 `(issuer, subject)` 持久化。管理员的浏览器会话和 Agent Token 按显式 capability/scope 访问所有受管 session；session_id 是工作会话资源标识，不是用户身份或 Token 绑定键。敏感操作仍需非空有界 reason、预览、一次性 confirmation token、当前 resource version/generation 和审计。审计保存主体二元组及 operation 的阶段和结果，不保存 Token、授权码或原始 native 诊断。

Agent Token 是高熵不透明 Bearer 值，验证摘要存入共享状态，明文只返回一次。Token 创建按管理员 actor、operation_id 和 canonical request digest 幂等；服务在同一持久化事务中消费一次性披露 latch 后才发送唯一明文响应，并把 `token_id`、元数据和披露状态写入 operation result。管理员的并发或重试只能读取安全元数据，不能再次取得明文；响应丢失时，管理员用原 operation ID 查询、确认 token_id 后显式撤销，再用新 operation ID 创建，服务不得自动重发或猜测 bearer。机器 Token 不要求预绑定 session；只要管理员签发、未过期/撤销且 scope 允许，Agent API 可对目标 session 执行 protocol-4 action。浏览器 Cookie 只用于同源管理 API，不能调用 Agent API；`/agent/v1/control` 只接受 Bearer Token，并在每个请求重新检查 Token 的撤销、过期、scope、目标 session、generation 和 lease。Agent API 复用 protocol 4，不建立第二套生命周期语义。

统一服务必须执行固定路由、方法、请求大小、字段白名单、Origin/CSRF、静态资源清单和结构化脱敏日志；不得向浏览器提供任意命令、文件路径、内部 control body、Incus 管理入口或调试 handler。服务继续以现有 Incus 所需权限运行；Caddy 以独立非 root UID 运行。该设计明确接受高权限 Web 请求处理这一取舍，并将隔离责任转移到输入边界、身份授权、原子状态检查、系统服务限制和审计上。

## Consequences

- 蓝色和绿色拥有同一套 Web、认证、Token、Agent 和生命周期代码，发布、readiness、状态能力检查和原子切换更容易绑定到同一 release identity。
- OIDC state、浏览器会话、Token 摘要和审计不依赖进程内内存或黏性路由，回调和会话可以跨槽位恢复。
- 服务故障或 Web 漏洞的影响面包含高权限进程；实现必须通过固定路由、最小输入、脱敏、systemd hardening、禁止任意代理和隔离测试证明缓解措施有效。
- Caddy 不再是认证或授权组件；部署不得通过额外 Forward Auth、代理身份头或第二个 Web 服务补充未定义的身份事实。
- 历史记录不需要映射到 OIDC 用户；缺少可验证的 task、generation、lease、operation journal 或 native identity 时必须停在控制面 reconciliation 状态。不得通过邮箱、首次登录或猜测的管理员身份重建资源状态。

## Rejected alternatives

- **独立非 root Web 进程和管理 UDS**：即 ADR 0042 的历史选择；增加进程和内部 RPC，且无法自然地把 OIDC 事务、共享会话、Token 和蓝绿发布绑定为一个可回滚槽位。
- **在 Caddy 或其他代理中实现登录和资源授权**：代理不拥有 Agent VM 的目标状态、generation、租约、容量或 journal 语义，代理身份头也无法替代服务端 issuer/subject 校验。
- **另建 Web/BFF 或常驻 native service**：会形成独立发布、状态和恢复边界，违反完整槽位、单一控制权威和不扩展部署组件的合同。
- **把浏览器 Cookie 或 JWT 当作 Agent Token**：Cookie 会把浏览器会话扩散到机器 API；JWT 会增加长期签名密钥和撤销边界。首期使用可撤销、只保存摘要的不透明 Token。
