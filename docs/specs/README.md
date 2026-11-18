# 规格（Spec）总览

本目录按稳定主题保存长期需求合同，不作为单次任务的执行计划或进度看板。
新主题的 `SPEC.md` 保存需求与验收，`IMPLEMENTATION.md` 保存实现覆盖与启用事实，`HISTORY.md` 保存主题边界、兼容性与背景。
`Lifecycle` 使用 active、superseded 或 archived；实现状态与主题是否有效分别表达。
已有 Agent VM 文档保留原布局，其实现覆盖不在本次文档整理中重新评估。

## Index

| Topic | Lifecycle | Implementation | Spec | Successor | Notes |
| --- | --- | --- | --- | --- | --- |
| Native Incus Agent VM | active | 已有实现，覆盖未在本次重评 | [agent-vm/SPEC.md](agent-vm/SPEC.md)；[控制面生命周期与恢复设计](agent-vm/CONTROL-PLANE-LIFECYCLE-RECOVERY.md) | - | 运行期能力、协议、lease、operation journal 与 native recovery 边界。 |
| Agent VM 管理台 | active | 项目内置 Web 管理台的产品范围和 API 合同已分开维护 | [management-console/PRD.md](management-console/PRD.md)；[管理 API](agent-vm/contracts/management-api.md) | - | 维护者观察、镜像管理、受控操作和审计；不建立第二套生命周期权威。 |
| Agent VM 项目工程治理 | active | 仓库侧候选已实施；平台保护、通知 transport 和正式发布未启用 | [engineering-governance/SPEC.md](engineering-governance/SPEC.md) | [IMPLEMENTATION.md](engineering-governance/IMPLEMENTATION.md) | 五项必需 Topic 覆盖日常变更、门禁、调度、通知和手动发布。 |
| 项目文档与独立文档站 | active | 最小站点已有实现；全项目整理与分层发布尚未实现 | [documentation-review/SPEC.md](documentation-review/SPEC.md) | - | 产品目标见 [PRD.md](documentation-review/PRD.md)，实现覆盖见 [IMPLEMENTATION.md](documentation-review/IMPLEMENTATION.md)。 |
