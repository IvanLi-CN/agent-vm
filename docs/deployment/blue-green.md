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

The helper passes the DNSPod module and its repository-pinned commit to `xcaddy`, checks the resulting Go build metadata for Caddy v2 and the DNSPod module, and rejects non-v2 or implicit `latest` inputs. The operator must record and verify the custom binary checksum before placing it at `/usr/local/bin/caddy`.

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
  slots/{blue,green}/bin/agent-vm-control
  slots/{blue,green}/release-manifest.json
/var/lib/agent-vm-native/
  active -> slots/{blue,green}
  slots/{blue,green}/app.sock -> /run/agent-vm-native/{blue,green}/app.sock
  caddy/
  rqlite/
/run/agent-vm-native/{blue,green}/app.sock
```

`agent-vm-native@.service` starts one Go service per slot and binds that slot's manifest into the fixed manifest path required by the Go binary. The service runs with the existing Incus control boundary and the `agent-vm-web` socket group. `agent-vm-caddy.service` is the only edge proxy unit. The retired `agent-vm-web.service` is not part of the blue-green path.

Run `systemd-sysusers` and `systemd-tmpfiles --create` with the supplied `sysusers.d` and `tmpfiles.d` entries before starting units. These entries create both the dedicated rqlite user and the unprivileged `caddy` user. The `active` path is intentionally not created by tmpfiles because it must be an atomic symlink, not a directory.

## Caddy TLS and rqlite CA

Create a deployment-local CA for the loopback-only rqlite HTTPS service in an empty operator-owned directory:

```sh
bash skills/agent-vm-ops/scripts/generate_deployment_ca.sh \
  --output-dir /var/lib/agent-vm-native/bootstrap-ca \
  --hostname <operator-provided-hostname>
```

Install `ca.crt` and the rqlite leaf pair into the paths named by the units. Keep the CA private key and rqlite private key outside the repository, assign the rqlite key to `agent-vm-rqlite`, and use mode `0600`. Install the CA into the host trust store only when the Go control service must validate the rqlite HTTPS endpoint.

Caddy is the certificate owner. Its custom v2 binary includes `github.com/caddy-dns/dnspod`, and the Caddyfile enables `tls { dns dnspod {env.DNSPOD_TOKEN} }` so Caddy performs ACME DNS-01 through Tencent DNSPod. Put `AGENT_VM_DOMAIN` and the operator-managed `DNSPOD_TOKEN` in the protected `/etc/agent-vm-native/caddy.env`; neither value belongs in this repository. Caddy persists certificates, keys and ACME state in `/var/lib/agent-vm-native/caddy`, and the service account owns that directory. Do not install a static Caddy leaf or an independent ACME client.

The Caddy example disables the admin API and HTTP-to-HTTPS redirects, removes query values and sensitive headers from logs, blocks `/internal/v1/*` at the edge, and forwards public requests to the active Unix socket. It does not add an identity header or disable upstream TLS verification.

## rqlite boundary

Copy `rqlite-auth.json.example` to the protected `/etc/agent-vm-native/rqlite/auth.json` path and replace both placeholders with operator-managed secrets. The control user is limited to the SQL permissions used by the state adapter. The backup user cannot execute SQL. The rqlite unit binds HTTP and raft to loopback and enables HTTPS plus HTTP authentication.

The state URL in each slot configuration may use the operator-managed HTTPS URL and credentials after the client transport has been configured to trust the deployment CA. Do not put that URL, credentials, or CA private key in a release manifest or source file.

## Cold cutover

Start and validate the candidate slot before changing `active`:

```sh
sudo systemctl start agent-vm-native@green.service
sudo bash skills/agent-vm-ops/scripts/agent_vm_cutover.sh --slot green
```

The cutover command must run as root because the internal health endpoint accepts only the protected peer identity. It verifies the candidate manifest, exact socket link, and `{"ready":true}` health response, then atomically replaces `/var/lib/agent-vm-native/active`. Caddy has a startup condition and `ExecStartPre` check for that active Unix socket, so it fails closed when no slot is serving. Caddy resolves the active socket on subsequent requests; it does not need an external proxy or a second router.

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

The check builds the operator-selected amd64 Caddy v2 binary with the DNSPod module, verifies the pinned arm64 and amd64 rqlite checksums, and does not contact DNSPod or request a public certificate. It validates the Caddyfile with a placeholder token, uses a temporary rqlite certificate only for the local smoke test, verifies all three systemd units, starts a loopback-only authenticated TLS rqlite node, exercises the cutover guard, and proves both signed migration-fence rejection and restore admissibility. Set `CADDY_VERSION` to an explicit v2 release before running it. The temporary database, credentials, certificates, logs, signing key, public key, and evidence are removed at exit unless `KEEP_PROOF_DIR=1` is explicitly set inside the isolated VM.
