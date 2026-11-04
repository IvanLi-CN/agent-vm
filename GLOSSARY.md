# Agent VM 领域术语

项目领域术语使用中文说明，保留准确的英文名称。

## 产品交付

**Agent VM 产品版本（Product version）**：客户端与控制环境共同使用的 SemVer。
它表达公开接口的兼容性承诺，独立于协议版本、持久状态格式版本和 guest 镜像身份。

**Agent VM 产品发布（Product release）**：为一个产品版本交付 GitHub Release 及其全部版本化安装包，且完成产物校验。
它的完成条件不包含任何现网安装或升级。

**Agent VM 部署（Deployment）**：将已发布产物安装或升级到明确选定的控制环境及客户端的独立操作，需要明确触发。

_Avoid_：用“发布成功”同时指代 GitHub 产物发布与现网部署；用源码 SHA、协议版本或数据库 schema 版本代替产品版本。

## 部署控制

**部署控制服务**：位于产品蓝绿槽位之外、代表维护者执行部署事务的控制面。
它观察控制环境并协调槽位，不是业务状态权威、公共路由器或业务 API。

**部署事务**：一次部署、切换、回滚或恢复从准备到最终结果的可追踪操作，包含阶段、目标槽位、证据和结果。

**部署槽位**：承载一个完整、可验证 Agent VM 控制服务版本的蓝色或绿色运行单元；槽位可以同时存活，但不因此同时拥有后台控制权。

**活动槽位**：当前受保护路由选择所指向的部署槽位。
活动槽位表示新请求的目标，不表示另一个槽位已经停止或没有在途请求。
_Avoid_：把活动槽位当成唯一存活进程或共享状态权威。

**控制器主权**：某个槽位执行 scheduler、reconciliation、age GC 和 scheduler-driven native mutation 的资格。
它独立于活动槽位和公共流量角色，同一时刻只能有一个槽位拥有该资格。

**流量排空**：槽位停止接受新的公共请求，同时允许已经开始的请求完成，并等待其 native worker 和 pending operation 收敛的阶段。
_Avoid_：把排空理解为立即停止进程或强制终止 guest。

## 受管资源与管理台

**Agent VM 管理台（Management console）**：维护者对 Agent VM 受管资源进行全局观察和授权管理的操作面。

**管理台演示（Web demo）**：复用管理台界面和交互、以模拟数据展示 Agent VM 管理行为的可分享体验。
演示中的资源和操作不代表真实受管资源或管理操作。

**演示控制面板（Demo inspector）**：用于选择和调整管理台演示场景，并查看模拟操作与分享状态的控制面。

**管理 API（Management API）**：内置管理台使用的同源 `/api/v1` 浏览器合同。
它独立于 CLI protocol 4、内部 control API 和部署侧观测接口。

**受管 VM（Managed VM）**：由 Agent VM 根据会话归属、资源和租约规则管理的工作环境。
管理台中的 VM 指已有 Agent 会话的工作环境。

**受管镜像（Managed image）**：纳入 Agent VM 管理、具有明确来源和不可变内容身份的基础环境。
新导入镜像必须分别通过完整性与适用性校验后才能被选择为新会话默认镜像；迁移保留的旧配置默认镜像是显式标记的兼容例外。

**会话镜像固定（Session image pinning）**：一个会话的 VM 及其缓存持续绑定同一镜像内容身份的规则。
切换默认镜像只影响新会话，旧会话换镜像需要明确销毁并重建。

**VM 缓存（VM cache）**：VM 停止后保留、可供同一会话恢复的工作环境及其数据。
它可以被回收，不构成持久数据保证。

**资源版本（Resource version）**：管理 API 为目标资源返回的不透明并发令牌。
写请求提交该令牌以检测目标变化；它不替代会话身份、generation 或产品版本。

**管理操作（Management operation）**：具有 UUID、操作者、原因、目标、阶段和结果的持久管理动作记录。
浏览器断开或 native 结果不确定时，仍可按 operation ID 查询；它不替代现有会话生命周期 journal。

**native_missing（native 缺失观测）**：一次精确、带实例与 project 上下文的 native not-found 观测。它是 status/admin-status 的 observation field，不是 task state；传输失败、超时、截断或上下文不匹配都不能设置它。

**outcome unknown（结果未知）**：native effect 或 SQLite commit 是否发生无法证明的 journal outcome。它必须保留原 operation identity、generation、digest 和 worker evidence，不能通过新 operation 重放。

**terminal-absent（终态 absent）**：只有 destroy journal、generation tombstone、同代 worker quiescence 和精确 native absence 都已验证后，才允许删除 durable task row 的恢复结论。它不是对单次 not-found 错误的别名。

**maintenance cursor（维护游标）**：`meta` 中 additive 的 JSON 状态，分别保存 reconcile 与 age-GC lane 的下一次尝试时间、退避次数和 task keyset 游标。它只在该轮 durable work 提交后前移，旧数据库没有该 key 时按立即执行处理。

_Avoid_：把受管镜像、VM 缓存与 Agent VM 产品发布包统称为“镜像”；把“释放租约”理解为保留访问权的普通关机。

## 身份与入口

**静态入口（Static entry）**：管理台提供的固定 HTML、JavaScript、CSS 和允许的页面深链接。静态入口可访问只证明产品资源可达，不证明浏览器已建立管理会话或获得任何管理权限。

**管理会话（Browser management session）**：由 OIDC 登录成功后创建、由服务端校验并授予管理员 capability 的浏览器身份。它与 Agent Token、OIDC 一次性事务和受管 VM 会话相互独立。

**认证门面（Authentication gate）**：管理台在加载受管数据前确认管理会话、呈现登录或失效状态并控制恢复的用户界面边界。它改善入口和状态表达，但不替代后端认证、授权或 CSRF 校验。

**登录回跳（Login return path）**：OIDC 登录事务中保存的、经过服务端校验的同源管理台页面路径。它只用于登录成功后的界面导航，不改变身份、能力或 API 授权范围。

## 资源与初始化

**Incus project**：Agent VM 所在的 Incus 隔离与策略边界。
它不是 Git 项目，也不等同于 Agent VM 控制服务的持久状态或资源 admission。

**项目级聚合配额（Project aggregate quota）**：由 Incus project 对多个实例合计施加的 CPU、内存、进程或实例数量限制。
Agent VM 部署基线不使用它作为控制服务 admission 上限。

**实例级资源限制（Instance resource limit）**：由 Incus profile 或实例配置对单个 Agent VM 容器施加的限制，例如 `limits.processes=512`。

**部署初始化基线（Deployment initialization baseline）**：在受控部署维护期间，对 Incus project/profile 必须检查或设置的最小状态集合；它不包含地址、凭据、库存、状态数据库或实时部署证据。
