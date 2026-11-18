---
name: documentation-review
description: "维护 Agent VM 独立文档站及设计文档展示，检查单源正文、阅读体验、来源身份和当前交付边界。"
---

# 文档展示与维护

读取 [PRD](../../../docs/specs/documentation-review/PRD.md)、[SPEC](../../../docs/specs/documentation-review/SPEC.md)、[实现说明](../../../docs/specs/documentation-review/IMPLEMENTATION.md) 与 [站点命令](../../../docs-site/README.md)。本 Skill 保留原入口名称，负责项目文档整理与展示维护，不授权新增审阅产品。

## 内容与范围

- 新增或归档材料时按[文档组织与编写规范](../../../docs/README.md)选择位置、同步索引并判断是否发布到网页；目录职责以该页为单一来源。
- 文档站以文档阅读为主入口；方便检查设计通过清晰正文、目录、链接、图表和按需交互实现。不要将其解释成独立审阅页、需求追溯工具或需求管理工作台。
- 修改原始 Markdown／MDX，保持唯一可编辑正文；不编辑 `.generated/`，不维护同义副本，不要求作者增加 design-id、设计 covers 或 trace-note。已有 Spec 编号和验收 covers 是原文内容，不驱动站点追溯界面。
- 全项目整理覆盖根文档、docs、deploy、images、贡献及工程治理材料，逐项记录处置与网页发布状态；当前指南和开发正文站内可读，历史材料通过有状态的索引访问，Agent 指令和私有运行记录不混入读者导航。PRD、指南和合同的具体目录职责只以文档组织规范为准。
- 产品、指南与实现冲突时核对当前源码及合同，分别记录需求、实现、启用和环境证据；不能用旧提案暗示当前已实现，也不因整理文档顺手修改产品行为。
- 站点以项目和读者任务组织导航，不以当前开发任务组织导航。首页提供使用指南和开发文档；文档站自身材料属于开发文档下的主题，不得以“本次设计”“来源材料”作为站点主导航。
- 来源清单、标题、分组和稳定 slug 在 `docs-site/content/catalog.ts`，生成逻辑在 `docs-site/content/generate.ts`。检查相对链接、资源、标题和来源身份；本地修改不得伪装成远端提交。
- UI 库优先：修改界面前先检查已安装 Fumadocs 的公开组件、类型及官方用法；优先使用完整布局，再使用其组件和样式原语，最后才考虑现有 Radix 能力。只有现有库缺少对应能力时实现项目特有交互，并在实现说明记录缺口与选择理由。
- 外壳、目录树、正文与响应式页内目录分别复用 `DocsLayout`、`fumadocs-core/page-tree`、`DocsPage`／`DocsBody`；折叠、代码复制、提示和按钮分别使用 Accordion、CodeBlock、Callout 和 `buttonVariants`。Fumadocs 按钮公开 API 是样式原语，配合语义化 button 使用。
- 验证时检查所有新增或修改的交互控件都已映射到现有库能力或有明确缺口说明，删除被替代的状态逻辑和样式。Markdown 标题、列表、表格及链接保留语义化 HTML；Mermaid 渲染与来源快照行定位属于项目逻辑。
- 正文使用自然文档流，只有源码使用预格式。外壳样式使用专属类名，避免污染 Fumadocs 组件。验证桌面、中间宽度、窄屏与键盘导航。
- 全项目文档整理按 Spec 执行；正文默认用 Markdown，MDX 按阅读需要采用。管理台嵌入、公开托管、产品发布、现网操作与文档批注仍不包含。

## 验证

运行 `bun run docs:check`、`bun test docs-site/content`、`bun run docs:build`；页面检查使用 `DOCS_PORT=<leased-port> bun run docs:test:e2e`。组件状态构建使用 `bun run --cwd docs-site build:storybook`；首次测试按 README 安装 Chromium。本地启动前取得端口租约。

修改本主题 Spec 时执行 `bun run bin/spec_contract_check.ts --path docs/specs/documentation-review/SPEC.md`，并检查 `python3 bin/test_agent_vm_documentation.py`。同步实现覆盖与已退出的要求，不保留会误导后续 Agent 的旧交付条件。

全项目整理还须核对所有发布页面的正文、链接、锚点、资源与历史入口，测试实际目录中的来源热更新。负责人在当前候选走通首次使用、部署维护和理解设计；页面采用交互时再检查对应交互。交接记录实际测试及未完成的人工阅读确认。
