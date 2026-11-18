#!/usr/bin/env bun

/**
 * Evidence-gated SQLite snapshot and restore admissibility checks.
 *
 * This command never imports rows into rqlite. The Go state package owns that
 * operation after a service/deployment coordinator supplies the real writer,
 * worker and native quiescence fence. Keeping the boundary here explicit
 * prevents an operator from turning a backup helper into a second authority.
 */

import { createHash, createVerify } from "node:crypto";
import { chmodSync, existsSync, lstatSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { dirname, isAbsolute, resolve } from "node:path";

const usage = `usage:
  agent_vm_state_migration.ts preflight --source <sqlite-path> --evidence <json-path> --evidence-public-key <pem-path> --output-dir <empty-dir>
  agent_vm_state_migration.ts restore-check --backup-manifest <json-path> --tail-evidence <json-path> --evidence-public-key <pem-path> --output <json-path>`;

function fail(message: string): never {
  console.error(`state migration: ${message}`);
  process.exit(1);
}

function value(args: string[], name: string): string {
  const index = args.indexOf(name);
  if (index < 0 || !args[index + 1]) fail(`${name} is required\n${usage}`);
  return args[index + 1];
}

function readJson(path: string): Record<string, unknown> {
  try {
    const value = JSON.parse(readFileSync(path, "utf8"));
    if (!value || typeof value !== "object" || Array.isArray(value)) fail(`${path} must contain a JSON object`);
    return value as Record<string, unknown>;
  } catch (error) {
    fail(`cannot read JSON ${path}: ${error instanceof Error ? error.message : String(error)}`);
  }
}

function absolute(path: string, label: string): string {
  if (!isAbsolute(path)) fail(`${label} must be absolute`);
  return resolve(path);
}

function canonicalJson(value: unknown): string {
  if (value === null) return "null";
  if (typeof value === "string" || typeof value === "boolean") return JSON.stringify(value);
  if (typeof value === "number") {
    if (!Number.isFinite(value)) throw new Error("canonical JSON cannot contain a non-finite number");
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (typeof value === "object") {
    const object = value as Record<string, unknown>;
    return `{${Object.keys(object).sort().map((key) => `${JSON.stringify(key)}:${canonicalJson(object[key])}`).join(",")}}`;
  }
  throw new Error("canonical JSON cannot contain undefined or unsupported values");
}

function requireRegularFile(path: string, label: string): NonNullable<ReturnType<typeof lstatSync>> {
  const file = lstatSync(path, { throwIfNoEntry: false });
  if (!file || file.isSymbolicLink() || !file.isFile()) fail(`${label} must be a regular file`);
  return file as NonNullable<ReturnType<typeof lstatSync>>;
}

function requireSignedEvidence(path: string, publicKeyPath: string): Record<string, unknown> {
  requireRegularFile(path, "evidence");
  const wrapper = readJson(path);
  const payload = wrapper.payload;
  const signatureText = wrapper.signature;
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) fail(`evidence ${path} needs a payload object`);
  if (typeof signatureText !== "string" || !/^[A-Za-z0-9+/]+={0,2}$/.test(signatureText) || signatureText.length % 4 !== 0) {
    fail(`evidence ${path} needs a base64 signature`);
  }
  const publicKeyFile = requireRegularFile(publicKeyPath, "evidence public key");
  if ((Number(publicKeyFile.mode) & 0o022) !== 0) fail(`evidence public key ${publicKeyPath} must not be group/world writable`);
  let canonicalPayload: string;
  let signature: Buffer;
  try {
    canonicalPayload = canonicalJson(payload);
    signature = Buffer.from(signatureText, "base64");
    if (signature.length === 0) fail(`evidence ${path} has an empty signature`);
    const verifier = createVerify("sha256");
    verifier.update(canonicalPayload, "utf8");
    if (!verifier.verify(readFileSync(publicKeyPath, "utf8"), signature)) fail(`evidence ${path} signature verification failed`);
  } catch (error) {
    if (error instanceof Error && error.message.startsWith("state migration:")) throw error;
    fail(`evidence ${path} signature verification failed: ${error instanceof Error ? error.message : String(error)}`);
  }
  for (const key of ["state_write_fenced", "workers_quiescent", "sqlite_writers_closed"]) {
    if ((payload as Record<string, unknown>)[key] !== true) fail(`evidence ${path} does not prove ${key}=true`);
  }
  if ((payload as Record<string, unknown>).schema_version !== 1 || (payload as Record<string, unknown>).producer !== "agent-vm-control") {
    fail(`evidence ${path} has an unsupported producer or schema`);
  }
  const evidence = payload as Record<string, unknown>;
  if (typeof evidence.evidence_id !== "string" || evidence.evidence_id.length < 8) {
    fail(`evidence ${path} needs an evidence_id`);
  }
  if (evidence.release_fence_path !== "/run/agent-vm-native/release-activation.fence") {
    fail(`evidence ${path} has an unexpected release fence path`);
  }
  if (!Number.isInteger(evidence.fence_generation) || Number(evidence.fence_generation) < 1) {
    fail(`evidence ${path} needs a positive fence_generation`);
  }
  for (const key of ["state_epoch", "state_generation", "trusted_commit"]) {
    if (typeof evidence[key] !== "string" || evidence[key].length === 0) fail(`evidence ${path} needs ${key}`);
  }
  const observedAt = Date.parse(String(evidence.observed_at));
  const expiresAt = Date.parse(String(evidence.expires_at));
  if (!Number.isFinite(observedAt) || !Number.isFinite(expiresAt) || expiresAt <= observedAt || expiresAt <= Date.now()) {
    fail(`evidence ${path} has an invalid or expired validity window`);
  }
  if (observedAt > Date.now() + 5 * 60 * 1000) fail(`evidence ${path} is from the future`);
  return evidence;
}

function runPython(script: string, args: string[]): Record<string, unknown> {
  const result = Bun.spawnSync(["python3", "-c", script, ...args], { stdout: "pipe", stderr: "pipe" });
  const stderr = new TextDecoder().decode(result.stderr).trim();
  if (result.exitCode !== 0) fail(stderr || "SQLite inspection failed");
  try {
    return JSON.parse(new TextDecoder().decode(result.stdout)) as Record<string, unknown>;
  } catch (error) {
    fail(`SQLite inspector returned invalid JSON: ${error instanceof Error ? error.message : String(error)}`);
  }
}

const sqliteInspection = String.raw`
import hashlib
import json
import os
import sqlite3
import sys

path = sys.argv[1]
uri = "file:" + path + "?mode=ro"
connection = sqlite3.connect(uri, uri=True)
connection.row_factory = sqlite3.Row
try:
    integrity = connection.execute("PRAGMA integrity_check").fetchone()[0]
    if integrity != "ok":
        raise SystemExit("SQLite integrity check failed: " + str(integrity))
    objects = connection.execute(
        "SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type, name"
    ).fetchall()
    tables = [row[1] for row in objects if row[0] == "table" and not row[1].startswith("sqlite_")]
    counts = {}
    for table in tables:
        quoted = '"' + table.replace('"', '""') + '"'
        counts[table] = connection.execute("SELECT COUNT(*) FROM " + quoted).fetchone()[0]
    state_epoch = None
    if "meta" in tables:
        row = connection.execute("SELECT value FROM meta WHERE key = 'state_epoch'").fetchone()
        state_epoch = row[0] if row else None
    generations = []
    if "tasks" in tables:
        generations = [
            [row[0], row[1]]
            for row in connection.execute("SELECT session_id, generation FROM tasks ORDER BY session_id, generation")
        ]
    tombstones = []
    if "meta" in tables:
        tombstones = [
            [row[0], row[1]]
            for row in connection.execute("SELECT key, value FROM meta WHERE key LIKE 'generation_tombstone:%' ORDER BY key")
        ]
    state_generation = hashlib.sha256(json.dumps({"tasks": generations, "tombstones": tombstones}, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    pending = 0
    if "operations" in tables:
        for operation_id, raw in connection.execute("SELECT operation_id, response_json FROM operations"):
            try:
                response = json.loads(raw)
            except Exception as error:
                raise SystemExit(f"operation {operation_id} has invalid response JSON: {error}")
            outcome = response.get("outcome") if isinstance(response, dict) else None
            if outcome not in ("success", "pending", "error"):
                raise SystemExit(f"operation {operation_id} has unsupported outcome {outcome!r}")
            if outcome == "pending" or (outcome == "error" and isinstance(response.get("error"), dict) and response["error"].get("code") == "operation_outcome_unknown"):
                pending += 1
    if "management_operations" in tables:
        for operation_id, status in connection.execute("SELECT operation_id, status FROM management_operations"):
            if status in ("awaiting_upload", "queued", "running", "reconciling", "outcome_unknown"):
                pending += 1
            elif status not in ("succeeded", "failed"):
                raise SystemExit(f"management operation {operation_id} has unsupported status {status!r}")
    schema = "\n".join("|".join("" if value is None else str(value) for value in row) for row in objects)
    print(json.dumps({
        "integrity": integrity,
        "schema_sha256": hashlib.sha256(schema.encode()).hexdigest(),
        "tables": tables,
        "row_counts": counts,
        "state_epoch": state_epoch,
        "state_generation": state_generation,
        "pending_operation_count": pending,
        "source_size": os.stat(path).st_size,
    }, sort_keys=True))
finally:
    connection.close()
`;

function preflight(args: string[]): void {
  const source = absolute(value(args, "--source"), "source");
  const evidencePath = absolute(value(args, "--evidence"), "evidence");
  const publicKeyPath = absolute(value(args, "--evidence-public-key"), "evidence-public-key");
  const outputDir = absolute(value(args, "--output-dir"), "output-dir");
  const sourceStat = lstatSync(source, { throwIfNoEntry: false });
  if (!sourceStat) fail("source does not exist");
  if (sourceStat.isSymbolicLink() || !sourceStat.isFile()) fail("source must be a regular SQLite file");
  if (existsSync(outputDir)) {
    if (!statSync(outputDir).isDirectory()) fail("output-dir must be a directory");
    if (readdirSync(outputDir).length > 0) fail("output-dir must be empty");
  } else {
    mkdirSync(outputDir, { recursive: true, mode: 0o700 });
  }
  chmodSync(outputDir, 0o700);
  const evidence = requireSignedEvidence(evidencePath, publicKeyPath);
  const inspection = runPython(sqliteInspection, [source]);
  if (inspection.integrity !== "ok") fail("SQLite integrity is not ok");
  if (inspection.pending_operation_count !== 0) fail("management operation journal contains unresolved operations");
  const sourceHash = createHash("sha256").update(readFileSync(source)).digest("hex");
  if (evidence.source_sha256 !== sourceHash || evidence.source_size !== sourceStat.size) {
    fail("fence evidence does not describe the exact SQLite source");
  }
  if (evidence.state_epoch !== inspection.state_epoch || evidence.state_generation !== inspection.state_generation) {
    fail("fence evidence does not describe the observed SQLite state epoch and generation");
  }

  const backupPath = `${outputDir}/state.sqlite3`;
  const backupResult = Bun.spawnSync(["python3", "-c", String.raw`
import sqlite3
import sys
source, target = sys.argv[1], sys.argv[2]
connection = sqlite3.connect("file:" + source + "?mode=ro", uri=True)
destination = sqlite3.connect(target)
try:
    connection.backup(destination)
finally:
    destination.close()
    connection.close()
`, source, backupPath], { stdout: "pipe", stderr: "pipe" });
  if (backupResult.exitCode !== 0) fail(new TextDecoder().decode(backupResult.stderr).trim() || "SQLite backup failed");
  chmodSync(backupPath, 0o600);
  const manifest = {
    schema_version: 1,
    kind: "sqlite-state-snapshot",
    source_path: source,
    backup_path: backupPath,
    evidence_id: evidence.evidence_id,
    fence: {
      state_write_fenced: evidence.state_write_fenced,
      workers_quiescent: evidence.workers_quiescent,
      sqlite_writers_closed: evidence.sqlite_writers_closed,
      release_fence_path: evidence.release_fence_path,
      fence_generation: evidence.fence_generation,
    },
    inspection,
    backup_sha256: createHash("sha256").update(readFileSync(backupPath)).digest("hex"),
    trusted_commit: evidence.trusted_commit,
    state_epoch: evidence.state_epoch,
    state_generation: evidence.state_generation,
    evidence_payload_sha256: createHash("sha256").update(canonicalJson(evidence)).digest("hex"),
  };
  const manifestPath = `${outputDir}/snapshot-manifest.json`;
  writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o600 });
  console.log(JSON.stringify({ verdict: "preflight_ready", manifest: manifestPath, backup: backupPath, evidence_id: evidence.evidence_id }));
}

function restoreCheck(args: string[]): void {
  const manifestPath = absolute(value(args, "--backup-manifest"), "backup-manifest");
  const tailPath = absolute(value(args, "--tail-evidence"), "tail-evidence");
  const publicKeyPath = absolute(value(args, "--evidence-public-key"), "evidence-public-key");
  const outputPath = absolute(value(args, "--output"), "output");
  const manifest = readJson(manifestPath);
  if (manifest.kind !== "sqlite-state-snapshot" || manifest.schema_version !== 1) fail("backup manifest is not a supported state snapshot");
  const fence = manifest.fence;
  if (!fence || typeof fence !== "object" || fence === null) fail("backup manifest has no fence");
  const manifestFence = fence as Record<string, unknown>;
  for (const key of ["state_write_fenced", "workers_quiescent", "sqlite_writers_closed"]) {
    if (manifestFence[key] !== true) fail(`backup manifest does not prove ${key}=true`);
  }
  if (manifestFence.release_fence_path !== "/run/agent-vm-native/release-activation.fence" || !Number.isInteger(manifestFence.fence_generation)) {
    fail("backup manifest has no valid release fence identity");
  }
  const tail = requireSignedEvidence(tailPath, publicKeyPath);
  if (tail.tail_verified !== true) fail("tail evidence must set tail_verified=true");
  if (tail.coverage_boundary_verified !== true) fail("tail evidence must prove the coverage boundary");
  const trustedCommit = manifest.trusted_commit;
  const tailCommit = tail.last_verified_commit;
  if (typeof trustedCommit !== "string" || typeof tailCommit !== "string" || trustedCommit !== tailCommit || trustedCommit !== tail.trusted_commit) {
    fail("restore requires an exact trusted-commit and verified-tail match");
  }
  if (manifest.state_epoch !== tail.state_epoch || manifest.state_generation !== tail.state_generation || manifestFence.fence_generation !== tail.fence_generation) {
    fail("restore evidence does not describe the same fenced state");
  }
  const backupPath = manifest.backup_path;
  if (typeof backupPath !== "string" || !isAbsolute(backupPath) || !existsSync(backupPath)) fail("backup file is missing");
  const actualHash = createHash("sha256").update(readFileSync(backupPath)).digest("hex");
  if (actualHash !== manifest.backup_sha256 || actualHash !== tail.backup_sha256) fail("backup hash does not match the manifest and signed tail evidence");
  const authorization = {
    schema_version: 1,
    verdict: "restore_admissible",
    backup_manifest: manifestPath,
    tail_evidence: tailPath,
    trusted_commit: trustedCommit,
    state_epoch: manifest.state_epoch,
    state_generation: manifest.state_generation,
    fence_generation: manifestFence.fence_generation,
    backup_sha256: actualHash,
    tail_evidence_payload_sha256: createHash("sha256").update(canonicalJson(tail)).digest("hex"),
    note: "A service-owned rqlite restore is still required; this command does not write the state authority.",
  };
  mkdirSync(dirname(outputPath), { recursive: true, mode: 0o700 });
  writeFileSync(outputPath, `${JSON.stringify(authorization, null, 2)}\n`, { mode: 0o600 });
  console.log(JSON.stringify({ verdict: authorization.verdict, output: outputPath, trusted_commit: trustedCommit }));
}

const [command, ...args] = process.argv.slice(2);
if (command === "preflight") preflight(args);
else if (command === "restore-check") restoreCheck(args);
else fail(usage);
