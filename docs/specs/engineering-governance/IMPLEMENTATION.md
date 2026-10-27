# Agent VM 项目工程治理实现状态

> 长期需求以 [SPEC.md](SPEC.md) 为准；本文件记录实际覆盖、差距和启用前置条件，不构成实施计划或执行授权。

## Current Status

- Implementation：仓库侧治理候选已实施；平台保护、通知 transport 和正式发版仍未启用。
- Lifecycle：active。
- Catalog note：五项 Topic 的仓库 Policy、合同、执行工具和验证入口已进入当前候选；本 PR 不合并 main、不创建正式 tag/Release、不部署现网。

## 已核验的仓库与平台事实

以下事实对应本次候选开发基线 `cef5bd8a62f2856ddd817355dde701685404619c`；平台项仍需在真实 PR 和启用阶段重新核验。

- 默认分支为 `main`，仓库公开；当时没有产品 tag 或 GitHub Release 历史。
- 仓库现在声明 `CI (PR)`、`CI (Main)`、`Release`、`Release recovery`、两个失败通知 sidecar 和 `Notification smoke`；PR/Main gate 具有独立的非抢占 concurrency。
- 现有检查包含 Ubuntu/macOS、Bun 类型检查、仓库检查、两套 Bun 测试、Go tests/vet、Linux amd64/arm64 构建和 Ubuntu race 检查。
- `main` 当时未启用分支保护或 ruleset，无平台强制 required checks 或签名提交要求；基线提交本身的有效签名不能代替分支政策。
- `0.4.0-native` 没有正式发布记录，不是产品版本基线；新生产构建器要求 SemVer，首版只能是 `1.0.0`。
- format 3 将产品 `version` 与源码 `source_revision` 分开；协议 4 的兼容适配仍从源码 SHA 生成旧 `release_id`，旧格式 1/2 只读。
- 新增统一产品包构建、format 3 跨语言校验、GitHub `release-state/<stage>/<version>` 只创建 ref、append-only reservation/recovery 协调器和 `gh` adapter；平台保护与 immutable Release 仍是启用前置条件。
- 现有 webhook 不被当作通知 transport；项目合同明确 transport `unselected`，失败 sidecar 在 selection gate 阶段 fail-closed。
- GitHub Release immutability、main ruleset、required checks、签名提交和真实通知配置没有在本 PR 修改；`platform-audit.ts` 只读报告当前 `unavailable/blocked` 差距，合同状态保留为 `audit-required`。
- 三个目标 Policy package 与 `skills-lock.json` 已安装并锁定，quality-gates 本地审计通过。

## Implementation Coverage

| 需求 | 已有基础 | 尚缺的目标覆盖 |
| --- | --- | --- |
| REQ-ENG-001、REQ-ENG-022 | 五项 Topic Policy、lock、quality contract、release contract 和 notification contract。 | 真实实施 commit 的采用记录；平台规则与通知 transport 仍未启用。 |
| REQ-ENG-002、REQ-ENG-003 | SemVer Policy、版本影响模板、公开合同记录。 | 日常 API/state 变更的实际审阅证据；不把 target 绑定到影响分类。 |
| REQ-ENG-004 | SQLite、generation、operation journal 原有边界保持；旧状态读取路径保留。 | 同 minor/跨 minor/major 的历史状态 fixture 与升级证据。 |
| REQ-ENG-005、REQ-ENG-006 | `.github/quality-gates.json`、PR/Main gate、合同测试。 | 平台 required checks/ruleset 对齐；本 PR 只做只读审计。 |
| REQ-ENG-007 | 基线提交有有效签名。 | 平台 PR-only、required checks、签名、最新分支要求、禁止删除/force push 与例外核验。 |
| REQ-ENG-008 | 条件原生验收边界已写入 release contract；本轮不执行 Incus/systemd 安装运行。 | 命中 Incus/systemd/deployment 变更时的独立验收。 |
| REQ-ENG-009 | 精确 PR 事件、merge queue 事件、独立 namespace、`cancel-in-progress: false` 和合同测试。 | 真实 required queue/run history 对齐。 |
| REQ-ENG-010 至 REQ-ENG-014 | SemVer resolver、固定源码 preflight、format 3 builder、append-only state、恢复入口。 | 真实 GitHub Release immutability、发布记录权限和中断恢复实证。 |
| REQ-ENG-015 | 已有显式部署、drain、激活、健康和恢复工具。 | 只消费已完成发布的版本化产物，明确发布完成与部署结果的独立记录。 |
| REQ-ENG-016 至 REQ-ENG-020 | 全 workflow 唯一分类、两个 trusted-default-branch sidecars、metadata payload、selection gate。 | 选定 transport、凭据配置和真实 smoke；当前明确未选。 |
| REQ-ENG-021 | format 3、新 `version`/`source_revision`、客户端版本输出、Python/Bun/Go 读取适配。 | 旧 SHA 状态 fixture 和 Go runtime format 3 fixture 仍需补齐。 |
| REQ-ENG-023 | 已有公共边界与部署配置隔离。 | 将目标发布资产与迁移路径纳入同样的边界验证。 |

## 实现表面与完成判据

以下表面已经进入当前候选；完成证据明确区分仓库实现、真实行为验证与尚未启用的平台能力。

| Topic | 预期项目表面 | 完成证据 |
| --- | --- | --- |
| `manual-version-release-delivery` | `.github/release-contract.json`、`Release`/`Release recovery`、format 3 builder、受 identity 保护的本地镜像与 GitHub state refs。 | GitHub 平台不可变记录和真实发布/恢复仍待启用阶段。 |
| `semver-change-governance` | Policy、公开接口与状态模板、SemVer resolver 和独立 API/state 合同。 | 日常变更的实际影响记录；不自动驱动 target。 |
| `quality-gates` | Policy、`.github/quality-gates.json`、PR/Main gates、quality checker。 | GitHub ruleset 与 required checks 对齐；当前只读差距审计。 |
| `github-actions-scheduling` | 所有实际工作流的 events、namespaces、非抢占 concurrency、合同测试。 | live run history 和重跑/跨阶段证据。 |
| `release-failure-notification` | Policy、notification contract、两个 sidecars、metadata helper、selection gate。 | transport 选择、配置和真实 smoke；当前不发送通知。 |

三个项目 Policy 的安装写入范围为 `.agents/skills/semver-change-governance/`、`.agents/skills/quality-gates/`、`.agents/skills/release-failure-notification/` 及 CLI 管理的 `skills-lock.json`；已在当前候选写入并通过本地 lock/package 校验。

## 工作流设计映射

| Workflow name | 事件与职责 | 并发边界 | 通知分类 |
| --- | --- | --- | --- |
| `CI (PR)` | 面向 main 的 opened、synchronize、reopened、ready_for_review；产生 `PR gate`。 | per-PR required namespace，`queue: max`，不抢占。 | generic |
| `CI (Main)` | main push；在实际 merge SHA 产生 `Main gate`。 | 独立 main namespace，`queue: max`，不抢占。 | generic |
| `Release` | main 上显式 dispatch；固定事件 source，完成产物发布。 | 产品级 release 状态写入 namespace，`queue: max`，不抢占。 | Release-specific |
| `Notify failed workflow` | generic allowlist 的 completed workflow_run，显式 guard failure。 | 原 run ID 与 attempt；不与产品发布共享 group。 | selection-gated |
| `Notify failed release` | Release 的 completed workflow_run，额外核验发布 identity。 | 原 run ID 与 attempt；不与产品发布共享 group。 | selection-gated |
| `Notification smoke` | transport 选择后的独立手动发送验证；当前只证明 gate fail-closed。 | 独立 manual namespace。 | selection-gated |

本表对应当前候选 workflow inventory；平台启用和真实 run history 仍必须重新核验。
未来若新增 Deploy 或文档交付工作流，必须同时纳入阶段门禁、调度与失败分类，不能只补发布链路。

## 验证入口

当前验证入口为 `bun run test:policy-audit`、`bun run test:platform-audit`、`bun run check`、`bun run test:repo`、`bun run test:skills` 和 `go -C skills/agent-vm-ops/control test ./...`；`bun skills/agent-vm-ops/scripts/platform-audit.ts --repository IvanLi-CN/agent-vm` 是只读 live 审计入口，PR/Main workflow 还声明双架构 format 3 构建与 Linux race。
这些仓库证据不等于 GitHub ruleset、真实通知发送、正式发版或部署已启用。
目标合同还需要 SPEC 中 VER-ENG-001 至 VER-ENG-012 的验证，具体可执行入口在实现时补齐，不以占位命令冒充已有检查。
重型、原生进程树或 Incus/systemd 验证使用会话所属隔离环境，不在文档整理期间运行。

## Coverage / rollout summary

五项 Topic 的仓库侧能力已经应用，但不能宣称平台整体已启用或项目已完成正式发布。
当前候选未修改 GitHub ruleset/权限/secrets，未发送通知、创建正式 tag/Release、合并 main 或部署现网；本轮也不执行 Incus/systemd 安装运行验收。
发布子设计只覆盖发布阶段；日常 API/state 兼容、质量门禁、Actions 调度和通知边界仍由本 Spec 共同约束。

## Remaining Gaps

- 通知 transport 尚无项目认可的选择证据；不能从历史 reference、现有 webhook 或默认邮件反推。transport owner、configuration owner 与 required secrets 需随有效选择一并记录。
- 缺少选择证据时，notifier rollout 与整套新发布系统保持阻断；本 PR 不选择其他通知方案。
- 平台实施前还需落实发布记录创建身份与保护规则，核对发布资产保留、immutable Release 和对应权限；这些属于配置落实，不改变产品版本与发布终点。
- 适用的状态迁移/恢复 fixture、live PR checks、运行时 Tier 4 review 和最终平台审计仍属于 Step 5 收口证据，不在本地合同通过时提前宣称完成。

## Related Changes

Current candidate: repository governance implementation on `th/project-engineering-governance`; formal release and platform enablement remain separate follow-up operations.

## References

- [项目工程治理需求](SPEC.md)
- [主题边界与背景](HISTORY.md)
- [发布子设计](../../proposals/agent-vm-semver-release.md)
