# 文档组织与编写规范

先根据问题选择文档层次；系统架构中的目标设计应标明实现状态，不能将尚未实现的设计当成部署事实。

## 目录职责

本页是项目文档放置与维护规则的统一入口。文件系统按材料职责组织，网页导航按读者任务组织；两者不必逐级对应，但网页正文必须直接来自仓库中的唯一来源。

```text
README.md、PRODUCT.md       项目入口、产品范围
GLOSSARY.md                 领域术语
DESIGN.md                   界面设计系统
docs/
  README.md                 本规范与文档导航
  architecture/             当前系统结构、边界与实现状态
  deployment.md             部署入口
  deployment/               专题部署指南
  specs/<topic>/            稳定主题的长期需求与行为合同
    SPEC.md                 需求、范围与验收条件
    IMPLEMENTATION.md       实现覆盖、启用情况与验证事实
    HISTORY.md              背景、兼容性与主题演进
    contracts/              接口、协议、配置及行为细则
    assets/                 该主题引用的图片等受控材料
  adr/                      关键决策、取舍及替代关系
  proposals/                研究、候选方案与设计论证
  solutions/<topic>/        可复用的问题诊断与解决经验
  templates/                文档与治理记录的可复用模板
docs-site/                  站点应用、来源清单与展示组件
.agents/skills/             Agent 专项操作指引
```

以上描述现有职责，不要求为每个主题创建所有文件或空目录。现有 Agent VM 运维手册和历史专题保留原位置；新增同类内容先复用已有入口，不借日常修改批量搬迁。

## 新文档放在哪里

1. 先查下表及相关主题，能补充已有文档就直接更新，避免同一规则分散维护。
2. 新需求归入稳定的 `docs/specs/<topic>/SPEC.md`，接口细则放该主题的 `contracts/`。主题采用领域或能力名称，不使用日期、任务编号或 PR 编号。
3. 探索、比较与尚未收敛的设计放 `docs/proposals/<slug>.md`，明确其建议性质。形成共识后，将长期要求同步到 Spec，将当前系统说明更新到架构或使用文档；难逆转且存在真实取舍的关键决定按现有编号写入 `docs/adr/<NNNN>-<slug>.md`。保留提案作为论证来源，不能仅靠提案暗示某功能已经实现。
4. 使用、部署或运维说明放对应的现有项目入口；可复用故障经验放 `docs/solutions/<topic>/<slug>.md`。实际实现和验证事实更新主题的 `IMPLEMENTATION.md`，背景保留在 `HISTORY.md`，执行计划和临时日志不写入长期需求正文。
5. 图片与示例就近归属主题并使用相对链接；主题验收图片放其 `assets/`。生成输入、构建产物和临时截图留在忽略目录；部署地址、凭据和运行状态留在仓库外的受保护记录。
6. 新增材料同步相关索引和正文链接。确需新增目录职责时，同时更新本规范；不临时发明与既有职责重复的目录。

## 网页发布与单源维护

`docs-site/content/catalog.ts` 显式登记需要在网页阅读的原始文档、稳定 slug、导航标题和分组，`docs-site/app/page-tree.ts` 组织读者目录。新增文件不会自动发布；供用户或开发者长期阅读的文档，应同时检查是否需要登记网页入口。内部记录不因位于 `docs/` 就自动进入公开站点。

直接编辑原始 Markdown／MDX，站点构建时适配正文、链接、图表及来源快照。`docs-site/.generated/` 和静态构建目录不承载可编辑正文；内容展示组件只补充表达能力，不保存另一份规范。已发布页面的 slug 尽量保持稳定，调整路径时同时检查来源清单、相对链接和既有入口。

文档变更按[项目级维护 Skill](../.agents/skills/documentation-review/SKILL.md)及[站点命令](../docs-site/README.md)验证来源、链接、构建和阅读体验。人类规范以本页为准，Agent 指引引用本页，不再维护一份同义目录规则。

## 文档导航

| 需要了解 | 文档与职责 |
| --- | --- |
| 产品范围和项目入口 | [项目 README](../README.md)、[产品定义](../PRODUCT.md)。 |
| 客户端职责与依赖审查 | [客户端设计与审查](specs/agent-vm/CLIENT.md)，先区分现有实现、已确定约束与待审查方案。 |
| 客户端命令与控制协议 | [客户端 CLI 接口](specs/agent-vm/contracts/cli.md)定义命令行表面；[客户端与控制端通信协议](specs/agent-vm/contracts/control-protocol.md)解释双方的身份、兼容和恢复约束。 |
| 系统如何组成 | [系统架构](architecture/system.md)是唯一的架构文档，描述蓝绿槽位、Caddy、统一服务、OIDC、Token、rqlite、接口和故障边界，并标明尚未实现的部分。 |
| 长期行为与接口 | [Spec 索引](specs/README.md)、[Agent VM Spec](specs/agent-vm/SPEC.md)和其 [客户端 CLI](specs/agent-vm/contracts/cli.md)、[管理 API](specs/agent-vm/contracts/management-api.md)、[Token API](specs/agent-vm/contracts/token-api.md)、[Agent API](specs/agent-vm/contracts/agent-api.md)、[发布](specs/agent-vm/contracts/release.md)、[观测](specs/agent-vm/contracts/observability.md)、[部署控制器](specs/agent-vm/contracts/deployment-controller.md)合同。 |
| 已接受的关键决定 | `docs/adr/`，每份文件记录特定决策及状态；当前统一服务安全边界见 [ADR 0043](adr/0043-agent-vm-unified-service-security-boundary.md)，状态迁移见 [ADR 0044](adr/0044-agent-vm-state-migration-to-rqlite.md)，部署事务与双槽位协调见 [ADR 0045](adr/0045-agent-vm-deployment-controller.md)，部署输入与恢复点见 [ADR 0054](adr/0054-agent-vm-deployment-input-and-recovery-policy.md)。 |
| 日常操作与恢复 | [Agent VM 运维手册](specs/agent-vm/OPERATIONS.md)和部署方自己的受保护记录。 |
| 设计研究和建议 | `docs/proposals/` 保存决策备忘录，是否已采纳以对应 ADR、Spec 和实现为准。 |
| 独立文档站建设 | [需求合同](specs/documentation-review/SPEC.md)定义首期范围与验收，[实现覆盖](specs/documentation-review/IMPLEMENTATION.md)区分设计与实际能力；[内容研究](proposals/agent-vm-documentation-source.md)和[技术方案](proposals/agent-vm-documentation-stack.md)记录内容接入依据、已确认选型及其取舍。 |
| 可复用工程经验 | `docs/solutions/agent-vm/` 保存特定问题的解释与处理经验。 |
| 界面视觉与交互 | 根目录 [DESIGN.md](../DESIGN.md) 是界面设计系统，不是系统架构；[Web 表面简报](../.impeccable/surfaces/web.md)记录界面方向。 |

新增或修改设计时，架构文档解释组件、接口和故障边界；Spec 定义长期行为合同；ADR 记录难逆转的决策；操作文档说明运行与恢复。
实现状态应通过代码及相应合同确认，而不是仅凭目标图判断。
ADR 0042 是被 0043 替代的历史决策，不能作为当前服务边界依据。

`skills/**/SKILL.md` 是 Agent 操作说明，不属于项目设计文档；设计审查以本导航中的项目文档、对应合同和实现证据为依据。
