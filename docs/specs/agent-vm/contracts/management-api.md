# Agent VM 管理 API 合同

内置管理台使用由 [management-api.yaml](management-api.yaml) 及其[合成共享示例](management/examples/)定义的同源 `/api/v1` 接口。API 由当前活动的蓝色或绿色 Agent VM 服务提供。它管理已有的 Agent VM 资源，并独立于 protocol 4 的 [Agent API](agent-api.md)、[Token API](token-api.md) 和部署观测桥接。

## 认证、身份与服务边界

项目 Caddy 入口终止 TLS，并只把固定路径转发到活动服务。它不执行 Forward Auth，不注入 `X-Forwarded-User`，不接受浏览器提供的身份头，也不决定资源授权。系统不存在独立 Web 进程、管理 UDS、公共 TCP 绕过、任意路由器或浏览器到控制面的代理。相同的 Go/Gin 服务提供静态资源、OIDC 回调、`/api/v1`、`/agent/v1/control`、生命周期规则和 native worker；高权限浏览器边界是 [ADR 0043](../../../adr/0043-agent-vm-unified-service-security-boundary.md) 明确记录的决策。

首个版本使用外部 OIDC provider 作为 relying party 身份服务。`GET /api/v1/auth/oidc/start` 创建一次性的 Authorization Code + PKCE 事务，只允许 `code_challenge_method=S256`，并设置短时 `Secure`、`HttpOnly`、`SameSite=Lax` login-binding cookie；`plain` 或其他 PKCE 方法必须拒绝。事务在共享存储中绑定随机 `state`、`nonce`、受保护的 code verifier、浏览器绑定 cookie 的摘要、准确的 redirect URI、client ID、创建/过期时间和消费状态。`GET /api/v1/auth/oidc/callback` 只接受成功响应（`code` + 匹配的 `state`）或 provider 拒绝/错误（`error` + 匹配的 `state` 且没有 `code`）；两种形式都只消费一次事务并清除绑定 cookie。provider 错误不得创建会话或回显 `error_description`/`error_uri`，而是跳转到通用的本地化登录失败状态；格式错误、缺失 state、值不匹配、跨浏览器转移或重放都必须失败关闭。服务必须校验 TLS、授权码交换、redirect URI、discovery issuer、ID Token 签名/JWKS、准确的 `iss`、`aud`/`azp`、`exp`、`iat` 策略和 `nonce`；provider 不可用时必须失败关闭。服务和 Caddy 的访问日志不得记录 callback query string 和 `Referer`，应用日志不得保留 OIDC code、state、nonce、verifier、错误描述或错误 URI。

服务以大小写敏感的 `(issuer, subject)` 二元组表示唯一管理员。issuer 必须是配置的 discovery issuer，subject 是 OIDC `sub`；邮箱、显示名、group 和转发头都不是身份键。只有该二元组匹配配置的管理员主体时，登录才可接受，会话才获得配置的管理员 capability。系统不存在普通用户角色、用户到资源关系或历史主体映射；旧 task 和 native 记录通过 task identity、generation、lease、journal 和 native identity 进行 reconciliation。

服务创建不透明浏览器会话，属性为 `Secure`、`HttpOnly`、`SameSite=Strict`、`Path=/` 和有界 TTL。会话元数据在共享状态中保存主体二元组、角色、capability、创建/过期/撤销状态以及会话标识摘要。`GET /api/v1/session` 返回主体、角色、capability 和绑定会话的 CSRF token。每个 `POST` 和 `PUT` 都必须携带 cookie、`X-CSRF-Token`，并携带与配置 HTTPS origin 完全相同的 `Origin`；禁用 CORS。浏览器会话不是 Agent Token，绝不能用于 `/agent/v1/control`。

静态管理入口必须与认证数据面分离。服务可以在没有浏览器会话时返回固定 HTML、JavaScript、CSS 和允许的管理页面深链接；该响应不得包含管理数据，也不能证明浏览器已经认证。直接访问没有有效会话时，SPA 应将页面入口收敛到公开主页 `/`；公开主页可以通过无需认证的 `GET /api/v1/public/status` 展示脱敏、固定间隔更新的服务状态，但不得复用管理员 `overview` 或暴露管理员身份、capability、容量和压力明细。公开状态可以短时缓存，不能作为管理授权或实时事实。SPA 必须先通过认证门面请求 `/api/v1/session`，再展示管理员主体、capability 或受管资源。收到 `authentication_required` 时必须显示本地化登录状态和明确的登录操作，不得形成无界 retry 循环。已有管理会话发生 session loss 时，认证门面必须卸载后台普通 layout，保留当前管理路径并改为独立 auth-error layout；该 layout 不得渲染后台导航、后台数据或管理员身份，并在顶栏提供直接发起 OIDC 的登录按钮。登录按钮必须使用当前 allowlisted `return_to`，不得绕过 OIDC 或自动恢复未确认的 mutation。

`GET /api/v1/auth/oidc/start` 可以接收 `return_to` 路径。服务必须把它限制为同源管理页面允许列表（`/`、`/vms`、`/images`、`/operations` 或 `/system`），拒绝重复参数、未知参数、绝对 URL、协议相对 URL、路径逃逸和其他路径，并存入一次性 OIDC 事务。`oidc_transactions` 的既有列不得改写；回跳路径保存在以 `state_digest` 为键的 `oidc_transaction_return_paths` companion table 中，并由 `oidc-return-path@1` capability marker 表示。旧事务缺少 companion row 时必须按 `/` 处理。会话创建后，成功 callback 跳转到该路径；未提供时跳转到 `/`。provider 拒绝、主体不在 allowlist 或 callback 失败时跳转到 `/?auth=failed`，不得暴露 provider 诊断信息。前端消费该标记并从地址栏清除。

任何管理 API 请求返回 `401 authentication_required` 或 `csrf_invalid` 时，前端集中式 `ManagementAuthState` 必须在 `loading`、`unauthenticated`、`authenticated`、`suspended` 和 `error` 之间转移：真实 API 首先 bootstrap `/api/v1/session`，未认证时不得显示管理员主体、capability 或受管资源；直接访问的未认证状态进入公开主页，已建立会话发生失效时卸载后台普通 layout，显示独立 auth-error layout，暂停 query 和 mutation，保留当前页面路径与已有 operation ID，等待用户明确重新认证。登录成功后重新 bootstrap `/api/v1/session` 并加载页面数据。前端不得自动重放 mutation；已经或可能已经提交的 mutation 必须继续绑定原 operation ID，并通过 operation endpoint reconciliation。

部署输入必须明确；任何必要配置不完整时，管理 listener 必须关闭：

| 字段 | 含义与校验 |
| --- | --- |
| `management.enabled` | 默认为 `false`；只有全部 OIDC、origin、session、Token 和 staging 限制校验通过后才可启用。 |
| `management.external_origin` | 一个规范化 HTTPS origin；使用非默认端口时必须包含端口；不得包含路径、通配符或列表。 |
| `management.oidc.issuer` | 准确的 discovery issuer URL；discovery metadata 和 ID Token `iss` 必须匹配。 |
| `management.oidc.client_id` | 注册的 client ID，绑定 callback 和 ID Token audience 校验。 |
| `management.oidc.redirect_uri` | 稳定 Caddy origin 下的一个准确 callback URI；每个登录事务都保存它。 |
| `management.oidc.client_secret` | 只用于授权码交换的受保护部署 secret；绝不写入仓库文件或日志。 |
| `management.admin_principal` | 一个准确的 `(issuer, subject)` 管理员 allowlist 条目及其 capability；不得只配置邮箱或 group。 |
| `management.session_ttl_seconds` | 浏览器会话时长为 60 到 3600 秒；默认 900 秒。 |
| `management.token_ttl_seconds` | Agent Token 的最大生命周期；每个 Token 仍有自己的过期和撤销状态。 |
| `management.upload.*` | 由镜像合同规定的有界镜像上传、私有 staging 和最低剩余空间限制。 |

## 授权与敏感操作

唯一管理员使用浏览器会话和 [Token API](token-api.md)。API 没有普通用户或跨主体资源边界：管理员可以检查和管理配置 capability 允许的所有目标。Token 创建必须使用 UUID operation ID 和绑定准确管理员 `(issuer, subject)` 的规范重试身份。请求 `agent:operate` scope 时必须同时显式提供 `agent:read` scope；这是共同要求，不是隐式继承，使同一 Token 能在响应丢失后查询状态和 reconciliation。成功的持久 operation 状态必须暴露 `token_id`、安全元数据和一次性披露状态；成功的 `token_create` 结果不得缺少这些字段，并发或重复请求也不得再次返回明文。创建响应丢失时，管理员查询原 operation，确认并按 ID 撤销 Token，再使用新的 operation ID 创建替代 Token。`agent:observe_admin` 是显式机器 scope，不是用户角色。下方 capability 矩阵是规范性合同：管理员角色不自动包含所有操作 capability，每个 endpoint 都必须拒绝缺少对应 capability 的请求。

| 浏览器 capability | 授权的管理面 | 额外授权规则 |
| --- | --- | --- |
| `read` | `GET /api/v1/session`；`GET /api/v1/operations` 和 `GET /api/v1/operations/{operation_id}`。 | 只有准确的管理员主体可以访问浏览器 API；operation 元数据仍受字段白名单限制。 |
| `operate` | VM `restart`、`resize`、`release`、`destroy`、`port_add` 和 `port_remove` 的操作预览与提交。 | 重新校验目标 `target_version`、generation、lease 和状态；敏感操作仍需预览和确认。可以提供审计 reason，但不是必填项。 |
| `manage_tokens` | Token 列表、读取、创建、撤销和撤销预览 endpoint。 | Token 撤销必须使用当前 target version 和绑定预览的 confirmation token。 |
| `manage_images` | 镜像列表、导入、上传、默认选择、退役、删除及其预览。 | 完整性、适用性、引用、target version 和确认校验都必须保留。 |
| `maintain_system` | 系统维护预览与提交，包括 drain、reconcile 和垃圾回收。 | 发布事务和 state-write fence 仍是权威；浏览器请求不得重新开放 admission。 |
| `observe_admin` | 概览、VM 清单、镜像/系统/审计视图和管理员观测页面。 | 此 capability 本身不授予 mutation、Token 签发或机器 `agent:observe_admin` scope。 |

Capability 是管理员内部的最小权限门控，不是普通用户角色、Token 到用户的授权或 session 绑定。Capability 绝不能从邮箱、group 或未经验证的 header 推导；未知的 action/capability 组合必须在任何持久化或 native effect 之前失败。

每个管理员 mutation 都必须携带 UUID `operation_id`；已有资源的 mutation 携带当前 `target_version`，VM action 还携带准确的预期 generation。包括 Token 签发在内的持久 create 使用 operation ID 和规范请求身份，不要求 target version 或预览。任何 mutation 都可以包含审计 `reason`；提供时必须非空且不超过 512 个字符，省略是合法的，不得因此拒绝请求。有效 lease、镜像默认值/引用、删除、维护或已有 Token 的变更必须先返回影响预览。Token 创建是持久 create 例外，不需要单独预览。Confirmation token 必须是不透明、短时（不超过五分钟）、一次性的值，并绑定已认证 `(issuer, subject)`、准确目标、action、参数、target version 和 generation。提交时服务必须再次校验授权、引用、generation、lease、capacity 和状态。

完全相同的 operation 重试返回相同的持久结果；Token 创建的结果在一次性明文披露 latch 消费后只包含安全元数据。使用相同 operation ID 但规范请求不同返回 `operation_conflict`。浏览器断开、超时或 `outcome_unknown` 不授权创建新的 operation ID。管理员查询 `GET /operations/{operation_id}` 和审计记录，直到服务确定结果。Token 撤销必须在发送 `202` 响应前，在共享状态中原子提交 `revoked_at` 并推进 Token `resource_version`；Agent 授权以 linearizable consistency 读取撤销状态，提交后尚未越过授权读取的请求必须被拒绝。未经验证的 native result 必须保持 pending，绝不能自动重放。

审计记录覆盖被拒绝和被接受的请求，以及成功、失败和未知结果。每条记录必须保存当前管理员 actor 的 `(issuer, subject)` 与角色/capability、没有角色/capability 的 typed `unauthenticated` 或 `unauthorized` rejected-request actor，或者保留原始历史值的显式 `legacy_unknown` actor 三者之一；不得猜测 OIDC 身份。`legacy_unknown` 只用于历史记录，rejected-request actor 不携带推导出的 OIDC 身份。提供时保存可选 reason，省略时明确记录为 absent，并同时保存 operation ID、目标、阶段、安全错误码和时间戳。不得保存 OIDC code、Token 值、凭据、原始命令输出、SQL 或私有部署路径。响应使用字段白名单的结构化错误，不暴露内部 stack trace。

## Token 与 Agent API 的关系

Token API 创建 `POST /agent/v1/control` 使用的不透明 Bearer credential；完整的机器 Token、scope、一次性明文和撤销规则见 [token-api.md](token-api.md)。Agent API 保留 protocol 4 及其 operation/generation/journal 语义；认证和兼容性合同见 [agent-api.md](agent-api.md)。管理 API 不得包装任意 protocol-4 请求，也不得接受 Agent Token 作为浏览器会话。

## 接口行为

`management-api.yaml` 是同源 `/api/v1` 浏览器 API 的 wire 合同。后端和前端消费相同的请求、响应和错误形状。未知字段、不支持的枚举值、多余属性、过期 target version 以及不在枚举 allowlist 中的 action 都必须拒绝。

列表 endpoint 使用不透明 keyset cursor。默认 limit 为 25，最大为 100。时间戳使用 UTC RFC 3339。字节数和 generation 使用十进制字符串，避免 TypeScript 客户端损失精度。缺失、过期、失败或未经验证的观测以其状态和 null 值返回，绝不能转换为零、false 或 healthy。

每个资源都暴露不透明的 `resource_version`。写请求使用该版本作为 `target_version`；VM action 还携带准确的 `expected_generation`。版本是并发保护值，不是用户可编辑标签。当 version、generation、lease、native identity 或镜像引用改变时，API 返回 `stale_target`。客户端必须重新加载资源并请求新预览，不得静默改目标或重放危险 action。

镜像导入从持久 operation 开始。本地上传在上传前声明准确字节数和 SHA-256；服务在 staging 前和 staging 期间都执行配置的大小上限和物理空间预留校验。可信来源导入先解析准确 fingerprint 和镜像大小，再对配置的 Incus pool 检查同一预留。两条路径都必须在镜像可被选择前完成完整性和适用性校验。

API 使用稳定的机器错误码、阶段和少量安全结构化字段白名单。前端把错误码映射为英文和简体中文文案；未知错误码使用通用本地化失败消息。响应绝不包含原始命令输出、凭据、私有路径、数据库内容或内部 stack trace。Admission drain 时，System 页面必须显示恢复由发布事务管理，不提供浏览器 `drain_exit` 控件；只有活动发布事务可以重新开放 admission。

## 产品与语言行为

首个版本严格只有两种界面 locale：`zh-CN` 和 `en`。选择优先级为已保存的用户选择、浏览器语言、英文。每个导航标签、字段标签、可访问名称、确认、校验消息、状态说明和错误都必须提供两种翻译。技术标识、fingerprint、错误码和用户输入内容不翻译。

首屏打开 Overview（概览）。导航覆盖 Overview（概览）、VMs、Images、Operations & Audit（操作与审计）和 System（系统）。VM 页面始终同时显示选中资源身份、状态证据和允许的操作上下文，不提供手动创建 VM 流程。概览、VM、镜像、operation 和系统数据保留来源新鲜度与 unknown 状态。设计方向和响应式行为见 [DESIGN.md](../../../../DESIGN.md) 与 [Web 界面简报](../../../../.impeccable/surfaces/web.md)。

## 镜像与 VM 不变量

统一本地上传使用一个 `application/octet-stream` payload，并声明一个预期大小和 SHA-256。拆分本地上传使用 `multipart/form-data`，且必须恰好包含一个 `metadata` 和一个 `rootfs` part；导入请求分别声明每个 part 的预期大小和 SHA-256，上传上限适用于两个 payload 大小之和。原始文件名不得作为 Incus alias 或文件系统路径使用。

镜像由不可变的 Incus fingerprint 标识。版本标签和配置的 source ID 是元数据，不是身份。可信远程导入只接受已配置的 `source_id` 和预期 fingerprint，不接受任意 URL、alias 或 credential。本地上传在流开始前声明其包格式、准确字节数和 SHA-256。上传必须符合配置的最大值和 staging 预留，只接收一次且不可续传。过短、超大、摘要不匹配或中断的流都必须成为可查询的失败 operation，并清理不完整的 staging 数据。

完整性和 Agent VM 适用性是分开的结果。验证使用有界、隔离的临时 system container，不携带生产凭据、不挂载宿主目录、也不接入生产网络。只有完整性和适用性都通过的镜像才能被选为新默认值。选择变更只影响新会话；已有 task 和 cache 继续固定到存储的 fingerprint。退役镜像不得被新选择，但不使已有绑定失效。镜像为默认值，或存在 active、cached、releasing、pending-creation、verification 任一引用时，都必须拒绝删除；引用检查必须与镜像操作串行化。

## 状态迁移与包回滚

管理状态采用增量方式。`state_capabilities` registry 包含版本化 marker，API 将每个 marker 渲染为 `name@version`。初始 capability 集合保留既有 protocol-4 task、generation、lease、reservation、port 和 journal marker，并增加管理目录、session-image binding、operation-audit、OIDC return-path、principal-identity 和 Token-revocation marker。迁移必须保留 task ID、session ID、镜像 fingerprint、generation、lease、port、reservation、operation journal 和现有审计证据。旧 operation/audit actor 字符串只要无法直接证明配置的管理员身份，就按 `legacy_unknown` 保留；服务不得猜测或伪造 OIDC actor，也不得创建 user 到 session 或 user 到 Token 的 ownership 映射。

在迁移到 rqlite 前，SQLite 仍是唯一写入者。[ADR 0044](../../../adr/0044-agent-vm-state-migration-to-rqlite.md) 描述的保守迁移会进入 state-write fence，拒绝新的 browser/API/Agent 持久 mutation，暂停 scheduler/reconciliation/timer 的持久写入，并要求每个在途 native worker 在原 operation ID 下以持久结果结束，同时验证 process group、PID identity 和 Incus operation 已静止。任何 unresolved pending/unknown result、active worker 或未经验证的静止状态都会阻止最终导出；不能只记录结果后继续。只有全部事务和 worker 收敛后，服务才能关闭 SQLite writer 并导出一致备份。迁移必须校验 schema/capability/row count 和身份摘要，通过一个 rqlite transaction 导入，并在激活共享后端前完成验证。不得双写、从两个槽位打开共享 SQLite 文件、降级 schema 或重建状态以强制回滚。切换后 rqlite 是唯一在线权威；关键读取和观测 epoch 边界使用 linearizable consistency。rqlite 不可用时，服务必须封锁持久 mutation 和 native admission，同时保留有效 lease，不得猜测。经验证的 rqlite backup/snapshot 是首选恢复源，必须记录不早于最后可信提交的 coverage boundary。恢复必须使用可信 append-only journal 和 native-effect evidence 对该边界之后的 backup tail 做 reconciliation；schema、digest、epoch 或 journal 检查本身不足以证明覆盖完整。无法证明 backup tail 或切换后的任一写入时，恢复必须继续 fenced，等待管理员授权的 journal/native reconciliation；不得把旧 SQLite 作为竞争权威打开，也不得靠猜测重建状态。

包回滚期间绝不降级或重建数据库。已安装包只有在声明支持读写所有现存 capability marker 时才可激活。若目标无法保留管理员 issuer/subject actor、无管理员权限的 typed rejected-request actor、显式未知历史 actor、Token revocation、journal、generation 和 unknown-outcome 语义，必须在变更活动槽位之前拒绝。当前包保持选中，admission 保持 drain，等待操作员恢复。

## 兼容窗口

蓝绿发布合同至少保留当前和上一个已验证包，以及固定静态资源清单，持续一小时的 API/state 兼容窗口。每个包声明 protocol、request/response/semantic window、Token 和管理 API 版本，以及 state capability read/write 集合。收敛期间只有 operation 位于共享兼容集合中时两个槽位才可接收请求；Caddy 绝不重试写入。超过窗口的浏览器资源必须收到明确的刷新响应。SSH `legacy-bun`/`go-request` 在声明窗口结束前继续作为兼容传输；直接 HTTPS 使用相同的 protocol-4 body，不改变生命周期语义。

## 共享合同示例

本合同旁边的 JSON 文件是合成 fixture，不包含部署事实，供后端和前端合同测试共享：

- `overview.json` 覆盖来源新鲜度分离和未经验证的压力观测。
- `vms-page.json` 覆盖固定镜像、generation、有效 lease 和有界 port 数据。
- `images-page.json` 覆盖已验证候选镜像和保留的旧版兼容默认值。
- `image-import-request.json` 覆盖准确大小的本地上传声明。
- `image-import-split-request.json` 覆盖分离的 metadata 和 rootfs payload 声明。
- `vm-action-request.json` 覆盖绑定预览的 restart 请求。
- `operation.json` 覆盖持久异步进度和当前 issuer/subject actor。
- `audit-page.json` 覆盖当前 actor、typed rejected-request actor 或显式未知历史 actor，以及可选 reason、目标和结果。
- `system.json` 覆盖统一服务、Caddy、rqlite 和 Incus readiness。
- `error.json` 覆盖不包含内部诊断的 stale-target 冲突。
# Agent VM 管理合同

内置 management console 使用[management-api.yaml](management-api.yaml)及其[synthetic shared examples](management/examples/)定义的 same-origin `/api/v1` surface。API 由 active blue 或 green Agent VM service 提供。它管理 existing Agent VM resources，并独立于 protocol-4 [Agent API](agent-api.md)、[Token API](token-api.md)和 deployment observation bridge。

## 认证、身份与服务边界

项目 Caddy 入口终止 TLS，只向 active service 转发固定路径。
它不执行 Forward Auth，不注入 `X-Forwarded-User`，不接受浏览器提供的身份标头，也不决定资源授权。
系统没有独立 Web 进程、公共管理 UDS、公共 TCP 绕过路径、任意路由器或浏览器到控制服务的代理。
同一个 Go/Gin 服务提供静态资源、OIDC 回调、`/api/v1`、`/agent/v1/control`、生命周期规则和 native worker；
运维者专用的 deployd Unix socket 是独立的部署边界，Caddy 和浏览器都不可访问。
高权限浏览器边界是[ADR 0043](../../../adr/0043-agent-vm-unified-service-security-boundary.md) 明确记录的设计决定。

首个 release 使用 external OIDC provider 作为 relying-party identity service。
`GET /api/v1/auth/oidc/start` 创建一次性的 Authorization Code + PKCE transaction，仅允许 `code_challenge_method=S256`，并设置短时 `Secure`、`HttpOnly`、`SameSite=Lax` login-binding cookie；
`plain` 或其他 PKCE method 必须拒绝。
Transaction 在 shared storage 中绑定 random `state`、`nonce`、protected code verifier、browser-binding cookie 的 digest、exact redirect URI、client ID、creation/expiry time 和 consumed state。
`GET /api/v1/auth/oidc/callback` 接受 successful response（`code` + matching `state`）或 provider denial/error（`error` + matching `state` 且无 `code`）；
两种形式都只消费一次 transaction，并清除 binding cookie。
Provider error 重定向到 generic localized login-failure state，不创建 session，也不回显 `error_description`/`error_uri`；
malformed、missing-state、mismatch、cross-browser transfer 或 replay 必须 fail closed。
Service 验证 TLS、authorization code exchange、redirect URI、discovery issuer、ID Token signature/JWKS、exact `iss`、`aud`/`azp`、`exp`、`iat` policy 和 `nonce`；
provider outage 必须 fail closed。
Service 与 Caddy 从 access logs 中删除 callback query strings 和 `Referer`；
application logs 绝不保留 OIDC code、state、nonce、verifier、error description 或 error URI values。

Service 将 sole administrator 表示为精确且区分大小写的 `(issuer, subject)` pair。
Issuer 是 configured discovery issuer，subject 是 OIDC `sub`；
email、display name、groups 和 forwarded headers 不是 identity keys。
只有该 pair 匹配 configured administrator principal 时，successful login 才接受，并为 session 授予 configured administrator capabilities。
不存在 ordinary-user role、user-resource relation 或 historical principal mapping；
legacy task 和 native records 根据 task identity、generation、lease、journal 和 native identity reconcile。

Service 创建 opaque browser session，使用 `Secure`、`HttpOnly`、`SameSite=Strict`、`Path=/` 和 bounded TTL。
Session metadata 在 shared state 中存储 principal pair、role、capabilities、creation/expiry/revocation state 以及 session identifier 的 digest。
`GET /api/v1/session` 返回 principal、role、capabilities 和 session-bound CSRF token。
每个 `POST` 和 `PUT` 携带 cookie、`X-CSRF-Token` 以及与 configured HTTPS origin 完全相等的 `Origin`；
CORS disabled。
Browser session 不是 Agent Token，`/agent/v1/control` 永不接受它。

Required deployment inputs 必须显式提供；
configuration 不完整时，management listener disabled：

| 字段 | 含义与校验 |
| --- | --- |
| `management.enabled` | 默认值为 `false`；只有所有 required OIDC、origin、session、Token 和 staging limits 验证通过后才启用。 |
| `management.external_origin` | 一个 normalized HTTPS origin；使用 non-default port 时必须包含该 port；不允许 path、wildcard 或 list。 |
| `management.oidc.issuer` | Exact discovery issuer URL；discovery metadata 和 ID Token `iss` 必须匹配。 |
| `management.oidc.client_id` | 绑定 callback 和 ID Token audience check 的 registered client ID。 |
| `management.oidc.redirect_uri` | stable Caddy origin 下的一个 exact callback URI；存储在每个 login transaction 中。 |
| `management.oidc.client_secret` | 仅用于 authorization-code exchange 的 protected deployment secret；不得存储在 repository files 或 logs 中。 |
| `management.admin_principal` | 一个带 configured capabilities 的 exact `(issuer, subject)` administrator allowlist entry；不接受仅 email 或 group 的 entry。 |
| `management.session_ttl_seconds` | Browser session lifetime 为 60 至 3600 秒；默认 900 秒。 |
| `management.token_ttl_seconds` | Maximum Agent Token lifetime；每个 Token 还携带自己的 expiry 和 revocation state。 |
| `management.upload.*` | 由 image contract 定义的 bounded image upload、private staging 和 minimum free-space limits。 |

## 授权与敏感操作

Sole administrator 使用 browser session 和[Token API](token-api.md)。API 没有 ordinary-user 或 cross-principal resource boundary：administrator 可以 inspect/manage configured capability 允许的每个 target。Token creation 要求 UUID operation ID 和绑定 exact administrator `(issuer, subject)` pair 的 canonical retry identity。Requested `agent:operate` scope 必须同时显式带有 `agent:read` scope；这是 co-requirement，不是 implicit inheritance，因此 response 丢失后同一个 Token 仍可 query status/reconciliation。Succeeded durable operation status 暴露 `token_id`、safe metadata 和 one-time disclosure state；successful `token_create` result 不能省略这些 fields，并且 concurrent 或 repeated request 绝不重新签发 plaintext。Creation response 丢失时，administrator query original operation，使用 new operation ID 按 ID 确认并 revoke Token，然后用另一个 operation ID 创建 replacement。 `agent:observe_admin` 是 explicit machine scope，不是 user role。
下方 capability matrix 是 normative：administrator role 不代表拥有每个 action capability，每个 endpoint 都拒绝缺少对应 mapped capability 的 request。

| Browser capability | Authorized management surface | Additional authorization rule |
| --- | --- | --- |
| `read` | `GET /api/v1/session`；`GET /api/v1/operations` 和 `GET /api/v1/operations/{operation_id}`。 | 只有 exact administrator principal 可以访问 browser API；operation metadata 仍受 field whitelist 约束。 |
| `operate` | VM action preview 和 `restart`、`resize`、`release`、`destroy`、`port_add`、`port_remove` 的 submission。 | 重新检查 target `target_version`、generation、lease 和 state；sensitive actions 仍要求 preview 和 confirmation。可以提供 audit reason，但不是必需项。 |
| `manage_tokens` | Token list、read、create、revoke 和 revoke-preview endpoints。 | Token revocation 要求 current target version 和 preview-bound confirmation token。 |
| `manage_images` | Image list、import、upload、default selection、retirement、deletion 及其 previews。 | Integrity、suitability、reference、target-version 和 confirmation checks 仍是必需项。 |
| `maintain_system` | System maintenance preview 和 submission，包括 drain、reconcile 和 garbage collection。 | Release transaction 和 state-write fence 仍是权威；browser request 不能 reopen admission。 |
| `observe_admin` | Overview、VM inventory、image/system/audit views 和 administrator observation pages。 | 该 capability 本身不授予 mutation、Token issuance 或 machine `agent:observe_admin` scope。 |

Capabilities 是 administrator-internal least-privilege gates，不是 ordinary-user roles、Token-to-user grants 或 session bindings。
Capabilities 绝不从 email、group 或 unverified header 推断；
unknown action/capability combinations 必须在任何 durable 或 native effect 前失败。

Every administrator mutation 携带 UUID `operation_id`；
existing resource 的 mutation 携带 current `target_version`，VM actions 还携带 exact expected generation。
Durable creates（包括 Token issuance）使用 operation ID 和 canonical request identity，不要求 target version 或 preview。
任何 mutation MAY 包含 audit `reason`；
提供时必须 non-empty 且不超过 512 characters，省略合法，不会拒绝 request。
Effective lease、image default/reference、deletion、maintenance 或 existing Token 的 change，首先返回 impact preview。
Token creation 是 durable-create exception，不需要 separate preview。
Confirmation token 是 opaque、short-lived（不超过 five minutes）、single-use，并绑定 authenticated `(issuer, subject)`、exact target、action、parameters、target version 和 generation。
Submission 在 service 内重新执行 authorization、reference、generation、lease、capacity 和 state checks。

Identical operation retry 返回相同 durable outcome；
Token creation 在 one-time plaintext disclosure latch 被消费后，outcome 只包含 safe metadata。
使用不同 canonical request 重用 operation ID 时返回 `operation_conflict`。
Browser disconnect、timeout 或 `outcome_unknown` state 不授权创建 new operation ID。
Administrator 查询 `GET /operations/{operation_id}` 和 audit record，直到 service 建立 outcome。
Token revoke 在发送 `202` response 前，在 shared state 中原子 commit `revoked_at` 并推进 Token `resource_version`；
Agent authorization 以 linearizable consistency 读取 revocation，因此 commit 后尚未越过 authorization read 的 request 会被拒绝。
未验证的 native result 保持 pending，绝不自动 replay。

Audit records 覆盖 rejected/accepted requests，以及 successful、failed 和 unknown outcomes。
每条 record 存储以下三者之一：current administrator actor 的 `(issuer, subject)` 和 role/capability；
没有 role/capability 的 typed `unauthenticated` 或 `unauthorized` rejected-request actor；
或明确的 `legacy_unknown` actor，并逐字保留 original historical value；
绝不猜测 OIDC identity。
`legacy_unknown` 仅用于 historical records；
rejected-request actors 不携带 inferred OIDC identity。
提供 optional reason 时存储，省略时明确保持 absent，并同时存储 operation ID、target、phase、safe error code 和 timestamp。
绝不存储 OIDC codes、Token values、credentials、raw command output、SQL 或 private deployment paths。
Responses 使用 field-whitelisted structured errors，不暴露 internal stack traces。

## Token 与 Agent API 的关系

Token API 创建 `POST /agent/v1/control` 使用的 opaque Bearer credentials；
完整的 machine-Token、scope、one-time plaintext 和 revocation rules 见[token-api.md](token-api.md)。Agent API 保留 protocol 4 及其 operation/generation/journal semantics；authentication 和 compatibility contract 见[agent-api.md](agent-api.md)。
Management API 不得包装 arbitrary protocol-4 request，也不得将 Agent Token 作为 browser session 接受。

## Wire 行为

`management-api.yaml` 是 same-origin `/api/v1` browser API 的 wire contract。
Backend 和 frontend 消费相同的 request、response 和 error shapes。
Unknown fields、unsupported enum values、extra properties、stale target versions 以及不在 enumerated allowlist 中的 actions 都必须拒绝。

List endpoints 使用 opaque keyset cursors。
Default limit 为 25，maximum 为 100。
Timestamps 是 UTC RFC 3339 values。
Byte counts 和 generation values 使用 decimal strings，避免 TypeScript clients 丢失 precision。
Missing、stale、failed 或 unverified observation 返回其 state 和 null value，绝不转换为 zero、false 或 healthy。

Every resource 暴露 opaque `resource_version`。
Write requests 将该 version 作为 `target_version` 携带；
VM actions 还携带 exact `expected_generation`。
Version 是 concurrency guard，不是 user-editable label。
当 version、generation、lease、native identity 或 image references 变化时，API 返回 `stale_target`。
Client 必须 reload resource 并 request new preview；
不得静默 retarget 或 replay dangerous action。

Image import 以 durable operation 开始。
Local upload 在 upload 前声明 exact byte counts 和 SHA-256 values；
service 在 staging 前及 staging 期间应用 configured size ceiling 和 physical-space reserve。
Trusted-source import 先解析 exact fingerprint 和 image size，然后在 native copy 前针对 configured Incus pool 检查同一 reserve。
两条路径都必须在 image 可被 selected 前运行 integrity 和 suitability checks。

API 使用 stable machine error codes、phases 和小范围 safe structured fields allowlist。
Frontend 将 codes 映射为 English 和 Simplified Chinese copy。
Unknown codes 使用 generic localized failure message。
Responses 绝不包含 raw command output、credentials、private paths、database content 或 internal stack traces。
Admission drained 时，System page 显示 recovery 由 release 管理，不提供 browser `drain_exit` control；
只有 active release transaction 可以 reopen admission。

## 产品与语言行为

首个 release 只有两个 interface locales：`zh-CN` 和 `en`。
Selection priority 依次是 saved user choice、browser language、English。
每个 navigation label、field label、accessible name、confirmation、validation message、status explanation 和 error 都有两种翻译。
Technical identifiers、fingerprints、error codes 和 user-authored content 不翻译。

First viewport 打开 Overview。
Navigation 包含 Overview、VMs、Images、Operations & Audit 和 System。
VM page 同时保持 selected resource identity、status evidence 和 allowed action context 可见。
不提供 manual VM creation flow。
Overview、VM、image、operation 和 system data 保留 source freshness 与 unknown state。
Design direction 和 responsive behavior 见[DESIGN.md](../../../../DESIGN.md)及[Web surface brief](../../../../.impeccable/surfaces/web.md)。

## Image 与 VM 不变量

Unified local uploads 使用一个 `application/octet-stream` payload，携带一个 expected size 和 SHA-256。
Split local uploads 使用 `multipart/form-data`，且恰好包含一个 `metadata` part 和一个 `rootfs` part；
import request 为每个 part 声明自己的 expected size 和 SHA-256，upload ceiling 作用于两个 payload sizes 之和。
Original filenames 不作为 Incus aliases 或 filesystem paths。

Image 由 immutable Incus fingerprint 标识。
Version labels 和 configured source IDs 是 metadata，不是 identity。
Trusted-remote import 只接受 configured `source_id` 和 expected fingerprint，不接受 arbitrary URL、alias 或 credential。
Local upload 在 stream 开始前声明 package format、exact byte count 和 SHA-256。
Upload 必须符合 configured maximum 和 staging reserve，只接收一次且不可 resumable。
Short、oversized、mismatched 或 interrupted stream 会成为 queryable failed operation，其 incomplete staging data 会清理。

Integrity 和 Agent VM suitability 是独立 results。
Verification 使用有界、隔离的 temporary system container，不带 production credentials、host mounts 或 production network access。
只有 integrity 和 suitability 都 passed 的 image 才能作为 new default 被 selected。
Selection changes 只影响 new sessions。
Existing tasks 和 caches 继续 pin 到 stored fingerprint。
Retiring image 会阻止 new selection，但不使 existing bindings 失效。
Image 为 default，或存在 active、cached、releasing、pending-creation 或 verification reference 时，deletion 被拒绝；
reference checks 与 image operations 串行化。

## 状态迁移与包回滚

首次从 legacy 单实例切换到蓝绿布局时，发布器必须先启动槽位外的 `agent-vm-deployd.service`，再自动调用其 `POST /v1/migrate`。
Deployd 负责排空旧服务、确认没有 SQLite writer、创建迁移源 snapshot、启动并验证固定的 rqlite 单元、执行一次性导入和结果校验；
发布器不得要求运维者先手工导出、导入或停止旧服务，也不得在槽位启动后补做迁移。
迁移失败或结果不确定时，Deployd 返回原 transaction ID 并保持 `needs_recovery` 和 fence，发布器最多按该 transaction 自动执行一次 `retry-migration`。

普通 `stage`、`activate`、`rollback` 和管理 API 请求不触碰业务 SQLite 或 rqlite。
迁移成功后 rqlite 是唯一业务状态权威，Deployd 的 SQLite 只保存部署事务、阶段事件、失败证据和迁移 snapshot 引用，不保存业务资源状态。

Management state 是 additive。
`state_capabilities` registry 包含 versioned markers，API 将每个 marker 渲染为 `name@version`。
Initial capability set 保留既有 protocol-4 task、generation、lease、reservation、port 和 journal markers，并新增 management catalog、session-image binding、operation-audit、principal-identity 和 Token-revocation markers。
Migration 保留 task IDs、session IDs、image fingerprints、generations、leases、ports、reservations、operation journals 和 existing audit evidence。
除非 configured administrator identity 有直接证据，legacy operation/audit actor strings 保留为 `legacy_unknown` values；
service 绝不猜测或伪造 OIDC actor。
不创建 user-to-session 或 user-to-Token ownership mappings。

Pre-rqlite phase 中 SQLite 继续作为 single writer。
[ADR 0044](../../../adr/0044-agent-vm-state-migration-to-rqlite.md)描述的 conservative migration 进入 state-write fence，拒绝新的 browser/API/Agent durable mutations，暂停 scheduler/reconciliation/timer durable writes，并要求每个 in-flight native worker 在 original operation ID 下以 durable result 完成，同时验证 process-group、PID-identity 和 Incus-operation quiescence。
Unresolved pending/unknown result、active worker 或 unverified quiescence 会阻止 final export；
不能只记录 outcome 然后继续。
只有所有 transactions 和 workers converge 后，service 才关闭 SQLite writers 并导出 consistent backup。
它验证 schema/capability/row-count 和 identity summaries，通过一个 rqlite transaction import，并在激活 shared backend 前完成 validation。
绝不 dual-write、从两个 slots 打开 shared SQLite file、downgrade schema 或 recreate state 来强制 rollback。
Cutover 后 rqlite 是唯一 live authority；
critical reads 和 observation epoch boundaries 使用 linearizable consistency。
如果 rqlite unavailable，service fence durable mutations 和 native admission，并在不猜测的情况下保留 active leases。
Verified rqlite backup/snapshot 是第一恢复来源，必须记录不早于 last trusted commit 的 coverage boundary。
Recovery 必须使用 trusted append-only journal 和 native-effect evidence reconcile 该 boundary 之后的 backup tail；
仅 schema、digest、epoch 或 journal checks 不能证明 coverage。
只有证据证明没有 post-cutover durable write、epoch advance、lease change 或 native effect 时，pre-cutover SQLite snapshot 才可 re-import。
如果无法证明 backup tail 或任何 post-cutover write，recovery 保持 fenced，等待 administrator-authorized journal/native reconciliation；
不得打开 stale SQLite 作为 competing authority，也不得通过猜测重建 state。

Package rollback 期间绝不 downgrade 或 recreate database。
Installed package 只有声明支持 read/write 每个 present capability marker 时才能 activation。
无法保留 administrator issuer/subject actor、没有 administrator authority 的 typed rejected-request actors、explicit unknown historical actor values、Token revocation、journal、generation 和 unknown-outcome semantics 的 target，必须在改变 active slot 前拒绝。
Current package 保持 selected，admission 保持 drained，等待 operator recovery。

## 兼容窗口

Blue-green release contract 至少保留 current 和 previous verified packages 及其 fixed static resource manifests 一小时。
每个 package 声明 protocol、request/response/semantic windows、Token 和 management API versions，以及 state capability read/write sets。
Convergence 期间，只有 operation 位于 shared compatibility set 时，两个 slots 才能接收 requests；
Caddy 绝不 retry writes。
超过窗口的 browser asset 收到明确的 refresh response。
SSH `legacy-bun`/`go-request` 在声明窗口结束前继续作为 compatibility transport；
direct HTTPS 使用相同的 protocol-4 body，不改变 lifecycle semantics。

## 共享合同示例

本合同旁的 JSON files 是 synthetic fixtures，不包含 deployment facts，供 backend 和 frontend contract tests 共享：

- `overview.json` 覆盖 separate freshness 和 unverified pressure observation。
- `vms-page.json` 覆盖 pinned image、generation、effective lease 和 bounded port data。
- `images-page.json` 覆盖 verified candidate 和 preserved legacy compatibility default。
- `image-import-request.json` 覆盖 exact-size local upload declaration。
- `image-import-split-request.json` 覆盖 separate metadata 和 rootfs payload declarations。
- `vm-action-request.json` 覆盖 preview-bound restart request。
- `operation.json` 覆盖 durable asynchronous progress 和 current issuer/subject actor。
- `audit-page.json` 覆盖 current、typed rejected-request 或 explicitly unknown historical actor，以及 optional reason、target 和 outcome。
- `system.json` 覆盖 unified service、Caddy、rqlite 和 Incus readiness。
- `error.json` 覆盖不带 internal diagnostics 的 stale-target conflict。
