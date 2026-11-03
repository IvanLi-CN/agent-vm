# Agent VM memory reservation 验证

本文记录 reservation-only memory plan 的证据边界。
它区分确定性的离线行为、操作系统观测和部署观测；公共仓库不包含运维方控制入口、实时部署地址或 Grafana 线上实例。
Prometheus 的最终采集入口是槽位外 `agent-vm-deployd` 的只读 `/metrics` endpoint。

| 验收项 | 场景与证据 | 结果边界 |
| --- | --- | --- |
| A1 | `skills/agent-vm-ops/tests/test_resource_overcommit.py` 和 `skills/agent-vm-ops/tests/test_live_memory.py` 覆盖 pressure-only admission、logical ceiling rejection、缺失/矛盾 capacity 以及既有 lease 操作不变。 | 离线行为已覆盖；真实 CT cgroup/LXCFS 行为仍属于部署证据。 |
| A2 | Resource policy fixture 断言 `memory_budget_mib=32768`、`memory_overcommit_ratio=2`、CT-visible `32768 MiB`、logical limit `65536 MiB`，以及从 reservation 推导的 logical remaining。 | 离线合同已覆盖。 |
| A3 | Controller 和 CLI 测试覆盖无 session 的 `admin-metrics`、拒绝 session/operation 字段的只读行为、resource split 字段、CT-local memory observation、task/audit/failure summary 和 structured error。 | 离线协议已覆盖。 |
| A4 | `docs/specs/agent-vm/`、后继 memory ADR、运维引用和 Skills 统一使用 32/64 GiB authority，并把 pressure 与 admission 分开；文档测试覆盖结构与链接，行为测试覆盖数值策略。 | 公共合同已同步。 |
| A5 | 公共观测合同规定 deployd 读取 `admin-metrics`、通过 `/metrics` 输出稳定 Prometheus families、保留 freshness/up failure 语义、提供固定 32/64 GiB line、动态 reservation/remaining/actual/pressure series，并区分 disk domain。 | 运维方的 deployd、Prometheus scrape 配置和 Grafana provisioning 在本仓库之外部署和检查；受保护的运维证据记录部署 revision 与 reload 结果。公共源文件只记录合同，不复制部署细节。 |
| A6 | 必要的 live checks 是 `agent-vm doctor`、`agent-vm admin status`、`agent-vm admin metrics`，以及保留 Auto refresh 的 Grafana reload observation。 | CT-local live checks 和运维方 Grafana reload 通过配置的控制入口完成；受保护的运维证据记录命令输出和观测结果，不隐含 PVE 或猜测的主机。 |
| A7 | Implementation candidate `f58453a` 通过 `bun install --frozen-lockfile`、`bun run typecheck`、`bun run check`、`bun run test:repo`、`bun run test:skills`、Agent VM documentation tests、Python compilation 和 `git diff --check`。 | 验证绑定到 implementation candidate `f58453a`；后续文档变更不包含 runtime change，当前 HEAD 的 CI 仍属于独立 release gate。 |

Logical limit 是 reservation ceiling，不承诺 64 GiB guest RAM 可以在没有 CT memory pressure 或 OOM 的情况下同时运行。
`MemAvailable`、`memory.current` 和 cgroup OOM counter 仍然是 observation，不得替代或降低 logical reservation calculation。
