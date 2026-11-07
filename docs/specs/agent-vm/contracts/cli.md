# Agent VM CLI 合同

已安装的 executable 是 `agent-vm`，是在已安装 Bun 下直接运行的 TypeScript command line client。
规范源码是[agent_vm.ts](../../../../skills/agent-vm/scripts/agent_vm.ts)，其 sibling modules 保留用于 source-linked execution。Client 和两种受支持的 control transports 都使用 protocol 4。常驻 Go 实现是[agent-vm-control](../../../../skills/agent-vm-ops/control/cmd/agent-vm-control/main.go)；固定 Bun bridge 是[agent_vm_control_bridge.ts](../../../../skills/agent-vm-ops/scripts/agent_vm_control_bridge.ts)。
本机 Bun 必须支持 `bun:ffi`、`Bun.build` 和 direct TypeScript execution；
OpenSSH 与 Git 仍是 native tools，sync 还使用 rsync。
公共源码不会安装或下载 runtimes。

## 身份与配置

Global `--session-id <id>` 选择 managed target，可放在 command 前或后，也可用于 nested commands。
未提供时使用 `AGENT_VM_SESSION_ID`；
显式 input 优先。
两者都缺失时，session-scoped commands 在访问 local keys/journals 或执行 remote writes 前，以 JSON error code `missing_session` 失败。
`version` 和 local help 不需要 target。
`doctor` 允许省略 target。
`admin metrics` 无 session，忽略 session environment，并拒绝显式 `--session-id`。
`exec --` 或 `ssh --` 后的 tokens 属于 native command，绝不会解析为 client identity options。

Control endpoint 从 `AGENT_VM_CONTROL_HOST` 和已验证的 local release sidecar 中选择。
Format 1 使用 `AGENT_VM_REMOTE_CONTROL`；
带 `transport=go-request` 的 format 2 使用固定 Go target `/opt/agent-vm-native/bin/agent-vm-control` 和 socket `/run/agent-vm-native/control.sock`，只允许 deployment-safe test overrides。
Go service 读取 JSON file，其中 environment-specific fields 必须包含 project、profile、image、network、ACL、pool、control address、port range、guest address range、storage path 和 state database。
缺失或无效值必须以 `config_missing` 或 `config_invalid` fail closed。

Control-plane publication、update 和 rollback 都是 operator-owned deployment actions。
公共 `agent-vm` CLI 没有 `update` command，不安装 remote files、不修改 deployment configuration，也不获取 Incus/PVE authority。

独立的运维可执行程序 `agent-vm-deploy` 是部署控制器客户端，详细合同见[部署控制器合同](deployment-controller.md)。
它只连接 `agent-vm-deployd.service` 的受保护本机 Unix socket，并提供部署 `status`、`plan`、`verify`、`stage`、`activate`、`rollback`、`recover` 和一次性的 `migrate` 操作。
它报告事务阶段、选中槽位、进程/就绪状态、控制器主权、流量排空、在途请求、native-worker quiescence、新鲜度和来源。
它不是公共生命周期客户端；
普通蓝绿操作不直接调用 rqlite、不写业务状态，也不提供公共 HTTP/TCP 端点。
首次从 legacy SQLite 部署到蓝绿架构时，`migrate` 只向 Deployd 提交迁移事务，由 Deployd 自动完成排空、停写、快照、导入和验证；
CLI 不接收 rqlite 地址、数据库路径、SQL 或凭据，也不绕过 Deployd。

## 命令

| 命令 | 合同 |
| --- | --- |
| `version` | 不连接 control host，打印 local protocol 和 client version。 |
| `doctor` | 只读的 component 和 admission diagnostics。 |
| `status` | 选中 session 的只读 state。 |
| `acquire [--cpu N] [--memory SIZE] [--disk SIZE]` | 创建或复用选中 session 的 managed container。 |
| `renew` | 在 configured maximum 内延长当前 lease。 |
| `release` | 撤销 access、停止 guest，并保留 eligible cache。 |
| `resize [--cpu N] [--memory SIZE] [--disk SIZE] [--live] [--allow-restart]` | 应用显式授权的 resource change。 |
| `ssh [-- SSH_ARGS]` | 使用生成的 strict connection settings 打开 native SSH。 |
| `exec -- ARGV` | 运行 argument-vector command；需要 shell syntax 时显式使用 `sh -lc`。 |
| `sync --name NAME` | 通过 bundled synchronization helper 传输当前 worktree。 |
| `upload SOURCE DESTINATION` / `download SOURCE DESTINATION` | 在线传输 files，不 renew lease。 |
| `export PATH` | 在 stdout stream tar archive；diagnostics 写入 stderr。 |
| `port add NAME --guest-port N [--protocol tcp\|udp] [--host-port N]` | 创建精确的 native proxy mapping。 |
| `port list` / `port remove NAME` | 检查或移除精确 mapping。 |
| `admin status` | 只读的 native component、maintenance status 和有界 per-task inventory。 |
| `admin metrics` | 面向 `agent-vm-deployd` 的只读 controller metrics snapshot；包含 resource allocation、CT memory、disk capacity/admission、task、warning 和 failure summaries；不创建 session 或改变 state。 |
| `admin drain enter\|status\|exit` | 在允许安全 retrieval 和 release 的同时控制 admission。 |
| `admin session list` | 列出 managed session records。 |
| `admin reconcile [--apply]` / `admin gc [--apply]` | 默认 preview；只在 authorized operations task 内 apply。 |
| `admin reconcile-session --task-id ID --generation N --pending-operation-id ID [--apply]` | 仅对选中 session 的指定 task/generation 和单个未启动 acquire 做过期清理，保留 cache；默认只读 preview。 |

### 精确 session reconciliation

`admin-reconcile-session` 是独立的 Go control action，不降级为全局 `admin-reconcile`；不支持该 action 的 peer 必须在 effect 前拒绝。请求必须同时指定 `session_id`、`task_id`、正整数 `generation`、`pending_operation_id` 和 boolean `apply`。默认 preview 不携带 `operation_id`，不创建 operation intent；apply 使用自己的幂等 operation ID，且不得与目标 pending ID 相同。

该入口仅接受目标 generation 的 inactive、pending acquire：保存的 request identity/digest 必须一致，worker 必须明确为 `starting` 或 `exited`、`start_attempted=false`，且不存在 PID、process group 或 start-time identity。有效或尚未到期的 lease、外部 operation、其他同 session pending intent、未知 native ownership 和矛盾 worker evidence 均拒绝；不重放 acquire，不启动、续租、resize 或删除 container。

Apply 与全局维护共享 process-independent maintenance fence，并在精确 session lock 内重新检查 generation/deadline/ownership。它先保存不可变的原始 worker/request evidence，再通过既有 worker reconciliation 和 expiry-release 路径移除目标 mappings、停止过期 container、保留同 generation cache，最后将指定 acquire 标记为 `lease_released`。Cleanup 不确定时保留 pending journal 和恢复证据，不宣告成功。重复同 operation ID 使用既有 journal replay；不同 target 或 generation 不得复用该 intent。

该 action 不访问全局 pending recovery 列表、不推进维护 cursor、不唤醒全局 scheduler，也不取消其他 acquire。它不会自动部署自身；控制器升级的 startup reconciliation 仍是独立的全局行为，必须单独核对部署授权范围。

`agent-vm-deploy migrate` 是首次部署专用的自动迁移入口。
发布器在启动双槽位前调用它；
Deployd 负责检查 legacy 服务、启动并验证固定的 rqlite 单元、执行一次性 SQLite 导入、写入完成标记，并在任何不确定阶段保持 fenced。
迁移失败时，发布器只允许按原事务身份执行一次受限 retry；
不能把迁移改成发布后的人工前置，也不能把普通 `stage`、`activate` 或 `rollback` 当作迁移入口。

Cached `acquire` resume 受 task 记录的 `image_fingerprint` 保护。
Configured default 变化不会使该 binding 失效；
acquire 继续使用 recorded image。
只有 pinned image 缺失、retired 或其他原因未验证时，才会在启动、resize 或暴露 guest 前返回明确的 cache eligibility error。

`admin-metrics` result 具有稳定的 top-level fields：`observation_epoch`（采样窗口的 durable state epoch）、`observation_consistent`（native probe 前后 durable epoch 是否保持不变）、`observation_attempts`（有界 retry count）、`observed_at`（ISO-8601 string）、`up`、`draining` 和 `admission_ready`（booleans）、`reclaim_required`（boolean；
disk capacity 未验证时为 `null`）、`resources`、`capacity`、`disk_admission`、`default_admission`、`ct_cpu`、`ct_memory`、`instances`、`task_states`、`operations`、`pending_operations`、`image_unpack_failures`、`warnings`、`warning_counts` 和 `doctor`。
`resources` 包含 `allocated`、`reserved`、`allocated_and_reserved`、`remaining`、`limits`、`ct_capacity`、`memory`、`memory_pressure` 和 `cpu_pressure`；
`default_admission` 包含 authoritative default request、physical request cost、readiness、reasons 和 observations，供 deployd 估计可启动的 default managed containers。
`ct_cpu` 包含 CT-visible `capacity_vcpu`、verified CPU PSI observation 和 default-request `startup_pressure` result。
`ct_memory` 包含 CT-local `capacity_bytes`、`current_bytes`、`available_bytes`、`pressure_threshold_bytes`、`pressure` 和 `oom_events`。
`pending_operations` 是 `{operation_id, action, session_id, generation, created_at}` records 列表，用于区分 uncertain journal 与近期没有 operation。
`historical_unknown_operations` 为 superseded operations 使用相同的 identifying fields；
当当前 safety 已验证但原始 outcome 仍 unknown 时，retry 返回 `operation_outcome_unknown` 和 `outcome_verified: false`，绝不 replay original effect。
Controller 在 SQLite lock 外采样 native state，然后比较 probe 前后的 durable epoch；
只有 transaction commit durable SQLite change 时 epoch 才变化，successful read 或 no-op transaction 不会改变 epoch。
它会进行有界次数的 retry；
如果 state 持续变化，则设置 `observation_consistent=false` 并附带 warning。
未验证的 numeric 或 boolean observations 根据 field contract 设为 `null` 或省略，绝不转换成 healthy zero 或 false value。

`admin-metrics` 是 capacity dashboards 的 aggregate source。
`admin status` 是 inventory source，返回 managed states 的 bounded task summaries，包括 task/instance/session identifiers、lifecycle 和 native state、guest availability、requested resources、effective-lease state、lease expiry、SSH mapping 以及 structured last-error code/phase。
Deployd 可以按 task ID join 两个 read-only responses，但不得暴露 raw error text、credentials 或 deployment filesystem paths。
成功 scrape 后必须移除 stale task rows；
任一 action 无法读取或解析时，必须报告 `up=0` 和 freshness failure。

`admin-status` 接受可选的 numeric `limit` 和 task-id `cursor`。
默认 limit 为 25，最大为 100，超过最大值必须拒绝。
每页最多 8 个并发 native read，每个 probe deadline 为 1 秒，整页 deadline 为 5 秒。
结果包含 `limit`、`next_cursor`、`partial`、`unknown`、`observation_consistent`、`probed`、`verified` 和 `timed_out`。
native observation 不可用或不确定时，task 的 `native_status` 必须为 `unknown`、`verified` 为 false，并计入 `unknown`；
`native_missing` 仅是 observation detail，不创建 lifecycle state。

Unknown flags 和 command layouts 必须拒绝；
调用方应使用已安装 build 的 `--help`。

## Request 与 response

Pending operation entries 可以包含与 acquire 绑定的 exact task ID；
recovery proofs 除 session 和 generation identity 外，还要求该 binding。

对于 `admin-metrics`，只有 action 及其 initial Incus version/instance probe 均成功时，`up` 才是 `true`；
它与 `admission_ready` 和 runtime pressure 独立。

`observed_at` 是 final bounded native sampling window 的开始时间，不是 response serialization time；
deployd 的 `/metrics` encoder 评估 freshness 时必须保留它。

Session-scoped requests 包含 `protocol`、`action` 和 `session_id`；
sessionless read-only `admin-metrics` request 省略 `session_id` 和 `operation_id`。
Mutations 还包含 `generation` 和 `operation_id`。
Success envelopes 包含 `ok`、`protocol`、`server_time`、`action`、`session_id`、`generation`、`operation_id`、`outcome`、`result` 以及该 action 的 `response` schema/semantic metadata，并 MAY 包含 structured `warnings` array；
Client 在 finalize local journal 前，MUST 匹配每个提供的 request identity field，包括 `generation`。
省略这些 metadata 的 legacy control response 只有在带 structured compatibility warning 时才接受。
Error diagnostics 是 stderr 上的 JSON，exit status 非零，并 MUST 包含 `ok: false`、`protocol`、已提供的 request identity fields 和 structured `error`；
当 controller 确认不可能产生 native effect 时，标记 `error.detail.outcome_verified`，native-phase 或 durable-state uncertainty 仍保持 pending。
Client 只接受一个 matching error envelope；
unrelated、duplicate 或 type-mismatched envelopes 会使 journal 保持 pending。
SSH stderr 只在 64 KiB diagnostic bound 内保留；
truncated failure 为 `remote_failed`，其 mutation outcome 保持 unverified。
Transport timeout 报告 operation ID，但不声称 mutation 是否完成。
State-database failure 使用 `phase`、SQLite code/name、SQL operation category 和 `state_committed` 报告 `sqlite_unavailable`；
`null` 表示 commit result unknown，original operation 仍需通过 status/reconciliation 处理。

## 控制面兼容性

本节与发布合同（release contract）保持一致；
该英文短语是仓库链接检查使用的固定合同名称。

Release manifest 是 product SemVer、source provenance、artifact checksums、protocol 以及各方向 per-action request/response/semantic compatibility 的 authority。
Production format 3 携带独立的 `version` 和 `source_revision` fields、fixed Bun/Go toolchain metadata、target architecture 和 `legacy-bun|go-request` transport enum。
Formats 1 和 2 是只读 migration 与必要的 rollback inputs。
Compatible action MAY 跨不同 source revisions 运行，并 MUST 暴露 structured compatibility warning。
Client 和 control process MUST 在任何 state 或 native effect 前拒绝 incompatible protocol、schema、semantic behavior、unknown action、invalid transport 或 unverified artifact；
compatibility 绝不能仅根据 JSON parseability 推断。

完整的 manifest 结构、封装器动作白名单、发布顺序、回滚限制、部署控制器边界和私有部署边界定义于[控制面发布合同](release.md)。
公共客户端只读取本地 sidecar，不提供更新、发布、安装或回滚命令；
`agent-vm-deploy` 是独立的运维入口。

具体来说，Bun client sidecar 将 `agent_vm.ts` 标识为 `client_artifact`，并声明 `byte-fs.ts`、`client-io.ts`、`client-release.ts`、`client.ts`、`guest-source.ts`、`sync-native.ts` 和 `sync_worktree.ts` 的完整固定 `client_dependencies` array。
每个 dependency 包含 `name`、sibling-relative `path`、`role: support`、exact `sha256` 和固定 numeric `mode`（`sync_worktree.ts` 为 0755，其他 support files 为 0644）。
Format 3 新增 `version` 并保留 `source_revision`；
其 `client_view.transport` 必须是 `legacy-bun` 或 `go-request`。
存在的 sidecar 必须验证每个 module，拒绝 missing/duplicate/extra dependencies、symlinks 和 altered modes/bytes，并在 SSH 或 mutation journaling 前失败。
Legacy Python artifact declaration 必须拒绝。
缺失 sidecar 时保留显式的 legacy protocol-4 warnings。
Local state 和 operation journals 保持既有 session hash、paths 和 request shape；
即使 local release metadata 变化，retry 仍发送 journaled capability、operation ID 和 generation。
Integer tokens 超出 JavaScript safe-number range 后仍须精确，fractional tokens 与 integer identity fields 不同。

## Management API 兼容性

Management-enabled packages 使用 product manifest format 4 声明 durable state capabilities。
该 manifest change 不改变 protocol 4，也不新增 public client action；
formats 1 到 3 仍是 pre-management rollback targets，在 management-state markers 存在后必须拒绝。

内置 management console 使用[management-api.yaml](management-api.yaml)中的 same-origin `/api/v1` contract。它不包装 CLI output、不 tunnel protocol-4 requests，也不新增 public `agent-vm` command。OIDC 将唯一 administrator actor 确定为精确的 `(issuer, subject)` pair；administrator actions 要求 explicit capability，提供 reason 时记录该 reason，并遵循 contract-defined preview/confirmation 和 audit。Existing `status`、`admin status`、`admin session list`、request envelopes 以及 protocol-4 action compatibility 保持不变。

## Agent API transport 兼容性

Direct HTTPS Agent API 通过稳定的 Caddy entry point 提供 `POST /agent/v1/control`。
它接受来自[Token API](token-api.md)的 opaque Bearer Token 和相同的 protocol-4 JSON body；不接受 browser cookies、forwarded identity headers 或 arbitrary control routes。Service 在 dispatch existing control action 前，检查 Token expiry/revocation、[Agent API 合同](agent-api.md)中的 fixed scope-to-action mapping、target session 的 native identity、generation、operation ID 和 request digest。
Machine Token 不 pre-bound 到 session；
只要 current state 通过现有 checks，valid scope 可以 target 任意 session。
Sessionless administrative observations 要求显式 `agent:observe_admin` scope；
unknown actions 或 scope combinations 必须在 effects 前拒绝。

Release manifest 为 `legacy-bun`、`go-request` 和 direct HTTPS transports 声明 per-action request、response 和 semantic compatibility。
在 blue-green compatibility window 期间，两个 slots 必须接受 common protocol-4 set，并共享 Token/session/journal capability markers。
SSH 在声明窗口关闭前继续作为 compatibility transport；
它不是第二个 lifecycle authority。
超出声明窗口的 transport 或 action 必须在 durable 或 native effects 前失败。

对于 session state，现有 `image_fingerprint` 是 pin 到该 task 的 immutable image identity。
Default 变化只影响 future task creation；
existing active task 或 cache 不会被改写。
Cached acquire 使用 recorded fingerprint 继续，只有 exact image 无法 verify 或 restore 时才拒绝。
只有 pinned image unavailable 时，operator 才必须显式 destroy exact session 的 cache。
Browser API 使用独立的 opaque `resource_version` 进行 compare-and-act，并携带 current VM `generation`；
两者都不能替换 session target 或 existing CLI request identity。

Management state migrations 不改变 protocol-4 request 或 response schema。
SQLite 在 state-write fence 拒绝新的 browser/API/Agent durable mutations、排空 in-flight transactions 和 native workers，并要求每个 native worker 在 original operation ID 下以 durable result 完成且 process-group、PID-identity 和 Incus-operation quiescence 已验证前，继续作为 sole writer。
Pending/unknown native result、active worker 或 unverifiable quiescence 会阻止 one-time export/import；
不能只记录它然后绕过。
Cutover 后 rqlite 是 shared authority，critical administrator-authorization/lease/generation/journal/epoch reads 使用 linearizable consistency。
Verified recovery backup 必须记录不早于 last trusted commit 的 coverage，tail 必须与 append-only journal 和 native-effect evidence reconcile；
仅 internal schema、digest、epoch 或 journal checks 不足以证明覆盖。
如果无法证明 coverage 或 tail，必须保持 fenced，等待 administrator-authorized reconciliation。
引入 new state capabilities 的 release 在 activation 或 rollback 前，必须通过[管理合同](management-api.md)和[发布合同](release.md)的 preflight rules。
即使 active backend 是 rqlite，state-backend outage 仍保留 legacy `sqlite_unavailable` label 以兼容 protocol-4；
新的 browser/API error documentation 使用 `state_unavailable`。

## 源码同步事务

维护中的本机 helper 是[sync_worktree.ts](../../../../skills/agent-vm/scripts/sync_worktree.ts)；[guest-source.ts](../../../../skills/agent-vm/scripts/guest-source.ts)会以内存 bundle 形式作为 one-shot guest action 运行。
Helper 首先检查 Git index 中的 submodules，然后使用 NUL-delimited byte paths 选择 tracked/unignored files。
它跳过 tracked deletions 和 `.git`，拒绝 reserved owner filename 与 symlink parents，计算 regular file bytes 的 hash，并保留 exact POSIX modes 和 symlink target bytes。
JSON paths 对 invalid bytes 使用带 surrogateescape 的 UTF-8，与之前的 manifest representation 一致；
native byte-path syscalls 避免 runtime string substitution。
无法在 subprocess argv 中精确表示的 local worktree root，必须在 transport 前显式视为 unsupported。
macOS filesystems 可能拒绝 invalid UTF-8 names；
其 codec tests 不能证明 Linux filesystem behavior。

Helper 只拥有 `/workspace/worktrees/<safe-name>/src`、该 workspace 的 exact transaction stage/record 及其 lock。
Adjacent results 和 unowned sources 保持不变。
Names 只能是一个 safe segment，transaction IDs 是 32 位 lowercase hex digits。
Guest 检查 canonical real parent directories，并在不跟随 symlinks 的情况下打开 metadata。
Native `flock` 以最多 60 秒的 lock wait 串行化 begin/commit/cleanup。
Begin 独占创建 stage 和 prepared record，并记录 source device/inode/transaction baseline。
Commit 拒绝 changed baseline 或 manifest，fsync regular files、ownership metadata 和 directories，然后对 absent source 使用 Linux `renameat2(RENAME_NOREPLACE)`，对 replacement 使用 `RENAME_EXCHANGE`。
Atomic capability failure 保留 prior source 和 recovery material；
没有 copy/delete replacement fallback。
Committed marker 和 parent directory 必须在 committed record 被确认前持久化。

Linux managed guest 要求已安装的 Bun，且支持 `bun:ffi`、libc byte-path calls、`flock` 和 `renameat2`，以及 GNU `timeout` 和 rsync。
缺少 Bun/timeout 时必须在 staging 前失败；
native capability resolution 先于 parent creation。
SSH command 运行 `timeout --signal=KILL 180s bun -e <in-memory-bundle>`，client transport budget 为 240 秒，stdout/stderr 有界，guest request limit 为 32 MiB。
不存在 guest service 或 opportunistic runtime installation。
Local rsync 使用 private mode-0600 NUL listing 和 SSH configuration，在所有路径移除 temporary configuration，并保留 transfer exit codes。
Successful synchronization 不 acquire 或 renew lease。

Lost、malformed、truncated 或 timed-out commit acknowledgement 返回 exit 75，并保留 exact transaction recovery record；
helper 在 commit 开始后绝不自动 abort。
Source owner marker 用于发现 record update 前已完成的 exchange。
`abort` 报告 `unverified`，并保留 committed recovery material。
显式 `finish` 确认 source marker，只删除 exact owned stage（包括 exchange 后记录的 baseline identity）和 record。
Unconfirmed finish、foreign stage 或 superseded source 都保留 evidence。
Confirmed commit 后的 cleanup failure 报告 success，并附带 diagnostic 和 retained recovery material。

Compatible evolution 保留 protocol 4。
Breaking protocol 或 semantic change 可能要求 coordinated release 和 explicit client update。
Remote control release 必须先 published 并 health-checked，之后才能改变 local client release；
因此旧 client 可以继续使用 compatible actions，新 client 则不允许假定 remote 支持 unsupported actions。

Acquire、restart 和 restart-capable resize 使用更长的 bounded transport timeout。
Acquire 最多等待 30 秒，让无关的 native lease-expiry cleanup 离开 transitional state；
只有 message 精确为 `transitional native instance blocks admission` 的 `resource_unverified` 才 retry，其他 `resource_unverified` errors 立即 fail closed。
Native startup 前，当 projected CPU 超过 CT-visible capacity 且 verified CPU PSI `full.avg10` 达到或超过 configured startup threshold 时，control process 可以返回 `resource_pressure`；
批准的 overcommit deployment profile 使用 50 percent。
Response 包含 observed pressure、threshold、projected CPU 和 CT capacity，并且不修改 existing guest。
Definitive pre-native `resource_pressure` 在 status 确认没有 active native effect 后，可以使用新的 operation ID retry；
uncertain timeout 或 pending operation 必须保留原始 operation ID 和 generation。
该等待保留 protocol、generation 和 operation ID，不创建 second intent。
任何 command 都不会隐式 renew lease。

Container rootfs growth 报告 `guest_filesystem` evidence，包括兼容的 `status`（`grown`、`grown_after_retry` 或 `already_grown`）、configured rootfs capacity、observed filesystem capacity、verification threshold 以及 storage/topology evidence。
Rootfs growth failure 返回 `guest_resize_unverified`，并包含 bounded diagnostics、failing phase、command 或 Incus operation、return code、适用时的 truncation flags、command timeout 时的 process-group cleanup evidence、capacity evidence 以及 bounded attempt/retry chain；
acquire cleanup 必须留下 no active lease 或 exposed SSH mapping，而 durable partial state 保持可 inspect 和 retry。

因此，失败后的清理必须明确证明 `no active lease or exposed SSH mapping`，并让 durable partial state 保持 `inspectable and retryable`。

当 acquire 在 control side 创建 durable task 后返回 error，client 执行一次 status read；
只有 status 携带 failed acquire 的 operation ID（位于 partial-failure evidence 中），或携带 controller-verified 且 generation 匹配的 committed acquire operation 时，才持久化 task ID、generation 和 private-key path。
Controller 持久化记录 partial-failure operation IDs，并从 operation journal 证明 committed operations。
Status 仍报告 effective active lease 的 committed operation 是 adoptable lease，虽然 failed acquire invocation 不打印它；
partial proof、pending operation 或没有 effective lease 的 committed proof 只能作为 cleanup handle。
Public `release` 和 `destroy` 在 cleanup-only state 下仍可用，因此 cached、transitional 和 partially cleaned tasks 可以 retry，包括 retry durable cache deletion。
Absent、stale、concurrent 或 mismatched status 不会创建 guessed local state。
来自 partial failure、pending operation 或无 effective lease 的 committed operation 的 recovery state 都是 cleanup-only：在 successful acquire 替换它之前，client 拒绝 SSH 和 lease mutations；
exact pending acquire 未解决时，controller 拒绝 active mutations。
Acquire retry 保留 original operation ID 和 generation；
active lease 只可收敛那些 recorded resource evidence 与 durable allocation 匹配的 pending requests，缺失或冲突的 evidence 仍保持 pending，等待显式 release 或 destruction。
如果 background cleanup 在 status read 前将 failed acquire 推进到 cached task，controller 会保留 task、generation 和 operation 绑定的 partial-failure marker，直到后续 acquire 或显式 destruction supersede 它，使相同 cleanup handle 始终可验证。

## SSH 与端口

命令超时的固定错误字段名是 `process-group cleanup evidence on command timeout`，用于证明超时后的进程组清理结果。

Generated SSH configuration 使用 configured control address、leased port、explicit identity 和 known-host files、generation-bound host-key alias、`StrictHostKeyChecking yes` 以及 `IdentitiesOnly yes`。
它不得禁用 host checking 或引入 gateway。
Port allocation 计算 listeners、native mappings、forwards、reservations 和 configured ephemeral range。
Guest 可复用前，lease cleanup 移除 mappings 及其 exact conntrack flows。

## 容量与清理

Capacity responses 标识 local filesystem scope、observed sources、allocation budgets、allocated/reserved values、logical remaining values 和 pressure observations。
Missing 或 contradictory capacity measurements 会拒绝 new demand，但不会停止 effective lease。
Reservation-only memory admission 下，low CT `MemAvailable` 报告为 pressure，但不会拒绝 logical memory ceiling 内的 request。
CPU PSI 是独立的 startup-schedulability guard：不会减少 logical CPU remaining，也不会停止 running lease。
Manual cleanup 默认 dry-run，并在 deletion 前于 object locks 下重新检查 ownership、generation 和 eligibility。

Dashboard 和 metric authority rules 见[观测合同](observability.md)。
Guest rootfs allocation、physical Incus pool usage、CT root/image staging capacity 和 native admission 是独立 measurements。

## 拓扑与恢复边界

Configured SSH control address 是 one-shot control process 的 entrypoint，不是 underlying host、CT109 LXC、Incus host、storage system 或 rebuild authority 的 identity。
Public contract 不提供 host 或 CT rebuild command；
证据边界和停止条件见[拓扑与恢复运行手册](../OPERATIONS.md)。

Control environment 是已观测的 CT109 LXC，并拥有该 CT 内按 session 管理的 Incus system containers。
Concrete private address、CT identifier 和 Incus identity 是独立事实。
Missing provenance、missing durable state 或 unverified native inventory 会阻止 recovery，而不是授权 guessed host operation 或 broad cleanup。

Recovery 期间保留 configured state database 和 WAL/journals、local operation journals、operation/audit records、generations、canonical configuration、protected connection material、native timer assets 以及 Incus container data。
在 drain 下恢复匹配的 client/control/unit revisions，然后在任何 apply 前检查 `agent-vm doctor`、`agent-vm admin status`、`agent-vm admin session list` 和 dry-run `agent-vm admin reconcile`。
`admin reconcile --apply` 只允许 confirmed ownership/generation 与 cleanup/rearm actions；
在确认 native instance 和 port 不存在后，也可修复 `absent` task 的 malformed 或 inconsistent nonzero resource/reservation records；
`admin gc --apply` 和 destructive cleanup 不是 recovery shortcuts。

Remote control-script 或 unit publication 在 activation 前进入 drain。
Drain 阻止 new acquisition 和 restart admission，但允许 status、retrieval、release 以及 expiry/reconciliation/cache-cleanup timers；
不会 force-kill in-flight operations。
Publication 等待 pending operations 和 task transitions 收敛，原子激活 verified artifacts，并在 `admin drain exit` 前检查 health、reconciliation 和 timers。
Pre-activation failure 保持 current release 不变；
post-activation health failure 允许向上一个 verified release 执行一次 bounded rollback，rollback failure 则保持 admission drained。

Transport timeout 或 pending operation 都是不确定结果。
保留 original operation ID、request 和 generation，检查 status/reconciliation，绝不创建 new operation ID 或 reset generation 来适配 request。
如果 durable/native state 无法匹配，保持 admission drained 并停止。
只有 health、state、generation、timer 和 CT-local capacity checks 全部通过后，才能运行 `agent-vm admin drain exit`，然后重复执行 `doctor` 和 `admin status`。

## 回收与重试行为

Reconciliation 和 garbage collection 只选择 expired、released 或按 configured policy 具备其他 eligible 条件的 records。
它们在持有 object lock 时重新读取 record，验证 session 和 generation，并且只移除该 record 拥有的 resources。
Stop、detach、proxy cleanup 或 metadata update 失败时，保留 retryable record 并报告 failed phase；
不得静默将 resource 标记为 reusable。
Transport timeout 后可安全使用相同 operation ID retry；
new intent 必须使用 new operation ID。

Acquire 和 restart 是有界 operations，transport failure 后可能返回 uncertain outcome。
Caller 检查 `status` 或 reconciliation，并使用 original operation ID 和 generation retry。
Renewal 绝不会作为 `ssh`、`exec`、`sync`、upload、download 或 export 的隐式副作用发生。

## 资源与 live-memory 规则

CPU、memory 和 disk requests 在 admission 前解析为 canonical units。
Admission 计算 configured budgets、overcommit ratios 和所有 effective reservations。
Capacity measurements missing 或 contradictory、requested value 超过 configured logical policy、disk admission failure 或处于 draining 时，request 被拒绝；
memory pressure 单独报告，不改变 logical remaining memory。
Existing effective lease 仍可 retrieval 和 release。

`resize --live` 仅限于受支持的 memory-only change。
CPU、disk 和其他不支持 live 的 change 必须直接失败，不能 fallback 到 restart。
可由 restart 完成的 change 必须显式提供 `--allow-restart`，并使用更长的 bounded timeout。
Live resize 会在 response 中同时记录 requested value 和 observed value，让调用方区分 pending reconciliation 与已完成的 change。

## 输出与 admission

Human-readable status 面向 operators；
automation 消费 JSON envelope，并检查 `protocol`、`action`、`session_id`、`generation`、`operation_id`、`outcome` 和 `result`。
Diagnostics 写入 stderr，绝不混入 `export` 的 archive stream。
Admission drain 阻止 new acquisition 和 restart admission，同时允许 status、retrieval 和 release；
只有 control process 已 durable record 新 admission state 后才退出。
Drain 前已跨过 short admission fence 的 restart 可以完成；
native stop/start work 不持有 global admission lock。
