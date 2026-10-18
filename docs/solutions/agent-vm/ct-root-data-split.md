---
title: Split an Incus Agent VM CT Root Filesystem from Its Data Pool
module: agent-vm
problem_type: operational-recovery
component: native-incus-control
tags:
  - incus
  - lxc
  - storage
  - capacity
  - recovery
status: active
related_specs:
  - docs/specs/agent-vm/SPEC.md
---

# Context

Use this procedure when an Agent VM control environment stores its Incus directory pool below the same filesystem as the CT root filesystem, and growth of VM data can exhaust the filesystem needed by SQLite, systemd, Incus and the native controller.

The goal is to keep the existing CT and existing Incus pool identity while moving the CT system files to a small system volume and retaining the old root volume as a data volume. Incus image staging must also be moved off the small system volume because image unpacking uses its global `images` directory before the final pool volume is created. This is a one-time infrastructure migration; routine Agent VM maintenance must not require PVE host access.

# Symptoms

- The CT root filesystem and the Incus pool report the same nearly full filesystem.
- `agent-vm doctor` reports `disk_reclaim_required` or admission is unavailable even though Incus itself is running.
- SQLite/WAL, reconciliation or garbage collection becomes unreliable near exhaustion.
- The `agent-vm` client executable may not be installed inside the CT even though the native control script and systemd units are present.
- The Incus data pool has ample space but image creation fails with `Unable to unpack image, run out of disk space`.

# Root Cause

The Incus directory pool is addressed by a stable configured path, but that path is backed by the CT root filesystem. The pool source is part of an existing Incus pool definition and cannot be changed with `incus storage set` while the pool is in the `Created` state. Changing the configured `storage_path` also breaks the durable state and pool identity contract.

Incus keeps image tarballs and image-unpack staging under its global `/var/lib/incus/images` path. A configured `storage.images_volume` changes final image storage but does not remove the transient unpack requirement from this path. Therefore a healthy data pool does not prove that an image can be unpacked when the CT root filesystem is nearly full.

The correct separation is filesystem-level: mount the old root volume as a CT data mount, then bind-mount the original pool directory from that data mount back onto the original configured pool path. Incus keeps its existing pool source and identity while actual pool I/O uses the data volume.

# Resolution

## One-time storage migration

The PVE operator owns this one-time layout change. Use an explicit maintenance window and exact volume identities.

1. Stop the existing CT and mount its current root volume read-only for measurement where possible.
2. Measure system content separately from the configured `storage_path`. Do not copy the pool data into the new system volume.
3. Allocate and format a new system volume sized for the measured non-pool content plus operating headroom.
4. Copy the old root filesystem to the new system volume while excluding the configured pool directory. Socket warnings from `tar` are expected; do not copy live Unix sockets.
5. Change the existing CT root volume to the new system volume and attach the unchanged old root volume as a data mount, for example at `/data`.
6. Inside the CT, stop native timers before changing mounts. Keep `storage_path` unchanged.
7. Bind-mount `/data<storage_path>` onto `<storage_path>` and persist that bind mount with a dependency on `/data`.
8. Move the Incus image staging directory to the data filesystem. If the old root volume already contains an identical copy at `/data/var/lib/incus/images`, verify names, sizes and hashes before removing the system-volume copy; otherwise copy the directory to the data filesystem and verify the copy before removal.
9. Stop Incus and its native maintenance units, bind-mount `/data/var/lib/incus/images` onto `/var/lib/incus/images`, and persist the bind mount with `x-systemd.requires-mounts-for=/data`.
10. Start Incus and verify both bind mounts, `incus storage show <pool>`, and `df` for `/`, `/var/lib/incus/images` and `<storage_path>`. The pool source should still be the original configured path.

Do not create a new CT, resize the old root volume, change an existing pool source, or delete the old root volume during this migration.

## Move Incus image staging off the CT root

The following is the required CT-side shape after the one-time migration. Replace `<data-mount>` with the existing CT data mount and keep the Incus path unchanged:

```bash
data_mount=/data
pool_path=/var/lib/incus/storage-pools/POOL
src=/var/lib/incus/images
dst="$data_mount/var/lib/incus/images"
fstab_line="$dst $src none bind,x-systemd.requires-mounts-for=/data 0 0"

findmnt -T "$data_mount"
findmnt -T "$src"
df -h / "$src" "$pool_path"
find "$src" -maxdepth 1 -mindepth 1 -printf '%y %f %s\n' | sort
find "$dst" -maxdepth 1 -mindepth 1 -printf '%y %f %s\n' | sort
```

Verify every source entry against the data-side copy with `sha256sum` before removing any source file. Stop `incus.service`, `incus.socket`, the native reconcile service and the age-GC timer, remove only the verified duplicate entries from the unmounted source directory, then mount the data-side directory:

```bash
systemctl stop agent-vm-native-age-gc.timer agent-vm-native-reconcile.service || true
systemctl stop incus.socket incus.service
mount --bind "$dst" "$src"
grep -Fqx "$fstab_line" /etc/fstab || printf '%s\n' "$fstab_line" >> /etc/fstab
systemctl daemon-reload
systemctl start incus.service
systemctl start agent-vm-native-age-gc.timer
systemctl start agent-vm-native-reconcile.service || true
```

Do not rely on `storage.images_volume` alone for this problem. It is an optional final image-volume setting; it does not replace the bind mount for transient unpack space.

## Routine recovery through the CT control alias

Use the configured SSH control alias that resolves directly to the CT. Do not use a PVE host address or `pct exec` for native runtime maintenance when the CT control alias is available.

The installed native entrypoint is normally:

```text
/usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py
```

The entrypoint speaks protocol 4 JSON over stdin. The `agent-vm` client is an operator-side executable and is not required to exist inside the CT.

Read-only health check:

```bash
printf '%s\n' '{"protocol":4,"action":"doctor"}' |
  ssh <ct-control-alias> /usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py
```

When a stopped instance is shown by Incus but its durable record is still `active` with no effective lease, use this bounded sequence:

```bash
operation_id="reconcile-preview-$(date +%s)"
printf '%s\n' \
  "{\"protocol\":4,\"action\":\"admin-reconcile\",\"generation\":0,\"operation_id\":\"$operation_id\",\"apply\":false}" |
  ssh <ct-control-alias> /usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py
```

After reviewing that the action is only `close-incomplete-or-expired` for the intended task, apply the reconciliation with a new operation ID. Then preview `admin-gc` with `generation: 0`; apply GC only when the candidate is a stopped managed cache with no effective lease and the candidate list contains only the authorized task.

Apply the reviewed reconciliation:

```bash
operation_id="reconcile-apply-$(date +%s)"
printf '%s\n' \
  "{\"protocol\":4,\"action\":\"admin-reconcile\",\"generation\":0,\"operation_id\":\"$operation_id\",\"apply\":true}" |
  ssh <ct-control-alias> /usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py
```

Preview and, only after reviewing the exact candidate, apply garbage collection:

```bash
operation_id="gc-preview-$(date +%s)"
printf '%s\n' \
  "{\"protocol\":4,\"action\":\"admin-gc\",\"generation\":0,\"operation_id\":\"$operation_id\",\"apply\":false}" |
  ssh <ct-control-alias> /usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py

operation_id="gc-apply-$(date +%s)"
printf '%s\n' \
  "{\"protocol\":4,\"action\":\"admin-gc\",\"generation\":0,\"operation_id\":\"$operation_id\",\"apply\":true}" |
  ssh <ct-control-alias> /usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py
```

The final command deletes only the candidates returned by the immediately preceding preview after the controller rechecks ownership, generation, stopped state and access cleanup.

After recovery, verify `doctor` reports `ready: true`, `admission_ready: true`, and an empty `issues` list. Also verify the pool mount and re-enable the native reconcile service and age-GC timer inside the CT.

```bash
ssh <ct-control-alias> systemctl enable agent-vm-native-reconcile.service
ssh <ct-control-alias> systemctl enable --now agent-vm-native-age-gc.timer
ssh <ct-control-alias> /bin/sh -c \
  "printf '%s\\n' '{\"protocol\":4,\"action\":\"doctor\"}' | /usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py"
ssh <ct-control-alias> findmnt -T /var/lib/incus/storage-pools/<pool>
ssh <ct-control-alias> findmnt -T /var/lib/incus/images
ssh <ct-control-alias> df -h / /var/lib/incus/images /var/lib/incus/storage-pools/<pool>
```

For a bounded end-to-end check, use a unique test session through the protocol 4 control entrypoint, confirm that `acquire` reaches `guest_available: true`, then issue `release` with the returned generation and preview/apply `admin-gc` only for that test task. This tests image unpacking, VM creation, proxy setup and cleanup; a read-only pool health check does not.

## Temporary migration residue

The one-time PVE copy may leave only an empty temporary mount directory after unmounting the new system volume. Verify it is not a mount point and is empty before removing it with `rmdir`. Never remove the new system volume, the old data volume, the CT configuration, or the original pool path.

# Guardrails / Reuse Notes

- Measure capacity only from inside the CT's configured Incus pool filesystem for native admission decisions.
- Check both the configured data pool and the Incus image staging filesystem. Data-pool free space alone cannot explain or prevent a root-filesystem `Unable to unpack image` failure.
- Keep the original `storage_path` and Incus pool source stable; use a bind mount to change the backing filesystem.
- Keep `/var/lib/incus/images` on the data filesystem with a persistent bind mount; do not treat `storage.images_volume` as a substitute for transient staging capacity.
- Use a direct CT SSH control alias for routine health, reconciliation, GC and timer maintenance.
- Preview reconciliation and GC before applying them. Include protocol 4, an operation ID and `generation: 0` for admin write actions.
- Never manually delete directories inside the Incus pool to create space. Delete only controller-selected, ownership-verified stopped caches.
- Do not treat `agent-vm` command-not-found inside the CT as proof that Incus is broken; inspect the native systemd `ExecStart` and invoke the installed control script directly.
- Keep real CT IDs, addresses, SSH identities, deployment revisions and checksums in operator-owned records, not in this public solution.

# References

- [Agent VM specification](../../specs/agent-vm/SPEC.md)
- [CLI contract](../../specs/agent-vm/contracts/cli.md)
- [Native deployment boundary](../../../skills/agent-vm-ops/references/deployment.md)
- [Incus server configuration](https://linuxcontainers.org/incus/docs/main/server_config/)
- [Incus storage volumes](https://linuxcontainers.org/incus/docs/main/howto/storage_volumes/)
- [Incus issue: image unpacking still uses the global images directory](https://github.com/lxc/incus/issues/1426)
