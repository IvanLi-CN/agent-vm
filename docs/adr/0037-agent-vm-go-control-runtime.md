# ADR 0037: Go Resident Control Runtime

## Status

Accepted for the Agent VM control-plane migration.

## Context

The existing Agent VM control path is a Bun process started once per request. It owns SQLite state, Incus lifecycle actions, maintenance scheduling and observation sampling in the same execution model. A resident parent service must be introduced without creating a second writer, changing operation identity, or making expiry depend on an external trigger.

The migration also needs to preserve the current SSH client contract while allowing SSH to be removed later. A transport adapter must therefore be replaceable without moving protocol validation, lifecycle policy, journal handling or cleanup into the client.

## Decision

The control environment uses a Go 1.27.3 binary as the single resident control authority. The binary provides `serve`, `request` and `native-worker` subcommands. `serve` owns the SQLite write connection, lifecycle state machine, admission, native-worker supervision, observation snapshots and in-process expiry/cache scheduler. The `request` subcommand is a bounded one-shot HTTP client for the service; it does not open SQLite or call Incus directly.

Gin v1 handles the fixed internal HTTP routes. The service listens on a protected Unix domain socket and verifies peer credentials. SQLite remains the existing database and is accessed through `database/sql` with `modernc.org/sqlite`; SQL is explicit and no ORM is introduced. Linux-specific locking, peer credentials, subreaper and pidfd handling use `x/sys/unix`; native commands use `os/exec` and structured logs use `slog`.

The first release keeps SSH as a compatibility transport. The local Bun client selects `legacy-bun` for a format-1 sidecar or `go-request` for a format-2 sidecar. In the latter case SSH starts the fixed Go `request` adapter, which sends the original protocol-4 body over HTTP on the UDS. The adapter holds no control state and the service owns the only live authority. A future HTTPS or directly managed UDS client may replace this adapter without changing the protocol-4 body, operation identity, SQLite schema, lifecycle service or scheduler.

Format 2 declares the target OS/architecture, Go toolchain, immutable artifact checksums, the fixed client transport enum and the old systemd units retired by the resident scheduler. Format 1 remains valid for the legacy Bun release during the support window.

## Consequences

The service must start under systemd before admitting new work and must reconcile overdue leases, pending operations and resident workers before reporting ready. Expiry, retry and age-GC continue while the HTTP or SSH request path is unavailable. The old Bun controller remains only as a verified compatibility release; it must not run concurrently with the Go service against the live database.

The migration has a larger native-process and deployment test surface. Linux process-tree, systemd, SQLite recovery and isolated Incus evidence are required before declaring the cutover merge-ready. A failed activation permits one rollback to the immutable format-1 bridge bundle or previous verified format-2 release without down-migrating durable state.

## Rejected alternatives

- Making the Bun client a second lifecycle authority would preserve transport shape but duplicate state and cleanup decisions.
- Using an ORM would hide transaction and numeric identity details that are part of the existing journal contract.
- Introducing the Incus Go SDK in the same change would expand the native control boundary and make rollback depend on two independent integrations.
- Requiring SSH permanently would make a future direct HTTPS/UDS transport a business-logic migration instead of an adapter replacement.
