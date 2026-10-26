# One-shot control runtime

The resident control implementation is `control/cmd/agent-vm-control/main.go`, built with Go 1.27.3 and invoked as one binary for `serve`, `request` and `native-worker`. Its lifecycle, state, scheduler and native worker packages are one control authority; the TypeScript bridge remains a fixed compatibility adapter during the support window. The guest disk action is `scripts/guest-root-growth.ts`, bundled in memory from the verified release and supplied to `incus exec <instance> -- bun run - <disk-gib>` on stdin. It exits after reporting measured capacity. There is no Python control or disk-growth guest fallback.

## Prerequisites

The native control host and disk-growth guest require Linux with procfs, cgroup v2 where observed, `flock`, child-subreaper support and identity-checked pidfds. The Go worker uses `prctl`, `pidfd_open`, `pidfd_send_signal` and `waitpid` through `x/sys/unix`; glibc 2.36 or newer and Linux 5.3 or newer provide the required interfaces. Capacity observations verify the visible cgroup and LXCfs boundaries before using their data. Missing native ownership or capacity evidence fails closed.

Install the pinned Go 1.27.3 toolchain when building the resident binary and verify `CGO_ENABLED=0` Linux artifacts before activation. The control host does not install a runtime during a request. Systemd invokes `/opt/agent-vm-native/bin/agent-vm-control`; the compatibility bridge may use the protected Bun runtime only while forwarding to the UDS. The guest must resolve `bun` before any root-device change. The public repository does not download a toolchain at runtime.

Keep Incus, systemd, OpenSSH, conntrack-tools, `ss` and filesystem-capacity tools available on the control host. Guest growth requires a direct partition-backed ext4 or xfs root and `findmnt`, `lsblk`, `blockdev`, `growpart`, plus `resize2fs` or `xfs_growfs`. Unknown topology, missing tools, stale virtual-disk size and unverifiable post-growth capacity refuse success. Tool success alone is insufficient: disk, partition and filesystem measurements must satisfy the requested size and the existing 95-percent filesystem threshold.

The client and guest source transaction run directly in Bun. Deploy the complete client package from the [release contract](../../../docs/specs/agent-vm/contracts/release.md): `agent_vm.ts` and `sync_worktree.ts` at `0755`, with `byte-fs.ts`, `client-io.ts`, `client-release.ts`, `client.ts`, `guest-source.ts` and `sync-native.ts` at `0644`. The local sidecar hashes all eight files. Python remains the manual publisher/wrapper and release-policy tooling boundary; it does not run a client, controller or guest source fallback. Source sync requires Bun FFI, native `flock`, Linux `renameat2`, GNU `timeout`, Git and rsync as specified by the client/source contract. Verify them in the managed image before source staging; do not install a runtime during a request. The controller's generic image prerequisite list may include additional tools, including Python, independently of the Bun source action.

## Durable and transport compatibility

Protocol 4, request identity, per-action compatibility windows, generation checks, operation IDs and the 2-MiB management-request limit remain unchanged. The primary request path is HTTP over the protected UDS; the existing SSH entry and TypeScript bridge can forward the same JSON contract for compatibility. Success is JSON on stdout or the UDS response body; structured failures preserve exit status 2 for the request adapter, and unexpected failures use status 3. The client retains its 240-second ordinary and 600-second startup/restart-capable timeout budgets and its bounded output readers.

Existing SQLite task, port, operation, audit and metadata tables are opened in place with WAL and full synchronous durability. Pending records retain their original request, digest and uncertainty; after native worker identity reconciliation, startup may perform one same-operation recovery only for a valid drain transition with no native worker effect. Native worker-bearing operations are never replayed automatically while their effect is uncertain, so admission remains closed until reconciliation is explicit and safe. Integer tokens remain exact beyond JavaScript's safe-number boundary, fractional JSON tokens retain their distinction from integers, and request digests use the existing sorted, ASCII-escaped JSON spelling. SQLite signed-integer limits remain a separate durable-storage boundary. Overcommit ceilings use decimal integer arithmetic and conservative current/target reservations.

Native file locks remain visible across independent processes. Session-scoped lifecycle locks and a short maintenance lock coordinate durable transitions without serializing unrelated sessions; native guest waits run outside SQLite transactions and the maintenance lock. Every journaled native command records its PID/start-time identity, and timeout diagnostics distinguish verified process cleanup, leader reaping and pipe drainage from an uncertain outcome. Only identity-verified owned descendants or recovered workers may be killed.

## Validation evidence

Fake Incus and guest-command scenarios establish controller behavior without changing infrastructure. Linux tests with real flock, SQLite, procfs, subprocess trees and pipes establish isolated native OS behavior; Agent VM checks additionally exercise systemd and nested Incus system containers without requiring KVM. The reproducible `scripts/agent_vm_native_acceptance.sh` runner additionally checks an installed format-2 bundle, systemd activation, both direct UDS and Bun bridge requests, native-worker cleanup and a nested Incus system container. The nested path is an Incus system container with `security.nesting=true`; it does not require `/dev/kvm` or QEMU. Run it only inside the authorized session Agent VM after installing a candidate bundle.

The focused acceptance suite is run from the source checkout with the host's Bun and Python available:

```bash
bun test skills/agent-vm-ops/tests/*.test.ts
python3 -m unittest discover -s skills/agent-vm-ops/tests -p 'test_*.py'
go -C skills/agent-vm-ops/control test ./...
bun test skills/agent-vm/tests/*.test.ts

# Run inside the session Agent VM after candidate installation:
skills/agent-vm-ops/scripts/agent_vm_native_acceptance.sh
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
| 09: installed Linux/systemd/nested-Incus acceptance | `scripts/agent_vm_native_acceptance.sh` |

Linux cases deliberately skip on other platforms; a passing macOS run alone is insufficient evidence for the native control host. The baseline SQL and request fixtures were generated by the accepted predecessor StateStore at the source revision recorded in the SQL fixture. Tests consume those bytes without importing the retired implementation.
