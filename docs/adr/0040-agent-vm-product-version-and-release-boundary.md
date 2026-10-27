---
status: accepted
---

# Agent VM 统一产品版本并分离发布与部署

Agent VM 的客户端、控制服务、兼容桥和安装资产以一个 SemVer 共同发布，首个公开版本为 `1.0.0`，从此承诺文档化对外接口的稳定兼容性。
维护者仅通过单一手动 `target` 选择发布，产物发布与校验成功构成产品发布完成；现网部署独立触发，以保留清晰的源码、产物与运维责任边界。
API 与持久状态分别治理，程序回滚不隐含数据库回退；新发布只生成 SemVer manifest，旧 SHA manifest 仅供迁移和必要回退读取，避免长期维持两个发布模型。

项目级兼容性、门禁、调度与失败通知合同见[工程治理需求](../specs/engineering-governance/SPEC.md)，发布机制见[发布子设计](../proposals/agent-vm-semver-release.md)。
实际覆盖与启用前置条件见[实现状态](../specs/engineering-governance/IMPLEMENTATION.md)；设计接受不构成实施授权。
