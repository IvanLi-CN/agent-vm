# Native deployment boundary

Install the matching client, one-shot control script and systemd assets from one checked-out Agent Toolkit revision. Record the revision and file checksums in protected operations records; do not create an installed-source fork.

If the control environment must be rebuilt or recovered, preserve the state and VM data and follow the [topology and recovery runbook](../../../docs/specs/agent-vm/OPERATIONS.md) before replacing files. This deployment reference does not identify the host below CT109 or provide a host rebuild procedure.

| Surface | Deployment input |
| --- | --- |
| Control script | `/opt/agent-vm-native/agent_vm_control.py` or an explicitly configured path |
| Configuration | root-owned JSON at `/etc/agent-vm-native/config.json` |
| Durable state | configured local state database and lock directory |
| Native units | systemd assets copied atomically into the configured unit directory |
| Local client | installed executable and protected session state under the operator's chosen XDG paths |

The deployment supplies the control alias, Incus project/profile/image, network ACL, control address, guest address pool, host port range, storage path, state database and timer directory through the example configuration. Placeholder values must be replaced before installation; missing or invalid values fail closed.

The control entrypoint and every bundled systemd unit read the same canonical path, `/etc/agent-vm-native/config.json`. Deployment selects the project, image, network, address pool, port range, storage, state database and ACL by installing a protected JSON at that path; alternate runtime config paths are intentionally unsupported so one-shot requests and expiry/reconciliation timers cannot select different state stores.

Before any mutation, inspect the actual Incus identities, proxy permission, ACL ownership, guest isolation, local filesystem capacity, timers and protocol versions. Keep credentials and connection material outside source. Do not add a resident broker, HTTP listener, extra SSH daemon, gateway or automatic keepalive.

Prepare and verify the pinned guest image separately. Confirm fresh guests generate distinct host keys, reach readiness and expose only the configured services. Verify expiry, exact mapping cleanup, cache reclamation, capacity pressure and generation handling with isolated resources; do not use production operations as a migration gate.

Protocol upgrades enter drain, converge pending operations without replay, replace matching client/control files atomically and preserve the state database, generations and journals. A health failure keeps drain enabled. Daily requests never edit Incus project policy. Keep run records and any live infrastructure evidence outside the public repository.
