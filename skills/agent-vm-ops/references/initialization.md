# Agent VM deployment initialization

This reference defines the deployment-independent Incus project/profile baseline for Agent VM. It is an operator runbook, not a product or lifecycle specification. Deployment aliases, addresses, credentials, inventories, state databases, live Incus output and maintenance records remain outside the repository.

## Ownership boundary

The control service owns CPU, memory and disk admission using its durable task state and configured budgets. Incus supplies the container isolation boundary and the per-instance profile limits. A normal client request never edits Incus project policy.

The initialization baseline is intentionally narrow:

| Surface | Required state | Owner |
| --- | --- | --- |
| Incus project | `limits.processes`, `limits.cpu`, `limits.memory` and `limits.instances` are unset | Operator initialization |
| `agent-vm` profile | `limits.processes=512` unless the deployment has an explicitly reviewed different value | Operator initialization |
| Profile CPU, memory, disk, network and security settings | Deployment-specific; inspect and preserve | Deployment operator |
| Guest instances, images, pools, networks and state database | Preserve; outside this initializer | Existing lifecycle/deployment procedures |

Leaving the project-level aggregate process quota unset prevents stopped cache instances from consuming a second, implicit process budget. The profile-level process limit remains the per-instance protection. This does not increase the control service's logical CPU or memory ceilings.

容器内运行 Incus 时，先按[嵌套权限手册](../../../deploy/docs/nested-incus.md)检查外层有效 UID/GID、内层 subordinate ID 授权、helper 和安全能力，并独立验收声明支持的 guest 模式。此初始化器不会检查或修改 `security.privileged`、`security.nesting`、`security.idmap.*`、`raw.idmap` 或 `/etc/subuid`、`/etc/subgid`；`aligned` 只表示本文列出的配额基线一致，不代表非特权嵌套可用。

## Preconditions

Use this procedure only for an explicitly authorized infrastructure task.

1. Read the configured control alias from the protected deployment configuration. Do not copy the alias, address or credentials into this repository.
2. Run `agent-vm doctor`, `agent-vm admin status`, `agent-vm admin session list` and `ssh -G <configured-control-alias>`.
3. Inspect the exact Incus project and profile identities, native service health, pending operations and current task leases.
4. 按[维护 drain 与恢复](../../../deploy/docs/maintenance.md)确认没有活动发布或未知 owner，使用本次唯一的 `release-maintenance-<unique-id>` 会话执行 `agent-vm --session-id <该会话> admin drain enter`。保存同一会话直到退出；普通会话进入成功不保证其有权退出。等待有效租约和 pending transition 收敛后才能应用策略修改。
5. Keep a protected maintenance record of the preview output and the operator, time and deployment identity.

Do not combine initialization with broad reconcile, garbage collection, instance deletion, image replacement, network changes, storage changes or state-database edits.

## Check and apply

The script uses the Incus JSON API through `incus query` for reads and narrow Incus CLI mutations for writes. It requires an explicit remote alias and defaults to a read-only preview:

```sh
bun skills/agent-vm-ops/scripts/agent_vm_incus_init.ts \
  --remote <configured-control-alias>
```

The preview exits with status `0` when aligned and status `2` when it finds changes. Review its JSON actions. Apply only the reviewed plan while drain is active:

```sh
bun skills/agent-vm-ops/scripts/agent_vm_incus_init.ts \
  --remote <configured-control-alias> \
  --project <incus-project> \
  --profile <incus-profile> \
  --apply
```

`--apply` only unsets the four project aggregate keys when present and sets the profile process limit when absent or different. It never deletes or restarts an instance and never edits images, pools, networks, storage, credentials or the state database. The script rereads both resources after mutation and fails if the canonical baseline is not verified. A failed partial action is safe to rerun after inspecting the error.

The default deployment values are `--project agents`, `--profile agent-vm` and `--process-limit 512`. Override them only when the protected deployment configuration and maintenance record identify a different intended scope.

## Post-checks

After successful verification:

1. Re-run the initializer without `--apply`; it must report `aligned` with no actions.
2. Re-run `agent-vm doctor` and `agent-vm admin metrics`; confirm admission is ready and drain is still the only expected maintenance state.
3. 只有 service health、reconciliation、expiry/GC 和 storage admission 检查通过后，才按维护手册用原 owner 会话执行 `agent-vm --session-id <原会话> admin drain exit`；立即确认 `draining=false`、完整一致的 task 观测及活动槽位 traffic 已恢复。退出失败时保留原身份和证据，不能直接修改数据库或覆盖他人的 drain。
4. Keep the preview, apply result and post-check evidence in protected operations records, not in the repository.

This procedure does not repair historical failed or closing task journals. Reconcile those records only as a separately authorized operation with an exact identity and evidence review.

## Compatibility rules

The script avoids human-oriented YAML and Incus inventory output. It reads the project and profile through stable JSON API paths, treats Incus configuration values as strings or scalar values, and uses explicit command arguments rather than shell interpolation. Its tests use local fixtures; normal repository validation never requires a live Incus daemon or deployment access.
