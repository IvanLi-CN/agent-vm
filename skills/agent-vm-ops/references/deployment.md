# 原生部署边界

从同一个 checkout 的 Agent Toolkit revision 安装匹配的客户端、Go 控制服务、兼容桥、槽位外部署控制器和 systemd 资源。
将 revision 与文件校验和记录在受保护的运维记录中，不创建已安装源码分支。
下方没有槽位后缀的单服务路径是 format-2 兼容基线；目标蓝绿布局和事务边界见[Deployd 合同](../../../docs/specs/agent-vm/contracts/deployment-controller.md)以及[蓝绿资源](../../../docs/deployment/blue-green.md)。

如果必须重建或恢复控制环境，应保留 state 和 container data，并在替换文件前遵循[拓扑与恢复运行手册](../../../docs/specs/agent-vm/OPERATIONS.md)。
本部署参考不识别 CT109 下方的宿主机，也不提供宿主机重建流程。

| 面 | 部署输入 |
| --- | --- |
| 旧版 format-2 控制服务 | `/opt/agent-vm-native/bin/agent-vm-control`，由 `agent-vm-native.service` 启动 |
| 目标蓝绿槽位服务 | `/opt/agent-vm-native/slots/{blue,green}/bin/agent-vm-control`，由 `agent-vm-native@{blue,green}.service` 启动 |
| Deployd | `/opt/agent-vm-native/deployd/bin/agent-vm-deployd`，由两个槽位外的 `agent-vm-deployd.service` 启动；Prometheus 从其内部管理监听的 `GET /metrics` 拉取指标 |
| 槽位观测目录 | `/run/agent-vm-native/{blue,green}/metrics`；每个槽位独占自己的 snapshot 目录，deployd 按槽位读取 |
| 目标部署状态 | `/var/lib/agent-vm-native/deployment/deployment.sqlite3`，独立于业务状态和 rqlite |
| 兼容 bridge | `/opt/agent-vm-native/agent_vm_control_bridge.ts`；format-1 release 可以保留旧版 Bun controller |
| 控制解释器 | format 2 使用 Go binary；root-owned `/usr/local/bin/bun` 只对 format-1 Bun release 和 client package 保持必需 |
| 配置 | `/etc/agent-vm-native/config.json` 中由 root-owned 的 JSON |
| Web 配置 | 启用管理功能时，`/etc/agent-vm-native/web.json` 中由 root-owned 且 `agent-vm-web` group-readable 的 JSON |
| 持久状态 | 配置的本地 state database 和 lock directory |
| 旧版原生 unit | `agent-vm-native.service` 和 format 4 可选的 `agent-vm-web.service`；format-1 timer unit 仅在兼容支持期间保留 |
| 本机客户端 | 完整的八文件 Bun package 和带依赖 hash 的 `client-release.json`；受保护的 session state 位于运维者选择的 XDG paths |

部署通过复制或派生的 configuration 提供 control alias、Incus project/profile/image、network ACL、control address、guest address pool、host port range、storage path、state database 和 timer directory。
`config.json.example` 是通用 schema/template，不是部署证据；对于锁定的部署策略，已安装 configuration 必须设置 `memory_budget_mib=32768`、`memory_overcommit_ratio=2` 和 `cpu_startup_pressure_full_avg10_percent=50`。
后者是 startup-schedulability threshold，不是 logical CPU ceiling。
安装前必须替换 placeholder；缺失或无效值必须 fail closed。

启用管理功能时，必须在启动 unit 前创建专用的 `agent-vm-web` system group。
Web configuration 必须以 `root:agent-vm-web`、mode `0640` 安装，所在 directory 保持为 `root:agent-vm-web`、mode `0750`。
Web unit 启动时会重新应用这些固定的 metadata permissions；它不会授予 Web UID 访问 primary control configuration、SQLite state 或 generic Incus administration 的权限。
native unit 以 root 运行，并将 `agent-vm-web` 作为 effective group，使专用 authority socket 使用 `root:agent-vm-web`、mode `0660`，位于 `root:agent-vm-web`、mode `0750` 的 runtime directory 中。

与部署无关的 Incus project/profile baseline 及其默认只读的 initializer 见[初始化参考（initialization reference）](initialization.md)。
只能在明确授权且 admission drain 已生效的维护任务中应用它；日常请求绝不修改 project policy。

控制服务和兼容请求适配器读取同一个规范路径：`/etc/agent-vm-native/config.json`。
部署通过在该路径安装受保护 JSON 来选择 Incus project、image、network、address pool、port range、storage、state database 和 ACL；其他运行时配置路径明确不支持。
在单服务基线中，Go 服务根据 SQLite deadline 负责 expiry、reconciliation、retry 和 age-GC scheduling。
在目标蓝绿布局中，这些后台循环只有持有 deployd 已认证 controller role 的槽位才能启用；standby 和 retiring 槽位必须保持 scheduler-driven native mutation fenced。
`timer_dir` 仅为 format-1 兼容和历史记录保留。
静态 release unit 校验始终使用 `/etc/systemd/system`。

任何 mutation 前，检查实际的 Incus identity、proxy permission、ACL ownership、guest isolation、本机 filesystem capacity、service readiness、UDS peer policy 和 protocol version。
凭据和 connection material 必须放在 source 之外。
受保护 UDS 是内部 control boundary，不是 public gateway。

单独准备并验证 pinned guest image。
确认新 guest 生成不同的 host key、达到 readiness，并且只暴露已配置的 service。
使用隔离资源验证 expiry、精确 mapping cleanup、cache reclamation、capacity pressure 和 generation handling；不要把生产操作用作 migration gate。

## 控制面发布

control-plane release 是一个逻辑 compatibility unit，包含 local client、Go control service 或 legacy bridge、systemd assets、release metadata 和 exact artifact checksums。
它排除 deployment configuration、credentials、connection material、state databases 及 WAL/journals、locks、Incus container data 和 guest images。
release manifest 声明 protocol、source revision、artifact integrity、适用时的 target/toolchain，以及每个支持方向上的 per-operation request/response/semantic compatibility。

公共 manifest、wrapper、publisher 和 failure-state contract 见[Agent VM 控制面发布合同](../../../docs/specs/agent-vm/contracts/release.md)。
只有使用 owner 提供的 deployment input 时才能运行仓库脚本；不得把这些 input 写入仓库。

发布是人工且由运维者负责的操作。
在目标蓝绿布局中，运维者通过受保护的本机 deployd socket 使用 `agent-vm-deploy`；SSH/wrapper 发布保留为兼容路径。
Prometheus 只访问 deployd 的内部 `GET /metrics`，不访问槽位 socket、部署 SQLite、snapshot 文件或 Caddy。
切换期间两个槽位可以同时存活，因此 deployd 输出的槽位级指标必须带 `slot` label；Prometheus 只配置 deployd 一个 scrape target。
不得把两个槽位都配置为共享的 `/run/agent-vm-native/metrics`，否则 snapshot 会互相覆盖。
它解析明确的 source revision，在本地 build 和 check manifest，暂存精确候选产物，并在 activation 前验证 owner、mode、syntax、checksum、unit contents、readiness、capability、controller role、traffic counter 和 native quiescence。
部署身份可以调用用于旧版受保护产物和 systemd 路径的最小范围 root-owned wrapper，但不得获得 PVE、host、Incus project-policy、guest、额外 SSH、HTTP 或 gateway access。
不得通过公共 `agent-vm` CLI 或 VM guest 发布。

remote control release 必须先激活并完成 health check，之后才能修改 local client。
固定 wrapper 只会把完整 Bun client package、按 format 选择的 Go binary 或 bridge、带 dependency hash 的 sidecar、release manifest 和 systemd assets 安装到受保护目标。
兼容的旧 client 可以带 structured warning 继续使用受支持的 action；不支持的 action、不兼容的 protocol/schema/semantic change、无效 transport 或未验证 checksum 必须在产生副作用前失败。
兼容演进保留 protocol 4；breaking change 可能需要显式 coordinated update。
无人值守的 control-plane update 默认关闭。

远端 script 或 unit 变更必须在 activation 前进入 admission drain。
wrapper 持有固定的 `/run/agent-vm-native/release-activation.fence`，直到 activation 或 rollback 完成；control-plane drain exit 会在 `admission.lock` 下重新检查该 fence，因此 admission 不会在 drain 检查和 pointer replacement 之间重新开放。
Drain 会阻止新的 acquisition 和 restart admission，保留 status/retrieval/release 以及 expiry/reconciliation/cache-cleanup timers 可用，并且绝不强制终止 in-flight operations。
等待 pending operations 和 task transitions 收敛，原子激活候选 release，运行 health/reconciliation/timer checks，并且只有成功后才能 exit drain。
activation 前失败会保持当前 release 不变。
activation 后 health failure 允许向上一个 verified release 执行一次有界 rollback；rollback 失败时保持 admission drain，绝不改写 state、generations 或 uncertain operations。
至少保留当前和上一个 verified release，以及它们的 manifest 和 checksum。

日常请求绝不修改 Incus project policy。
release manifests、run records、protected recovery copies 和 live infrastructure evidence 必须放在公共仓库之外。

活动的 format-2 runtime 包含 Go binary、fixed Bun client package、bridge、sidecar、manifest 和 `agent-vm-native.service`；只有 startup reconciliation 就绪后才会 retire 旧的 expiry/reconcile units。
Activation 仅在旧版 `agent_vm.py` 和 `agent_vm_control.py` 存在于原固定 target 时才会 snapshot 并移除；activation 失败时，整个 fixed package 会恢复它们原有的 bytes、owner 和 mode。
无关的 operator files 和保留的 release directories 不是 cleanup target。
历史 Python-only control path 不能作为任一 format 的 fallback。

通用 format-1 流程是 `python3 skills/agent-vm-ops/scripts/agent_vm_release.py build --source-root <source> --output <bundle>`、本地 `verify`，然后 `publish`；format 2 使用 `bun skills/agent-vm-ops/scripts/build_go_release.ts --source-revision <revision> --target <linux/amd64|linux/arm64> --output <bundle>` 和同一个受保护 wrapper transaction。
保留旧调用时，可选的 legacy `--wrapper` argument 必须等于 `/usr/local/sbin/agent-vm-release-wrapper`；Go binary、bridge 或 socket 不接受 publisher override。
`rollback` 是绑定 transaction 和 release identity 的独立 operator action；它的 bundle 会在 drain exit 前恢复匹配的八文件 client package 和 control artifacts。
本地安装需要用于 client package 的 Bun、受限的 staged `version` self-check、私有的 complete-package recovery record 和 exclusive lock。
sidecar 最后安装；recovery pending 时 clients fail closed，中断安装会恢复整个 previous package。
固定 mode、exact owned paths 和 hash-validated recovery 见 release contract。
format-2 client 通过 SSH 调用 Go `request` adapter，保留 protocol 4、受限的 stdin/stdout/stderr 和 action-specific timeouts。
wrapper 的 fixed command set 是唯一的 remote activation interface。

## 观测边界

Dashboard 及其 provisioning source 由部署方拥有。
指标语义使用[公共观测合同](../../../docs/specs/agent-vm/contracts/observability.md)，但 Grafana JSON、Prometheus scrape target、collector path 和 datasource identity 必须保留在 deployment repository。
Go service 发布固定的 read-only snapshots，protected relay 读取这些 snapshot 时不会触发 sampling；capacity、inventory 和 filesystem textfiles 分别具有独立的 freshness 和 health。
发布对应 dashboard 行之前，验证 deployment configuration 的 32768-MiB memory base、2x memory ratio 和 50 percent CPU startup-pressure threshold。
将 CT-local CPU PSI 与 startup-pressure observation 独立于 logical CPU admission 导出；不要把 Grafana database export 或 Incus project limit `-1` 当作权威的 native admission metric。

## 运行时维护边界

Go service 在配置的 control environment 内执行 health check、reconciliation、lease expiry、bounded retry、cache age-GC 和 observation sampling。
SSH 只是 request compatibility entry；当 SSH 或 HTTP request adapters 不可用时，direct internal scheduler calls 仍会继续。
这些操作不得要求访问 PVE host，也不得使用 `pct exec` 代替 native control path。
将 CT root filesystem 与 Incus data pool 一次性分离，包括移动用于 transient image unpacking 的 `/var/lib/incus/images`，属于 operator-owned infrastructure migration，另见[CT root/data split 方案](../../../docs/solutions/agent-vm/ct-root-data-split.md)。
