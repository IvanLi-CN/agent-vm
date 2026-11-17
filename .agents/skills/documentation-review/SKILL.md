---
name: documentation-review
description: "维护 Agent VM 独立文档站、进入 Web 审阅流程的设计材料及需求追溯，检查单源内容、异常预览和审阅验收边界。"
---

# 文档站与设计审阅

## 读取当前合同

以仓库根目录为基准，读取 [文档审阅 Spec](../../../docs/specs/documentation-review/SPEC.md) 和 [实现覆盖](../../../docs/specs/documentation-review/IMPLEMENTATION.md)。需求与验收由 Spec 定义，实现覆盖记录已经存在的能力和证据；本 Skill 只约束 Agent 的工作过程。

设计范围或工程边界变化时，读取[需求收敛说明](../../../docs/proposals/agent-vm-documentation-review.md)与[技术方案](../../../docs/proposals/agent-vm-documentation-stack.md)，区分已确认选择、推荐实现和后续范围。普通文档修改不因此触发全量站点迁移或新的设计访谈。

## 修改材料

1. 找到唯一可编辑正文和现有稳定标识，直接修改来源；检查原始相对链接、显式锚点及验收 `covers:` 的受影响关系。接入 Web 的范围以当前来源清单为准，尚无清单时以本主题首期范围为准。
2. 新增或改变关联时，在原始设计材料中维护必要声明，让站点生成索引；按 Spec 的 REQ-DOCS-002、REQ-DOCS-005 检查作者维护成本。设计标题下使用普通列表项 `- design-id: \`DESIGN-DOCS-SOURCE\`` 与 `- covers: \`REQ-DOCS-002\`, \`REQ-DOCS-007\``；缺口原因仅在原始需求段落内写 `- trace-note: 原因`。不要编辑 `.generated/` 或复制规范正文到站点组件。
3. 检查问题预览与首期完成验收各自的条件，按 REQ-DOCS-006 区分缺少关联、引用损坏和解析失败。保留诊断和来源；链接存在、需求被接受、代码实现与验收通过分别报告。
4. 若本次任务包含设计收敛，维护决策树，每轮仅询问前提已经确定的全部未决选择，给出推荐答案并等待回复；及时记录已确认结论，在分支处理完后请求最终共同理解确认。未决选择留在收敛说明，最终确认前不宣称设计已经收敛。局部文档修正沿用已确认结论。
5. 新领域概念按仓库术语表检查；新增 ADR 前按当前可用的 `domain-modeling` Skill 检查准入。选型被接受本身不构成创建 ADR 的理由。框架、阶段和目录变化先同步其规范来源，再更新本 Skill 的入口或流程。

## 验证并交接

从仓库配置发现当前可用的检查入口；修改本主题 Spec 时运行 `bun run bin/spec_contract_check.ts --path docs/specs/documentation-review/SPEC.md`，相关文档合同运行 `python3 bin/test_agent_vm_documentation.py`。检查修改材料的链接、锚点和来源声明；检查结果说明覆盖范围。

涉及站点实现时，运行 `bun run docs:check`、`bun test docs-site/content` 和 `bun run docs:build`；浏览器检查使用 `DOCS_PORT=<leased-port> DOCS_STORYBOOK_PORT=<gallery-leased-port> bun run docs:test:e2e`，首次按站点 README 安装 Chromium。组件状态使用 `bun run --cwd docs-site build:storybook`。本地开发和静态预览分别使用 `docs:dev`、`docs:preview`，启动前按项目端口租约约束提供 `DOCS_PORT`。命令与来源清单见 [docs-site/README.md](../../../docs-site/README.md) 和 `docs-site/content/generate.ts`。

默认预览保留缺失、失效和无法解析诊断；严格检查不得放过失效引用或没有说明的关联缺失。检查页面展示的基准提交、修改状态、内容摘要与来源快照，不能把本地修改链接伪装成远端提交。按 Spec 的 Verification 保存真实结果；人工审阅按 REQ-DOCS-008 的真实路径交给项目负责人确认，设计共识、截图和自动测试不能替代此项确认。

交接说明变更的来源、受影响关联、已运行检查及未解决问题。设计确认、文档检查和站点实现是不同事实；是否继续实现、发布或嵌入管理台，以当前用户授权范围为准。
