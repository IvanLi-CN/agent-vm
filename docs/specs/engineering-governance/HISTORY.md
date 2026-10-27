# Agent VM 项目工程治理主题背景

> 本文记录主题边界与兼容性背景；长期需求以 [SPEC.md](SPEC.md) 为准，实际覆盖以 [IMPLEMENTATION.md](IMPLEMENTATION.md) 为准。

## Lifecycle / Compatibility

本主题为 active，覆盖项目全部工程交付阶段，不替代 Agent VM 的运行期能力合同。
兼容性治理、质量门禁、调度和失败通知适用于日常变更，并不以发版为前提。
SemVer 产品版本与协议号、schema 版本和镜像来源分别表达不同合同；旧格式读取迁移不意味着继续双格式发布。

## Replacements / Background

[发布设计](../../proposals/agent-vm-semver-release.md)承担产品版本选择、来源固定、产物、状态记录、恢复和发布/部署边界的子设计职责。
五项 Topic 的项目级需求统一由本主题持有；仅有发布设计不足以表达所有普通 PR、CI 和治理要求。
项目现状、Policy 安装表面、平台差距和启用前置条件归入 IMPLEMENTATION，不混入产品发布设计或长期需求正文。
本主题没有废弃或迁移现有 Agent VM Spec，也不因设计确认而改变现有运行期行为。

[统一产品版本与发布、部署边界 ADR](../../adr/0040-agent-vm-product-version-and-release-boundary.md)保存单一产品 SemVer 和独立部署的决策依据。
[控制面更新边界 ADR](../../adr/0030-agent-vm-control-plane-update-boundary.md)继续约束部署权限、drain、激活与恢复；新的产物发布机制不能绕过这些边界。

## Related Changes

None

## References

- [项目工程治理需求](SPEC.md)
- [实现覆盖与启用前置条件](IMPLEMENTATION.md)
- [Agent VM 能力合同](../agent-vm/SPEC.md)
