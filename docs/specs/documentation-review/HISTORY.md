# Agent VM 文档审阅主题背景

## Lifecycle / Compatibility

本主题为 active，独立于 Agent VM 运行期、协议与工程治理合同；不替代既有 Spec，不迁移其来源路径，也不改变生产或 Demo 的行为。

## Background

项目文档需要同时服务用户使用、开发者理解和候选设计审阅。Fumadocs 与单源内容原则已接受，选型理由与成本保存在[技术方案](../../proposals/agent-vm-documentation-stack.md)，单源内容约束见 [SPEC.md](SPEC.md) 的 REQ-DOCS-002。

首期以文档站自身的需求与设计为审阅对象，以可筛选需求追溯作为实际交互，并要求独立构建、可复现预览和明确源码身份；正式域名与每个 PR 自动托管预览均不构成首期完成条件。

作者维护成本限定为原始材料中的少量显式关联；追溯问题保留可审阅预览，完成验收时修复失效引用并说明未关联条目。交互价值由项目负责人走通真实审阅路径及缺少关联的案例确认，不以正式计时实验作为首期前提。

项目负责人已确认本阶段的整体设计共识，包括独立站点的最小工程布局及项目级 Skill 的职责。本阶段完成需求、设计材料和 Agent 指引整理，站点实现及其实际验收另行开展。

先完成当前设计文档，再建设最小独立站点；此后分别开展文档嵌入验证、完整项目文档与站点展示、生产及 Demo 完整嵌入。阶段背景在这里保存，当前功能覆盖由 [IMPLEMENTATION.md](IMPLEMENTATION.md)记录。

## References

- [需求合同](SPEC.md)
- [需求收敛说明](../../proposals/agent-vm-documentation-review.md)
- [工程组织研究](../../proposals/agent-vm-documentation-stack.md)
