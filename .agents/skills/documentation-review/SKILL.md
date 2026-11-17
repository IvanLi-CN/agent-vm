---
name: documentation-review
description: "维护 Agent VM 独立文档站及设计文档展示，检查单源正文、阅读体验、来源身份和当前交付边界。"
---

# 文档展示与维护

读取 [SPEC](../../../docs/specs/documentation-review/SPEC.md)、[实现说明](../../../docs/specs/documentation-review/IMPLEMENTATION.md) 与 [站点命令](../../../docs-site/README.md)。本 Skill 保留原入口名称，负责文档展示维护，不授权新增审阅产品。

## 内容与范围

- 文档站以文档阅读为主入口；方便检查设计通过清晰正文、目录、链接、图表和按需交互实现。不要将其解释成独立审阅页、需求追溯工具或需求管理工作台。
- 修改原始 Markdown／MDX，保持唯一可编辑正文；不编辑 `.generated/`，不维护同义副本，不要求作者增加 design-id、设计 covers 或 trace-note。已有 Spec 编号和验收 covers 是原文内容，不驱动站点追溯界面。
- 站点以项目和读者任务组织导航，不以当前开发任务组织导航。首页提供使用指南和开发文档；文档站自身材料属于开发文档下的主题，不得以“本次设计”“来源材料”作为站点主导航。
- 来源清单、标题、分组和稳定 slug 在 `docs-site/content/catalog.ts`，生成逻辑在 `docs-site/content/generate.ts`。检查相对链接、资源、标题和来源身份；本地修改不得伪装成远端提交。
- UI 库优先：修改界面前先检查已安装 Fumadocs 的公开组件、类型及官方用法；优先使用完整布局，再使用其组件和样式原语，最后才考虑现有 Radix 能力。只有现有库缺少对应能力时实现项目特有交互，并在实现说明记录缺口与选择理由。
- 外壳、目录树、正文与响应式页内目录分别复用 `DocsLayout`、`fumadocs-core/page-tree`、`DocsPage`／`DocsBody`；折叠、代码复制、提示和按钮分别使用 Accordion、CodeBlock、Callout 和 `buttonVariants`。Fumadocs 按钮公开 API 是样式原语，配合语义化 button 使用。
- 验证时检查所有新增或修改的交互控件都已映射到现有库能力或有明确缺口说明，删除被替代的状态逻辑和样式。Markdown 标题、列表、表格及链接保留语义化 HTML；Mermaid 渲染与来源快照行定位属于项目逻辑。
- 正文使用自然文档流，只有源码使用预格式。外壳样式使用专属类名，避免污染 Fumadocs 组件。验证桌面、中间宽度、窄屏与键盘导航。
- 管理台嵌入、全量文档迁移、公开托管与文档批注属于后续范围；范围变更以用户授权及当前 Spec 为准，不自动扩大功能。

## 验证

运行 `bun run docs:check`、`bun test docs-site/content`、`bun run docs:build`；页面检查使用 `DOCS_PORT=<leased-port> bun run docs:test:e2e`。组件状态构建使用 `bun run --cwd docs-site build:storybook`；首次测试按 README 安装 Chromium。本地启动前取得端口租约。

修改本主题 Spec 时执行 `bun run bin/spec_contract_check.ts --path docs/specs/documentation-review/SPEC.md`，并检查 `python3 bin/test_agent_vm_documentation.py`。同步实现覆盖与已退出的要求，不保留会误导后续 Agent 的旧交付条件。

交接记录实际测试与未完成的人工阅读确认。漂亮页面与构建成功不等于读者已经理解设计；人工验收检查真实内容阅读与定位，不再要求走需求追溯或异常关联路径。
