# 一次性控制运行时

常驻业务控制实现是 `control/cmd/agent-vm-control/main.go`，使用 Go 1.27.3 构建，并以一个二进制文件提供 `serve`、`request` 和 `native-worker`。
它的生命周期、状态、scheduler 和 native worker 包在每个槽位内构成一个业务控制权威；槽位外的 Deployd（`agent-vm-deployd`）是独立的目标服务，负责部署事务和槽位角色协调，不是第二个生命周期权威。
TypeScript bridge 在兼容窗口内仍是固定适配器。
Guest disk action 是 `scripts/guest-root-growth.ts`，从已验证的 release 在内存中打包，并通过 stdin 提供给 `incus exec <instance> -- bun run - <disk-gib>`；它报告测得容量后退出。
没有 Python control 或 disk-growth guest fallback。

## 前置条件

native 控制宿主和 disk-growth guest 要求 Linux、procfs、观测到的 cgroup v2、`flock`、child-subreaper 支持以及完成 identity check 的 pidfd。
Go worker 通过 `x/sys/unix` 使用 `prctl`、`pidfd_open`、`pidfd_send_signal` 和 `waitpid`；glibc 2.36 或更新版本以及 Linux 5.3 或更新版本提供所需接口。
容量观测在使用数据前会验证可见 cgroup 和 LXCfs 边界。
缺少 native ownership 或容量证据时必须 fail closed。

构建常驻二进制文件时安装固定版本的 Go 1.27.3 toolchain，并在激活前验证 `CGO_ENABLED=0` 的 Linux 产物。
控制宿主不会在请求期间安装 runtime。
Systemd 调用 `/opt/agent-vm-native/bin/agent-vm-control`；兼容 bridge 仅在转发到 UDS 时使用受保护的 Bun runtime。
Guest 必须在任何 root-device 变更前解析出 `bun`。
公共仓库不会在运行时下载 toolchain。

控制宿主必须提供 Incus、systemd、OpenSSH、conntrack-tools、`ss` 和 filesystem-capacity 工具。
Guest growth 要求直接由分区承载的 ext4 或 xfs root，以及 `findmnt`、`lsblk`、`blockdev`、`growpart` 和 `resize2fs` 或 `xfs_growfs`。
拓扑未知、工具缺失、virtual-disk size 过期或增长后的容量无法验证时，不能报告成功。
工具执行成功本身不足够：disk、partition 和 filesystem 测量必须满足请求大小及现有的 95% filesystem threshold。

客户端和 guest source transaction 直接在 Bun 中运行。
按照[发布合同](../../../docs/specs/agent-vm/contracts/release.md)部署完整客户端包：`agent_vm.ts` 和 `sync_worktree.ts` 使用 `0755`，`byte-fs.ts`、`client-io.ts`、`client-release.ts`、`client.ts`、`guest-source.ts` 和 `sync-native.ts` 使用 `0644`。
本机 sidecar 对全部八个文件计算 hash。
Python 仍是人工 publisher/wrapper 和 release-policy tooling 边界；它不运行 client、controller 或 guest source fallback。
Source sync 依赖 client/source 合同规定的 Bun FFI、native `flock`、Linux `renameat2`、GNU `timeout`、Git 和 rsync。
源文件暂存前必须在 managed image 中验证这些依赖；不得在请求期间安装 runtime。
Controller 的通用 image 前置条件可以独立于 Bun source action 额外包含 Python 等工具。

## 持久化与传输兼容性

Protocol 4、request identity、per-action compatibility window、generation check、operation ID 和 2-MiB management-request limit 保持不变。
主要请求路径是受保护 UDS 上的 HTTP；现有 SSH 入口和 TypeScript bridge 可以转发同一 JSON 合同以保持兼容。
成功结果是 stdout 或 UDS response body 中的 JSON；结构化失败对 request adapter 保留 exit status 2，意外失败使用 status 3。
客户端继续使用普通操作 240 秒、startup/restart-capable 操作 600 秒的 timeout budget，以及有界 output reader。

现有业务 SQLite 的 task、port、operation、audit 和 metadata 表原地打开，并使用 WAL 与 full synchronous durability。
Pending 记录保留原始 request、digest 和不确定性；完成 native worker identity reconciliation 后，启动阶段只有在 drain transition 有效且没有 native worker effect 时，才能执行一次同操作恢复。
在蓝绿目标中，槽位服务必须先取得 deployd 已认证的 controller role，才能运行 startup reconciliation、scheduler、timer reconciliation、age-GC 或 scheduler-driven native mutation；standby service 可以提供受保护的 readiness 和只读观测，但不得运行这些循环。
Native worker-bearing operation 在 effect 不确定时永远不能自动 replay，admission 必须保持关闭，直到显式且安全地完成 reconciliation。
Integer token 必须在 JavaScript safe-number boundary 之外保持精确，fractional JSON token 必须保留与 integer 的区别，request digest 使用既有 sorted、ASCII-escaped JSON spelling。
SQLite signed-integer limit 仍是独立的持久化存储边界；overcommit ceiling 使用 decimal integer arithmetic 和 conservative current/target reservation。

Native file lock 在独立进程之间保持可见。
Session-scoped lifecycle lock 和短时 maintenance lock 用于协调持久状态转换，但不会串行化无关 session；native guest wait 在 SQLite transaction 和 maintenance lock 外执行。
每个写入 journal 的 native command 都记录 PID/start-time identity，timeout diagnostics 区分已验证的 process cleanup、leader reap、pipe drainage 和不确定结果。
只有 identity 已验证且归属明确的 descendant 或 recovered worker 可以被 kill。

## 验证证据

Fake Incus 和 guest-command 场景在不改变基础设施的情况下验证 controller 行为。
使用真实 flock、SQLite、procfs、subprocess tree 和 pipe 的 Linux 测试验证隔离的 native OS 行为；Agent VM 检查还会验证 systemd 和 nested Incus system container，且不要求 KVM。
可复现的 `scripts/agent_vm_native_acceptance.sh` runner 还会检查已安装的 format-2 bundle、systemd activation、直接 UDS 和 Bun bridge 请求、native-worker cleanup 以及 nested Incus system container。
Nested 路径使用 `security.nesting=true` 的 Incus system container，不要求 `/dev/kvm` 或 QEMU。
只有在候选 bundle 安装完成后，才能在已授权的会话 Agent VM 内运行它。

在 source checkout 中、确保宿主机提供 Bun 和 Python 后运行以下聚焦验收套件：

```bash
bun test skills/agent-vm-ops/tests/*.test.ts
python3 -m unittest discover -s skills/agent-vm-ops/tests -p 'test_*.py'
go -C skills/agent-vm-ops/control test ./...
bun test skills/agent-vm/tests/*.test.ts

# 在候选版本安装完成后，于会话 Agent VM 内运行：
skills/agent-vm-ops/scripts/agent_vm_native_acceptance.sh
```

| VM 控制合同 | 证据面 |
| --- | --- |
| 01: protocol and configuration | `control-lifecycle.test.ts`, `control-process.test.ts` |
| 02: prior state, digests and idempotence | `fixtures/control-baseline.sql`, `fixtures/control-baseline.json`, `control-kernel.test.ts`, `control-lifecycle.test.ts`, `control-process.test.ts` |
| 03: native process locks, admission and expiry | Linux cases in `control-kernel.test.ts` and `control-process.test.ts` |
| 04: integer precision, decimal ratios and transactions | `control-kernel.test.ts`, `control-resources.test.ts` |
| 05: lifecycle, reclaim and partial failures | `control-lifecycle.test.ts`, `control-resources.test.ts` |
| 06: native process trees and bounded pipes | Linux CommandRunner cases in `control-kernel.test.ts` |
| 07: guest growth and prerequisites | `guest-root-growth.test.ts`, guest prerequisite and stdin-bundle cases in the control suites |
| 08: current client, release and systemd contracts | `control-process.test.ts`, `control-kernel.test.ts`, complete-package Python release/recovery tests and `skills/agent-vm/tests/transport.test.ts` and `client.test.ts` |
| 09: installed Linux/systemd/nested-Incus acceptance | `scripts/agent_vm_native_acceptance.sh` |

Linux case 会在其他平台上主动跳过；仅 macOS 运行通过不足以证明 native control host 合格。
基线 SQL 和 request fixture 由已接受的前身 StateStore 在 SQL fixture 记录的 source revision 生成。
测试直接消费这些字节，不导入已退役的实现。
