# Decision Memo：Agent VM 管理台演示

## 背景与范围

为维护者、评审者和潜在使用者提供可分享的独立静态 Web Demo，以模拟数据体验 Agent VM 管理台。演示复用正式管理台的页面、路由和 API 调用面；它不证明正式管理台已经交付，也不连接真实受管资源。

本记录整理已确认的需求与推荐基线，不是实施计划或正式产品 Spec。实际公开发布、托管位置和生产环境接入均不由本记录授权。

## 仓库与参考证据

- [管理台产品边界](../../PRODUCT.md)和[管理 API 合同](../specs/agent-vm/contracts/management-api.md)定义了五个页面、已有会话 VM、镜像和受控维护操作；演示不扩展正式产品的权限或资源范围。
- `web/src/app/api.ts` 使用构建环境变量在真实 API 与模块内模拟数据之间切换；Demo 构建通过显式运行时和 `/demo` 路径复用正式页面与路由后缀，并在模拟写入后更新内存模型、操作记录和审计。
- `package.json` 锁定 React Doctor `0.9.18` 与版本化 `doctor.config.json`，提供 staged/changed 两种门禁命令；CI 在现有 PR/Main gate 中执行 changed 检查、静态 Demo 构建和 Storybook accessibility 浏览器检查。Storybook 已配置 accessibility addon，页面级浏览器测试覆盖 Demo 隔离与响应式控制面板。
- Style Playbook 的 `web-demo` Topic 以 `projects/octo-rill.md` 为结构化控制面板、隔离运行时和分享链接的主要参考；`projects/loadlynx.md` 与 `projects/mains-aegis.md` 支持复用正式界面、路由与安全模拟数据。Storybook 不能替代页面级演示。
- 对 `web-demo`、`react-app`、`typescript-web`、`shadcn-ui`、`playwright-e2e`、`storybook-component-qa`、`storybook-accessibility`、`responsive-web-ui`、`ui-version-display` 和 `embedded-admin-ui` 的本地 Policy 审计均无适用 Policy closure。`quality-gates` 审计验证本地 package 和 lock 已安装；此结果不证明各 Topic 的功能实现或平台规则已对齐。

## 推荐与已确认需求

### 演示与隔离

- Demo 使用独立构建目标及可静态托管的子路径，复用正式 App 和正式路由后缀。Demo bootstrap 在渲染、认证及初始数据请求之前完成模拟环境准备；准备失败时停止并展示安全错误，不得回退到真实 API。
- 模拟写入仅改变内存模型，在正式界面回显影响并留下明确标记的近期模拟操作记录。刷新恢复由场景和分享设置确定的初始状态，不持久化操作结果。文件上传仅模拟流程，不读取、处理或保存用户文件。
- 演示标识、模拟身份和模拟数据须清楚可辨；可显示可追溯的构建版本或修订，不能把模拟数据或版本当作真实部署证明。分享链接不得包含用户填写的原因、文件内容、凭据或其他敏感数据。
- 支持正式管理台范围内 VM、镜像和系统维护的预览、填写原因、确认、状态变化与操作记录；包含冲突及不确定结果的可查询状态。模拟行为应体现既有管理合同，不能暗示真实操作已执行。

### 场景与控制面板

首版提供正常运行、容量或数据异常、操作冲突或结果不确定、空状态四类预设。网络故障可独立叠加；未知、过期和不可用不得显示为健康零值。模拟身份和权限状态覆盖既有管理员访问与拒绝访问，不新增正式 RBAC 角色。

演示控制面板以结构化字段提供场景、身份与权限状态、网络、数据开关、模拟操作记录和分享入口。场景及可分享设置以 URL 为准；面板布局只在本地保存。更新场景与分享状态使用客户端导航，避免整页重载；正式页面路径、浏览器前进后退及静态子路径深链刷新均须可用。

宽屏可停靠，普通桌面可使用有安全边界的浮层，移动端使用可收起的入口和抽屉；各布局均应允许收起与恢复，不遮蔽待审查的正式界面。

Demo Inspector 的结构、布局、位置控制、浮动拖动热区、键盘替代、响应式和可访问性要求以[Web Demo Inspector UX/UI 合同](../specs/agent-vm/contracts/web-demo-inspector.md)为规范来源。位置图标不是关闭按钮：单击只应用当前视口默认布局，右键、长按或 `Shift+F10` 才打开位置菜单；浮动模式必须显示独立的 `Drag`/`拖动` 手柄，不能把标题文字作为隐含热区。

### 验收与相邻对齐

- 使用页面级浏览器验证覆盖隔离及启动失败、五个页面、模拟写入与记录、异常和空状态、权限拒绝、URL 分享与深链刷新、浏览器导航、面板响应式布局及键盘操作。验证不得依赖真实服务或资源。
- Storybook 继续承担组件和交互检查，新增 accessibility 检查并覆盖控制面板关键组件；自动检查不能替代键盘与页面级检查。
- React Doctor 采用锁定版本与版本化配置，本地针对 staged React 文件、CI 针对 changed React 文件；诊断 error 阻断，warning 可见。范围无法计算时失败，确认空集合时跳过。
- `web/` 已建立可审计的 shadcn/ui 基线：`components.json`、`@/components/ui`/`@/lib/utils` aliases、Tailwind v4、CVA、`clsx`/`tailwind-merge`、Radix primitives 和 Lucide icons。Demo Inspector 的 Button、Select、ContextMenu/DropdownMenu、Tooltip 直接使用 checked-in open-code primitives；其余旧页面通过兼容导出渐进迁移，不因库名要求一次性重写全部旧组件。
- 优先将 Demo 构建、浏览器测试和 React Doctor 纳入现有稳定 PR gate，按平台能力分配检查；检查失败必须传递到对应 gate。若改变 required check 集合，更新 `.github/quality-gates.json`，并在获适当授权后核验 GitHub 分支保护与声明一致。当前平台状态未经本次核验。
- 正式管理台的真实产品版本、认证、API、资源打包、安装和回滚另按既有产品合同验收，不以 Demo 成功替代。

## 备选方案与取舍

| 方案 | 优点 | 代价或不采用的原因 |
| --- | --- | --- |
| 独立模拟构建，复用正式 App（推荐） | 可分享、可复现，覆盖真实界面和交互，同时隔离真实资源。 | 需要维护模拟合同、静态深链和页面级验证。 |
| 仅本地 preview 或 Storybook | 启动和维护成本较低，适合开发和组件检查。 | 不提供独立可分享的完整页面演示，也不能覆盖运行时与路由组合。 |
| 连接真实管理服务演示 | 展示真实服务状态。 | 依赖部署和认证，不能稳定重现异常场景，真实操作风险不符合本次演示边界。 |

## 后续事项

实施时确定具体静态托管平台、部署路径和深链恢复机制；公开发布须按适用授权流程处理。实现方案还应验证模拟 transport 与管理 API 合同的一致性、场景切换时缓存隔离、资源状态和操作记录的一致性，并避免将模拟入口暴露为正式构建的任意运行时开关。
