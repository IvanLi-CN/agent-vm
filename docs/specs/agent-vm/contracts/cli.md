# Agent VM CLI Contract

The installed executable is `agent-vm`, a Python 3 command line client. The client and one-shot control process must both speak protocol 4.

## Identity and configuration

Global `--session-id <id>` selects the ownership identity and is accepted before or after a command, including nested commands. If omitted, `AGENT_VM_SESSION_ID` is used. Explicit input wins. Commands that can reach the control host fail with JSON error code `missing_session` before a remote write when neither is present. `version` and local help do not require an identity.

The control endpoint is selected from `AGENT_VM_CONTROL_HOST` and `AGENT_VM_REMOTE_CONTROL`, with deployment-safe defaults only for the executable name and remote path. The operations control process reads a JSON file whose environment-specific fields are required: project, profile, image, network, ACL, pool, control address, port range, guest address range, storage path, state database and timer directory. Missing or invalid values fail closed with `config_missing` or `config_invalid`.

## Commands

| Command | Contract |
| --- | --- |
| `version` | Print local protocol and client version without contacting the control host. |
| `doctor` | Read-only component and admission diagnostics. |
| `status` | Read-only state for the selected session. |
| `acquire [--cpu N] [--memory SIZE] [--disk SIZE]` | Create or reuse the selected session VM. |
| `renew` | Extend the current lease within the configured maximum. |
| `release` | Revoke access, stop the guest and retain an eligible cache. |
| `resize [--cpu N] [--memory SIZE] [--disk SIZE] [--live] [--allow-restart]` | Apply an explicitly authorized resource change. |
| `ssh [-- SSH_ARGS]` | Open native SSH with generated strict connection settings. |
| `exec -- ARGV` | Run an argument-vector command; use `sh -lc` explicitly for shell syntax. |
| `sync --name NAME` | Transfer the current worktree through the bundled synchronization helper. |
| `upload SOURCE DESTINATION` / `download SOURCE DESTINATION` | Transfer files online without renewing. |
| `export PATH` | Stream a tar archive on stdout; diagnostics go to stderr. |
| `port add NAME --guest-port N [--protocol tcp\|udp] [--host-port N]` | Create an exact native proxy mapping. |
| `port list` / `port remove NAME` | Inspect or remove an exact mapping. |
| `admin status` | Read-only native component and maintenance status. |
| `admin drain enter\|status\|exit` | Control admission while allowing safe retrieval and release. |
| `admin session list` | List managed session records. |
| `admin reconcile [--apply]` / `admin gc [--apply]` | Preview by default; apply only inside an authorized operations task. |

Unknown flags and command layouts must be rejected; callers should use `--help` for the installed build.

## Request and response

Requests contain `protocol`, `action`, `session_id`, and mutation fields `generation` and `operation_id`. Success envelopes contain `ok`, `protocol`, `server_time`, `action`, `session_id`, `generation`, `operation_id`, `outcome` and `result`. Error diagnostics are JSON on stderr with a nonzero exit status. A transport timeout reports the operation ID without claiming whether the mutation completed.

Acquire, restart and restart-capable resize use a longer bounded transport timeout. No command implicitly renews a lease. Retry an uncertain mutation only with the original operation ID and generation after inspecting status or reconciliation.

## SSH and ports

Generated SSH configuration uses the configured control address, leased port, explicit identity and known-host files, a generation-bound host-key alias, `StrictHostKeyChecking yes` and `IdentitiesOnly yes`. It must not disable host checking or introduce a gateway. Port allocation considers listeners, native mappings, forwards, reservations and the configured ephemeral range. Lease cleanup removes mappings and their exact conntrack flows before the guest becomes reusable.

## Capacity and cleanup

Capacity responses identify their local filesystem scope, observed sources, allocation budgets, requested values and pressure threshold. Missing or contradictory measurements refuse new demand without stopping an effective lease. Manual cleanup is dry-run by default and rechecks ownership, generation and eligibility under object locks before deletion.

## Reclamation and retry behavior

Reconciliation and garbage collection select only records that are expired, released or otherwise eligible under the configured policy. They re-read the record while holding the object lock, verify the session and generation, and remove only resources owned by that record. A failed stop, detach, proxy cleanup or metadata update leaves a retryable record and reports the failed phase; it does not silently mark the resource reusable. The same operation ID is safe to retry after a transport timeout, while a new operation ID is required for a new intent.

Acquire and restart are bounded operations that may return an uncertain outcome after transport failure. Callers inspect `status` or reconciliation and retry with the original operation ID and generation. Renewal never occurs implicitly as a side effect of `ssh`, `exec`, `sync`, upload, download or export.

## Resource and live-memory rules

CPU, memory and disk requests are parsed into canonical units before admission. Admission accounts for configured budgets, overcommit ratios, observed host pressure and all effective reservations. A request is rejected when measurements are missing or contradictory, or when the requested value exceeds the configured policy; an existing effective lease remains available for retrieval and release.

`resize --live` is limited to a supported memory-only change. CPU, disk and unsupported live changes fail without falling back to a restart. `--allow-restart` is required for restart-capable changes and uses the longer bounded timeout. A live resize records the requested and observed values in the response so callers can distinguish a pending reconciliation from a completed change.

## Output and admission

Human-readable status is for operators; automation consumes the JSON envelope and checks `protocol`, `action`, `session_id`, `generation`, `operation_id`, `outcome` and `result`. Diagnostics are written to stderr and never mixed into an archive stream from `export`. Admission drain blocks new acquisition and restart work while permitting status, retrieval and release, and exits only after the control process has durably recorded the new admission state.
