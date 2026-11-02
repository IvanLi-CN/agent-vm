# Agent VM 控制面发布合同

本文定义 Agent VM 客户端和控制环境的人工发布边界。
它不是公共的 `agent-vm` 生命周期命令，也不授予 PVE、宿主机管理器、Incus 策略 API、guest 或 HTTP 网关的访问权限。
产品发布完成的标志是经过验证的 GitHub Release 及其版本化安装包；部署是独立且必须显式触发的操作。
蓝绿设计使用两个完整的 Agent VM 服务槽位、一个稳定的 Caddy 入口，以及一个位于槽位之外、供运维者使用的 `agent-vm-deployd` 控制器。
它不增加公共路由、Web/BFF、公共 native 生命周期服务或公共发布封装器；下文的 format-1/2 封装器仅作为旧版兼容路径保留。

## 产品格式 3 与 4

Client product package 使用 `release-manifest.json` format 3 和一个 product SemVer。
Native Linux control packages 使用固定的 format-2 Go manifest，由已安装的 legacy wrapper 消费。
Management-enabled product package 使用 format 4，并新增 embedded Web UI、local fonts、OIDC/Token/Agent API compatibility 以及下方的 state-compatibility declaration。
Product formats 3 和 4 携带相同的 exact 40-character Git `source_revision`、SHA-derived `release_id`、target、fixed Bun/Go toolchain、protocol 4、action compatibility windows 和 per-file byte/mode digests。
`client-release.json` 携带 package 的 client view。
只有 builder 证明 checked-out commit 等于声明的 `source_revision` 时，package 才接受；任意 SHA string 不是 provenance。

`release_id` 不是第二个 product version。
它是 legacy protocol-4 peer capability field，仍然是 Git SHA；绝不能填入 SemVer product version。
Format 1 仍是 read-only legacy input，format 2 是 native deployment 和 rollback format，format 3 是 read-only product/rollback input，management-enabled production builders 写入 format 4。

安装有效 format-3 或 format-4 sidecar 时，client 打印 package SemVer；development 或没有 product version 的 legacy installations 打印 `dev`。
Go control response 对 format-3 和 format-4 product manifests 包含 `version`，同时保留 SHA-derived `release_id` 和 `source_revision` compatibility fields。

## 发布清单

Python release tools 使用[`release_manifest.py`](../../../../skills/agent-vm-ops/scripts/release_manifest.py)验证 format-1/2 migration、native format-2 package 和 product formats 3/4；Bun controller 使用[`release-manifest.ts`](../../../../skills/agent-vm-ops/scripts/release-manifest.ts)验证 runtime manifests、product manifests 和 artifact bytes。
Format 2 仍是 native deployment path；format 3 是 read-only product compatibility path；format 4 是由 Bun、Python 和 Go 消费的 management-enabled product path。
Bun 和 Go test suites 覆盖 isolated package 和 transaction behavior；native Linux/Incus evidence 仍以 deployment-boundary change 为条件，不属于当前 governance candidate。

在发布 format-4 package 前，Bun、Python 和 Go readers、client sidecar readers、builder、verifier、installer 和 rollback wrapper 都必须验证 format 4。
Format-4 package 保留 protocol 4；manifest-format compatibility 与 client/control protocol compatibility 是独立 contracts。

每个 artifact 只声明 relative path、role、其 bytes 的 exact SHA-256 和 numeric mode。
Artifact path 必须解析为 release root 内的 regular file；absolute paths、parent traversal、symlinks、non-regular files、manifest self-checksums 以及 mode/checksum mismatches 都 fail closed。
Bun client package 要求 `agent_vm.ts` 和 `sync_worktree.ts` 使用 mode `0755`，`byte-fs.ts`、`client-io.ts`、`client-release.ts`、`client.ts`、`guest-source.ts` 和 `sync-native.ts` 使用 mode `0644`。
Complete release 必须同时携带该 package、`agent_vm_control.ts`、`release_manifest.py`、controller TypeScript support modules（包括 `guest-root-growth.ts`）和 fixed systemd assets。
它排除 configuration、credentials、SSH material、state databases 及 WAL/journals、locks、Incus container data 和 guest images。
在 Bun entrypoint 旁恢复 Python client 不是 compatibility strategy。

每个 action 声明 `request.schema`、`request.semantic_revision`、`response.schema`、`response.semantic_revision`，以及包含 `schema_min`、`schema_max`、`semantic_min` 和 `semantic_max` 的 explicit `client_to_control` 和 `control_to_client` objects。
JSON parseability 不是 compatibility evidence。
Control process 在 journal record、native handler 或 timer preemption 前检查 protocol、action、request window、response window 和所有 artifact bytes。

## 蓝绿兼容窗口

Publisher 在 slot switch 后至少保留 current 和 previous verified package、static resource manifest 及 state capability declaration 一小时。
每个 candidate 声明 common protocol-4 action set、direct HTTPS Agent API transport、Token API 和 browser management API 的 request/response/semantic windows。
Candidate 只有在能够 read/write shared state 中每个 present capability，且 OIDC issuer/client/redirect configuration compatible 时，才可成为 active。
Caddy 只切换受保护的 `active` link；绝不 retry write，也不根据 external request 选择 slot。

一小时 window 覆盖 in-flight browser assets、OIDC callback transactions、browser sessions、Token revocation 和 protocol-4 requests。
超出该 window 的 page 或 client 收到 structured compatibility 或 refresh result。
Previous package 只有在其 capability 和 API declarations compatible 时才是 valid rollback target；rollback 绝不 downgrade、recreate 或 overwrite shared state。

<a id="deployment-controller-boundary"></a>
## 部署控制器边界

完整的槽位角色、阶段、状态和数据库合同见[部署控制器合同](deployment-controller.md)。

蓝绿发布事务只处理服务槽位和路由选择，不执行业务状态迁移、rqlite backup restore、业务数据库替换或状态后端 endpoint 切换；这些操作属于独立的状态后端流程。

当前蓝绿部署事务由稳定的宿主级 `agent-vm-deployd.service` 负责。
它运行在两个槽位之外，不会随槽位切换而被替换。
`agent-vm-deploy` 是它的运维 CLI，只连接受保护的本机 Unix socket。
该控制器不提供公共业务监听器或业务 API；它另外提供仅限内部管理网络的只读 `GET /metrics` 给 Prometheus。
它没有 Incus/PVE 管理权限，也没有写入业务状态或 rqlite 的权限。

Deployd 使用部署专用的本机 SQLite 数据库，路径为 `/var/lib/agent-vm-native/deployment/deployment.sqlite3`。
数据库保存部署事务标识、阶段转换、阶段事件、失败证据和最新观测快照；不保存任务/会话状态、lease、generation、operation journal 或 rqlite 数据，也不把每次 native 轮询作为权威历史。
这里选择 SQLite，是因为需要阶段转换原子性、并发运维读取、可索引的事务历史和守护进程崩溃恢复。
JSONL 可以导出为审计证据，但不是部署事务权威；deployd 不使用业务状态后端保存部署事务。

控制器从受保护的 `active` 链接、发布清单、systemd、两个槽位 socket、槽位就绪/角色合同、状态纪元和能力观测、native-worker/pending-operation 状态以及流量计数器读取事实。
它记录这些事实的新鲜度和来源；SQLite 快照不能覆盖更新或互相矛盾的来源事实。
候选槽位可以在当前槽位保持 active 时以 `standby` 运行。
即使两个槽位进程同时存活，也只能有一个槽位拥有运行 scheduler、reconciliation、age GC 和 scheduler-driven native mutation 所需的控制器主权。

事务阶段为 `stage`、`standby_validation`、`controller_handoff`、`traffic_enable`、`active_switch`、`drain_old`、`quiescence`、`finalize`、`rollback` 和 `needs_recovery`。
当就绪状态、状态能力、状态纪元、控制器主权、流量排空、在途请求或 native-worker 证据缺失或矛盾时，activation 或 rollback 必须 fail closed。
旧版封装器可以 stage 和 verify 兼容的 format-1/2 安装包，但不是当前蓝绿事务的权威。

## 管理状态兼容性

State backend 的 `state_capabilities` registry 存储 versioned durable-state markers。
Migration 前从 SQLite 读取；migration 后从 shared rqlite 读取。
Capability set 由 management System API 以 sorted `name@version` strings 返回。
没有 capability table 的 database，只有在其 existing protocol-4 schema 验证通过后，才视为 `legacy-agent-vm-core@1`；unknown 或 malformed state fail closed。

第一个 management-enabled release 使用以下 markers：

| Capability | 含义 |
| --- | --- |
| `legacy-agent-vm-core@1` | Existing protocol-4 tasks、generations、leases、reservations、ports 和 operation journal。 |
| `managed-image-catalog@1` | Immutable image records、labels、verification、references 和 default selection。 |
| `session-image-binding@1` | Session 和 cache identity 继续绑定 exact image fingerprint。 |
| `management-operation-audit@1` | Durable administrator operation identity、current actor、typed rejected-request 或 `legacy_unknown` actor、optional reason、target、phase 和 outcome。 |
| `principal-identity@1` | Configured OIDC issuer/subject 是 sole administrator actor；绝不猜测 claims 或 historical user mappings。 |
| `token-revocation@1` | Opaque machine-Token digest、resource version、scope、expiry 和 revocation state 在 slots 间 durable。 |

Format-4 manifest 声明：

```json
"state_compatibility": {
  "reads": [
    "legacy-agent-vm-core@1",
    "managed-image-catalog@1",
    "management-operation-audit@1",
    "principal-identity@1",
    "session-image-binding@1",
    "token-revocation@1"
  ],
  "writes": [
    "legacy-agent-vm-core@1",
    "managed-image-catalog@1",
    "management-operation-audit@1",
    "principal-identity@1",
    "session-image-binding@1",
    "token-revocation@1"
  ]
}
```

两个 arrays 都必须 sorted、duplicate-free，且只列出 exact supported markers。
Activation 要求 candidate 对 database 中每个 present marker 同时声明 read 和 write support。
Formats 1、2 和 3 没有 declaration，只表示支持 `legacy-agent-vm-core@1`；它们与 migrated management database 不兼容。

Installer 在改变 active pointers 或启动 services 前执行 candidate package 的 preflight check。
Rollback preflight 对 rollback target 执行同样的 comparison。
Compatible rollback 恢复完整的 earlier product package，并继续使用同一 database。
Incompatible target 必须在 activation 或 service start 前拒绝；current package 保持 selected，admission 保持 drained，等待 operator recovery。
Installer 绝不丢弃 capability markers、downgrade schema、recreate database，或让 older process 针对其无法 read/write 的 state 启动。
Post-migration health failure 不能绕过该 check。

Client 只读取 local sidecar client view。
Legacy format 1/2 sidecars 具有 exact top-level fields `format`、`release_id`、`source_revision`、`protocol`、`client_artifact`、`client_dependencies` 和 `client_view`；format 3 将 product identity portion 替换为 `format`、`version`、`source_revision`、`protocol`、`client_artifact`、`client_dependencies` 和 `client_view`。
`client_artifact` 以 role `client` 标识 `agent_vm.ts`；`client_dependencies` 以 role `support` 列出其他七个 fixed client files。
每个 entry 恰好有 `name`、`path`、`role`、`sha256` 和 `mode`，sidecar 旁只允许 basename-only paths。
Missing、duplicate、extra 或 changed dependencies 必须在 SSH 前失败。
View 包含 complete fixed action set 和 explicit request/response compatibility windows。
Missing sidecar 选择 explicit `legacy-protocol-4` capability 并返回 structured warning，不会静默声称 current release。
Present 但 malformed、path-invalid、mode-invalid 或 checksum-invalid 的 sidecar fail closed，包括 old Python-only sidecar。
不同 revisions 间的 compatible action 成功时返回 top-level warning，write-operation replay 保留该 warning。

### SQLite 到 rqlite 迁移

Initial state migration 采用 conservative 方式。
Service 进入 state-write fence、拒绝新的 browser/API/Agent durable mutations、暂停 scheduler/reconciliation/timer durable writes，并排空 in-flight transactions 和 native workers 期间，SQLite 继续作为 sole writer。
每个 native worker 必须在 original operation ID 下以 durable result 完成；service 在关闭 SQLite writers 前必须验证其 PID start identity、process-group absence 和 known Incus operation identity 已 quiescent。
Pending/unknown native outcome、active worker 或 unverifiable quiescence 会阻止 final export；service 不能只记录它然后继续。
只有之后才能创建包含 schema、capability、row-count、generation、journal 和 identity summaries 的 consistent backup/export。
Schema/capability preflight 后，目标 rqlite state 通过一个 `POST /db/execute?transaction` request import；migration 不使用 SQL `BEGIN`/`COMMIT`、dual writes、shared SQLite file 或 guessed reverse synchronization。
Target 在所有 summaries 和 journal ordering 验证前保持 read-only，然后启动一个 slot 完成 readiness，最后才改变 protected active link。

Cutover 后，rqlite 是唯一 live state authority。
Critical administrator-authorization、lease、generation、operation-journal、Token-revocation、capability 和 observation-epoch reads 使用 linearizable consistency。
Cross-row updates 使用一个带 conditional updates 或 unique constraints 的 rqlite transaction。
Lost commit response 或 native result 在 original operation ID 下保持 pending/unknown，绝不以 new ID replay。
如果 rqlite unavailable 或其 data validation failure，release 保持 state-write fence 和 native admission closed，同时保留 active leases。
Recovery 首先要求 verified post-cutover rqlite backup/snapshot，其 recorded coverage boundary 不早于 last trusted commit。
Recovery process 必须将该 boundary 之后的 backup tail 与 trusted append-only journal 和 native-effect evidence reconcile；仅 schema、digest、epoch 或 journal checks 不能证明 backup coverage。
只有证明没有 post-cutover durable write、epoch advance、lease change 或 native effect 时，pre-cutover SQLite export 才可采用。
没有 admissible backup 和 proven tail 时，必须在 resume 前执行 administrator-authorized journal/native reconciliation；service 不得打开 stale SQLite 作为 second authority。
旧 SQLite file 是 protected recovery material，不是可并发打开或 downgrade 的 rollback database。

### 状态恢复材料保留

蓝绿发布成功不构成状态备份删除条件。
恢复材料保留合同固定为最多三个已验证恢复点、三十天候选年龄，以及同一控制环境内全部恢复材料 1 GiB 的总硬上限。
1 GiB 按实际文件大小计算，不是预分配空间，也不存在体积下限。
最后两个可验证恢复点不能删除；迁移源 SQLite snapshot 在迁移完成并通过新的 rqlite restore check 后保留七天。
只有在没有 pending/unknown operation 或未解决的 `needs_recovery`、对应保留期已到期，且清理后仍保留两个恢复点时，材料才可进入 `purge_eligible`。
唯一 active `deployd` 的 retention worker 在每次成功备份后和每小时执行清理。
预算不足时必须返回 `backup_storage_exhausted` 并拒绝新的备份或状态迁移；任何删除都必须针对准确的材料 ID 执行并写入审计。
状态数据库中的终态操作和审计历史 MUST 默认保留三十天后回收；不得通过放大恢复材料预算替代历史记录回收。

## Wrapper 边界

封装器激活规则的固定合同短语是 `does not stop or disable`，含义是它不会停止或禁用 Go service scheduler。

本节的固定封装器是既有的 format-1/2 兼容发布器。
当前蓝绿的槽位就绪状态、共享状态检查、控制器主权、流量排空、受保护的 `active` 切换和事务恢复由槽位外的 `agent-vm-deployd` 服务负责。
封装器仍是兼容发布器，不是第二个部署控制器。
封装器和 deployd 都不得独立拥有业务生命周期状态或写入 rqlite。

Format 4 client sidecars 使用与 format 3 相同的 exact product identity 和 client-view fields，只是 `format: 4`。
它们不携带 server database 的 `state_compatibility` declaration；该 declaration 只属于 complete product release manifest，并由 installer enforce。

Operator 在固定 path `/usr/local/sbin/agent-vm-release-wrapper` 预安装 root-owned narrow wrapper，并提供 protected deployment configuration。
Format 1 使用 `/opt/agent-vm-native/agent_vm_control.ts` 和 `/usr/local/bin/bun`；format 2 安装 `/opt/agent-vm-native/bin/agent-vm-control`、fixed bridge 和 `agent-vm-native.service`。
Systemd target directory 固定为 `/etc/systemd/system`，activation fence 固定为 `/run/agent-vm-native/release-activation.fence`；wrapper configuration 不能 redirect 这些 targets。
其 fixed actions 是 `stage`、`verify`、`activate`、`rollback` 和 `status`。
Callers 只提供 transaction/release identity 和 structured action；wrapper 不接受 arbitrary destinations、shell commands、configuration paths、state database paths 或 VM paths。

`stage` 从 standard input 读取 private bundle，只将 regular files 解包到 transaction staging。
它拒绝 absolute 或 parent-traversing names、symlinks、hard links、devices、duplicates 和 oversized members。
`verify` 检查 selected format、exact bytes、regular-file owner 和 mode；不执行 candidate binaries，验证 Go target/toolchain 和 retired-unit set，编译 Python support artifacts，使用固定 `/usr/local/bin/bun` 将 manifest-listed TypeScript 作为 data transpile，验证 fixed systemd unit names 和 canonical bytes，并 enforce selected fixed artifact set。
`activate` 要求 verified transaction 和 admission drain，在 drain check 前创建 fixed activation fence，重新验证 retained current/previous releases，原子保留 current/previous release pointers，安装 complete fixed client package 和 selected control artifacts，并 MAY 运行 `systemctl daemon-reload`；它不停止或禁用 Go service 的 in-process scheduler。
`rollback` 使用相同 artifact 和 systemd checks 验证 previous retained release，拒绝 stale transactions，每个 transaction 最多允许一次 automatic attempt（包括 interrupted activation record），绝不重写 durable lifecycle state、generations 或 uncertain operations。

## 发布事务

手工发布器 [`agent_vm_release.py`](../../../../skills/agent-vm-ops/scripts/agent_vm_release.py) 仍是 format-1 发布器和旧版兼容路径，默认构建 legacy format-1 client bundle，并且只能发布通过同一 fixed manifest verifier 的 bundle。
Format-2 native package 和 product formats 3/4 由各自的专用 builder 构建，并分别执行其 artifact 和 state checks。
Format-2 bundle 构建器是[`build_go_release.ts`](../../../../skills/agent-vm-ops/scripts/build_go_release.ts)；它使用 `CGO_ENABLED=0` 为 `linux/amd64` 或 `linux/arm64` 构建二进制文件，在不执行候选产物的情况下校验字节和元数据，并同时发布固定 bridge 与 `agent-vm-native.service`。
当前蓝绿生产激活通过 deployd 执行，而该发布器继续负责产物校验和兼容安装包行为。
发布必须保留本地校验、远端暂存/校验、通过当前客户端传输执行的排空，以及最多每 1 秒一次的 `admin-metrics` 轮询。
Format-2 迁移排空预算默认 8100 秒且不得超过 8100 秒；激活另有 300 秒上限。
它不得停止或禁用 Go 服务 scheduler、强制终止在途 operation，或创建替代 operation ID。
固定的远端封装器仍是 `/usr/local/sbin/agent-vm-release-wrapper`。

Convergence 后，publication 必须激活 verified remote release，并检查 `doctor`、`admin-metrics`、pending operations、task transitions 以及 timer/reconcile health。
用于 convergence 的 pending-operation view 只包含已越过 native-effect barrier（`running` 或 `reconciling`）的 operations；queued work 尚未开始，可以 retry，并且 drain active 时由 durable fence 阻止进入 native execution。
Pre-activation failure 保持 current release active 且 drained。
Post-activation health failure 允许一次 rollback attempt；successful rollback 必须在 drain exit 前重新验证，failed rollback 则保持 admission drained。
只有 remote health 成功后，publisher 才可安装并 self-check local client。
本地安装获取 exclusive package lock，stage 全部八个 client files 和 sidecar，并在 sanitized environment 和 30 秒 budget 下运行 `bun --no-macros <staged-package>/agent_vm.ts version`。
Missing Bun 或 unverified sidecar 必须在 active files 改变前失败。
每个 file 使用 exclusive temporary creation、fsync 和 atomic rename；sidecar 最后安装。
Private format-2 recovery record 在 mutation 前绑定每个 previous/target file 的 hash/mode、fixed destination 和 precise staging path。
该 record 存在时 clients 拒绝 requests，包括 sidecar 暂时 absent 的情况。
Installer 在 durable mark completion 前验证 installed package；incomplete transaction 的 recovery 恢复所有 previous files，completed transaction 则保留所有 verified target files。
它在任何 recovery mutation 前验证每个 destination、backup 和 staging entry，并且自身可在 interruption 后 resume。
Unknown paths、unverified backups、partial/legacy recovery records 和 old Python-only installations 要求 operator inspection，并 fail closed。
Per-file atomic renames 和 recovery fence 提供 complete-package transaction，但不声称 simultaneous directory exchange。
Local client failure 必须保留 old client、报告 structured warning、不 rollback healthy remote release，并保持 drain closed。
Manual rollback 必须验证 supplied previous bundle、rollback remote transaction、安装同一个 complete local client package，并且只有两侧都 healthy 后才 exit drain。
Complete publication 最后 exit drain。

Activation timeout 为 300 seconds，default poll interval 为 1 second，migration drain budget 默认 8100 seconds。
Operators 可以降低这些值，但不能超过 activation、drain 或 poll safety bounds。
Old releases 保留；deletion、unattended updates 和 deployment inventory management 不属于本合同。
Format-2 scheduler 自行执行 lease expiry、bounded retry 和 age-GC；external expiry units 由 manifest retire。

## 公共与私有记录

Public manifests、tests 和 documents 只包含 relative source paths 和 neutral deployment placeholders。
Live SSH alias、identity、credentials、release root、wrapper configuration、target paths、state database、timer inventory 和 controlled staging observations 都是 operator-owned records，保留在本仓库之外。
