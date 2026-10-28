# Agent VM 项目工程治理

> 本文定义长期需求与验收合同；实现覆盖和启用前置条件见 [IMPLEMENTATION.md](IMPLEMENTATION.md)，主题边界与背景见 [HISTORY.md](HISTORY.md)。

## Context and Scope

本主题覆盖 Agent VM 从日常变更到 PR、main、产物发布和独立部署的工程治理。
发布是其中一个阶段；兼容性判断、质量门禁、Actions 调度与失败通知也约束不产生新版本的普通变更。
已有实现只用于识别迁移成本，不能自动成为目标需求。

| 必须应用的 Topic | 项目级职责 | 主要需求 |
| --- | --- | --- |
| `semver-change-governance` | 日常变更的公开接口与持久状态兼容性判断。 | REQ-ENG-002、REQ-ENG-003、REQ-ENG-004 |
| `quality-gates` | 本地、PR、main、发布与部署的门禁，以及默认分支平台保护。 | REQ-ENG-005 至 REQ-ENG-008 |
| `github-actions-scheduling` | 所有工作流的事件、并发、取消、运行身份与 required check 可见性。 | REQ-ENG-009 |
| `release-failure-notification` | 所有实际存在且预期成功的交付工作流失败通知。 | REQ-ENG-016 至 REQ-ENG-020 |
| `manual-version-release-delivery` | 维护者手动选择版本、固定来源、不可变产物、恢复与独立部署。 | REQ-ENG-010 至 REQ-ENG-015、REQ-ENG-021 |

范围包括客户端、兼容桥、Go 控制服务、协议与状态合同、安装资产、仓库工具、文档合同和工作流。
本主题不新增业务能力、托管服务或自动现网部署，不更换 Go/Bun/Incus 技术边界，不授权实现、平台写入、凭据配置或真实通知。
运行期租约与原生资源行为仍由 [Agent VM 能力合同](../agent-vm/SPEC.md) 负责；具体发布机制由[发布子设计](../../proposals/agent-vm-semver-release.md)细化。

## Terms and Interfaces

- 产品版本：整个 Agent VM 产品单元的 SemVer；协议号、schema 版本和 guest 镜像指纹各自独立。
- 产品发布：固定源码对应的全部版本化安装包和 GitHub Release 发布并校验成功。
- 部署：向明确目标安装、激活或升级已经完成发布的产物，需要独立触发与授权。
- 发布 identity：产品版本、源码提交、政策 revision、版本占用记录以及冻结后的各目标产物 digest 的绑定关系。
- 变更兼容性依据：对公开接口和持久状态分别说明影响、理由、支持范围与验证证据；它不是发版输入。
- `.github/quality-gates.json`：声明实际门禁、阶段、check names、签名和平台保护要求及例外。
- `.github/release-contract.json`：声明产品发布输入、来源、版本政策、产物集合和条件验收边界。
- `.github/release-failure-notification.json`：声明工作流分类、失败路由、通知配置合同与选型证据。

## Requirements

### REQ-ENG-001 — 五项 Topic 的项目级适用范围

五项 Topic 必须共同应用于本项目，不得以“没有发版”为由跳过日常变更治理、PR 门禁、调度或 CI 失败通知。
项目必须维护可审查的本地合同，并在相关合同或交付表面变动时验证其一致性。
其中 `semver-change-governance`、`quality-gates`、`release-failure-notification` 必须落成项目级 Policy 及其受管理的锁定记录；仅引用 Topic 文档不能算已应用。

### REQ-ENG-002 — 统一 SemVer 与公开接口承诺

客户端、Go 控制服务、兼容桥与 systemd 安装资产必须共用一个 SemVer 2.0.0 产品版本，首个公开版本为 `1.0.0`。
兼容性承诺必须覆盖文档化 CLI 命令与参数、JSON 输出、退出码、用户配置及客户端与控制服务之间的协议行为。
patch 必须保持公开合同兼容；向后兼容的功能增加和弃用声明使用 minor；破坏公开合同使用 major。
内部函数与未公开实现细节不构成公开接口，协议号、schema 号和镜像指纹不得替代产品版本。

### REQ-ENG-003 — 每项相关变更分别判断 API 与状态影响

影响公开接口或持久状态的变更必须分别给出 API 与状态影响分类、具体理由、受支持版本范围和验证依据，并区分计划支持与已验证支持。
声称仅影响内部实现时，必须说明公开合同与持久状态均未受影响的依据；CI 绿色不能代替语义判断。
这些依据可以保留在变更说明、设计或发布说明中，不要求每个 PR 生成持久化发布记录，不新增版本 label 或候选版本文件。
影响分类不得替代维护者的单一 `target`，不得自动汇总 PR 分类来决定发版版本，也不得把分类模板的存在设成手动发版资格。

### REQ-ENG-004 — 持久状态兼容与程序回滚

patch 必须保证同一 minor 内受支持版本之间新旧状态可读；minor 必须保证新版读取上一受支持 minor 的状态，但不承诺旧版读取新版状态；major 必须提供从上一 major 升级的验证路径。
跨多个 major 的升级必须明确中间版本，各发布的受支持范围和已验证证据必须可追溯。
程序回滚前必须证明目标程序能够读取当前状态；不得自动回退、清空或重建数据库、generation 或 operation journal 来制造兼容。

### REQ-ENG-005 — 项目全表面的质量合同

本地质量声明必须覆盖 Bun 客户端和桥、Go 服务、协议与持久状态、安装资产、文档边界以及工作流合同。
必须保留 Ubuntu/macOS、Bun 类型检查与两套测试、仓库检查、Go tests/vet、Linux 双架构构建及 race 验证所证明的现有合同，并补齐本主题需要的行为与合同验证。
门禁必须证明真实行为、来源或边界，不得以安装 Policy、存在配置文件或复制测试清单代替验证。

### REQ-ENG-006 — 分阶段、固定来源的门禁

本地、PR、main、Release 和 Deploy 必须分别定义职责；前一阶段成功不得冒充后一阶段验证。
`CI (PR)` 必须产生稳定的 `PR gate`；`CI (Main)` 必须在实际合并提交上产生 `Main gate`；结果必须绑定对应 SHA。
Release 必须验证选定 source 上有效的 main 结果及发布合同，Deploy 必须验证已完成发布及目标产物。
聚合门禁必须在必要子 job 失败、取消或意外跳过时失败，不得用 `continue-on-error` 或缺席的 check 伪造成功。

### REQ-ENG-007 — 默认分支平台保护与例外

`main` 必须 PR-only，要求签名提交、来自预期 GitHub Actions 来源的稳定 `PR gate`、合并前分支保持最新，并禁止 force push 与删除。
不强制独立人工批准人数，不引入 workflow-based review gate；管理员和发布身份不得获得日常直推通道。
本地质量声明与实际平台规则必须通过只读核验保持一致，无法核验时不得宣称已强制生效。
必要例外必须显式记录 owner、理由、范围及到期条件，不得通过移除检查或放宽保护掩盖失败。

### REQ-ENG-008 — 按真实边界触发原生验收

Incus、systemd 或部署边界发生变化时，必须在一次性 Linux/Incus 隔离环境中，用本次产物验证安装、启动、关键租约生命周期、进程回收与清理/卸载。
发布阶段必须比较上个已完成 final source 到本次 source 的完整范围；首版按首次安装边界执行，范围无法确定必须显式失败。
未触及这些边界的版本不要求重复整套原生验收，但必须记录判定理由并完成常规门禁与产物校验。
Go race、原生进程树等针对性验证仍按实际触及的合同执行；交叉编译成功不得声称已经证明原生安装与运行。

### REQ-ENG-009 — 全部 Actions 的调度合同

每个实际工作流必须声明事件、head SHA、并发 namespace、取消条件、稳定 check name 和运行身份。
required PR gate 只能监听 `opened`、`synchronize`、`reopened`、`ready_for_review`，不得监听 `pull_request.edited`。
required gate 必须使用 per-PR 非抢占队列，`queue: max` 与 `cancel-in-progress: false`；PR、main、release、deploy 和其他手工操作不得共享宽泛的可取消 group。
只有完全可被新运行替代的普通 candidate/preview 才可在独立 per-PR namespace 取消过期运行；不因存在此规则而新增这类工作流。
required check 不得因 workflow-level path/event filter 缺席；排队后的结果必须仍对应被评估的提交，重跑必须保留 run ID 与 attempt。
队列容量有界，按进入等待的时间处理，不得承诺无限容量或 dispatch 时刻的严格顺序；版本正确性不得依赖队列代替原子占用。

### REQ-ENG-010 — 单一手动版本输入

产品发版只能由 `main` 上的 GitHub Actions `workflow_dispatch` 显式触发，唯一版本输入为无默认值的 `target`。
必须接受 `patch`、`minor`、`major` 或完整合法 SemVer，包括预发布；合并 PR 不得自动发版。
执行端必须以最高已完成 final 版本作为可信基线计算 symbolic target；首次只能显式提供 `1.0.0`，不得使用现有 `0.4.0-native`、版本文件或 SHA 推测基线。
新分配的 explicit target 必须高于可信 final baseline，同一核心版本的预发布序列必须递增；预发布不推进 final baseline，build metadata 不增加优先级，也不得制造同一优先级的重复版本占用。
产品 tag 必须为 `v<version>` 且指向固定 source，版本本身不包含 `v`。
Agent 必须原样传递维护者输入，不预先计算替换 symbolic target。

### REQ-ENG-011 — 固定发布源码与政策

发版源码必须固定为 dispatch 事件绑定、已经通过适用门禁的 `main` 提交，排队或重试期间不得换成新的分支头。
必须核验正常 PR 来源、提交签名、适用检查、相对可信基线的源码祖先关系，以及源码中发布政策的内容 revision。
构建器必须验证实际 checkout 与该 SHA 一致，不得只信任调用参数中的来源字符串。
历史未经门禁的提交不能通过手动 dispatch 绕过来源合同。

### REQ-ENG-012 — 先占用版本，再产生发布副作用

版本必须在构建、上传或发布前被原子占用，并绑定 target、canonical version、source、policy、维护者与初始运行。
占用、冻结产物、完成或明确终止的记录必须持久、受保护且只追加；同名记录只能精确匹配后复用，冲突必须失败。
不得覆盖、删除或自动过期释放版本；未解决的占用必须先恢复或明确终止，不得自动跳到下一版本隐藏失败。
发布、恢复与终止必须共用同一非抢占状态写入边界，不能把多个 ref 的独立写入视为一个原子事务。

### REQ-ENG-013 — 一次构建、不可变产物与完整发布

产品资产必须包括 Bun 客户端包、Linux amd64/arm64 控制环境包、校验和及结构化来源记录；控制环境包包含对应 Go binary、桥与 systemd 资产。
产品版本、源码、固定工具链、目标架构、manifest、文件字节与 mode、协议及 per-action compatibility windows 必须一致且可验证。
各目标可有不同 digest，但 PR/main/Release/Deploy 不得各自重建不同字节冒充同一已冻结产物；源码归档不得替代安装包。
必须先冻结完整 digest，完成 draft 上传与下载复核，再发布 immutable GitHub Release，发布后复核全部映射并写入完成记录。
完成记录、产物和恢复依据不得依赖会过期的 Actions 临时 artifact，也不得构成循环哈希依赖。

### REQ-ENG-014 — 同一 identity 的恢复与冲突处理

占用后的重试必须读取原版本、source、policy 和已冻结 digest，只补齐缺失步骤；不得重新计算 symbolic target、重绑源码、覆盖不同内容或自动递增版本。
已发布但未写完成记录时，必须完整核验原 Release 后补写记录；已绑定产物不可取得时必须显式阻断，不得用不同字节重建替代。
必须修改源码时，应走正常 PR，并明确处理原占用后重新选择新版本；终止必须独立明确授权且不回收旧版本号。
首版 `1.0.0` 无法沿原 source 恢复时，必须先显式修订 bootstrap 决策，不能擅自重绑或改发 `1.0.1`。

### REQ-ENG-015 — 发布与部署分离

GitHub Release 及全部版本化安装包发布并校验成功，且完成记录有效，必须构成产品发布完成，不以现网升级成功作为完成条件。
部署必须独立显式触发，只消费已完成发布的原始产物及对应架构 digest，不重新构建。
目标预检、drain、激活、健康验证和本地客户端安装必须遵守既有部署权限与恢复边界；本主题不得引入自动现网部署。
部署失败不得篡改产品发布结论，程序回滚仍必须满足状态兼容条件。

### REQ-ENG-016 — 所有交付工作流的失败分类与路由

每个实际 top-level workflow name 必须在受版本管理的 allowlist 中精确且唯一地归类为 generic、Release-specific 或 excluded。
所有实际存在且预期成功的 CI、required gate、Release、部署或文档交付工作流必须进入相应失败路由；不得因为 Topic 名称包含 release 就只覆盖发布。
通知器和通知 smoke 自身必须排除，新增未分类工作流、重复路由或订阅漂移必须使合同检查失败。
不得为了填充分类而新增不需要的部署或文档工作流。

### REQ-ENG-017 — 失败事件与可核验上下文

通知必须通过独立 sidecar 精确订阅 allowlist 的 completed `workflow_run`，校验 repository、workflow、event、ref 和原运行身份，并显式只接受 `conclusion == failure`。
成功、取消或跳过不得触发自动失败通知。
generic payload 必须包含 repository、workflow、event、ref/branch、head SHA、PR（存在时）、run ID、attempt、actor、运行链接及排查后重跑指引，不得伪造产品版本。
Release-specific payload 只有在发布记录核验成功后才可补充版本、tag、source、reservation、已绑定产物及恢复入口；占用前失败必须如实说明尚未绑定 identity。

### REQ-ENG-018 — 通知的凭据与执行信任边界

带 secret 的通知路径必须只消费失败运行的 metadata，使用可信默认分支 helper 或已经选定且只处理 metadata 的 delivery。
不得 checkout 或执行失败运行的 PR head、不可信 caller code 或附带脚本；权限必须限于读取所需运行上下文及已选 transport 的发送需求。
共享 reusable workflow 不得被当成项目已选择 transport 的证据，也不得扩大通知凭据的可见范围。

### REQ-ENG-019 — transport selection gate

启用通知前必须以项目已检入合同或主人明确选择为证据，记录 transport、transport owner、configuration owner、required secrets 和所需权限。
Topic 不提供默认 transport、地址或 reusable notifier；reference、现有 release webhook、GitHub 默认邮件或人工查看均不得被推定为已选通知路径。
缺少选择证据必须阻断 notifier rollout，且本项目不得宣称五项 Topic 已全部应用或启用整套新发布系统。
不得无证据新增外部 notifier、secret 或渠道；选定后缺少配置必须显式失败，不得静默 skip 成绿色。

### REQ-ENG-020 — 通知独立恢复与发送验证

通知结果必须与原 CI、发布或部署结果分别记录；通知失败不得触发重新构建、重新发版或重新部署。
重试必须绑定原 run ID 与 attempt，并按该身份去重，保留可定位的发送结果与失败原因。
已选 transport 必须提供独立手动 smoke 入口，初次启用或 delivery 配置变更后必须验证真实发送；发送仍需相应授权，不得由设计确认代替。

### REQ-ENG-021 — 新 manifest 与旧格式读取边界

`1.0` 起的新生产路径必须只输出含独立 `version` 与 `source_revision` 的 SemVer manifest format 3，客户端 sidecar、Go metadata 和版本输出必须一致；开发源码不得冒充公开发布版本。
旧 SHA manifest 只能用于明确迁移和必要回退读取，不得长期双格式生产或把历史 SHA 记录伪装为正式 SemVer。
协议 4 需要的旧 `release_id` 必须按 `source_revision` 的原 SHA 语义写入 format-3 manifest 并由兼容适配边界复用，不得用它分配产品版本。
迁移必须保留数据库、generation、operation journal 以及必要恢复记录；新部署必须按版本和 digest 选择产物。

### REQ-ENG-022 — 应用完成的证据

五项 Topic 的应用完成必须同时具备项目 Policy 与锁定记录、本地合同检查、真实行为验证、实际平台规则核验及已授权的通知 smoke 证据。
必须按真实实现与验证结果运行联合 Policy 审计，并将采用证据关联到实际实施 commit；不得提前生成 adoption record 或把设计接受、配置存在、CI 绿色单独当成全部完成。
需求结论、实现覆盖和启用状态必须分别表达；任何未覆盖的合同必须如实保留为实现缺口。

### REQ-ENG-023 — 配置、数据与授权边界

发布资产与公共文档必须排除部署地址、凭据、inventory、数据库、WAL/journal、锁、Incus 容器数据、guest 镜像及消费者渲染的观测文件。
设计或需求确认不得扩大为代码实施、Policy 安装、GitHub 权限/规则变更、secret 创建、发送通知、发版或部署授权。
工程治理不得新增宿主或虚拟化管理权限，不得重建业务状态来绕过既有 Go/Bun、SQLite、协议 4 和 nested Incus 合同。

## Verification

### VER-ENG-001 — Topic 适用范围与采用审计

- covers: `REQ-ENG-001`, `REQ-ENG-022`
- 方法：逐项对照五个 Topic、三个项目 Policy 和锁定记录，检查联合审计及实际实施证据。
- 通过条件：日常变更、PR、main、发布、部署和通知均有对应合同，无仅以文档或文件存在冒充完成的条目。

### VER-ENG-002 — 公开接口与变更兼容性

- covers: `REQ-ENG-002`, `REQ-ENG-003`
- 方法：使用真实 CLI/JSON/退出码/配置/协议的兼容与破坏案例，审查 API 和状态各自的影响理由、支持范围及证据。
- 通过条件：共同产品版本一致，已验证与计划支持可区分；影响分类不会变成自动 target、label 或每 PR 发布记录。

### VER-ENG-003 — 状态升级与程序回滚

- covers: `REQ-ENG-004`, `REQ-ENG-021`
- 方法：验证受支持 reader/writer 组合、前一 minor/major 升级、旧 manifest 读取、新格式生产和协议 4 适配路径。
- 通过条件：各级状态承诺成立，不支持的组合在副作用前被拒绝，不能通过清空状态或改写历史版本获得成功。

### VER-ENG-004 — 分阶段门禁与平台保护

- covers: `REQ-ENG-005`, `REQ-ENG-006`, `REQ-ENG-007`
- 方法：核验真实检查覆盖、各阶段 SHA、聚合 gate 失败案例、本地质量声明与 GitHub 只读 API 返回。
- 通过条件：必需检查可见且失败不被吞掉，保护规则与声明一致；无强制批准人数或隐含直推旁路，例外具备完整边界。

### VER-ENG-005 — 原生验收范围

- covers: `REQ-ENG-008`
- 方法：验证未触边界、跨多个提交触边界、首次发布和范围不明的判定案例；适用时用对应 digest 的产物进行隔离验收。
- 通过条件：只有适用范围触发整套原生验收，首次安装被覆盖，未知范围不会被静默跳过，结果绑定实际产物。

### VER-ENG-006 — 调度与运行身份

- covers: `REQ-ENG-009`
- 方法：核验 workflow 事件与并发合同、实际 run history、required check 配置及同 PR/跨 PR/跨阶段的排队与重跑案例。
- 通过条件：required gate 非抢占且不缺席，无 metadata 事件误取消、跨阶段互相取消或错认 SHA/attempt，容量与顺序说明符合平台语义。

### VER-ENG-007 — 版本政策与固定来源

- covers: `REQ-ENG-010`, `REQ-ENG-011`
- 方法：覆盖 bootstrap、symbolic/explicit target、预发布、build metadata、冲突、未验证源码及排队后 main 前移。
- 通过条件：版本只按手动输入和可信 final baseline 分配，实际构建始终对应固定的已验证 source 与 policy。

### VER-ENG-008 — 原子占用、不可变产物与故障恢复

- covers: `REQ-ENG-012`, `REQ-ENG-013`, `REQ-ENG-014`
- 方法：验证并发 claim、记录保护、产物元数据/字节/mode、draft 下载复核，以及各状态间中断、重试、缺失产物与冲突案例。
- 通过条件：只有完整且一致的不可变发布才有完成记录，所有重试维持原 identity，旧占用不被回收或覆盖。

### VER-ENG-009 — 发布与部署边界

- covers: `REQ-ENG-015`
- 方法：验证产物发布完成但未部署、部署失败及程序回滚案例，核对所消费的版本与架构 digest。
- 通过条件：发版终点明确，部署独立触发且不重建产物，部署和回滚结果不伪改产品发布或数据库状态。

### VER-ENG-010 — 工作流分类与失败上下文

- covers: `REQ-ENG-016`, `REQ-ENG-017`
- 方法：枚举所有实际 top-level workflow names，验证漏分类、重复路由、成功/取消/跳过、generic 失败和占用前后 Release 失败案例。
- 通过条件：预期成功的交付工作流均有唯一正确路由，只有 failure 通知，上下文指向准确运行且不存在伪造发布 identity。

### VER-ENG-011 — 通知安全、选型与独立恢复

- covers: `REQ-ENG-018`, `REQ-ENG-019`, `REQ-ENG-020`
- 方法：审查凭据路径与可信代码来源，验证缺少选择证据、已选配置缺失、重复通知与发送失败案例；在明确授权后执行真实 transport smoke。
- 通过条件：无不可信代码进入 secret 路径，无未经选定的通知依赖，缺失配置明确失败，重试只处理原通知且真实发送可验证。

### VER-ENG-012 — 公共产物与权限边界

- covers: `REQ-ENG-023`
- 方法：检查资产清单、公共文档、变更范围和执行授权，验证安装与迁移不包含部署私有数据、不扩大管理权限。
- 通过条件：私有配置与业务状态不进入产品资产，确认设计不会被当作实施或外部写入授权。

## Related ADRs

- [控制面更新边界](../../adr/0030-agent-vm-control-plane-update-boundary.md)
- [独立仓库边界](../../adr/0039-agent-vm-standalone-repository.md)
- [统一产品版本与发布、部署边界](../../adr/0040-agent-vm-product-version-and-release-boundary.md)

## References

- [实现覆盖与启用前置条件](IMPLEMENTATION.md)
- [主题边界与背景](HISTORY.md)
- [发布子设计](../../proposals/agent-vm-semver-release.md)
- [现有运行期发布合同](../agent-vm/contracts/release.md)
- [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)
- [GitHub Actions concurrency](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency)
