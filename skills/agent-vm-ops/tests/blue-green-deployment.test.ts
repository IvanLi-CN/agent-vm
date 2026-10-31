import { expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";

const root = resolve(import.meta.dir, "../../..");
const read = (path: string) => readFileSync(resolve(root, path), "utf8");
const json = (path: string) => JSON.parse(read(path));
const run = (command: string[], options: Record<string, unknown> = {}) => Bun.spawnSync(command, {
  cwd: root,
  stdout: "pipe",
  stderr: "pipe",
  ...options,
});

test("blue-green deployment metadata and static validator stay pinned", () => {
  const metadata = json("skills/agent-vm-ops/assets/blue-green/deployment-components.json");
  expect(metadata.caddy.version).toBe("2.11.7");
  expect(metadata.rqlite.version).toBe("10.5.2");
  expect(metadata.policy.automatic_install).toBe(false);
  expect(metadata.caddy.artifacts[0].sha512).toHaveLength(128);
  expect(metadata.rqlite.artifacts[0].sha256).toHaveLength(64);
  expect(json(".github/release-contract.json").deployment_artifacts.components).toHaveLength(2);

  const result = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/validate_blue_green_deployment.ts")]);
  expect(result.exitCode).toBe(0);
  const output = JSON.parse(new TextDecoder().decode(result.stdout));
  expect(output.verdict).toBe("pass");
  expect(output.isolated_runtime_required).toBe(true);
});

test("deployment examples keep the edge and state boundaries fail-closed", () => {
  const caddy = read("skills/agent-vm-ops/assets/blue-green/Caddyfile.example");
  expect(caddy).toContain("admin off");
  expect(caddy).toContain("\tlog default");
  expect(caddy).toContain("reverse_proxy unix//var/lib/agent-vm-native/active/app.sock");
  expect(caddy).toContain("request>uri regexp");
  expect(caddy).not.toContain("tls_insecure_skip_verify");
  expect(caddy).toContain("@internal path /internal/v1/*");
  expect(caddy).toContain("respond 404");

  const rqlite = read("skills/agent-vm-ops/assets/systemd/agent-vm-rqlite.service");
  expect(rqlite).toContain("User=agent-vm-rqlite");
  expect(rqlite).toContain("-http-addr 127.0.0.1:4001");
  expect(rqlite).toContain("-auth /etc/agent-vm-native/rqlite/auth.json");
  expect(rqlite).not.toContain("0.0.0.0");

  const slot = read("skills/agent-vm-ops/assets/systemd/agent-vm-native@.service");
  expect(slot).toContain("BindReadOnlyPaths=/opt/agent-vm-native/slots/%i/release-manifest.json:/opt/agent-vm-native/release-manifest.json");
  expect(slot).toContain("--manifest /opt/agent-vm-native/release-manifest.json");
  expect(read("skills/agent-vm-ops/assets/systemd/agent-vm-caddy.service")).toContain("User=caddy");
});

test("CA generation and state preflight require operator evidence", () => {
  const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-blue-green-test-"));
  try {
    const caDir = join(temporaryRoot, "ca");
    const ca = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/generate_deployment_ca.sh"), "--output-dir", caDir, "--hostname", "localhost"]);
    expect(ca.exitCode).toBe(0);
    expect(existsSync(join(caDir, "ca.crt"))).toBe(true);
    expect(statSync(join(caDir, "ca.key")).mode & 0o777).toBe(0o600);
    expect(statSync(join(caDir, "caddy/server.key")).mode & 0o777).toBe(0o600);

    const source = join(temporaryRoot, "state.sqlite3");
    const database = run(["python3", "-c", "import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute(\"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)\"); c.execute(\"INSERT INTO meta VALUES ('state_epoch','epoch-1')\"); c.commit(); c.close()", source]);
    expect(database.exitCode).toBe(0);
    const invalidEvidence = join(temporaryRoot, "invalid.json");
    writeFileSync(invalidEvidence, JSON.stringify({ evidence_id: "invalid-0001", state_write_fenced: true, workers_quiescent: false, sqlite_writers_closed: true }));
    const rejected = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/agent_vm_state_migration.ts"), "preflight", "--source", source, "--evidence", invalidEvidence, "--output-dir", join(temporaryRoot, "rejected")]);
    expect(rejected.exitCode).not.toBe(0);

    const evidence = join(temporaryRoot, "evidence.json");
    writeFileSync(evidence, JSON.stringify({ evidence_id: "valid-0001", trusted_commit: "commit-1", state_write_fenced: true, workers_quiescent: true, sqlite_writers_closed: true }));
    const migrationDir = join(temporaryRoot, "migration");
    const preflight = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/agent_vm_state_migration.ts"), "preflight", "--source", source, "--evidence", evidence, "--output-dir", migrationDir]);
    expect(preflight.exitCode).toBe(0);
    expect(existsSync(join(migrationDir, "state.sqlite3"))).toBe(true);
    expect(JSON.parse(readFileSync(join(migrationDir, "snapshot-manifest.json"), "utf8")).fence.sqlite_writers_closed).toBe(true);

    const blockedSource = join(temporaryRoot, "blocked.sqlite3");
    const blockedDatabase = run(["python3", "-c", "import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.executescript(\"CREATE TABLE management_operations (status TEXT); INSERT INTO management_operations VALUES ('outcome_unknown');\"); c.commit(); c.close()", blockedSource]);
    expect(blockedDatabase.exitCode).toBe(0);
    const blocked = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/agent_vm_state_migration.ts"), "preflight", "--source", blockedSource, "--evidence", evidence, "--output-dir", join(temporaryRoot, "blocked-output")]);
    expect(blocked.exitCode).not.toBe(0);
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});

test("cutover dry-run accepts only a verified blue or green layout", () => {
  const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-cutover-test-"));
  try {
    const layout = join(temporaryRoot, "layout");
    const runtime = join(temporaryRoot, "runtime");
    mkdirSync(join(layout, "slots/blue"), { recursive: true });
    mkdirSync(join(runtime, "blue"), { recursive: true });
    writeFileSync(join(layout, "slots/blue/release-manifest.json"), "{}\n");
    symlinkSync(join(runtime, "blue/app.sock"), join(layout, "slots/blue/app.sock"));
    const accepted = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/agent_vm_cutover.sh"), "--root", layout, "--runtime-root", runtime, "--slot", "blue", "--dry-run"]);
    expect(accepted.exitCode).toBe(0);
    expect(JSON.parse(new TextDecoder().decode(accepted.stdout)).candidate).toBe("blue");
    const rejected = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/agent_vm_cutover.sh"), "--root", layout, "--runtime-root", runtime, "--slot", "red", "--dry-run"]);
    expect(rejected.exitCode).not.toBe(0);
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});
