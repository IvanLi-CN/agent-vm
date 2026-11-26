# 项目文档发布索引

本索引是全项目人类文档的处置清单。`发布正文`表示在独立文档站有稳定页面；`索引访问`表示材料保留在仓库并通过本页或主题索引定位，正文不进入主导航；`排除`表示仅供 Agent、临时执行或受保护环境使用，不作为项目读者文档发布。网页正文始终来自表中列出的仓库文件，不在 `docs-site/` 维护同义副本。

## 处置规则

| 状态 | 含义 | 入口 |
| --- | --- | --- |
| 发布正文 | 用户、维护者或开发者需要直接阅读的当前说明、合同或设计 | 文档站页面和仓库原文 |
| 索引访问 | 历史决策、研究提案或解决经验，保留状态和上下文但不作为默认阅读入口 | 本页、主题索引或固定源码链接 |
| 排除 | Agent 操作指引、执行计划、临时日志、私有记录和生产证据 | 不进入站点导航 |

## 项目入口

| 文件 | 处置 | 理由 |
| --- | --- | --- |
| [README.md](../README.md) | 发布正文 | 项目定位、边界、构建入口和阅读路线。 |
| [PRODUCT.md](../PRODUCT.md) | 发布正文 | 项目级 PRD，定义角色、场景、能力、边界和成功标准。 |
| [CONTRIBUTING.md](../CONTRIBUTING.md) | 发布正文 | 开发、测试和贡献入口。 |
| [GLOSSARY.md](../GLOSSARY.md) | 发布正文 | 会话、租约、镜像、发布等领域术语。 |
| [DESIGN.md](../DESIGN.md) | 发布正文 | 管理台和 Web 表面的设计基线。 |

## 使用指南

| 文件 | 处置 | 理由 |
| --- | --- | --- |
| [docs/usage/quickstart.md](usage/quickstart.md) | 发布正文 | 首次配置、获取、连接和释放。 |
| [docs/usage/connections.md](usage/connections.md) | 发布正文 | SSH、HTTPS、Token、host key 和文件传输。 |
| [docs/usage/workflows.md](usage/workflows.md) | 发布正文 | 日常会话、同步、续租、调整、恢复和维护工作流。 |
| [docs/usage/console.md](usage/console.md) | 发布正文 | 内置管理台五页、预览、确认和审计。 |
| [docs/usage/images.md](usage/images.md) | 发布正文 | 镜像来源、fingerprint、默认选择、退役和导入。 |
| [docs/usage/troubleshooting.md](usage/troubleshooting.md) | 发布正文 | 按症状排查客户端、控制面、部署和镜像问题。 |

## 架构、部署和镜像

| 文件 | 处置 | 理由 |
| --- | --- | --- |
| [docs/architecture/system.md](architecture/system.md) | 发布正文 | 当前系统组成、边界、故障和实现状态。 |
| [docs/deployment.md](deployment.md) | 发布正文 | 部署入口和阅读顺序。 |
| [docs/deployment/blue-green.md](deployment/blue-green.md) | 发布正文 | 蓝绿资源和切换说明。 |
| [deploy/README.md](../deploy/README.md) | 发布正文 | 部署工程总览和受保护边界。 |
| [deploy/docs/install.md](../deploy/docs/install.md) | 发布正文 | 安装、配置输入和固定路径。 |
| [deploy/docs/blue-green.md](../deploy/docs/blue-green.md) | 发布正文 | 蓝绿发布和激活流程。 |
| [deploy/docs/legacy-release.md](../deploy/docs/legacy-release.md) | 发布正文 | format-2 兼容发布和迁移。 |
| [deploy/docs/maintenance.md](../deploy/docs/maintenance.md) | 发布正文 | drain、维护、恢复和权限边界。 |
| [deploy/docs/nested-incus.md](../deploy/docs/nested-incus.md) | 发布正文 | 嵌套 Incus 和 guest 验收。 |
| [deploy/docs/observability.md](../deploy/docs/observability.md) | 发布正文 | 观测、快照和新鲜度边界。 |
| [deploy/docs/acceptance.md](../deploy/docs/acceptance.md) | 发布正文 | 部署验收和故障证据。 |
| [images/README.md](../images/README.md) | 发布正文 | 镜像构建、锁、sidecar、验证和导入。 |

## 主题需求与合同

| 文件 | 处置 | 理由 |
| --- | --- | --- |
| [docs/specs/README.md](specs/README.md) | 发布正文 | 稳定主题、生命周期和实现覆盖的索引。 |
| `docs/specs/agent-vm/` | 索引访问 | 当前主题没有单独 PRD；项目级 PRD 和 [Agent VM Spec](specs/agent-vm/SPEC.md)共同承担范围说明。 |
| [docs/specs/agent-vm/SPEC.md](specs/agent-vm/SPEC.md) | 发布正文 | Agent VM 长期行为、准入、租约和恢复合同。 |
| [docs/specs/agent-vm/CLIENT.md](specs/agent-vm/CLIENT.md) | 发布正文 | 客户端职责、依赖和审查边界。 |
| [docs/specs/agent-vm/CLIENT-REPAIR.md](specs/agent-vm/CLIENT-REPAIR.md) | 发布正文 | 客户端修复方案与验证边界。 |
| [docs/specs/agent-vm/OPERATIONS.md](specs/agent-vm/OPERATIONS.md) | 发布正文 | 拓扑、部署控制器和恢复手册。 |
| [docs/specs/agent-vm/CONTROL-PLANE-LIFECYCLE-RECOVERY.md](specs/agent-vm/CONTROL-PLANE-LIFECYCLE-RECOVERY.md) | 发布正文 | 生命周期、租约和未知结果恢复。 |
| [docs/specs/agent-vm/RETENTION-DESIGN.md](specs/agent-vm/RETENTION-DESIGN.md) | 发布正文 | 缓存、保留和回收设计。 |
| [docs/specs/agent-vm/RECLAMATION-DESIGN.md](specs/agent-vm/RECLAMATION-DESIGN.md) | 发布正文 | 回收条件和安全边界。 |
| [docs/specs/agent-vm/host-selection.md](specs/agent-vm/host-selection.md) | 发布正文 | 客户端主机选择和目标绑定。 |
| [Agent VM 合同目录](specs/README.md) | 发布正文 | CLI、控制协议、Agent/Token/管理 API、发布、观测和部署控制器合同；具体文件从主题索引进入。 |
| [docs/specs/management-console/PRD.md](specs/management-console/PRD.md) | 发布正文 | 管理台角色、任务、页面、边界和成功标准。 |
| [docs/specs/engineering-governance/SPEC.md](specs/engineering-governance/SPEC.md) | 发布正文 | 工程治理的长期要求。 |
| [docs/specs/engineering-governance/IMPLEMENTATION.md](specs/engineering-governance/IMPLEMENTATION.md) | 发布正文 | 工程治理的当前覆盖和启用事实。 |
| [docs/specs/engineering-governance/HISTORY.md](specs/engineering-governance/HISTORY.md) | 发布正文 | 工程治理背景和兼容性。 |
| [docs/specs/documentation-review/PRD.md](specs/documentation-review/PRD.md) | 发布正文 | 文档整理和独立站的产品范围。 |
| [docs/specs/documentation-review/SPEC.md](specs/documentation-review/SPEC.md) | 发布正文 | 文档内容、来源和站点验收合同。 |
| [docs/specs/documentation-review/IMPLEMENTATION.md](specs/documentation-review/IMPLEMENTATION.md) | 发布正文 | 站点实现覆盖和验证事实。 |
| [docs/specs/documentation-review/HISTORY.md](specs/documentation-review/HISTORY.md) | 发布正文 | 文档主题背景与边界演进。 |

本表中的 `contracts/` 是目录入口；站点清单按文件登记需要稳定页面的合同，未登记的历史或机器可读附属材料仍可通过主题页面和仓库入口定位。

## 决策、提案和解决经验

关键决策和研究材料继续保留原文件名。重复编号按完整文件名区分，不能只写编号；历史材料是否已采纳以当前 Spec、架构和实现覆盖为准。

### ADR：索引访问

以下文件保留为历史决策索引，正文不进入默认读者导航；需要核对取舍时从本表打开固定原文。

- `docs/adr/0025-native-incus-agent-vm-lifecycle.md`
- `docs/adr/0026-agent-vm-cpu-memory-overselling.md`
- `docs/adr/0028-agent-vm-topology-recovery-boundary.md`
- `docs/adr/0029-agent-vm-memory-reservation-only-admission.md`
- `docs/adr/0030-agent-vm-control-plane-update-boundary.md`
- `docs/adr/0031-agent-vm-cpu-startup-pressure-boundary.md`
- `docs/adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md`
- `docs/adr/0033-agent-vm-capacity-and-inventory-observability.md`
- `docs/adr/0034-agent-vm-transport-independent-control.md`
- `docs/adr/0035-agent-vm-built-in-expiry-cleanup.md`
- `docs/adr/0036-agent-vm-cold-cutover.md`
- `docs/adr/0037-agent-vm-go-control-runtime.md`
- `docs/adr/0038-native-incus-agent-container-lifecycle.md`
- `docs/adr/0039-agent-vm-standalone-repository.md`
- `docs/adr/0040-agent-vm-product-version-and-release-boundary.md`
- `docs/adr/0041-agent-vm-incus-project-initialization.md`
- `docs/adr/0041-agent-vm-session-pinned-image-identity.md`
- `docs/adr/0042-agent-vm-web-management-boundary.md`
- `docs/adr/0043-agent-vm-unified-service-security-boundary.md`
- `docs/adr/0044-agent-vm-state-migration-to-rqlite.md`
- `docs/adr/0045-agent-vm-deployment-controller.md`
- `docs/adr/0045-agent-vm-lifecycle-recovery-backpressure.md`
- `docs/adr/0046-agent-vm-failure-notification-transport.md`
- `docs/adr/0047-agent-vm-maintenance-progress-and-recovery.md`
- `docs/adr/0048-agent-vm-release-fetch-and-deployment-failure-boundary.md`
- `docs/adr/0049-agent-vm-host-profiles-and-session-binding.md`
- `docs/adr/0050-agent-vm-ubuntu24-language-image-families.md`
- `docs/adr/0051-agent-vm-image-runtime-variants.md`
- `docs/adr/0052-client-runtime-release-boundary.md`
- `docs/adr/0053-image-names-and-operation-pinning.md`
- `docs/adr/0054-agent-vm-deployment-input-and-recovery-policy.md`
- [root SSH 请求身份](adr/0055-root-ssh-request-identity.md)：已接受的安全边界决定，通过本索引阅读，未作为独立站点导航。
- [VM 保留与租约区间](adr/0056-agent-vm-vm-hold-and-lease-bands.md)：已接受的生命周期与准入决定，定义保留状态、动作矩阵、租约区间和受保护销毁确认。
- [状态标记不再是准入条件](adr/0057-agent-vm-state-markers-are-informational.md)：已接受的部署面决定，状态标记只作观测，不阻断启动、激活、回滚或恢复。
- [恢复点预算按角色释放](adr/0058-agent-vm-recovery-point-budget-relief.md)：已接受的保留决定，最旧恢复点的可回收性由引用与租约决定，不再由年龄决定。

### 提案：索引访问

- `docs/proposals/agent-vm-documentation-review.md`：文档整理主题的历史范围，当前要求以本主题 PRD/SPEC 为准。
- `docs/proposals/agent-vm-documentation-source.md`：内容来源和适配研究。
- `docs/proposals/agent-vm-documentation-stack.md`：独立文档站技术方案。
- `docs/proposals/agent-vm-go-parent-service.md`：Go 父服务研究。
- `docs/proposals/agent-vm-semver-release.md`：版本策略研究。
- `docs/proposals/agent-vm-web-console.md`：管理台产品研究。
- `docs/proposals/agent-vm-web-demo.md`：Demo 表面研究。
- `docs/proposals/agent-vm-wide-screen-layout.md`：宽屏布局研究。
- `docs/proposals/agent-vm-deployment-robustness.md`：既有安装的升级与部署健壮性诊断与方案。
- `docs/proposals/ci-optimization-design.md`：CI 优化研究。

### 解决经验：索引访问

- `docs/solutions/agent-vm/ct-root-data-split.md`
- `docs/solutions/agent-vm/lease-loss-during-mutation.md`
- `docs/solutions/agent-vm/maintenance-recovery.md`
- `docs/solutions/agent-vm/memory-reservation-validation.md`
- `docs/solutions/agent-vm/observability-dashboard.md`

这些材料面向需要查证背景或故障根因的读者，不作为首次使用和部署流程的替代说明。

## 排除发布

`AGENTS.md`、`.agents/skills/`、`skills/**/SKILL.md`、执行计划、临时截图、生成目录、测试结果、部署地址、凭据、库存、状态数据库和消费方渲染的观测文件不进入读者导航。它们可能被开发或运维工具使用，但不能作为项目产品文档或生产证据发布。

## 维护要求

新增或修改人类文档时，先按[文档组织与编写规范](README.md)选择唯一来源，再更新本索引、`docs-site/content/catalog.ts`和必要的首页入口。站点默认使用 Markdown；只有静态 Markdown 无法表达所需正文时才使用 MDX，并且正文仍只维护一份。提交前运行 `bun run docs:check`、`bun test docs-site/content` 和 `bun run docs:build`，并检查新增页面的相对链接、资源、锚点和窄屏阅读。
