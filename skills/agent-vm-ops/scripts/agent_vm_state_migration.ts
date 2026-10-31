#!/usr/bin/env bun

/**
 * Evidence-gated SQLite snapshot and restore admissibility checks.
 *
 * This command never imports rows into rqlite. The Go state package owns that
 * operation after a service/deployment coordinator supplies the real writer,
 * worker and native quiescence fence. Keeping the boundary here explicit
 * prevents an operator from turning a backup helper into a second authority.
 */

import { createHash } from "node:crypto";
import { chmodSync, existsSync, lstatSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { dirname, isAbsolute, resolve } from "node:path";

const usage = `usage:
  agent_vm_state_migration.ts preflight --source <sqlite-path> --evidence <json-path> --output-dir <empty-dir>
  agent_vm_state_migration.ts restore-check --backup-manifest <json-path> --tail-evidence <json-path> --output <json-path>`;

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

function requireEvidence(path: string): Record<string, unknown> {
  const evidence = readJson(path);
  for (const key of ["state_write_fenced", "workers_quiescent", "sqlite_writers_closed"]) {
    if (evidence[key] !== true) fail(`evidence ${path} does not prove ${key}=true`);
  }
  if (typeof evidence.evidence_id !== "string" || evidence.evidence_id.length < 8) {
    fail(`evidence ${path} needs an evidence_id`);
  }
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
    generation = None
    if "meta" in tables:
        row = connection.execute("SELECT value FROM meta WHERE key = 'generation'").fetchone()
        generation = row[0] if row else None
    pending = 0
    if "management_operations" in tables:
        pending = connection.execute(
            "SELECT COUNT(*) FROM management_operations WHERE status IN ('queued', 'running', 'reconciling', 'unknown', 'outcome_unknown')"
        ).fetchone()[0]
    schema = "\n".join("|".join("" if value is None else str(value) for value in row) for row in objects)
    print(json.dumps({
        "integrity": integrity,
        "schema_sha256": hashlib.sha256(schema.encode()).hexdigest(),
        "tables": tables,
        "row_counts": counts,
        "state_epoch": state_epoch,
        "generation": generation,
        "pending_operation_count": pending,
        "source_size": os.stat(path).st_size,
    }, sort_keys=True))
finally:
    connection.close()
`;

function preflight(args: string[]): void {
  const source = absolute(value(args, "--source"), "source");
  const evidencePath = absolute(value(args, "--evidence"), "evidence");
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
  const evidence = requireEvidence(evidencePath);
  const inspection = runPython(sqliteInspection, [source]);
  if (inspection.integrity !== "ok") fail("SQLite integrity is not ok");
  if (inspection.pending_operation_count !== 0) fail("management operation journal contains unresolved operations");

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
      state_write_fenced: true,
      workers_quiescent: true,
      sqlite_writers_closed: true,
    },
    inspection,
    backup_sha256: createHash("sha256").update(readFileSync(backupPath)).digest("hex"),
    trusted_commit: typeof evidence.trusted_commit === "string" ? evidence.trusted_commit : null,
  };
  const manifestPath = `${outputDir}/snapshot-manifest.json`;
  writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o600 });
  console.log(JSON.stringify({ verdict: "preflight_ready", manifest: manifestPath, backup: backupPath, evidence_id: evidence.evidence_id }));
}

function restoreCheck(args: string[]): void {
  const manifestPath = absolute(value(args, "--backup-manifest"), "backup-manifest");
  const tailPath = absolute(value(args, "--tail-evidence"), "tail-evidence");
  const outputPath = absolute(value(args, "--output"), "output");
  const manifest = readJson(manifestPath);
  if (manifest.kind !== "sqlite-state-snapshot" || manifest.schema_version !== 1) fail("backup manifest is not a supported state snapshot");
  const fence = manifest.fence;
  if (!fence || typeof fence !== "object" || fence === null || (fence as Record<string, unknown>).sqlite_writers_closed !== true) {
    fail("backup manifest does not prove closed SQLite writers");
  }
  const tail = requireEvidence(tailPath);
  if (tail.tail_verified !== true) fail("tail evidence must set tail_verified=true");
  const trustedCommit = manifest.trusted_commit;
  const tailCommit = tail.last_verified_commit;
  if (typeof trustedCommit !== "string" || typeof tailCommit !== "string" || trustedCommit !== tailCommit) {
    fail("restore requires an exact trusted-commit and verified-tail match");
  }
  const backupPath = manifest.backup_path;
  if (typeof backupPath !== "string" || !isAbsolute(backupPath) || !existsSync(backupPath)) fail("backup file is missing");
  const actualHash = createHash("sha256").update(readFileSync(backupPath)).digest("hex");
  if (actualHash !== manifest.backup_sha256) fail("backup hash does not match the manifest");
  const authorization = {
    schema_version: 1,
    verdict: "restore_admissible",
    backup_manifest: manifestPath,
    tail_evidence: tailPath,
    trusted_commit: trustedCommit,
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
