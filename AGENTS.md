# Agent VM Repository Guide

## Scope

This repository is the canonical source for the Agent VM client and native
control service. Keep the client, compatibility bridge, Go service, release
manifest, systemd assets and behavior contracts versioned together.

## Technical boundaries

- Go `1.27.3` is the control runtime. Keep the module at
  `skills/agent-vm-ops/control/` and use its locked dependencies.
- Gin v1 is the protected internal HTTP boundary. Business and state packages
  must remain testable without a web context.
- SQLite uses `database/sql` with `modernc.org/sqlite`; preserve the existing
  schema, protocol 4, generation and operation-journal semantics.
- Bun remains the local client, compatibility bridge and guest one-shot runtime.
- Agent VM uses nested Incus system containers. Do not introduce KVM, QEMU or
  `/dev/kvm` requirements.

## Validation

新增或调整文档时，先按 [文档组织与编写规范](docs/README.md)选择唯一来源与目录，并同步索引及必要网页入口。

文档站或进入 Web 展示的设计材料发生变化时，读取项目级 [documentation-review Skill](.agents/skills/documentation-review/SKILL.md)，按当前合同检查来源、阅读体验与验收边界。

Keep tests beside the contract they prove. Run `bun run check`, both Bun test
scopes and `go -C skills/agent-vm-ops/control test ./...` locally. Run Go race,
native process-tree and isolated Incus/systemd checks in the session-owned
Agent VM when the change touches those boundaries.

Never commit deployment addresses, credentials, inventories, state databases or
consumer-rendered observability files.
