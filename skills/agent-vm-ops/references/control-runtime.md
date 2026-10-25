# One-shot control runtime

The active control implementation is `scripts/agent_vm_control.ts`, invoked directly with Bun. Its sibling modules are implementation modules of that same process; none is a second controller or resident service. The guest disk action is `scripts/guest-root-growth.ts`, bundled in memory from the verified release and supplied to `incus exec <instance> -- bun run - <disk-gib>` on stdin. It exits after reporting measured capacity. There is no Python control or disk-growth fallback.

## Prerequisites

The native control host and disk-growth guest require Linux with procfs, cgroup v2 where observed, `flock`, child-subreaper support and identity-checked pidfds. The libc loaded by Bun FFI must expose `flock`, `prctl`, `pidfd_open`, `pidfd_send_signal` and `waitpid`; glibc 2.36 or newer and Linux 5.3 or newer provide these interfaces. Capacity observations verify the visible cgroup and LXCfs boundaries before using their data. Missing native ownership or capacity evidence fails closed.

Install an operator-selected Bun supporting `bun:ffi`, `bun:sqlite`, `Bun.build` and direct TypeScript execution on both control host and generic image. Verify those capabilities before activation rather than installing a runtime during a request. Systemd and the protected release wrapper invoke the root-owned `/usr/local/bin/bun`; noninteractive control SSH must resolve `bun` to that installed runtime. The guest must resolve `bun` before any root-device change. The public repository does not pin or download a toolchain.

Keep Incus, systemd, OpenSSH, conntrack-tools, `ss` and filesystem-capacity tools available on the control host. Guest growth requires a direct partition-backed ext4 or xfs root and `findmnt`, `lsblk`, `blockdev`, `growpart`, plus `resize2fs` or `xfs_growfs`. Unknown topology, missing tools, stale virtual-disk size and unverifiable post-growth capacity refuse success. Tool success alone is insufficient: disk, partition and filesystem measurements must satisfy the requested size and the existing 95-percent filesystem threshold.

The client and guest source transaction run directly in Bun. Deploy the complete client package from the [release contract](../../../docs/specs/agent-vm/contracts/release.md): `agent_vm.ts` and `sync_worktree.ts` at `0755`, with `byte-fs.ts`, `client-io.ts`, `client-release.ts`, `client.ts`, `guest-source.ts` and `sync-native.ts` at `0644`. The local sidecar hashes all eight files. Python remains the manual publisher/wrapper and release-policy tooling boundary; it does not run a client, controller or guest source fallback. Source sync requires Bun FFI, native `flock`, Linux `renameat2`, GNU `timeout`, Git and rsync as specified by the client/source contract. Verify them in the managed image before source staging; do not install a runtime during a request. The controller's generic image prerequisite list may include additional tools, including Python, independently of the Bun source action.

## Durable and transport compatibility

Protocol 4, request identity, per-action compatibility windows, generation checks, operation IDs and the 2-MiB management-request limit remain unchanged. Requests travel as one UTF-8 JSON object on SSH stdin. Success is JSON on stdout; structured failures are JSON on stderr with exit status 2, and unexpected failures use status 3. The client retains its 240-second ordinary and 600-second startup/restart-capable timeout budgets and its bounded output readers.

Existing SQLite task, port, operation, audit and metadata tables are opened in place with WAL and full synchronous durability. Pending records retain their original request digest and uncertainty; restart never rewrites a request to make it replayable. Integer tokens remain exact beyond JavaScript's safe-number boundary, fractional JSON tokens retain their distinction from integers, and request digests use the existing sorted, ASCII-escaped JSON spelling. SQLite signed-integer limits remain a separate durable-storage boundary. Overcommit ceilings use decimal integer arithmetic and conservative current/target reservations.

Native file locks remain visible across independent processes. Session, operation, admission and short global-state locks retain their responsibilities; native guest waits run outside SQLite transactions and the global lock. Timeout diagnostics distinguish verified process cleanup, leader reaping and pipe drainage from an uncertain outcome. Only identity-verified owned descendants may be killed.

## Validation evidence

Fake Incus and guest-command scenarios establish controller behavior without changing infrastructure. Linux tests with real flock, SQLite, procfs, subprocess trees and pipes establish isolated native OS behavior; they do not establish Incus or image deployment acceptance. A production release additionally needs an explicitly authorized installation and observed native image, expiry, recovery and reconciliation checks. Report that deployment evidence as unavailable when no such release occurred.

The focused acceptance suite is run from the source checkout with the host's Bun and Python available:

```bash
bun test skills/agent-vm-ops/tests/*.test.ts
python3 -m unittest discover -s skills/agent-vm-ops/tests -p 'test_*.py'
bun test skills/agent-vm/tests/*.test.ts
```

| VM Control contract | Evidence surface |
| --- | --- |
| 01: protocol and configuration | `control-lifecycle.test.ts`, `control-process.test.ts` |
| 02: prior state, digests and idempotence | `fixtures/control-baseline.sql`, `fixtures/control-baseline.json`, `control-kernel.test.ts`, `control-lifecycle.test.ts`, `control-process.test.ts` |
| 03: native process locks, admission and expiry | Linux cases in `control-kernel.test.ts` and `control-process.test.ts` |
| 04: integer precision, decimal ratios and transactions | `control-kernel.test.ts`, `control-resources.test.ts` |
| 05: lifecycle, reclaim and partial failures | `control-lifecycle.test.ts`, `control-resources.test.ts` |
| 06: native process trees and bounded pipes | Linux CommandRunner cases in `control-kernel.test.ts` |
| 07: guest growth and prerequisites | `guest-root-growth.test.ts`, guest prerequisite and stdin-bundle cases in the control suites |
| 08: current client, release and systemd contracts | `control-process.test.ts`, `control-kernel.test.ts`, complete-package Python release/recovery tests and `skills/agent-vm/tests/transport.test.ts` and `client.test.ts` |

Linux cases deliberately skip on other platforms; a passing macOS run alone is insufficient evidence for the native control host. The baseline SQL and request fixtures were generated by the accepted predecessor StateStore at the source revision recorded in the SQL fixture. Tests consume those bytes without importing the retired implementation.
