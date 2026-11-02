# Agent VM blue-green deployment resources

These files define the deployment boundary for the native Go control service. They are installation resources and isolated acceptance checks; they do not perform a production deployment, contain an inventory, or store credentials. The parent service integration must still provide the real migration fence before SQLite state can be imported into rqlite.

## Component policy

The authoritative, machine-readable metadata is [`deployment-components.json`](../../skills/agent-vm-ops/assets/blue-green/deployment-components.json). Caddy is built from an operator-selected official Caddy v2 release with the pinned official DNSPod provider source. The repository does not select or pin a Caddy patch release, and the operator records the checksum of the resulting custom binary before installation.

| Component | Version | Runtime role | Supported artifact |
| --- | --- | --- | --- |
| Caddy | major 2 | HTTPS edge proxy | `xcaddy` custom build with `dns.providers.dnspod`, operator-recorded SHA-512 |
| rqlite | 10.5.2 | Single-node state authority | Linux amd64 and arm64 tarballs, SHA-256 verified |

Automatic installation is disabled. Install `xcaddy`, choose an explicit `v2.x.y` release, and build both supported Linux targets with the repository helper:

```sh
bun skills/agent-vm-ops/scripts/build_caddy.ts \
  --caddy-version <v2.x.y> \
  --target linux/amd64 \
  --output /tmp/caddy
```

The helper passes the DNSPod module and its repository-pinned commit to `xcaddy`, checks the resulting Go build metadata for the requested Caddy v2 release and provider commit, emits the custom binary SHA-512 plus Go VCS metadata, and rejects non-v2 or implicit `latest` inputs. When an operator has a pre-recorded digest, pass it as `--expected-sha512 <128-lowercase-hex>` to make the build fail closed on mismatch. The isolated acceptance script accepts the same value through `CADDY_SHA512`. The operator must record and verify the custom binary checksum before placing it at `/usr/local/bin/caddy`.

## Resource layout

The systemd units in `skills/agent-vm-ops/assets/systemd/` use this layout:

```text
/etc/agent-vm-native/
  Caddyfile
  caddy.env
  rqlite/auth.json
  tls/rqlite/{server.crt,server.key}
  slots/{blue,green}/config.json
/opt/agent-vm-native/
  release-manifest.json
  slots/{blue,green}/                # complete verified release package
    bin/agent-vm-control
    release-manifest.json
/var/lib/agent-vm-native/
  active -> slots/{blue,green}
  .active.cutover.pending
  slots/{blue,green}/app.sock -> /run/agent-vm-native/{blue,green}/app.sock
  caddy/
  rqlite/
/run/agent-vm-native/{blue,green}/app.sock
```

`agent-vm-native@.service` starts one Go service per slot, passes the explicit `%i` slot identity, uses that slot's complete release package and manifest, and overrides its control socket to `/run/agent-vm-native/%i/app.sock`. The Go service accepts a socket override only when it matches that blue/green slot's fixed runtime path and all existing parent directories are non-symlinked directories. The service runs with the existing Incus control boundary and the `agent-vm-web` socket group. `agent-vm-caddy.service` is the only edge proxy unit. The retired `agent-vm-web.service` is not part of the blue-green path.

Run `systemd-sysusers` and `systemd-tmpfiles --create` with the supplied `sysusers.d` and `tmpfiles.d` entries before starting units. These entries create both the dedicated rqlite user and the unprivileged `caddy` user. The `active` path is intentionally not created by tmpfiles because it must be an atomic symlink, not a directory.

## Caddy TLS and rqlite CA

Create a deployment-local CA for the loopback-only rqlite HTTPS service in an empty operator-owned directory:

```sh
bash skills/agent-vm-ops/scripts/generate_deployment_ca.sh \
  --output-dir /var/lib/agent-vm-native/bootstrap-ca \
  --hostname <operator-provided-hostname>
```

Install `ca.crt` and the rqlite leaf pair into the paths named by the units. Keep the CA private key and rqlite private key outside the repository, assign the rqlite key to `agent-vm-rqlite`, and use mode `0600`. Install the CA into the host trust store only when the Go control service must validate the rqlite HTTPS endpoint.

Caddy is the certificate owner. Its custom v2 binary includes `github.com/caddy-dns/dnspod`, and the Caddyfile enables `tls force_automate { dns dnspod {env.DNSPOD_TOKEN} }` so Caddy performs ACME DNS-01 through Tencent DNSPod without creating an HTTP listener. Put `AGENT_VM_DOMAIN` and the operator-managed `DNSPOD_TOKEN` in `/etc/agent-vm-native/caddy.env` with root-only permissions:

```sh
install -o root -g root -m 0600 /path/to/caddy.env.example /etc/agent-vm-native/caddy.env
```

Replace the placeholders using a root-only editor and preserve `root:root` ownership and mode `0600`; neither value belongs in this repository. Caddy persists certificates, keys and ACME state in `/var/lib/agent-vm-native/caddy`, and the service account owns that directory. Do not install a static Caddy leaf or an independent ACME client.

The Caddy example disables the admin API and all automatic HTTP listeners, removes query values and sensitive headers from logs, blocks `/internal/v1/*` at the edge, forwards only `/`, `/index.html`, `/assets/*`, `/api/v1`, `/api/v1/*` and `/agent/v1/control` to the active Unix socket, and returns `404` for every other path. It does not add an identity header or disable upstream TLS verification.

## rqlite boundary

Copy `rqlite-auth.json.example` to the protected `/etc/agent-vm-native/rqlite/auth.json` path with `install -o agent-vm-rqlite -g agent-vm-rqlite -m 0600`, then replace both placeholders with operator-managed secrets. The rqlite unit checks that the file is owned by `agent-vm-rqlite` and remains mode `0600` before startup. The control user is limited to the SQL permissions used by the state adapter. The backup user cannot execute SQL. The rqlite unit binds HTTP and raft to loopback and enables HTTPS plus HTTP authentication.

The state URL in each slot configuration may use the operator-managed HTTPS URL and credentials after the client transport has been configured to trust the deployment CA. Do not put that URL, credentials, or CA private key in a release manifest or source file.

## Cold cutover

Start and validate the candidate slot before changing `active`:

```sh
sudo systemctl start agent-vm-native@green.service
sudo bash skills/agent-vm-ops/scripts/agent_vm_cutover.sh --slot green
```

The cutover command must run as root because the internal health endpoint accepts only the protected peer identity. It first holds a kernel lock on the verified release-root directory while creating and opening the regular `/var/lib/agent-vm-native/.cutover.lock` file, then takes that file lock with `flock`; candidate slot publication and every writer of the protected `active` link must use this same locking sequence. The locks cover the current-slot read, candidate validation, health checks, active-link consistency check, atomic replacement and rollback, and are released automatically if the process is killed. Cutover revalidates the candidate after acquiring the locks, verifies its manifest and exact socket link, and requires a `{"ready":true}` health response before writing a mode-0600 `.active.cutover.pending` record containing both slot names and their manifest SHA-256 identities. It then atomically replaces `/var/lib/agent-vm-native/active` and checks the new active link and candidate manifest identity again before clearing recovery state. A later invocation consumes that record: if a process died after the swap, it verifies that both slot manifests still match the recorded identities, health-checks the active candidate, and restores the previous verified link before proceeding; if the swap never happened, it only clears the record after revalidating the previous slot's manifest and health. If the candidate fails its post-swap check or manifest identity changes, the command revalidates the previous slot's layout and the manifest identity recorded before the swap before atomically restoring that link, or removes the new link when there was no previous active slot, and exits unsuccessfully. Caddy first checks the active Unix socket and then runs a root-executed readiness probe against `/internal/v1/health`, waiting briefly for a listener that is actually ready instead of trusting a stale socket inode. Caddy resolves the active socket on subsequent requests; it does not need an external proxy or a second router.

Use `--dry-run` to validate a candidate layout without changing the symlink. A rollback is the same health-gated operation against the previous slot, and is allowed only when the previous release is compatible with the current state protocol. No retrying write request is introduced by the proxy or cutover helper.

## State migration and restore

Migration is a one-way, evidence-gated operation:

1. The service coordinator records a signed canonical evidence payload with `state_write_fenced`, `workers_quiescent`, and `sqlite_writers_closed`, plus the exact source hash/size, state epoch/generation, trusted commit, fence identity, and a short validity window. The private signing key remains with the service coordinator; only its operator-installed public key is accepted by this helper.
2. The operator runs `agent_vm_state_migration.ts preflight` against a regular SQLite file outside the repository. The command performs an integrity check, records schema/table counts, rejects unresolved management operations, and writes a mode-0600 snapshot and manifest.
3. The Go state service owns the eventual rqlite import and must verify schema, `state_epoch`, generation monotonicity, valid VM rows, and the operation journal before accepting the new authority.
4. Until that coordinator and deployment fence exist, the exported Go migration entry remains fail-closed. Do not use a shell, Python, rqlite CLI, or direct HTTP request to import rows.

Example preflight with operator-supplied evidence:

```sh
bun skills/agent-vm-ops/scripts/agent_vm_state_migration.ts preflight \
  --source /var/lib/agent-vm-native/state.sqlite3 \
  --evidence /var/lib/agent-vm-native/migration/fence-evidence.json \
  --evidence-public-key /etc/agent-vm-native/migration/fence.pub \
  --output-dir /var/lib/agent-vm-native/migration/snapshot
```

The restore check accepts a snapshot only when the backup hash matches the manifest and a separately signed tail-evidence file, the backup proves closed SQLite writers, and the tail proves an exact trusted-commit, state-epoch/generation, fence, and coverage-boundary match. It emits an authorization record but never writes rqlite. If the tail cannot be verified, leave the service fenced and reconcile from the rqlite authority or an operator-approved recovery procedure.

## Isolated acceptance

Run the following in the session-owned Agent VM, not on the developer host:

```sh
bash skills/agent-vm-ops/scripts/agent_vm_deployment_acceptance.sh
```

The same acceptance script is a required Ubuntu PR-gate step with a CI-resolved stable Caddy v2 input and a pinned `xcaddy` builder. Run it in the session-owned Agent VM as well because the CI job does not replace the repository's isolated-environment evidence requirement. The check builds the operator-selected amd64 Caddy v2 binary with the DNSPod module, verifies the requested Caddy release and pinned provider ref from Go build metadata, emits the custom binary SHA-512, verifies the pinned arm64 and amd64 rqlite checksums, and does not contact DNSPod or request a public certificate. It validates the Caddyfile with a placeholder token, uses a temporary rqlite certificate only for the local smoke test, verifies all three systemd units, explicitly executes the staged rqlite-auth and Caddy-environment `ExecStartPre` metadata checks that `systemd-analyze --root verify` does not run, starts a loopback-only authenticated TLS rqlite node, exercises actual cutover, post-swap rollback, and pending recovery, and proves both signed migration-fence rejection and restore admissibility. Set `CADDY_VERSION` to an explicit v2 release before running it. The temporary database, credentials, certificates, logs, signing key, public key, and evidence are removed at exit unless `KEEP_PROOF_DIR=1` is explicitly set inside the isolated VM; the temporary rqlite auth file and curl credential config are removed even when proof retention is enabled.
