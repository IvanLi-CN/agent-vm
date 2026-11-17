# Agent VM 客户端 CLI 接口

本文特指用户运行的 `agent-vm` 客户端命令行接口，不包含独立部署工具 `agent-vm-deploy`、控制服务 `agent-vm-server serve` 或浏览器管理接口。客户端职责、依赖及审查问题见[客户端设计与审查](../CLIENT.md)，双方共同的请求、响应和恢复边界见[客户端与控制端通信协议](control-protocol.md)。

规范入口是 [agent_vm.ts](../../../../skills/agent-vm/scripts/agent_vm.ts)，由已安装的 Bun 直接执行，支持保留相邻模块的源码链接方式。客户端不是常驻服务，不直接运行 Incus，也不自动下载安装运行时。

当前实现使用 Bun；SSH 控制传输与来宾连接使用 OpenSSH，源码同步额外使用 Git、rsync、`Bun.build` 和原生文件系统能力。本地状态锁使用 `bun:ffi`。这些是按功能区分的实现依赖，不表示客户端自身必须位于 Git 仓库中。核心命令隐式探测 Git、发布清单阻断运行等问题尚未修复，见[依赖审查](../CLIENT.md#依赖审查)。

## 文档范围与详细条款

| 内容 | 所属文档 |
| --- | --- |
| 命令、参数、配置、输出和退出行为 | 本文 |
| 请求身份、响应、幂等与兼容协商 | [通信协议](control-protocol.md)；原详细条款及锚点暂保留在本文 |
| 控制端生命周期、准入、租约与资源状态 | [Agent VM SPEC](../SPEC.md) |
| 部署事务与部署命令 | [部署控制器合同](deployment-controller.md) |
| 发布身份、sidecar、哈希和安装 | [产品发布与部署合同](release.md) |

下文保留的历史格式、控制端恢复和部署细节用于维持既有引用与行为约束，不把它们视为客户端应自行承担的职责。源码入口对发布清单的依赖是现有实现问题；文档整理未改变其行为，也未批准目标解耦方案的具体协议实现。

## 身份与配置

`--host <alias>` 选择控制主机。选择顺序为显式参数、`AGENT_VM_HOST`、项目绑定、`default_host`。`--session-id <id>` 选择会话目标；未提供时读取 `AGENT_VM_SESSION_ID`，显式参数优先。这两个全局参数可以放在命令前后，也适用于嵌套命令。

主机配置保存在 `~/.config/agent-vm/config.toml`，由以下本地命令管理；凭据只通过环境变量或外部文件引用，不直接写进配置。

| 命令 | 用途 |
| --- | --- |
| `config init` | 初始化本机配置 |
| `config list` | 列出主机和项目绑定，不输出凭据 |
| `config host add` | 添加控制主机及其连接配置 |
| `config host set-default` | 设置默认主机 |
| `config project bind` | 设置项目与主机的绑定 |

需要会话的命令在缺少会话 ID 时返回 `missing_session`，并且不会先访问本地密钥、创建操作日志或执行远端写入。`version` 和帮助命令不需要会话或控制主机；`doctor` 允许省略会话，但仍需要控制主机配置。`admin metrics` 不使用会话，忽略会话环境变量，并拒绝显式的 `--session-id`。`exec --` 和 `ssh --` 后面的参数直接交给目标命令，不再作为客户端参数解析。

主机配置选择 SSH 或 HTTPS 传输，认证材料随所选主机配置解析。当前实现仍强制读取发布清单；这是[客户端设计审查](../CLIENT.md#发布信息与运行时通信)指出的耦合问题，不应将清单误当作连接配置。

SSH 控制传输调用远端固定的 Go 请求适配器 `/opt/agent-vm-native/bin/agent-vm-server request`，再由适配器连接受保护的 Unix socket。默认 socket 为 `/run/agent-vm-native/control.sock`；实际路径可以由部署配置指定。旧版清单格式及旧控制入口仅用于历史兼容，其适用范围见[产品发布与部署合同](release.md)。控制服务自己的镜像、存储、网络和数据库配置不属于客户端配置。

`agent-vm` 没有 `update` 命令，不安装远端文件，不修改部署配置，也不获得 Incus 或 PVE 运维权限。独立程序 `agent-vm-deploy` 的升级、迁移、事务诊断及回滚规则见[部署控制器合同](deployment-controller.md)和[产品发布与部署合同](release.md)。

## 命令

| 命令 | 行为 |
| --- | --- |
| `version` | 输出本地协议与客户端版本，不连接控制主机 |
| `doctor` | 查询组件健康状态与资源准入诊断 |
| `status` | 查询所选会话的状态 |
| `acquire [--cpu N] [--memory SIZE] [--disk SIZE] [--explain]` | 创建或复用所选会话的受管容器；启动失败时返回有界的只读诊断 |
| `renew [--explain]` | 在配置上限内延长租约 |
| `release [--explain]` | 撤销访问、停止来宾，并保留符合回收条件的缓存 |
| `destroy [--explain]` / `restart [--explain]` | 执行明确请求的销毁或重启动作 |
| `resize [--cpu N] [--memory SIZE] [--disk SIZE] [--live] [--allow-restart] [--explain]` | 调整资源；在线调整和允许重启的调整使用不同选项 |
| `ssh-info [--format json\|ssh-config]` | 获取并验证当前任务的连接材料 |
| `ssh [-- SSH_ARGS]` | 使用生成的严格连接配置调用本机 SSH |
| `exec -- ARGV` | 执行指定程序及参数；需要 shell 语法时显式使用 `sh -lc` |
| `sync --name NAME` | 将当前工作区同步到工具拥有的来宾源码目录 |
| `upload SOURCE DESTINATION` / `download SOURCE DESTINATION` | 上传或下载文件，不延长租约 |
| `export PATH` | 向标准输出写入 tar 数据流，诊断写入标准错误 |
| `port add NAME --guest-port N [--protocol tcp\|udp] [--host-port N] [--explain]` | 请求创建指定的端口映射 |
| `port list` / `port remove NAME [--explain]` | 查询或移除指定映射 |
| `admin status` | 查询组件、维护状态和有界的任务清单 |
| `admin metrics` | 查询供部署控制器使用的指标快照；不创建会话或改变状态 |
| `admin drain enter [--explain]\|status\|exit [--explain]` | 暂停、查询或恢复新任务准入；安全取回和释放操作继续可用 |
| `admin session list` | 列出受管会话记录 |
| `admin reconcile [--apply] [--explain]` / `admin gc [--apply] [--explain]` | 默认只预览；只有获授权的维护任务可以执行 `--apply` |
| `admin reconcile-session --task-id ID --generation N --pending-operation-id ID [--apply] [--explain]` | 对指定任务、代次和单个未启动的 acquire 执行过期清理，保留缓存；默认只预览 |

会话变更命令失败时默认返回有界的状态诊断；`--explain` 才增加指标摘要和只读恢复预览。只有 `--apply` 允许恢复操作创建变更意图与本地操作日志。未知选项和不支持的命令格式必须拒绝，具体选项以当前客户端的 `--help` 为准。

暂停准入的持有会话、健康前提、受限接管及恢复准入检查见[维护手册](../../../../deploy/docs/maintenance.md)。退出要求带 `release-` 前缀的会话匹配已保存的暂停持有者。普通会话成功暂停准入不代表它有权恢复准入；不得覆盖其他发布事务的持有者或直接改写状态清除暂停。

### 精确会话恢复

<a id="精确-session-reconciliation"></a>

`admin-reconcile-session` 是独立控制动作，不能降级成全局 `admin-reconcile`。不支持该动作的控制端必须在产生影响前拒绝。请求需要 `session_id`、`task_id`、正整数 `generation`、`pending_operation_id` 和布尔值 `apply`。预览不携带 `operation_id` 或创建变更意图；实际执行使用自己的幂等操作 ID，不能复用目标待处理操作的 ID。

目标必须是指定代次中已不活动、仍待确认的 acquire。保存的请求身份和摘要必须一致；执行器只能处于 `starting` 或 `exited`，且 `start_attempted=false`，没有 PID、进程组或启动时间身份。有效或尚未到期的租约、外部操作、同会话的其他待处理意图、未知原生资源归属或矛盾执行证据都会阻止恢复。该动作不重放 acquire，不启动、续租、调整或删除容器。

执行时与全局维护共享跨进程互斥保护，并在指定会话锁内复查代次、截止时间和归属。先保存不可变的原始请求与执行器证据，再通过既有恢复和过期释放路径移除映射、停止过期容器，保留同代缓存，最后将指定 acquire 标为 `lease_released`。清理结果不确定时保留待处理日志及证据，不宣告成功。同 ID 的重复请求使用日志重放结果；不同目标或代次不能复用该意图。

归属预检只发送固定的 Incus GET 查询，保留取消和进程树跟踪，但不能占用或替换变更执行器的身份。移除映射和停止容器仍经过原有执行日志及启动保护。新意图只接受 `active`、`preparing` 或 `closing` 目标；已缓存目标或未确认的清理结果不能充当本次成功证据。

该动作不读取全局待恢复列表，不推进维护游标，不唤醒全局调度器，也不取消其他 acquire。它不会自动部署自身；控制器升级时的启动恢复属于独立的全局操作，必须单独核对授权。

`admin-reconcile-history` 是受保护的运维动作，用于封存明确列出的历史 `operation_outcome_unknown` 记录。它要求 `operation_ids`、`reason` 和布尔值 `apply`。预览不写日志；执行前必须确认没有待处理或在途操作，且原生执行已静止。它只添加 `_historical_reconciled` 审计标记，保留原请求、错误和结果证据；不重放或删除操作，也不修改任务、租约、代次及资源。证据不足时拒绝执行。

### 镜像绑定

缓存恢复使用任务记录的 `image_fingerprint`。默认镜像变化不会改写已有任务或缓存的绑定。只有指定镜像缺失、已退役或无法验证、恢复时，才在启动、调整资源或暴露来宾前返回明确错误；仅在绑定镜像不可用时，才要求运维者显式销毁指定会话的缓存。

### 指标快照与任务清单

`admin metrics` 返回聚合指标，`admin status` 返回任务清单。前者回答“容量和健康状态如何”，后者回答“有哪些任务、各自是否可用”。字段名保留协议原文，含义如下。

| 指标字段 | 含义 |
| --- | --- |
| `observation_epoch` | 本次采样对应的持久状态纪元，用于判断采样期间状态是否变化 |
| `observation_consistent` | 原生资源探测前后的状态纪元是否一致 |
| `observation_attempts` | 本次采样尝试次数，重试次数有上限 |
| `observed_at` | 最后一次采样窗口的开始时间，采用 ISO-8601 格式；不是响应序列化时间 |
| `up` | 本次指标查询及初始 Incus 版本、实例探测是否成功；不等同于可以创建新任务 |
| `draining` | 是否正在暂停新任务准入 |
| `admission_ready` | 是否满足新任务的准入条件 |
| `reclaim_required` | 是否需要回收磁盘；磁盘容量未验证时为 `null` |
| `resources` | 已分配、已预留、剩余资源、限制、内存压力及 CPU 压力 |
| `capacity`、`disk_admission` | 容量测量结果与磁盘准入判断 |
| `default_admission` | 默认资源请求、实际资源成本、准入判断、原因与观测，供部署控制器估计可启动任务数 |
| `ct_cpu` | 控制容器可见 CPU 容量、PSI 压力及默认请求的启动压力判断 |
| `ct_memory` | 控制容器可见内存容量、用量、可用量、压力阈值及 OOM 事件 |
| `instances`、`task_states`、`operations` | 实例、任务状态及操作的汇总 |
| `pending_operations` | 待确认操作，含 `operation_id`、`action`、`session_id`、`generation`、`created_at` |
| `historical_unknown_operations` | 已封存但原始结果仍未知的历史操作，使用相同身份字段 |
| `image_unpack_failures` | 镜像解包失败的汇总 |
| `warnings`、`warning_counts`、`doctor` | 警告明细、计数及健康诊断 |

`resources` 包含 `allocated`、`reserved`、`allocated_and_reserved`、`remaining`、`limits`、`ct_capacity`、`memory`、`memory_pressure` 和 `cpu_pressure`。`ct_cpu` 包含 `capacity_vcpu`、CPU PSI 和 `startup_pressure`；`ct_memory` 包含 `capacity_bytes`、`current_bytes`、`available_bytes`、`pressure_threshold_bytes`、`pressure` 和 `oom_events`。

控制器在状态写入锁之外探测原生资源，再比较探测前后的持久状态纪元。只有提交了持久状态变更才增加纪元；成功读取或无实际变更的事务不会增加。状态持续变化时，在有限重试后返回 `observation_consistent=false` 并附带警告。没有验证的数值或布尔值必须按字段规则设为 `null` 或省略，不能伪装成健康的零值或 `false`。

对原始结果未知的历史操作，即使已验证当前安全状态，重试仍返回 `operation_outcome_unknown` 和 `outcome_verified: false`，不能重放原始动作。

`admin status` 的任务摘要包括任务、实例与会话标识，生命周期及原生状态，来宾可用性，请求资源，有效租约、截止时间、SSH 映射和最后错误的代码及阶段。协议动作 `admin-status` 接受可选的数值 `limit` 和任务 ID `cursor`；默认每页 25 条，上限 100，超过上限必须拒绝。这是协议能力，不表示 CLI 已提供对应选项。

每页最多并发 8 次原生只读探测，单次截止时间为 1 秒，整页为 5 秒。响应包含 `limit`、`next_cursor`、`partial`、`unknown`、`observation_consistent`、`probed`、`verified` 和 `timed_out`。无法验证时，任务的 `native_status` 为 `unknown`、`verified=false`，并计入 `unknown`；`native_missing` 只是观测细节，不是新的生命周期状态。

部署控制器可以按任务 ID 关联指标与清单，但不能暴露原始错误文本、凭据或部署文件路径。成功抓取后移除过期任务行；读取或解析任一来源失败时报告 `up=0` 和数据新鲜度故障。容量指标和清单的独立采样规则见[观测合同](observability.md)。

## 请求与响应

<a id="request-与-response"></a>

会话请求包含 `protocol`、`action` 和 `session_id`；变更请求还包含 `generation` 和 `operation_id`。无会话的只读动作 `admin-metrics` 省略 `session_id` 和 `operation_id`。待处理操作可以包含 acquire 绑定的任务 ID；恢复证据必须同时匹配任务、会话和代次。

成功响应包含 `ok`、`protocol`、`server_time`、`action`、`session_id`、`generation`、`operation_id`、`outcome`、`result` 和响应 schema、语义版本，可包含结构化 `warnings`。客户端终结本地操作日志前，必须匹配请求提供的所有身份字段，包括 `generation`。旧控制端省略元数据的兼容规则以当前实现为准，并且必须显式报告警告；不能据此放宽新协议的校验。

错误诊断以 JSON 写入标准错误，并使用非零退出码。控制端错误响应必须含 `ok: false`、`protocol`、已提供的请求身份字段及结构化 `error`。匹配的 JSON 错误不能被 SSH 标准错误重复打印；其他诊断仍受长度限制。

会话变更失败时，默认诊断包含状态、任务、代次、租约、原生执行验证和恢复阻断原因。`--explain` 才增加有界的指标摘要与指定会话恢复预览。字段缺失或旧控制端不支持时，标记 `diagnosis_completeness: partial`，不能宣告恢复成功。`admin reconcile` 和 `admin gc` 默认预览不携带变更意图或创建本地日志，只有 `--apply` 才创建。

租约未到期而返回 `stale_target` 时，以控制端 `error.detail.diagnosis` 为准，包含 `recovery_allowed=false`、`recovery_blocker=lease_unexpired`、`lease_expires_at`、目标任务、会话、代次及 `retry_condition`。只有控制端确认不可能发生原生变更时，才标记 `error.detail.outcome_verified`；原生执行或持久状态仍不确定时，操作继续待确认。

客户端只接受一个身份匹配的错误响应。无关、重复或字段类型不符的响应不能终结日志。SSH 标准错误最多保留 64 KiB；截断时返回 `remote_failed`，变更结果仍未验证。传输超时返回操作 ID，但不判断变更是否完成。

状态数据库故障的历史协议代码为 `sqlite_unavailable`，诊断包括 `phase`、SQLite 错误代码和名称、SQL 操作类别及 `state_committed`。后者为 `null` 时表示提交结果未知，需要用原操作身份查询状态或执行授权恢复。当前后端与新接口命名的区别见[Agent API](agent-api.md)。

部署控制器评估数据新鲜度时必须保留指标的 `observed_at`，不能使用响应生成时间替代。`up`、准入就绪和运行压力是三个不同判断。

## 控制面兼容性

发布清单记录产品 SemVer、源码来源、产物摘要、协议及各动作双向的 schema 和语义兼容范围。不同源码修订可以执行明确兼容的动作，但必须返回结构化警告。协议、schema、语义、动作、传输或产物验证不符合要求时，在持久写入或原生动作前拒绝；不能仅因 JSON 可解析而推断兼容。

清单格式、文件依赖、校验、发布顺序、安装和回滚见[产品发布与部署合同](release.md)。当前客户端只读本地 `client-release.json`，不提供更新、发布、安装或回滚命令；`agent-vm-deploy` 是独立运维入口。

原文中“缺清单时警告并使用旧协议”的行为是历史兼容行为，不能代表当前实现：当前 `loadRelease()` 在文件缺失时返回 `manifest_missing`。清单的依赖列表及传输枚举也应以所用产品格式和 reader 为准，不能沿用旧版固定文件数推断新版能力。当前运行时耦合问题及解耦方向见[客户端设计与审查](../CLIENT.md#发布信息与运行时通信)。

请求重试继续使用日志中保存的能力声明、operation ID 和 generation，不能因本地发布信息变化而改写原意图。超过 JavaScript 安全整数范围的整数必须精确保留，带小数点的数字不能当作整数身份字段。

## 管理接口兼容性

<a id="management-api-兼容性"></a>

管理功能使用产品清单格式 4 声明持久状态能力，不改变 protocol 4，也不新增公共客户端动作。格式 1 至 3 是管理功能引入前的回滚目标；已经写入管理状态标记后，必须按兼容规则拒绝不支持它们的回滚版本。

内置管理台使用同源 `/api/v1` 接口，定义见[管理 API](management-api.yaml)。它不包装 CLI 输出、不转发 protocol 4 请求，也不新增 `agent-vm` 命令。管理员由精确的 OIDC `(issuer, subject)` 标识；动作需要明确能力，并按合同执行预览、确认和审计，提供的原因也进入记录。已有状态命令、响应结构及动作兼容规则保持不变。

## Agent API 传输兼容性

<a id="agent-api-transport-兼容性"></a>

HTTPS 入口为经 Caddy 转发的 `POST /agent/v1/control`，使用[Token API](token-api.md)签发的不透明 Bearer Token 和相同的 protocol 4 请求体。不接受浏览器 cookie、转发身份头或任意控制路由。执行前检查 Token 到期与撤销、[Agent API](agent-api.md)规定的权限范围，以及目标原生身份、代次、操作 ID 和请求摘要。

机器 Token 不预先绑定会话；权限范围允许且当前状态通过检查时，可以访问请求指定的会话。无会话的管理观测要求显式的 `agent:observe_admin`。未知动作或权限组合在产生影响前拒绝。

传输兼容范围由对应发布合同声明；历史路径包括 `legacy-bun`，当前路径包括 `go-request` 和直接 HTTPS。蓝绿兼容窗口内，两槽位接受共同的 protocol 4 动作集，并共享 Token、会话和操作日志的状态能力标记。SSH 在声明窗口关闭前是兼容传输，不是第二套生命周期权威。不受支持的动作或传输必须在持久写入和原生变更前失败。

任务的 `image_fingerprint` 是固定的镜像身份。默认镜像变化只影响后续新任务，不改写活动任务或缓存。恢复继续使用记录的镜像，只有指定镜像无法验证或恢复时才拒绝。浏览器的 `resource_version` 用于比较后执行，并携带当前 VM 的 `generation`；两者不能代替会话和既有 CLI 请求身份。

状态迁移不改变 protocol 4。切换前，SQLite 仍是唯一写入者；状态写入保护阻止新的持久变更，并等待在途事务和原生执行结束。每个执行器必须在原操作 ID 下记录持久结果，并验证进程组、PID 身份和 Incus 操作均已静止。待确认结果、活动执行器或无法验证的静止状态都阻止导出导入。

切换后，rqlite 是共享状态权威；管理员授权、租约、代次、日志和纪元等关键读取使用线性一致性。恢复备份必须覆盖最后可信提交，其后的记录需与追加日志和原生执行证据核对。仅验证 schema、摘要、纪元或日志内部一致性不能证明覆盖完整；无法证明时保持写入保护，等待管理员授权的恢复。引入新状态能力的发布在激活或回滚前必须满足[管理合同](management-api.md)和[发布合同](release.md)的检查要求。

即使后端已切换到 rqlite，protocol 4 仍兼容历史错误名 `sqlite_unavailable`；新浏览器和 API 文档使用后端中性的 `state_unavailable`。

## 源码同步事务

<a id="source-synchronization-transaction"></a>

本机辅助工具为 [sync_worktree.ts](../../../../skills/agent-vm/scripts/sync_worktree.ts)，[guest-source.ts](../../../../skills/agent-vm/scripts/guest-source.ts) 打包到内存后在来宾执行一次。它先拒绝 Git 索引中的子模块，再用 NUL 分隔的字节路径选择已跟踪及未忽略文件，跳过已删除文件和 `.git`，拒绝保留的归属文件名与符号链接父目录，并保留文件哈希、POSIX 权限和链接目标字节。

无法解码的路径字节使用 UTF-8 surrogateescape 表示；原生字节路径调用避免运行时替换。不能精确放入子进程参数的本机工作区根路径必须在传输前拒绝。macOS 可能拒绝无效 UTF-8 文件名，因此编码测试不能代替 Linux 文件系统验证。

工具只拥有 `/workspace/worktrees/<safe-name>/src` 和对应事务的暂存目录、记录及锁，不修改相邻结果或其他来源的源码。名称只能是单个安全路径段，事务 ID 为 32 位小写十六进制。来宾验证规范化的真实父目录，不跟随元数据符号链接。

`flock` 串行化准备、提交和清理，最多等待锁 60 秒。准备阶段独占创建暂存目录和记录，保存源码的设备号、inode 与事务基线。提交时拒绝基线或文件清单变化，同步文件、归属元数据和目录到磁盘；源码不存在时用 Linux `renameat2(RENAME_NOREPLACE)`，替换时用 `RENAME_EXCHANGE`。原子操作不可用时保留旧源码和恢复材料，不降级为复制后删除。确认提交记录前，提交标记和父目录必须持久化。

来宾需要支持 `bun:ffi` 的 Bun、libc 字节路径调用、`flock`、`renameat2`、GNU `timeout` 和 rsync。缺少 Bun 或 timeout 时在暂存前失败，原生能力解析先于父目录创建。SSH 执行 `timeout --signal=KILL 180s bun -e <in-memory-bundle>`，客户端传输时限 240 秒，输出长度受限，来宾请求上限 32 MiB。不启动来宾服务，也不临时安装运行时。

本机 rsync 使用权限为 0600 的 NUL 文件列表和 SSH 配置；所有退出路径删除临时配置，保留传输退出码。同步成功不获取或延长租约。

提交确认丢失、格式错误、截断或超时时返回退出码 75，保留对应恢复记录；提交开始后不自动中止。源码归属标记用于识别“目录交换已完成但记录尚未更新”。`abort` 返回 `unverified`，保留已提交事务的恢复材料。显式 `finish` 验证源码标记后，只删除确属该事务的暂存目录及记录，包括交换后记录的原基线身份。无法确认、目录归属不符或源码已被后续事务替换时保留证据。已确认提交后的清理失败仍报告同步成功，同时附带诊断和保留材料的位置。

### 生命周期等待与失败恢复

兼容演进保留 protocol 4；破坏性协议或语义变更可能需要协调发布和明确更新客户端。控制端先发布并通过健康检查，再更新客户端；旧客户端可继续使用兼容动作，新客户端不能假定控制端支持新动作。

acquire、restart 和允许重启的 resize 使用较长但有上限的传输时限。acquire 最多等待 30 秒，让无关租约的原生过期清理离开过渡状态；只对消息精确为 `transitional native instance blocks admission` 的 `resource_unverified` 重试，其他同码错误立即拒绝。等待保留原协议、代次和操作 ID，不创建第二个意图。

启动前，如果预计 CPU 超过控制容器可见容量，且已验证的 PSI `full.avg10` 达到配置阈值，控制端可以返回 `resource_pressure`。已批准的超配策略使用 50% 阈值。响应包含实测压力、阈值、预计 CPU 和容量，不修改已有来宾。明确发生在原生启动前的拒绝，在状态确认没有活动原生影响后可以用新操作 ID 重试；超时或待确认操作仍必须保留原 ID 和代次。任何命令都不隐式续租。

根文件系统扩容返回 `guest_filesystem` 证据：状态为 `grown`、`grown_after_retry` 或 `already_grown`，并给出配置的根文件系统容量、实测容量、验证阈值、存储与拓扑证据。失败使用 `guest_resize_unverified`，包含有界诊断、失败阶段、命令或 Incus 操作、返回码、适用的截断标记、超时后的进程组清理证据、容量证据及有上限的尝试链。

acquire 失败后的清理必须证明没有有效租约或暴露的 SSH 映射，保留的持久部分状态仍能检查和重试。不能把清理请求已发送当作清理完成。

控制端已创建持久任务但 acquire 返回错误时，客户端读取一次状态。只有证据包含失败 acquire 的操作 ID，或包含控制端验证且代次匹配的已提交 acquire，才保存任务 ID、代次和私钥路径。控制端保存部分失败的操作身份，并通过操作日志证明提交。

若已提交操作仍有有效活动租约，客户端可以接纳该租约，即使失败调用没有输出成功结果。部分失败、待确认操作或没有有效租约的提交证据只能生成清理句柄。清理专用状态允许 `release` 和 `destroy`，支持缓存、过渡任务及部分清理结果的重试；成功 acquire 替换该状态前，拒绝 SSH 和租约变更。目标不存在、证据过期、并发冲突或身份不符时不猜测本地状态。

目标 acquire 尚未解决时，控制端拒绝活动变更。重试保留原操作 ID 和代次；活动租约只能收敛资源证据与持久分配一致的待处理请求。缺少或矛盾的证据继续待确认，等待显式释放或销毁。若后台清理先将任务变成缓存，控制端仍保留绑定任务、代次和操作的部分失败标记，直到后续 acquire 或显式销毁取代它，保证清理句柄仍可验证。

## SSH 与端口

生成的来宾 SSH 配置包含配置的控制地址、租约端口、明确的私钥和已知主机文件、绑定代次的 host key 别名，以及 `StrictHostKeyChecking yes`、`IdentitiesOnly yes`。不得禁用主机密钥校验或引入网关。

端口分配需考虑监听端口、原生映射、转发、预留及配置的临时端口范围。来宾可复用前，租约清理必须移除映射及其对应的 conntrack 流。

## 容量与清理

容量响应说明本地文件系统范围、观测来源、分配预算、已分配和预留值、逻辑剩余量及压力。测量缺失或矛盾会拒绝新需求，但不能停止有效租约。只按预留量进行内存准入时，控制容器 `MemAvailable` 偏低作为压力报告，不拒绝逻辑上限内的请求。CPU PSI 是独立的启动保护，不减少逻辑剩余 CPU，也不停止运行中的租约。

手工清理默认只预览，删除前持有目标锁复查归属、代次与资格。来宾根文件系统分配、Incus 存储池物理用量、控制容器根目录与镜像暂存容量、原生准入是不同测量，不能混为一谈，详见[观测合同](observability.md)。

## 拓扑与恢复边界

配置的 SSH 控制地址是请求入口，不能证明底层宿主、CT109 LXC、Incus 宿主、存储系统或重建权限。公共客户端没有宿主或控制容器重建命令；证据边界及停止条件见[拓扑与恢复运行手册](../OPERATIONS.md)。

控制环境位于已观测的 CT109 LXC，管理其中按会话隔离的 Incus 系统容器。私有地址、CT 标识和 Incus 身份是独立事实。缺少来源、持久状态或经过验证的原生清单时必须停止恢复，不能猜测归属或扩大清理。

恢复保留配置的数据库与 WAL、日志、本地操作日志、审计记录、代次、规范配置、受保护连接材料、定时器文件和容器数据。在暂停准入期间恢复匹配的客户端、控制端与服务单元，再检查 `doctor`、`admin status`、`admin session list` 和 `admin reconcile` 预览。`admin reconcile --apply` 只执行已验证归属和代次的清理、重新准备；确认原生实例和端口不存在后，可以修复 `absent` 任务中不一致的非零资源预留记录。垃圾回收和破坏性清理不能当作恢复捷径。

部署激活前暂停准入，阻止新建和重启，但允许查询、取回、释放及过期、恢复、缓存清理调度；不强杀在途操作。待处理操作和任务过渡收敛后，原子激活已验证产物，检查健康、恢复和调度状态后再恢复准入。激活前失败保持当前发布；激活后健康失败允许一次回滚到上个已验证发布，回滚失败继续暂停准入。

超时或待处理操作意味着结果不确定。保留原请求、ID 和代次，查询状态与恢复证据，不能改 ID 或重置代次以适配请求。持久状态和原生状态无法核对时停止，保持准入暂停。健康、状态、代次、定时器及控制容器本地容量都验证后，才执行 `admin drain exit`，再复查 `doctor` 和 `admin status`。

## 回收与重试行为

恢复与垃圾回收只选择已到期、已释放或按策略符合条件的记录。持有目标锁后重新读取，验证会话和代次，只移除该记录拥有的资源。停止、分离、代理清理或元数据更新失败时，保留可重试记录并报告阶段，不能静默标为可复用。

传输超时后的重试使用相同操作 ID；新意图才用新 ID。acquire 和 restart 有执行时限，传输失败不等于远端失败；调用方先查询状态或恢复证据，再用原 ID 和代次重试。SSH、执行、同步及文件传输都不会自动续租。

## 资源与在线内存调整

<a id="资源与-live-memory-规则"></a>

CPU、内存和磁盘请求在准入前转换为统一单位。准入计算配置预算、超配比例及全部有效预留。测量缺失或矛盾、请求超出逻辑上限、磁盘准入失败或暂停准入时，拒绝相应新请求；内存压力单独报告，不改写逻辑剩余量。已有有效租约仍可取回和释放。

`resize --live` 只支持在线内存调整，CPU、磁盘或其他不支持的调整直接失败，不降级成重启。允许重启的调整必须显式提供 `--allow-restart`，并使用更长的有界时限。在线调整响应同时记录请求值和观测值，以区分待确认与已完成。

## 输出与准入

<a id="输出与-admission"></a>

人工阅读状态用于操作判断；自动化应读取 JSON，验证 `protocol`、`action`、`session_id`、`generation`、`operation_id`、`outcome` 和 `result`。诊断写入标准错误，不混入 `export` 数据流。

暂停准入阻止新建和重启，但允许查询、取回和释放。恢复准入必须先由控制端持久记录新状态。暂停前已越过短暂准入检查的重启可以完成，原生停止和启动期间不持有全局准入锁。

## 镜像选择

`agent-vm image list` 是无会话只读查询，忽略 session 环境变量并拒绝显式 `--session-id`。返回按完整指纹排序的 `name`（可空）、`fingerprint`、`architecture`、`is_default`，不返回部署路径或其他会话。HTTPS 需要 `agent:read`。

`agent-vm acquire --image <名称或完整指纹>` 以精确、区分大小写的 NFC 名称或 64 位小写完整指纹选择镜像；不支持指纹前缀。显式选择发送 acquire 请求语义修订 2。旧端必须在副作用前拒绝，客户端绝不去掉选择器重试。未选择镜像时保持语义修订 1。

首次接收操作时，控制端在认领日志的同一事务内保存实际指纹，并以待创建引用保护镜像。名称改绑和默认切换不能改变已接收操作的指纹。已有会话省略镜像沿用原绑定；显式不同返回 `image_conflict`，保留原实例、数据、租约和缓存。成功结果包含 `image_fingerprint`，显式指纹请求还要核对结果完全一致。

本地帮助、版本和配置明确的控制请求不依赖 Git、仓库、发布清单或构建。同步保留 Git 文件选择；安装校验与运行兼容检查属于不同边界，见[客户端职责](../CLIENT.md)。
