#!/usr/bin/env bun

import { readFileSync, statSync } from "node:fs";
import { join, resolve } from "node:path";

const repositoryRoot = resolve(import.meta.dir, "../..");
const assetsRoot = join(repositoryRoot, "deploy/assets");
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
  requireCheck(JSON.stringify(metadata.rqlite?.supported_architectures) === JSON.stringify(["amd64", "arm64"]), "rqlite supports the product Linux architectures");
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
requireText(rqliteUnit, "SupplementaryGroups=agent-vm-web", "rqlite can traverse the protected parent directories");
requireText(rqliteUnit, "-http-addr 127.0.0.1:4001", "rqlite HTTP binds to loopback");
requireText(rqliteUnit, "-raft-addr 127.0.0.1:4002", "rqlite raft binds to loopback");
requireText(rqliteUnit, "-auth /etc/agent-vm-native/rqlite/auth.json", "rqlite auth file is explicit");
requireText(rqliteUnit, "-http-cert /etc/agent-vm-native/tls/rqlite/server.crt", "rqlite TLS certificate is explicit");
requireText(rqliteUnit, "-http-key /etc/agent-vm-native/tls/rqlite/server.key", "rqlite TLS key is explicit");
forbidText(rqliteUnit, "0.0.0.0", "rqlite does not bind to all interfaces");

const slotUnit = read(join(assetsRoot, "systemd/agent-vm-native@.service"));
requireText(slotUnit, "ExecStart=/var/lib/agent-vm-native/slots/%i/bin/agent-vm-control", "slot service executes the atomically staged candidate binary");
requireText(slotUnit, "--manifest /var/lib/agent-vm-native/slots/%i/release-manifest.json", "slot service uses its staged manifest path");
forbidText(slotUnit, "BindReadOnlyPaths=", "slot service does not alias a candidate manifest over the shared release root");
requireText(slotUnit, "Group=agent-vm-web", "slot service owns the protected web socket group");
requireText(slotUnit, "Requires=agent-vm-rqlite.service", "slot service requires the state authority");
forbidText(slotUnit, "ExecStart=/usr/bin/caddy", "slot service does not start a second proxy");

const deploydUnit = read(join(assetsRoot, "systemd/agent-vm-deployd.service"));
requireText(deploydUnit, "ExecStart=/opt/agent-vm-native/bin/agent-vm-deployd --config /etc/agent-vm-native/deployd.json", "Deployd unit starts the fixed daemon and config");
requireText(deploydUnit, "ReadWritePaths=/var/lib/agent-vm-native /run/agent-vm-native", "Deployd write paths include the state root required for atomic active replacement");
requireText(deploydUnit, "User=root", "Deployd owns the host deployment boundary");
forbidText(deploydUnit, "Requires=agent-vm-rqlite.service", "Deployd does not depend on the business state authority");

const deploydConfigText = read(join(assetsRoot, "deployd.json.example"));
try {
  const deploydConfig = JSON.parse(deploydConfigText);
  const migration = deploydConfig.migration;
  requireCheck(migration?.legacy_unit === "agent-vm-native.service", "migration fences the fixed legacy singleton");
  requireCheck(migration?.rqlite_unit === "agent-vm-rqlite.service", "migration starts the fixed rqlite unit");
  requireCheck(migration?.rqlite_url === "https://127.0.0.1:4001", "migration uses the fixed local HTTPS rqlite endpoint");
  requireCheck(migration?.rqlite_ca_file === "/etc/agent-vm-native/tls/rqlite/server.crt", "migration trusts the configured rqlite certificate");
  requireCheck(migration?.rqlite_auth_file === "/etc/agent-vm-native/rqlite/auth.json", "migration reads the protected rqlite auth file");
  requireCheck(migration?.drain_timeout_seconds === 8100, "migration drain budget is bounded at 8100 seconds");
} catch (error) {
  errors.push(`deployd config example is invalid JSON: ${error instanceof Error ? error.message : String(error)}`);
}

const caddyUnit = read(join(assetsRoot, "systemd/agent-vm-caddy.service"));
requireText(caddyUnit, "User=caddy", "Caddy does not run as root");
requireText(caddyUnit, "SupplementaryGroups=agent-vm-web", "Caddy can access the protected socket group");
requireText(caddyUnit, "ConditionPathExists=/var/lib/agent-vm-native/active/app.sock", "Caddy requires an active socket at startup");
requireText(caddyUnit, "ExecStartPre=/usr/bin/test -S /var/lib/agent-vm-native/active/app.sock", "Caddy checks the active socket before starting");
requireText(caddyUnit, "ReadOnlyPaths=/etc/agent-vm-native/Caddyfile", "Caddy configuration is read-only");
requireText(caddyUnit, "CapabilityBoundingSet=CAP_NET_BIND_SERVICE", "Caddy has only its bind capability");
forbidText(caddyUnit, "User=root", "Caddy is not configured as root");

const sysusers = read(join(assetsRoot, "sysusers.d/agent-vm-native.conf"));
requireText(sysusers, "u agent-vm-rqlite", "the state authority user is declared");
requireText(sysusers, "u caddy", "the edge proxy user is declared");

const tmpfiles = read(join(assetsRoot, "tmpfiles.d/agent-vm-native.conf"));
forbidText(tmpfiles, "/var/lib/agent-vm-native/active 0750", "tmpfiles does not pre-create the active symlink as a directory");
requireText(tmpfiles, "/var/lib/agent-vm-native/slots 0750", "slot storage has a fixed owner and mode");
requireText(tmpfiles, "/var/lib/agent-vm-native/rqlite 0700", "rqlite data has private mode");

const scriptPaths = [
  join(import.meta.dir, "generate_deployment_ca.sh"),
  join(import.meta.dir, "agent_vm_cutover.sh"),
  join(import.meta.dir, "agent_vm_state_migration.ts"),
  join(import.meta.dir, "agent_vm_deployd_systemd_acceptance.sh"),
];
const realSystemdAcceptance = read(join(import.meta.dir, "agent_vm_deployd_systemd_acceptance.sh"));
requireText(realSystemdAcceptance, "assert_exec_start", "real systemd acceptance compares ExecStart argv exactly");
requireText(realSystemdAcceptance, "refusing filesystem restore while a managed unit remains active", "real systemd acceptance refuses unsafe cleanup");
requireText(realSystemdAcceptance, "systemctl is-active --quiet \"$unit\"", "real systemd acceptance verifies stopped units during cleanup");
requireText(realSystemdAcceptance, "real agent-vm-control binary did not pass its version smoke test", "systemd acceptance keeps a real control binary smoke check");
requireText(realSystemdAcceptance, "slot_runtime\":\"testslot-adapter\"", "systemd acceptance labels its test slot adapter coverage honestly");
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
