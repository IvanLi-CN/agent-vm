#!/usr/bin/env bun

import { readFileSync, statSync } from "node:fs";
import { join, resolve } from "node:path";

const repositoryRoot = resolve(import.meta.dir, "../../..");
const assetsRoot = join(repositoryRoot, "skills/agent-vm-ops/assets");
const blueGreenRoot = join(assetsRoot, "blue-green");
const errors: string[] = [];
const checks: string[] = [];

function read(path: string): string {
  try {
    return readFileSync(path, "utf8");
  } catch (error) {
    errors.push(`cannot read ${path}: ${error instanceof Error ? error.message : String(error)}`);
    return "";
  }
}

function requireCheck(condition: boolean, message: string): void {
  if (condition) checks.push(message);
  else errors.push(message);
}

function requireText(text: string, needle: string, message = `contains ${needle}`): void {
  requireCheck(text.includes(needle), message);
}

function forbidText(text: string, needle: string, message = `does not contain ${needle}`): void {
  requireCheck(!text.includes(needle), message);
}

const metadataPath = join(blueGreenRoot, "deployment-components.json");
const metadataText = read(metadataPath);
let metadata: any = null;
try {
  metadata = JSON.parse(metadataText);
} catch (error) {
  errors.push(`deployment metadata is invalid JSON: ${error instanceof Error ? error.message : String(error)}`);
}

if (metadata) {
  requireCheck(metadata.schema_version === 2, "deployment metadata schema_version is 2");
  requireCheck(metadata.policy?.automatic_install === false, "deployment metadata disables automatic installation");
  requireCheck(metadata.policy?.verification === "component-specific-checksum", "deployment metadata uses component-specific verification");
  requireCheck(metadata.caddy?.version_major === 2, "Caddy is constrained to major version 2");
  requireCheck(metadata.caddy?.version_policy === "operator-selected-v2-release", "Caddy patch selection remains an explicit deployment input");
  requireCheck(metadata.caddy?.builder === "xcaddy", "Caddy is built with xcaddy");
  requireCheck(metadata.caddy?.provider_module === "github.com/caddy-dns/dnspod", "Caddy includes the official DNSPod provider module");
  requireCheck(metadata.caddy?.provider_module_name === "dns.providers.dnspod", "Caddy exposes the DNSPod provider module name");
  requireCheck(typeof metadata.caddy?.provider_ref === "string" && /^[0-9a-f]{40}$/.test(metadata.caddy.provider_ref), "DNSPod provider source is pinned to an official commit");
  requireCheck(metadata.caddy?.artifact_verification === "operator-recorded-sha512", "Caddy custom-build checksums remain operator-recorded");
  requireCheck(!Object.hasOwn(metadata.caddy ?? {}, "version"), "Caddy metadata does not pin a patch version");
  requireCheck(!Object.hasOwn(metadata.caddy ?? {}, "artifacts"), "Caddy is not distributed as an unextended release artifact");
  requireCheck(metadata.rqlite?.version === "10.5.2", "rqlite is pinned to 10.5.2");
  requireCheck(JSON.stringify(metadata.rqlite?.supported_architectures) === JSON.stringify(["amd64", "arm64"]), "rqlite supports the product Linux architectures");
  const rqliteArtifacts = Array.isArray(metadata.rqlite?.artifacts) ? metadata.rqlite.artifacts : [];
  for (const artifact of rqliteArtifacts) {
    requireCheck(typeof artifact.url === "string" && artifact.url.startsWith("https://github.com/"), `official source for ${artifact.filename}`);
  }
  for (const artifact of rqliteArtifacts) {
    requireCheck(typeof artifact.sha256 === "string" && /^[0-9a-f]{64}$/.test(artifact.sha256), `SHA-256 is pinned for ${artifact.filename}`);
  }
}

const releaseContractText = read(join(repositoryRoot, ".github/release-contract.json"));
let releaseContract: any = null;
try {
  releaseContract = JSON.parse(releaseContractText);
} catch (error) {
  errors.push(`release contract is invalid JSON: ${error instanceof Error ? error.message : String(error)}`);
}
const releaseCaddy = releaseContract?.deployment_artifacts?.components?.find((component: any) => component?.name === "caddy");
requireCheck(!!releaseCaddy, "release contract contains the Caddy deployment component");
if (metadata && releaseCaddy) {
  for (const field of ["version_major", "version_policy", "builder", "provider_module", "provider_ref", "provider_module_name"] as const) {
    requireCheck(releaseCaddy[field] === metadata.caddy?.[field], `release contract Caddy ${field} matches deployment metadata`);
  }
  requireCheck(releaseCaddy.checksum_algorithm === "sha512", "release contract records SHA-512 for custom Caddy artifacts");
  requireCheck(releaseCaddy.artifact_verification === "operator-recorded-custom-build", "release contract requires operator-recorded Caddy build verification");
  requireCheck(!Object.hasOwn(releaseCaddy, "version"), "release contract does not pin a Caddy patch version");
  requireCheck(!Object.hasOwn(releaseCaddy, "artifacts"), "release contract does not publish an unextended Caddy artifact");
}

const caddyfile = read(join(blueGreenRoot, "Caddyfile.example"));
requireText(caddyfile, "admin off", "Caddy admin API is disabled");
requireText(caddyfile, "auto_https off", "Caddy does not expose an automatic HTTP listener");
requireText(caddyfile, "storage file_system /var/lib/agent-vm-native/caddy", "Caddy certificate storage is persistent and explicit");
requireText(caddyfile, "dns dnspod {env.DNSPOD_TOKEN}", "Caddy owns DNS-01 through the DNSPod provider");
requireText(caddyfile, "\tlog default", "Caddy access logging uses the filtered logger");
requireText(caddyfile, "reverse_proxy unix//var/lib/agent-vm-native/active/app.sock", "Caddy uses the protected active Unix socket");
requireText(caddyfile, "header_up -X-Forwarded-User", "Caddy strips browser-supplied identity headers");
requireText(caddyfile, "@internal path /internal/v1/*", "internal routes are explicitly matched");
requireText(caddyfile, "respond 404", "internal routes return 404 at the edge");
requireText(caddyfile, "@public path / /index.html /assets/* /api/v1 /api/v1/* /agent/v1/control", "Caddy forwards only the fixed public paths");
requireText(caddyfile, "handle @public", "Caddy applies the active socket proxy only to the fixed public matcher");
requireText(caddyfile, "request>uri regexp", "request query values are filtered from logs");
requireText(caddyfile, "request>headers>Authorization delete", "authorization headers are filtered from logs");
requireText(caddyfile, "request>headers>Cookie delete", "cookie headers are filtered from logs");
requireText(caddyfile, "request>headers>X-Csrf-Token delete", "CSRF tokens are filtered from logs");
requireText(caddyfile, "force_automate", "Caddy keeps certificate automation explicit when automatic HTTP is disabled");
forbidText(caddyfile, "auto_https disable_redirects", "Caddy does not create an automatic HTTP listener");
forbidText(caddyfile, "/etc/agent-vm-native/tls/caddy", "Caddy does not load a static leaf certificate");
forbidText(caddyfile, "certbot", "Caddy does not delegate certificate management to certbot");
forbidText(caddyfile, "lego", "Caddy does not delegate certificate management to lego");
forbidText(caddyfile, "forward_auth", "Caddy does not provide Forward Auth");
forbidText(caddyfile, "header_up X-Forwarded-User", "Caddy does not inject identity headers");
forbidText(caddyfile, "edgeone", "Caddy does not route through EdgeOne");
forbidText(caddyfile, "tls_insecure_skip_verify", "Caddy does not disable upstream TLS verification");
forbidText(caddyfile, "0.0.0.0", "Caddy does not bind or forward to an unspecified address");

const caddyEnv = read(join(blueGreenRoot, "caddy.env.example"));
const blueGreenDoc = read(join(repositoryRoot, "docs/deployment/blue-green.md"));
requireText(caddyEnv, "AGENT_VM_DOMAIN=<operator-provided-hostname>", "Caddy hostname remains an operator input");
requireText(caddyEnv, "DNSPOD_TOKEN=<operator-managed-dnspod-token>", "DNSPod credentials remain an operator-managed placeholder");
forbidText(caddyEnv, "api_key", "Caddy environment does not use a legacy DNSPod API key");
requireCheck(blueGreenDoc.includes("install -o root -g root -m 0600"), "Caddy environment installation requires root-only mode 0600");

const authText = read(join(blueGreenRoot, "rqlite-auth.json.example"));
try {
  const users = JSON.parse(authText);
  requireCheck(Array.isArray(users) && users.length === 2, "rqlite auth example has two least-privilege users");
  for (const user of users) {
    requireCheck(typeof user.username === "string" && typeof user.password === "string", "rqlite auth entries have placeholders");
    requireCheck(user.password.startsWith("<") && user.password.endsWith(">"), `rqlite password for ${user.username} is not committed`);
    requireCheck(!user.perms?.includes("all"), `rqlite user ${user.username} does not have all permissions`);
  }
  const controlUser = users.find((user: any) => user.username === "agent-vm-control");
  const backupUser = users.find((user: any) => user.username === "agent-vm-backup");
  requireCheck(controlUser?.perms?.includes("execute") && controlUser?.perms?.includes("query"), "control user can execute and query state SQL");
  requireCheck(backupUser?.perms?.includes("backup") && !backupUser?.perms?.includes("execute"), "backup user cannot execute state SQL");
} catch (error) {
  errors.push(`rqlite auth example is invalid JSON: ${error instanceof Error ? error.message : String(error)}`);
}

const rqliteUnit = read(join(assetsRoot, "systemd/agent-vm-rqlite.service"));
requireText(rqliteUnit, "User=agent-vm-rqlite", "rqlite runs as a dedicated user");
requireText(rqliteUnit, "SupplementaryGroups=agent-vm-web", "rqlite can traverse the protected parent directories");
requireText(rqliteUnit, "-http-addr 127.0.0.1:4001", "rqlite HTTP binds to loopback");
requireText(rqliteUnit, "-raft-addr 127.0.0.1:4002", "rqlite raft binds to loopback");
requireText(rqliteUnit, "-auth /etc/agent-vm-native/rqlite/auth.json", "rqlite auth file is explicit");
requireText(rqliteUnit, "ExecStartPre=/usr/bin/test -f /etc/agent-vm-native/rqlite/auth.json", "rqlite auth file is checked before startup");
requireText(rqliteUnit, "agent-vm-rqlite:600", "rqlite auth file is dedicated-user mode 0600");
requireText(rqliteUnit, "/usr/bin/stat", "rqlite auth metadata check uses an absolute stat path");
requireText(blueGreenDoc, "install -o agent-vm-rqlite -g agent-vm-rqlite -m 0600", "deployment docs install rqlite auth as dedicated-user mode 0600");
requireText(rqliteUnit, "-http-cert /etc/agent-vm-native/tls/rqlite/server.crt", "rqlite TLS certificate is explicit");
requireText(rqliteUnit, "-http-key /etc/agent-vm-native/tls/rqlite/server.key", "rqlite TLS key is explicit");
forbidText(rqliteUnit, "0.0.0.0", "rqlite does not bind to all interfaces");

const slotUnit = read(join(assetsRoot, "systemd/agent-vm-native@.service"));
requireText(slotUnit, "--manifest /opt/agent-vm-native/release-manifest.json", "slot service uses the fixed manifest path");
requireText(slotUnit, "--slot %i", "slot service passes its blue-green identity");
requireText(slotUnit, "--socket /run/agent-vm-native/%i/app.sock", "slot service uses its per-slot runtime socket");
requireText(slotUnit, "BindReadOnlyPaths=/opt/agent-vm-native/slots/%i/release-manifest.json:/opt/agent-vm-native/release-manifest.json", "slot manifest is isolated in the service mount namespace");
requireText(slotUnit, "Group=agent-vm-web", "slot service owns the protected web socket group");
requireText(slotUnit, "Requires=agent-vm-rqlite.service", "slot service requires the state authority");
forbidText(slotUnit, "ExecStart=/usr/bin/caddy", "slot service does not start a second proxy");

const caddyUnit = read(join(assetsRoot, "systemd/agent-vm-caddy.service"));
const acceptance = read(join(repositoryRoot, "skills/agent-vm-ops/scripts/agent_vm_deployment_acceptance.sh"));
const cutover = read(join(import.meta.dir, "agent_vm_cutover.sh"));
const caddyBuilder = read(join(import.meta.dir, "build_caddy.ts"));
const caGenerator = read(join(import.meta.dir, "generate_deployment_ca.sh"));
const ciWorkflow = read(join(repositoryRoot, ".github/workflows/ci.yml"));
requireText(caddyUnit, "User=caddy", "Caddy does not run as root");
requireText(caddyUnit, "After=network-online.target", "Caddy waits for network readiness before DNS-01");
requireText(caddyUnit, "SupplementaryGroups=agent-vm-web", "Caddy can access the protected socket group");
requireText(caddyUnit, "ConditionPathExists=/etc/agent-vm-native/caddy.env", "Caddy requires its protected environment file");
requireText(caddyUnit, "EnvironmentFile=/etc/agent-vm-native/caddy.env", "Caddy receives DNSPod credentials from its environment file");
requireText(caddyUnit, "ExecStartPre=/usr/bin/test -S /var/lib/agent-vm-native/active/app.sock", "Caddy checks the active socket before starting");
requireText(caddyUnit, "ExecStartPre=+/bin/sh -c", "Caddy runs its active socket readiness probe with root peer credentials");
requireText(caddyUnit, 'while [ "$$attempt" -lt 30 ]', "systemd preserves the Caddy readiness loop shell variable");
requireText(caddyUnit, "attempt=$$(($$attempt + 1))", "systemd preserves the Caddy readiness loop arithmetic");
requireText(caddyUnit, "--unix-socket /var/lib/agent-vm-native/active/app.sock http://localhost/internal/v1/health", "Caddy waits for a ready native listener, not only a socket inode");
requireText(caddyUnit, "/usr/bin/stat -c", "Caddy verifies the environment file metadata before starting");
requireText(caddyUnit, "0:0:600", "Caddy requires a root-owned mode-0600 environment file");
requireText(caddyUnit, "ReadOnlyPaths=/etc/agent-vm-native/Caddyfile", "Caddy configuration is read-only");
requireText(caddyUnit, "ReadOnlyPaths=/etc/agent-vm-native/Caddyfile /etc/agent-vm-native/caddy.env", "Caddy configuration and credentials are read-only");
requireText(caddyUnit, "ReadWritePaths=/var/lib/agent-vm-native/caddy", "Caddy can persist ACME state in its dedicated directory");
forbidText(caddyUnit, "--environ", "Caddy does not dump the DNSPod environment at startup");
requireText(acceptance, "rqlite-curl.conf", "rqlite readiness credentials use a temporary curl config");
requireText(acceptance, "umask 077", "temporary rqlite curl credentials are owner-only");
requireText(acceptance, "require_command flock", "isolated acceptance checks the active link lock dependency");
requireText(acceptance, "env -i", "isolated runtime children do not inherit ambient credentials");
requireText(acceptance, "stop_process", "isolated acceptance bounds runtime child cleanup");
requireText(acceptance, "process_alive", "isolated acceptance checks direct process liveness");
requireText(acceptance, "capture_process_group", "isolated acceptance records each runtime process group");
requireText(acceptance, "kill -KILL", "isolated acceptance force-cleans stalled runtime children");
requireText(acceptance, "setsid --wait", "isolated acceptance gives runtime children a dedicated process group");
requireText(acceptance, 'kill -TERM -- "-$group_id"', "isolated acceptance terminates the runtime process group");
requireText(acceptance, "process_group_for", "isolated acceptance resolves the runtime process group");
requireText(acceptance, 'wait "$pid"', "isolated acceptance waits for runtime process termination");
requireText(acceptance, "chown 994:994", "staged rqlite auth is owned by the service UID");
requireText(acceptance, "staged rqlite auth metadata does not satisfy ExecStartPre", "staging executes the rqlite metadata precondition");
requireText(acceptance, "staged Caddy environment metadata does not satisfy ExecStartPre", "staging executes the Caddy metadata precondition");
requireText(acceptance, '"$systemd_root/bin/sh" -c', "staging executes preconditions through the staged shell path");
requireText(acceptance, '"$2" -c "%U:%a"', "staging exercises the systemd-expanded owner metadata format");
for (const commandPath of ["$systemd_root/bin/sh", "$systemd_root/usr/bin/curl", "$systemd_root/usr/bin/sleep", "$systemd_root/usr/bin/stat"]) {
  requireText(acceptance, commandPath, `systemd staging root provides ${commandPath}`);
}
requireText(acceptance, 'rqlite_auth_file="$proof_dir/auth.json"', "temporary rqlite auth file has an explicit path");
requireText(acceptance, 'chmod 0600 "$rqlite_auth_file"', "temporary rqlite auth file is owner-only");
requireText(acceptance, 'rm -f -- "$rqlite_auth_file"', "temporary rqlite auth file is removed during cleanup");
requireText(acceptance, "failed post-swap health", "isolated acceptance executes the rollback path");
requireText(acceptance, 'previous=blue\\ncandidate=green', "isolated acceptance executes pending recovery");
requireText(acceptance, 'previous_manifest=%s\\ncandidate_manifest=%s', "isolated acceptance binds pending recovery to manifest identities");
requireText(acceptance, "pending recovery did not restore blue", "isolated acceptance verifies pending recovery");
requireText(acceptance, "exit 1", "isolated acceptance fails when cleanup is incomplete");
requireText(caddyBuilder, "mkdtempSync", "Caddy builds into a private staging directory");
requireText(caddyBuilder, "renameSync(stagingOutput, options.output)", "Caddy installs only after verification via atomic rename");
requireText(caddyBuilder, "isSymbolicLink()", "Caddy rejects symlinked output paths");
requireText(caGenerator, '[[ -L "$output_dir" ]]', "CA generation rejects a symlinked output directory");
requireText(caGenerator, '"$output_dir/ca.key"', "CA generation cleans the private key after a failed run");
requireText(ciWorkflow, "github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.4", "Ubuntu PR gate installs a pinned xcaddy builder");
requireText(ciWorkflow, "skills/agent-vm-ops/scripts/agent_vm_deployment_acceptance.sh", "Ubuntu PR gate runs isolated deployment acceptance");
requireText(ciWorkflow, "repos/caddyserver/caddy/releases", "Ubuntu PR gate resolves an official Caddy release");
requireText(ciWorkflow, "CADDY_VERSION=$caddy_version", "Ubuntu PR gate passes an explicit resolved Caddy v2 input");
requireText(ciWorkflow, "^v2\\\\.", "Ubuntu PR gate selects only stable Caddy v2 releases");
forbidText(ciWorkflow, "CADDY_VERSION: v2.", "Ubuntu PR gate does not pin a Caddy patch release");
forbidText(acceptance, '--user "agent-vm-backup:$backup_secret"', "rqlite credentials are not placed in curl argv");
requireText(cutover, 'health_check "$active/app.sock"', "cutover rechecks the active socket after atomic replacement");
requireText(cutover, "slot_manifest_digest", "cutover records release manifest identities for recovery");
requireText(cutover, "assert_manifest_digest", "cutover fails closed on a changed recovery manifest");
requireText(cutover, 'assert_manifest_digest "$current_slot" "$pending_previous_manifest"', "cutover binds post-swap rollback to the recorded previous manifest");
requireText(cutover, "active_matches_current || die", "cutover refuses to replace an active link changed during readiness");
requireText(cutover, "rollback_link", "cutover has a protected rollback link for post-swap health failure");
requireText(cutover, "pending_file", "cutover persists an interrupted-swap recovery record");
requireText(cutover, "recover_pending_cutover", "cutover recovers an interrupted swap before a new replacement");
requireText(cutover, "validate_candidate_layout", "cutover revalidates the candidate while holding the active lock");
requireText(cutover, '! -L "$slot_dir"', "cutover rejects symlinked slot directories");
requireText(cutover, '! -L "$slot_dir/release-manifest.json"', "cutover rejects symlinked release manifests");
requireText(cutover, '! -L "$root_dir/slots"', "cutover rejects a symlinked slot root");
requireText(cutover, '[[ -e "$pending_file" || -L "$pending_file" ]]', "cutover fails closed on dangling recovery links");
requireText(cutover, 'validate_slot_layout "$current_slot"', "cutover validates the active slot before recovery");
requireText(cutover, "sync -f", "cutover flushes recovery and active-link directory updates");
requireText(cutover, "candidate_record/app.sock", "cutover confirms a pending candidate before rollback");
requireText(cutover, "flock --wait 30 9", "cutover serializes active link writers with a kernel lock");
requireText(cutover, 'mktemp -d -- "$root_dir/.active-stage.XXXXXX"', "cutover uses PID-independent unique staging");
requireText(cutover, '[[ -L "$lock_path" ]]', "cutover rejects a symlinked active-link lock");
requireText(cutover, 'ln -- "$lock_seed" "$lock_path"', "cutover atomically creates the active-link lock without following a symlink");
requireText(cutover, 'mktemp "$stage_dir/pending.XXXXXX"', "cutover uses PID-independent pending staging");
forbidText(cutover, ".active.cutover.pending.$$.tmp", "cutover does not reuse PID-derived pending staging names");
forbidText(cutover, ".active.$$.tmp", "cutover does not reuse PID-derived staging names");
forbidText(cutover, 'mkdir -- "$lock_dir"', "cutover does not use a crash-stale directory lock");
forbidText(caddyUnit, "/etc/agent-vm-native/tls/caddy", "Caddy systemd unit does not require static certificate files");
requireText(caddyUnit, "CapabilityBoundingSet=CAP_NET_BIND_SERVICE", "Caddy has only its bind capability");
forbidText(caddyUnit, "User=root", "Caddy is not configured as root");

const sysusers = read(join(assetsRoot, "sysusers.d/agent-vm-native.conf"));
requireText(sysusers, "u agent-vm-rqlite", "the state authority user is declared");
requireText(sysusers, "u caddy", "the edge proxy user is declared");

const tmpfiles = read(join(assetsRoot, "tmpfiles.d/agent-vm-native.conf"));
requireText(tmpfiles, "z /etc/agent-vm-native/caddy.env 0600 root root", "tmpfiles locks the Caddy environment to root-only mode 0600");
forbidText(tmpfiles, "/var/lib/agent-vm-native/active 0750", "tmpfiles does not pre-create the active symlink as a directory");
requireText(tmpfiles, "/var/lib/agent-vm-native/slots 0750", "slot storage has a fixed owner and mode");
requireText(tmpfiles, "/var/lib/agent-vm-native/rqlite 0700", "rqlite data has private mode");

const scriptPaths = [
  join(import.meta.dir, "generate_deployment_ca.sh"),
  join(import.meta.dir, "build_caddy.ts"),
  join(import.meta.dir, "agent_vm_cutover.sh"),
  join(import.meta.dir, "agent_vm_state_migration.ts"),
];
for (const path of scriptPaths) {
  try {
    const mode = statSync(path).mode & 0o777;
    requireCheck((mode & 0o111) !== 0, `${path} is executable`);
  } catch (error) {
    errors.push(`cannot stat ${path}: ${error instanceof Error ? error.message : String(error)}`);
  }
}

const verdict = errors.length === 0 ? "pass" : "fail";
console.log(JSON.stringify({
  schema_version: 1,
  validator: "blue-green-deployment-static",
  verdict,
  checks,
  errors,
  isolated_runtime_required: true,
}, null, 2));
if (errors.length > 0) process.exit(1);
