# Agent VM Control-Plane Release Contract

This contract describes the manual release boundary for the Agent VM client and control environment. It is not a public `agent-vm` lifecycle command and it does not grant access to PVE, a host manager, an Incus policy API, a guest, or an HTTP gateway. Product release completion is the verified GitHub Release and its versioned installation packages; deployment is a separate explicit operation.

## Product formats 3 and 4

The current production package uses `release-manifest.json` format 3 and one product SemVer for the client, Go control service, compatibility bridge, and systemd installation assets. A management-enabled package uses format 4 and carries the same `version`, exact 40-character Git `source_revision`, target, fixed Bun/Go toolchain, protocol 4, action compatibility windows and per-file byte/mode digests, plus the state-compatibility declaration below. Its complete asset set includes the embedded management UI and local fonts. `client-release.json` uses the package format and carries the same `version` and `source_revision` in its client view. A package is accepted only when the builder proves that the checked-out commit equals the declared `source_revision`; an arbitrary SHA string is not provenance.

`release_id` is not a second product version. It is the legacy protocol-4 peer capability field and remains a Git SHA for old readers. Format 3 and format 4 readers derive that compatibility identity from `source_revision` at the protocol adapter boundary. Formats 1, 2 and 3 remain read-only migration and necessary rollback inputs after format 4 becomes available; management-enabled production builders write format 4.

The client prints the package SemVer when a valid format-3 or format-4 sidecar is installed and prints `dev` for development or legacy installations without a product version. The Go control response includes `version` for format-3 and format-4 product manifests while retaining the SHA-derived `release_id` and `source_revision` compatibility fields.

## Release manifest

The Python release tools use [`release_manifest.py`](../../../../skills/agent-vm-ops/scripts/release_manifest.py) for format-1/2 migration validation and format-3 product validation, while the Bun controller uses [`release-manifest.ts`](../../../../skills/agent-vm-ops/scripts/release-manifest.ts) to validate its runtime manifest and artifact bytes. The format-2 Go release contract remains a read-only compatibility path; format 3 is implemented by the shared product builder and is consumed by Bun, Python and Go. The Bun and Go test suites cover isolated package and transaction behavior; native Linux/Incus evidence remains conditional on a deployment-boundary change and is not part of the current governance candidate.

Before a format-4 package is published, the Bun, Python and Go readers, client sidecar readers, builder, verifier, installer and rollback wrapper must all validate format 4. A format-4 package retains protocol 4; manifest-format compatibility and client/control protocol compatibility are separate contracts.

Each artifact declares only a relative path, role, exact SHA-256 of its bytes, and numeric mode. The artifact path must resolve to a regular file inside the release root; absolute paths, parent traversal, symlinks, non-regular files, manifest self-checksums, and mode or checksum mismatches fail closed. The Bun client package requires `agent_vm.ts` and `sync_worktree.ts` at mode `0755`, and `byte-fs.ts`, `client-io.ts`, `client-release.ts`, `client.ts`, `guest-source.ts` and `sync-native.ts` at mode `0644`. A complete release must carry that package alongside `agent_vm_control.ts`, `release_manifest.py`, the controller TypeScript support modules including `guest-root-growth.ts`, and the fixed systemd assets. It excludes configuration, credentials, SSH material, state databases and WAL/journals, locks, Incus container data, and guest images. Restoring a Python client alongside the Bun entrypoint is not a compatibility strategy.

Each action declares `request.schema`, `request.semantic_revision`, `response.schema`, `response.semantic_revision`, and explicit `client_to_control` and `control_to_client` objects with `schema_min`, `schema_max`, `semantic_min`, and `semantic_max`. JSON parseability is not compatibility evidence. The control process checks protocol, action, request window, response window, and all artifact bytes before a journal record, native handler, or timer preemption.

## Management-state compatibility

The SQLite `state_capabilities` registry stores versioned durable-state markers. The capability set is returned by the management System API as sorted `name@version` strings. A database without a capability table is treated as `legacy-agent-vm-core@1` only after its existing protocol-4 schema validates; unknown or malformed state fails closed.

The first management-enabled release uses these markers:

| Capability | Meaning |
| --- | --- |
| `legacy-agent-vm-core@1` | Existing protocol-4 tasks, generations, leases, reservations, ports and operation journal. |
| `managed-image-catalog@1` | Immutable image records, labels, verification, references and default selection. |
| `session-image-binding@1` | Session and cache identity remains bound to the exact image fingerprint. |
| `management-operation-audit@1` | Durable administrator operation identity, actor, reason, target, phase and outcome. |

The format-4 manifest declares:

```json
"state_compatibility": {
  "reads": [
    "legacy-agent-vm-core@1",
    "managed-image-catalog@1",
    "management-operation-audit@1",
    "session-image-binding@1"
  ],
  "writes": [
    "legacy-agent-vm-core@1",
    "managed-image-catalog@1",
    "management-operation-audit@1",
    "session-image-binding@1"
  ]
}
```

Both arrays are sorted, duplicate-free and list only exact supported markers. Activation requires the candidate to declare both read and write support for every marker present in the database. Formats 1, 2 and 3 have no declaration and imply support for `legacy-agent-vm-core@1` only; they are incompatible with a migrated management database.

Installer preflight checks the candidate package before changing active pointers or starting its services. Rollback preflight performs the same comparison against the rollback target. A compatible rollback restores the complete earlier product package and keeps the same database. An incompatible target is rejected before activation or service start; the current package remains selected and admission remains drained for operator recovery. The installer never drops capability markers, downgrades the schema, recreates the database, or starts an older process against state it cannot read and write. A post-migration health failure does not bypass this check.

The client reads only its local sidecar client view. Legacy format 1/2 sidecars have the exact top-level fields `format`, `release_id`, `source_revision`, `protocol`, `client_artifact`, `client_dependencies` and `client_view`; format 3 replaces the product identity portion with `format`, `version`, `source_revision`, `protocol`, `client_artifact`, `client_dependencies` and `client_view`. `client_artifact` identifies `agent_vm.ts` with role `client`; `client_dependencies` lists the other seven fixed client files with role `support`. Each entry has exactly `name`, `path`, `role`, `sha256` and `mode`, with basename-only paths beside the sidecar. Missing, duplicate, extra or changed dependencies fail before SSH. The view contains the complete fixed action set and explicit request/response compatibility windows. A missing sidecar selects the explicit `legacy-protocol-4` capability and returns a structured warning; it does not silently claim a current release. A present but malformed, path-invalid, mode-invalid, or checksum-invalid sidecar fails closed, including an old Python-only sidecar. A compatible action across different revisions succeeds with a top-level warning, and write-operation replay preserves that warning.

## Wrapper boundary

Format 4 client sidecars use the same exact product identity and client-view fields as format 3 with `format: 4`. They do not carry the server database's `state_compatibility` declaration; that declaration belongs only to the complete product release manifest and is enforced by the installer.

The operator pre-installs a root-owned narrow wrapper at the fixed path `/usr/local/sbin/agent-vm-release-wrapper` with a protected deployment configuration. Format 1 uses `/opt/agent-vm-native/agent_vm_control.ts` with `/usr/local/bin/bun`; format 2 installs `/opt/agent-vm-native/bin/agent-vm-control`, the fixed bridge and `agent-vm-native.service`. The systemd target directory remains fixed at `/etc/systemd/system`, and the activation fence remains fixed at `/run/agent-vm-native/release-activation.fence`; the wrapper configuration cannot redirect these targets. Its fixed actions are `stage`, `verify`, `activate`, `rollback`, and `status`. Callers provide only a transaction/release identity and the structured action; the wrapper does not accept arbitrary destinations, shell commands, configuration paths, state database paths, or VM paths.

`stage` reads a private bundle from standard input and extracts regular files only into transaction staging. It rejects absolute or parent-traversing names, symlinks, hard links, devices, duplicates, and oversized members. `verify` checks the selected format, exact bytes, regular-file owner and mode, validates the Go target/toolchain and retired-unit set without executing candidate binaries, compiles Python support artifacts, transpiles manifest-listed TypeScript as data with the fixed `/usr/local/bin/bun`, verifies fixed systemd unit names and canonical bytes, and enforces the selected fixed artifact set. `activate` requires a verified transaction and admission drain, creates the fixed activation fence before the drain check, re-verifies retained current and previous releases, atomically retains the current and previous release pointers, installs the complete fixed client package and selected control artifacts, and may run `systemctl daemon-reload`; it does not stop or disable the Go service's in-process scheduler. `rollback` verifies the previous retained release with the same artifact and systemd checks, rejects stale transactions, permits one automatic attempt per transaction including an interrupted activation record, and never rewrites durable lifecycle state, generations, or uncertain operations.

## Publisher transaction

The manual publisher in [`agent_vm_release.py`](../../../../skills/agent-vm-ops/scripts/agent_vm_release.py) remains the format-1 publisher. The format-2 bundle builder is [`build_go_release.ts`](../../../../skills/agent-vm-ops/scripts/build_go_release.ts); it builds the binary for `linux/amd64` or `linux/arm64` with `CGO_ENABLED=0`, verifies bytes and metadata without executing a candidate, and publishes the fixed bridge and `agent-vm-native.service` together. Publication must preserve local verification, remote staging/verification, drain through the selected client transport, and `admin-metrics` polling at most 1 second apart. The format-2 migration drain budget defaults to 8100 seconds and is bounded to 8100 seconds; activation is separately bounded to 300 seconds. It must not stop or disable the Go service scheduler, kill in-flight operations, or invent a replacement operation ID. The fixed remote wrapper remains `/usr/local/sbin/agent-vm-release-wrapper`.

After convergence, publication must activate the verified remote release and check `doctor`, `admin-metrics`, pending operations, task transitions, and timer/reconcile health. A pre-activation failure keeps the current release active and drained. A post-activation health failure permits one rollback attempt; a successful rollback is revalidated before drain exit, while a failed rollback keeps admission drained. Only after remote health succeeds may the publisher install and self-check the local client. Local installation takes an exclusive package lock, stages all eight client files and the sidecar, and runs `bun --no-macros <staged-package>/agent_vm.ts version` with a sanitized environment and a 30-second budget. Missing Bun or an unverified sidecar fails before active files change. Each file uses an exclusive temporary creation, fsync and atomic rename; the sidecar is installed last. A private format-2 recovery record binds every previous and target file's hash/mode, fixed destination and precise staging path before mutation. Clients refuse requests while this record exists, including when the sidecar is temporarily absent. The installer verifies the installed package before durably marking completion; recovery restores all previous files for an incomplete transaction or retains all verified target files for a completed transaction. It validates every destination, backup and staging entry before any recovery mutation and can itself resume after interruption. Unknown paths, unverified backups, partial/legacy recovery records and old Python-only installations require operator inspection and fail closed. Per-file atomic renames and the recovery fence provide a complete-package transaction; they do not claim a simultaneous directory exchange. A local client failure must preserve the old client, report a structured warning, avoid rolling back the healthy remote release, and keep drain closed. Manual rollback must verify the supplied previous bundle, roll back the remote transaction, install that same complete local client package, and exit drain only after both sides are healthy. Complete publication exits drain last.

The activation timeout is 300 seconds, the default poll interval is 1 second, and the migration drain budget defaults to 8100 seconds. Operators may lower those values but cannot exceed the activation, drain or poll safety bounds. Old releases are retained; deletion, unattended updates and deployment inventory management are outside this contract. The format-2 scheduler performs lease expiry, bounded retry and age-GC itself; external expiry units are retired by the manifest.

## Public and private records

Public manifests, tests, and documents contain only relative source paths and neutral deployment placeholders. The live SSH alias, identity, credentials, release root, wrapper configuration, target paths, state database, timer inventory, and controlled staging observations remain operator-owned records outside this repository.
