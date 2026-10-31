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
  requireCheck(metadata.schema_version === 1, "deployment metadata schema_version is 1");
  requireCheck(metadata.policy?.automatic_install === false, "deployment metadata disables automatic installation");
  requireCheck(metadata.policy?.verification === "pinned-checksum", "deployment metadata requires pinned checksums");
  requireCheck(metadata.caddy?.version === "2.11.7", "Caddy is pinned to 2.11.7");
  requireCheck(metadata.rqlite?.version === "10.5.2", "rqlite is pinned to 10.5.2");
  const caddyArtifacts = Array.isArray(metadata.caddy?.artifacts) ? metadata.caddy.artifacts : [];
  const rqliteArtifacts = Array.isArray(metadata.rqlite?.artifacts) ? metadata.rqlite.artifacts : [];
  for (const artifact of [...caddyArtifacts, ...rqliteArtifacts]) {
    requireCheck(typeof artifact.url === "string" && artifact.url.startsWith("https://github.com/"), `official source for ${artifact.filename}`);
  }
  for (const artifact of caddyArtifacts) {
    requireCheck(typeof artifact.sha512 === "string" && /^[0-9a-f]{128}$/.test(artifact.sha512), `SHA-512 is pinned for ${artifact.filename}`);
  }
  for (const artifact of rqliteArtifacts) {
    requireCheck(typeof artifact.sha256 === "string" && /^[0-9a-f]{64}$/.test(artifact.sha256), `SHA-256 is pinned for ${artifact.filename}`);
  }
}

const caddyfile = read(join(blueGreenRoot, "Caddyfile.example"));
requireText(caddyfile, "admin off", "Caddy admin API is disabled");
requireText(caddyfile, "auto_https off", "Caddy automatic certificate issuance is disabled");
requireText(caddyfile, "\tlog default", "Caddy access logging uses the filtered logger");
requireText(caddyfile, "reverse_proxy unix//var/lib/agent-vm-native/active/app.sock", "Caddy uses the protected active Unix socket");
requireText(caddyfile, "@internal path /internal/v1/*", "internal routes are explicitly matched");
requireText(caddyfile, "respond 404", "internal routes return 404 at the edge");
requireText(caddyfile, "request>uri regexp", "request query values are filtered from logs");
requireText(caddyfile, "request>headers>Authorization delete", "authorization headers are filtered from logs");
requireText(caddyfile, "request>headers>Cookie delete", "cookie headers are filtered from logs");
forbidText(caddyfile, "tls_insecure_skip_verify", "Caddy does not disable upstream TLS verification");
forbidText(caddyfile, "0.0.0.0", "Caddy does not bind or forward to an unspecified address");

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
requireText(rqliteUnit, "-http-addr 127.0.0.1:4001", "rqlite HTTP binds to loopback");
requireText(rqliteUnit, "-raft-addr 127.0.0.1:4002", "rqlite raft binds to loopback");
requireText(rqliteUnit, "-auth /etc/agent-vm-native/rqlite/auth.json", "rqlite auth file is explicit");
requireText(rqliteUnit, "-http-cert /etc/agent-vm-native/tls/rqlite/server.crt", "rqlite TLS certificate is explicit");
requireText(rqliteUnit, "-http-key /etc/agent-vm-native/tls/rqlite/server.key", "rqlite TLS key is explicit");
forbidText(rqliteUnit, "0.0.0.0", "rqlite does not bind to all interfaces");

const slotUnit = read(join(assetsRoot, "systemd/agent-vm-native@.service"));
requireText(slotUnit, "--manifest /opt/agent-vm-native/release-manifest.json", "slot service uses the fixed manifest path");
requireText(slotUnit, "BindReadOnlyPaths=/opt/agent-vm-native/slots/%i/release-manifest.json:/opt/agent-vm-native/release-manifest.json", "slot manifest is isolated in the service mount namespace");
requireText(slotUnit, "Group=agent-vm-web", "slot service owns the protected web socket group");
requireText(slotUnit, "Requires=agent-vm-rqlite.service", "slot service requires the state authority");
forbidText(slotUnit, "ExecStart=/usr/bin/caddy", "slot service does not start a second proxy");

const caddyUnit = read(join(assetsRoot, "systemd/agent-vm-caddy.service"));
requireText(caddyUnit, "User=caddy", "Caddy does not run as root");
requireText(caddyUnit, "SupplementaryGroups=agent-vm-web", "Caddy can access the protected socket group");
requireText(caddyUnit, "ReadOnlyPaths=/etc/agent-vm-native/Caddyfile", "Caddy configuration is read-only");
requireText(caddyUnit, "CapabilityBoundingSet=CAP_NET_BIND_SERVICE", "Caddy has only its bind capability");
forbidText(caddyUnit, "User=root", "Caddy is not configured as root");

const tmpfiles = read(join(assetsRoot, "tmpfiles.d/agent-vm-native.conf"));
forbidText(tmpfiles, "/var/lib/agent-vm-native/active 0750", "tmpfiles does not pre-create the active symlink as a directory");
requireText(tmpfiles, "/var/lib/agent-vm-native/slots 0750", "slot storage has a fixed owner and mode");
requireText(tmpfiles, "/var/lib/agent-vm-native/rqlite 0700", "rqlite data has private mode");

const scriptPaths = [
  join(import.meta.dir, "generate_deployment_ca.sh"),
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
