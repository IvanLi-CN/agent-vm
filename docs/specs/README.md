# 规格（Spec）总览

本目录按稳定主题保存长期需求合同，不作为单次任务的执行计划或进度看板。
新主题的 `SPEC.md` 保存需求与验收，`IMPLEMENTATION.md` 保存实现覆盖与启用事实，`HISTORY.md` 保存主题边界、兼容性与背景。
`Lifecycle` 使用 active、superseded 或 archived；实现状态与主题是否有效分别表达。
已有 Agent VM 文档保留原布局，其实现覆盖不在本次文档整理中重新评估。

## Index

| Topic | Lifecycle | Implementation | Spec | Successor | Notes |
| --- | --- | --- | --- | --- | --- |
| Native Incus Agent VM | active | 已有实现，覆盖未在本次重评 | [agent-vm/SPEC.md](agent-vm/SPEC.md) | - | 运行期能力、协议与原生资源边界。 |
| Agent VM 项目工程治理 | active | 仓库侧候选已实施；平台保护、通知 transport 和正式发布未启用 | [engineering-governance/SPEC.md](engineering-governance/SPEC.md) | [IMPLEMENTATION.md](engineering-governance/IMPLEMENTATION.md) | 五项必需 Topic 覆盖日常变更、门禁、调度、通知和手动发布。 |
