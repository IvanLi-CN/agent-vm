# Native deployment boundary

Install the matching client, one-shot control script and systemd assets from one checked-out Agent Toolkit revision. Record the revision and file checksums in protected operations records; do not create an installed-source fork.

If the control environment must be rebuilt or recovered, preserve the state and VM data and follow the [topology and recovery runbook](../../../docs/specs/agent-vm/OPERATIONS.md) before replacing files. This deployment reference does not identify the host below CT109 or provide a host rebuild procedure.

| Surface | Deployment input |
| --- | --- |
| Control script | `/opt/agent-vm-native/agent_vm_control.py` |
| Configuration | root-owned JSON at `/etc/agent-vm-native/config.json` |
| Durable state | configured local state database and lock directory |
| Native units | static release units copied atomically into `/etc/systemd/system`; `timer_dir` is reserved for generated per-task expiry timers |
| Local client | installed executable and protected session state under the operator's chosen XDG paths |

The deployment supplies the control alias, Incus project/profile/image, network ACL, control address, guest address pool, host port range, storage path, state database and timer directory through a copied or derived configuration. `config.json.example` is a generic schema/template, not deployment evidence; for the locked deployment policy the installed configuration must set `memory_budget_mib=32768` and `memory_overcommit_ratio=2`. Placeholder values must be replaced before installation; missing or invalid values fail closed.

The control entrypoint and every bundled systemd unit read the same canonical path, `/etc/agent-vm-native/config.json`. Deployment selects the project, image, network, address pool, port range, storage, state database and ACL by installing a protected JSON at that path; alternate runtime config paths are intentionally unsupported so one-shot requests and expiry/reconciliation timers cannot select different state stores. Static release-unit verification always uses `/etc/systemd/system`, while `timer_dir` only controls generated per-task expiry timers.

Before any mutation, inspect the actual Incus identities, proxy permission, ACL ownership, guest isolation, local filesystem capacity, timers and protocol versions. Keep credentials and connection material outside source. Do not add a resident broker, HTTP listener, extra SSH daemon, gateway or automatic keepalive.

Prepare and verify the pinned guest image separately. Confirm fresh guests generate distinct host keys, reach readiness and expose only the configured services. Verify expiry, exact mapping cleanup, cache reclamation, capacity pressure and generation handling with isolated resources; do not use production operations as a migration gate.

## Control-plane publication

A control-plane release is a logical compatibility unit containing the local client, one-shot control script, systemd assets, release metadata, and exact artifact checksums. It excludes deployment configuration, credentials, connection material, state databases and WAL/journals, locks, Incus VM data, and guest images. The release manifest declares protocol, source revision, artifact integrity, and per-operation request/response/semantic compatibility in each supported direction.

The public manifest, wrapper, publisher, and failure-state contract is [the Agent VM control-plane release contract](../../../docs/specs/agent-vm/contracts/release.md). Use the repository scripts only with owner-provided deployment inputs; do not put those inputs in the repository.

Publication is manual and operator-owned. It resolves an explicit source revision, builds and checks the manifest locally, uploads exact candidates to a private staging area through the configured control SSH entry, and verifies remote owner, mode, syntax, checksum, and unit contents before activation. The deploy identity may invoke a narrowly scoped root-owned wrapper for the protected artifact and systemd paths; it must not gain PVE, host, Incus project-policy, guest, extra SSH, HTTP, or gateway access. Do not publish through the public `agent-vm` CLI or a VM guest.

The remote control release is activated and health-checked before the local client is changed. The fixed wrapper installs the client, control/support parser, release manifest and systemd assets only at its protected targets. A compatible older client may continue using supported actions with a structured warning; an unsupported action, incompatible protocol/schema/semantic change, or unverified checksum fails before side effects. Compatible evolution preserves protocol 4; a breaking change may require an explicit coordinated update. Unattended control-plane updates are disabled by default.

For remote script or unit changes, enter admission drain before activation. The wrapper holds the fixed `/run/agent-vm-native/release-activation.fence` until activation or rollback completes; control-plane drain exit rechecks that fence under `admission.lock`, so admission cannot reopen between the drain check and pointer replacement. Drain blocks new acquisition and restart admission, keeps status/retrieval/release and expiry/reconciliation/cache-cleanup timers available, and never force-kills in-flight operations. Wait for pending operations and task transitions to converge, atomically activate the candidates, run health/reconciliation/timer checks, and exit drain only after success. A pre-activation failure leaves the current release untouched. A post-activation health failure permits one bounded rollback to the previous verified release; a failed rollback keeps admission drained and never rewrites state, generations, or uncertain operations. Retain at least the current and previous verified releases with their manifests and checksums.

Daily requests never edit Incus project policy. Keep release manifests, run records, protected recovery copies, and live infrastructure evidence outside the public repository.

The generic operator sequence is `python3 skills/agent-vm-ops/scripts/agent_vm_release.py build --source-root <source> --output <bundle>`, local `verify`, then `publish --bundle <bundle> --control-host <configured-alias> --client-target <local-client-target>`. The optional legacy `--wrapper` argument, when retained by an existing invocation, must equal `/usr/local/sbin/agent-vm-release-wrapper`; the remote wrapper and control paths are otherwise fixed. `rollback` is a separate operator action bound to a transaction and release identity; its `--bundle` is the previous verified release bundle and its required `--client-target` restores the matching local client before drain exit. The remote control path is fixed at `/opt/agent-vm-native/agent_vm_control.py`; no caller override is accepted. The wrapper's fixed command set is the only remote activation interface; the public `agent-vm` client remains unchanged.

## Observability boundary

The dashboard and its provisioning source are deployment-owned. Use the [public observability contract](../../../docs/specs/agent-vm/contracts/observability.md) for metric semantics, but keep the Grafana JSON, Prometheus scrape target, collector path and datasource identity in the deployment repository. The one-shot controller is not a resident HTTP metrics service; add only an explicitly authorized read-only scrape bridge or textfile collector that invokes `admin-metrics` and exposes freshness/up failure semantics. Verify the deployment configuration's 32768-MiB memory base and 2x ratio before publishing a 65536-MiB logical limit. Do not treat a Grafana database export or an Incus project limit of `-1` as an authoritative native admission metric.

## Runtime maintenance boundary

Routine health checks, reconciliation, garbage collection and timer maintenance run through the configured SSH control alias that resolves directly to the CT. They must not require access to the PVE host or use `pct exec` as a substitute for the native control path. The one-time split of a CT root filesystem from its Incus data pool, including moving `/var/lib/incus/images` because it is used for transient image unpacking, is an operator-owned infrastructure migration and is documented separately in [the CT root/data split solution](../../../docs/solutions/agent-vm/ct-root-data-split.md).
