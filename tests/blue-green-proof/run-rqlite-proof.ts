import { createHash } from "node:crypto";
import { request as httpRequest } from "node:http";
import { createServer, createConnection, type AddressInfo, type Server, type Socket } from "node:net";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { spawn, type ChildProcess } from "node:child_process";

const TARGET_VERSION = "v10.5.2";
const DEFAULT_HOST = "127.0.0.1";
const DEFAULT_HTTP_PORT = 0;
const DEFAULT_RAFT_PORT = 0;

type JsonObject = Record<string, unknown>;
type Cell = string | number | boolean | null;
type QueryRow = Cell[];

class ProofFailure extends Error {
  constructor(message: string, readonly detail: JsonObject = {}) {
    super(message);
  }
}

class HttpFailure extends ProofFailure {
  constructor(readonly status: number, message: string, readonly body: unknown) {
    super(message, { status, body });
  }
}

type Options = {
  rqlited: string;
  proofDir: string;
  host: string;
  httpPort: number;
  raftPort: number;
};

type RqliteProcess = {
  process: ChildProcess;
  log: string[];
  stop: () => Promise<void>;
};

type DropProxy = {
  port: number;
  outcome: Promise<ProxyOutcome>;
  close: () => Promise<void>;
};

type ProxyOutcome = {
  complete: boolean;
  reason: string;
  status_code: number | null;
  response_bytes: number;
};

function sleep(milliseconds: number): Promise<void> {
  return new Promise(resolve => setTimeout(resolve, milliseconds));
}

function parseArgs(argv: string[]): Options {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (!argument.startsWith("--") || index + 1 >= argv.length) {
      throw new ProofFailure(`unsupported argument: ${argument}`);
    }
    values.set(argument, argv[++index]);
  }

  const required = (name: string): string => {
    const value = values.get(name);
    if (!value) throw new ProofFailure(`missing argument: ${name}`);
    return value;
  };

  const integer = (name: string, fallback: number): number => {
    const value = values.get(name);
    if (value === undefined) return fallback;
    const parsed = Number(value);
    if (!Number.isInteger(parsed) || parsed < 0 || parsed > 65535) {
      throw new ProofFailure(`invalid integer argument: ${name}`);
    }
    return parsed;
  };

  const host = values.get("--host") ?? DEFAULT_HOST;
  if (host !== DEFAULT_HOST) throw new ProofFailure("the proof only permits loopback host 127.0.0.1");

  return {
    rqlited: required("--rqlited"),
    proofDir: required("--proof-dir"),
    host,
    httpPort: integer("--http-port", DEFAULT_HTTP_PORT),
    raftPort: integer("--raft-port", DEFAULT_RAFT_PORT),
  };
}

function sqlText(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

function digest(value: JsonObject): string {
  return createHash("sha256").update(JSON.stringify(value)).digest("hex");
}

function assertCondition(condition: unknown, message: string, detail: JsonObject = {}): asserts condition {
  if (!condition) throw new ProofFailure(message, detail);
}

function asObject(value: unknown, context: string): JsonObject {
  assertCondition(value !== null && typeof value === "object" && !Array.isArray(value), `${context} is not an object`);
  return value as JsonObject;
}

function asRows(value: unknown, context: string): QueryRow[] {
  assertCondition(Array.isArray(value), `${context} is not a row array`);
  return value as QueryRow[];
}

async function jsonRequest(url: string, init: RequestInit = {}): Promise<{ status: number; body: JsonObject; text: string }> {
  const response = await fetch(url, init);
  const text = await response.text();
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new HttpFailure(response.status, "rqlite returned non-JSON", { text: text.slice(0, 400) });
  }
  const body = asObject(parsed, "rqlite response");
  if (!response.ok) throw new HttpFailure(response.status, `rqlite HTTP ${response.status}`, body);
  return { status: response.status, body, text };
}

function rqliteResultErrors(body: JsonObject): string[] {
  const results = Array.isArray(body.results) ? body.results : [];
  return results.flatMap(result => {
    if (result === null || typeof result !== "object") return [];
    const error = (result as JsonObject).error;
    return typeof error === "string" && error ? [error] : [];
  });
}

async function waitForReady(baseUrl: string, server: RqliteProcess): Promise<JsonObject> {
  let lastStatus: JsonObject = {};
  for (let attempt = 0; attempt < 300; attempt += 1) {
    if (server.process.exitCode !== null) {
      throw new ProofFailure("rqlite exited before becoming ready", { exit_code: server.process.exitCode, log: server.log.join("").slice(-2000) });
    }
    try {
      const response = await jsonRequest(`${baseUrl}/status`);
      lastStatus = response.body;
      const store = asObject(response.body.store, "status.store");
      if (store.ready === true) return response.body;
    } catch {
      // The process is still starting. The bounded loop is the readiness budget.
    }
    await sleep(100);
  }
  throw new ProofFailure("rqlite did not become ready", { lastStatus });
}

async function allocateFreePort(): Promise<number> {
  const server = createServer();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, DEFAULT_HOST, () => resolve());
  });
  const address = server.address() as AddressInfo;
  const port = address.port;
  await new Promise<void>(resolve => server.close(() => resolve()));
  return port;
}

function startRqlite(options: Options): RqliteProcess {
  mkdirSync(options.proofDir, { recursive: true });
  const dataDir = join(options.proofDir, "rqlite-data");
  const child = spawn(options.rqlited, [
    "-node-id", "blue-green-proof-1",
    "-http-addr", `${options.host}:${options.httpPort}`,
    "-raft-addr", `${options.host}:${options.raftPort}`,
    dataDir,
  ], { stdio: ["ignore", "pipe", "pipe"] });
  assertCondition(child.pid !== undefined, "rqlite child process has no PID");
  writeFileSync(join(options.proofDir, "rqlited.pid"), `${child.pid}\n`);
  const log: string[] = [];
  child.stdout?.on("data", chunk => log.push(String(chunk)));
  child.stderr?.on("data", chunk => log.push(String(chunk)));

  let stopPromise: Promise<void> | null = null;
  const stop = (): Promise<void> => {
    if (stopPromise) return stopPromise;
    stopPromise = (async () => {
      if (child.exitCode !== null) return;
      child.kill("SIGTERM");
      for (let attempt = 0; attempt < 50 && child.exitCode === null; attempt += 1) await sleep(100);
      if (child.exitCode === null) child.kill("SIGKILL");
      for (let attempt = 0; attempt < 100 && child.exitCode === null; attempt += 1) await sleep(100);
      if (child.exitCode === null) throw new ProofFailure("rqlite did not exit after SIGKILL", { pid: child.pid });
    })();
    return stopPromise;
  };

  return { process: child, log, stop };
}

async function execute(baseUrl: string, statements: string[]): Promise<JsonObject> {
  const response = await jsonRequest(`${baseUrl}/db/execute?transaction`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(statements),
  });
  const errors = rqliteResultErrors(response.body);
  if (errors.length > 0) throw new HttpFailure(response.status, "rqlite transaction returned a statement error", { errors, response: response.body });
  return response.body;
}

async function query(baseUrl: string, statement: string): Promise<QueryRow[]> {
  const response = await jsonRequest(`${baseUrl}/db/query?level=strong&q=${encodeURIComponent(statement)}`);
  const errors = rqliteResultErrors(response.body);
  if (errors.length > 0) throw new HttpFailure(response.status, "rqlite query returned a statement error", { errors, response: response.body });
  const results = asRows(response.body.results, "query.results");
  if (results.length === 0) return [];
  const first = asObject(results[0], "query result");
  return asRows(first.values ?? [], "query.values");
}

function conditionalWriteStatements(operationId: string, requestDigest: string, owner: string, slotId: string): string[] {
  const op = sqlText(operationId);
  const request = sqlText(requestDigest);
  const slot = sqlText(slotId);
  const ownerValue = sqlText(owner);
  const applied = `EXISTS (SELECT 1 FROM slots WHERE slot_id = ${slot} AND last_operation_id = ${op})`;
  return [
    `INSERT OR IGNORE INTO operations(operation_id, request_digest, outcome, response_json) VALUES (${op}, ${request}, 'pending', NULL)`,
    `INSERT INTO operation_conflict_guard(id) SELECT 1 WHERE EXISTS (SELECT 1 FROM operations WHERE operation_id = ${op} AND request_digest <> ${request})`,
    `UPDATE slots SET generation = generation + 1, owner = ${ownerValue}, last_operation_id = ${op} WHERE slot_id = ${slot} AND state = 'ready' AND generation = 0 AND EXISTS (SELECT 1 FROM operations WHERE operation_id = ${op} AND request_digest = ${request} AND outcome = 'pending')`,
    `UPDATE operations SET outcome = CASE WHEN ${applied} THEN 'success' ELSE 'conflict' END, response_json = CASE WHEN ${applied} THEN '{"outcome":"success"}' ELSE '{"outcome":"conflict"}' END WHERE operation_id = ${op} AND request_digest = ${request} AND outcome = 'pending'`,
  ];
}

async function readSlot(baseUrl: string, slotId: string): Promise<JsonObject> {
  const rows = await query(baseUrl, `SELECT slot_id, state, generation, owner, last_operation_id FROM slots WHERE slot_id = ${sqlText(slotId)}`);
  assertCondition(rows.length === 1, `expected one slot row for ${slotId}`, { rows });
  const [id, state, generation, owner, operationId] = rows[0];
  return { slot_id: id, state, generation, owner, last_operation_id: operationId };
}

async function readOperation(baseUrl: string, operationId: string): Promise<JsonObject | null> {
  const rows = await query(baseUrl, `SELECT operation_id, request_digest, outcome, response_json FROM operations WHERE operation_id = ${sqlText(operationId)}`);
  if (rows.length === 0) return null;
  assertCondition(rows.length === 1, `expected at most one operation row for ${operationId}`, { rows });
  const [id, requestDigest, outcome, responseJson] = rows[0];
  return { operation_id: id, request_digest: requestDigest, outcome, response_json: responseJson };
}

async function readEffectCount(baseUrl: string, operationId: string): Promise<number> {
  const rows = await query(baseUrl, `SELECT count(*) FROM effects WHERE operation_id = ${sqlText(operationId)}`);
  assertCondition(rows.length === 1 && rows[0].length === 1, "effect count query returned an unexpected shape", { rows });
  const count = Number(rows[0][0]);
  assertCondition(Number.isInteger(count), "effect count is not an integer", { rows });
  return count;
}

async function initializeSchema(baseUrl: string): Promise<void> {
  await execute(baseUrl, [
    "CREATE TABLE slots(slot_id TEXT PRIMARY KEY, state TEXT NOT NULL, generation INTEGER NOT NULL, owner TEXT, last_operation_id TEXT)",
    "CREATE TABLE operations(operation_id TEXT PRIMARY KEY, request_digest TEXT NOT NULL, outcome TEXT NOT NULL, response_json TEXT)",
    "CREATE TABLE effects(operation_id TEXT PRIMARY KEY, effect_key TEXT NOT NULL)",
    "CREATE TABLE failure_guard(id TEXT PRIMARY KEY)",
    "CREATE TABLE operation_conflict_guard(id INTEGER PRIMARY KEY)",
    "CREATE TRIGGER slot_effect AFTER UPDATE OF generation ON slots WHEN NEW.last_operation_id IS NOT NULL AND NEW.generation <> OLD.generation BEGIN INSERT INTO effects(operation_id, effect_key) VALUES (NEW.last_operation_id, NEW.last_operation_id); END",
    "INSERT INTO slots(slot_id, state, generation, owner, last_operation_id) VALUES ('shared', 'ready', 0, NULL, NULL)",
    "INSERT INTO slots(slot_id, state, generation, owner, last_operation_id) VALUES ('partial', 'ready', 0, NULL, NULL)",
    "INSERT INTO slots(slot_id, state, generation, owner, last_operation_id) VALUES ('lost', 'ready', 0, NULL, NULL)",
    "INSERT INTO failure_guard(id) VALUES ('fixed')",
    "INSERT INTO operation_conflict_guard(id) VALUES (1)",
  ]);
}

function requestDigestFor(operationId: string, owner: string, slotId: string): string {
  return digest({ action: "conditional-write", operation_id: operationId, session_id: "blue-green-proof", generation: 0, owner, slot_id: slotId });
}

function headersComplete(bytes: Buffer): { headerEnd: number; contentLength: number | null; chunked: boolean } | null {
  const headerEnd = bytes.indexOf(Buffer.from("\r\n\r\n"));
  if (headerEnd < 0) return null;
  const headers = bytes.subarray(0, headerEnd).toString("latin1");
  const contentLengthMatch = headers.match(/\r\ncontent-length:\s*(\d+)/i);
  const transferEncoding = headers.match(/\r\ntransfer-encoding:\s*([^\r\n]+)/i)?.[1]?.toLowerCase() ?? "";
  return { headerEnd, contentLength: contentLengthMatch ? Number(contentLengthMatch[1]) : null, chunked: transferEncoding.includes("chunked") };
}

function messageComplete(bytes: Buffer): boolean {
  const headers = headersComplete(bytes);
  if (!headers) return false;
  if (headers.contentLength !== null) return bytes.length >= headers.headerEnd + 4 + headers.contentLength;
  if (headers.chunked) return bytes.includes(Buffer.from("\r\n0\r\n\r\n"));
  return true;
}

function responseComplete(bytes: Buffer): boolean {
  const headers = headersComplete(bytes);
  if (!headers) return false;
  if (headers.contentLength !== null) return bytes.length >= headers.headerEnd + 4 + headers.contentLength;
  if (headers.chunked) return bytes.includes(Buffer.from("\r\n0\r\n\r\n"));
  return false;
}

async function startDropProxy(upstreamPort: number): Promise<DropProxy> {
  let resolveOutcome: (outcome: ProxyOutcome) => void = () => undefined;
  let outcomeSettled = false;
  const outcome = new Promise<ProxyOutcome>(resolve => { resolveOutcome = resolve; });
  const settleOutcome = (value: ProxyOutcome): void => {
    if (outcomeSettled) return;
    outcomeSettled = true;
    resolveOutcome(value);
  };
  const server: Server = createServer((client: Socket) => {
    let requestBytes = Buffer.alloc(0);
    let forwarded = false;
    client.on("error", () => undefined);
    client.on("data", (chunk: Buffer) => {
      if (forwarded) return;
      requestBytes = Buffer.concat([requestBytes, chunk]);
      if (!messageComplete(requestBytes)) return;
      forwarded = true;
      const upstream = createConnection({ host: DEFAULT_HOST, port: upstreamPort }, () => upstream.write(requestBytes));
      let responseBytes = Buffer.alloc(0);
      let closed = false;
      const closeBoth = (complete: boolean, reason: string): void => {
        if (closed) return;
        closed = true;
        settleOutcome({ complete, reason, status_code: responseStatus(responseBytes), response_bytes: responseBytes.length });
        upstream.destroy();
        client.destroy();
      };
      upstream.on("data", (responseChunk: Buffer) => {
        responseBytes = Buffer.concat([responseBytes, responseChunk]);
        if (responseComplete(responseBytes)) closeBoth(true, "upstream_response_complete");
      });
      upstream.on("end", () => closeBoth(false, "upstream_ended_before_complete"));
      upstream.on("error", error => closeBoth(false, `upstream_error:${error.message}`));
      setTimeout(() => closeBoth(false, "upstream_response_timeout"), 10_000).unref();
    });
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, DEFAULT_HOST, () => resolve());
  });
  const address = server.address() as AddressInfo;
  return {
    port: address.port,
    outcome,
    close: () => new Promise(resolve => {
      settleOutcome({ complete: false, reason: "proxy_closed_before_upstream_response", status_code: null, response_bytes: 0 });
      server.close(() => resolve());
    }),
  };
}

function responseStatus(bytes: Buffer): number | null {
  const headers = headersComplete(bytes);
  if (!headers) return null;
  const statusLine = bytes.subarray(0, headers.headerEnd).toString("latin1").split("\r\n", 1)[0] ?? "";
  const match = statusLine.match(/^HTTP\/\d(?:\.\d)?\s+(\d{3})\b/);
  return match ? Number(match[1]) : null;
}

async function postAndLoseResponse(port: number, statements: string[]): Promise<string> {
  const body = Buffer.from(JSON.stringify(statements));
  return new Promise((resolve, reject) => {
    let settled = false;
    let gotResponse = false;
    let timeout: ReturnType<typeof setTimeout>;
    const finish = (error?: Error): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      if (error) reject(error);
      else resolve("client observed a closed connection without an HTTP response");
    };
    const request = httpRequest({
      host: DEFAULT_HOST,
      port,
      path: "/db/execute?transaction",
      method: "POST",
      headers: { "content-type": "application/json", "content-length": body.length, connection: "close" },
    });
    timeout = setTimeout(() => finish(new Error("timed out waiting for the response-drop proxy")), 15_000);
    request.on("response", response => {
      gotResponse = true;
      response.resume();
      finish(new Error(`response-drop proxy unexpectedly returned HTTP ${response.statusCode}`));
    });
    request.on("error", error => {
      if (gotResponse) finish(error);
      else finish();
    });
    request.on("close", () => {
      if (!gotResponse) finish();
    });
    request.write(body);
    request.end();
  });
}

async function run(options: Options): Promise<JsonObject> {
  const httpPort = options.httpPort || await allocateFreePort();
  let raftPort = options.raftPort || await allocateFreePort();
  while (raftPort === httpPort) raftPort = await allocateFreePort();
  const runtimeOptions = { ...options, httpPort, raftPort };
  const baseUrl = `http://${runtimeOptions.host}:${runtimeOptions.httpPort}`;
  const server = startRqlite(runtimeOptions);
  let interrupted: NodeJS.Signals | null = null;
  const onSignal = (signal: NodeJS.Signals): void => {
    interrupted ??= signal;
    void server.stop();
  };
  process.once("SIGINT", onSignal);
  process.once("SIGTERM", onSignal);
  try {
    const status = await waitForReady(baseUrl, server);
    const build = asObject(status.build, "status.build");
    assertCondition(build.version === TARGET_VERSION, "rqlite version mismatch", { expected: TARGET_VERSION, observed: build.version });
    const http = asObject(status.http, "status.http");
    const cluster = asObject(status.cluster, "status.cluster");
    const store = asObject(status.store, "status.store");
    const os = asObject(status.os, "status.os");
    assertCondition(http.bind_addr === `${runtimeOptions.host}:${runtimeOptions.httpPort}` && cluster.api_addr === `${runtimeOptions.host}:${runtimeOptions.httpPort}`, "rqlite HTTP identity does not match the selected loopback port", { http_bind_addr: http.bind_addr, cluster_api_addr: cluster.api_addr, expected: `${runtimeOptions.host}:${runtimeOptions.httpPort}` });
    assertCondition(store.node_id === "blue-green-proof-1", "rqlite node identity does not match the child process", { node_id: store.node_id });
    assertCondition(Number(os.pid) === server.process.pid, "rqlite status PID does not match the child process", { status_pid: os.pid, child_pid: server.process.pid });
    await initializeSchema(baseUrl);

    const concurrentA = "concurrent-a";
    const concurrentB = "concurrent-b";
    const concurrentARequest = requestDigestFor(concurrentA, "blue", "shared");
    const concurrentBRequest = requestDigestFor(concurrentB, "green", "shared");
    await Promise.all([
      execute(baseUrl, conditionalWriteStatements(concurrentA, concurrentARequest, "blue", "shared")),
      execute(baseUrl, conditionalWriteStatements(concurrentB, concurrentBRequest, "green", "shared")),
    ]);
    const shared = await readSlot(baseUrl, "shared");
    const concurrentOperations = [await readOperation(baseUrl, concurrentA), await readOperation(baseUrl, concurrentB)];
    const sharedEffects = (await readEffectCount(baseUrl, concurrentA)) + (await readEffectCount(baseUrl, concurrentB));
    const concurrentSuccesses = concurrentOperations.filter(operation => operation?.outcome === "success").length;
    const concurrentConflicts = concurrentOperations.filter(operation => operation?.outcome === "conflict").length;
    assertCondition(concurrentSuccesses === 1 && concurrentConflicts === 1, "concurrent condition writes did not produce one success and one conflict", { concurrentOperations });
    assertCondition(shared.generation === 1 && sharedEffects === 1, "concurrent condition writes changed shared state more than once", { shared, sharedEffects });

    const partialOperation = "partial-failure";
    const partialDigest = requestDigestFor(partialOperation, "partial", "partial");
    let rollbackError: JsonObject | null = null;
    try {
      await execute(baseUrl, [
        `INSERT OR IGNORE INTO operations(operation_id, request_digest, outcome, response_json) VALUES (${sqlText(partialOperation)}, ${sqlText(partialDigest)}, 'pending', NULL)`,
        `UPDATE slots SET generation = generation + 1, owner = 'partial', last_operation_id = ${sqlText(partialOperation)} WHERE slot_id = 'partial' AND generation = 0`,
        "INSERT INTO failure_guard(id) VALUES ('fixed')",
      ]);
    } catch (error) {
      assertCondition(error instanceof HttpFailure, "expected the failing transaction to return an HTTP/statement error");
      rollbackError = error.detail;
    }
    assertCondition(rollbackError !== null, "the deliberately failing transaction unexpectedly succeeded");
    const partial = await readSlot(baseUrl, "partial");
    const partialOperationRow = await readOperation(baseUrl, partialOperation);
    const partialEffects = await readEffectCount(baseUrl, partialOperation);
    assertCondition(partial.generation === 0 && partial.owner === null && partial.last_operation_id === null, "failed transaction left partial state", { partial });
    assertCondition(partialOperationRow === null && partialEffects === 0, "failed transaction left a journal or effect row", { partialOperationRow, partialEffects });

    const lostOperation = "lost-response";
    const lostDigest = requestDigestFor(lostOperation, "lost", "lost");
    const proxy = await startDropProxy(runtimeOptions.httpPort);
    let transportObservation = "";
    let proxyObservation: ProxyOutcome;
    try {
      transportObservation = await postAndLoseResponse(proxy.port, conditionalWriteStatements(lostOperation, lostDigest, "lost", "lost"));
      proxyObservation = await proxy.outcome;
      assertCondition(proxyObservation.complete, "response-drop proxy did not observe a complete upstream response before closing the client", { proxyObservation });
    } finally {
      await proxy.close();
    }
    const lostAfterDisconnect = await readOperation(baseUrl, lostOperation);
    const lostSlotAfterDisconnect = await readSlot(baseUrl, "lost");
    const lostEffectsAfterDisconnect = await readEffectCount(baseUrl, lostOperation);
    assertCondition(lostAfterDisconnect?.outcome === "success", "lost response did not leave a recoverable successful operation", { lostAfterDisconnect });
    assertCondition(lostSlotAfterDisconnect.generation === 1 && lostEffectsAfterDisconnect === 1, "lost response did not leave exactly one durable effect", { lostSlotAfterDisconnect, lostEffectsAfterDisconnect });

    await execute(baseUrl, conditionalWriteStatements(lostOperation, lostDigest, "lost", "lost"));
    const lostAfterRetry = await readOperation(baseUrl, lostOperation);
    const lostSlotAfterRetry = await readSlot(baseUrl, "lost");
    const lostEffectsAfterRetry = await readEffectCount(baseUrl, lostOperation);
    assertCondition(lostAfterRetry?.outcome === "success", "same operation ID retry changed the recovered outcome", { lostAfterRetry });
    assertCondition(lostSlotAfterRetry.generation === 1 && lostEffectsAfterRetry === 1, "same operation ID retry duplicated the durable effect", { lostSlotAfterRetry, lostEffectsAfterRetry });

    let conflictError: JsonObject | null = null;
    try {
      await execute(baseUrl, conditionalWriteStatements(lostOperation, requestDigestFor(lostOperation, "different-owner", "lost"), "different-owner", "lost"));
    } catch (error) {
      assertCondition(error instanceof HttpFailure, "expected a mismatched operation retry to return an HTTP/statement error");
      conflictError = error.detail;
    }
    assertCondition(conflictError !== null, "a mismatched operation retry unexpectedly succeeded");
    const lostAfterConflict = await readOperation(baseUrl, lostOperation);
    const lostSlotAfterConflict = await readSlot(baseUrl, "lost");
    const lostEffectsAfterConflict = await readEffectCount(baseUrl, lostOperation);
    assertCondition(lostAfterConflict?.request_digest === lostAfterRetry?.request_digest && lostAfterConflict?.outcome === "success", "mismatched operation retry changed the original journal row", { lostAfterRetry, lostAfterConflict });
    assertCondition(lostSlotAfterConflict.generation === 1 && lostEffectsAfterConflict === 1, "mismatched operation retry changed durable state", { lostSlotAfterConflict, lostEffectsAfterConflict });

    if (interrupted) throw new ProofFailure(`interrupted by ${interrupted}`);
    return {
      schema_version: 1,
      experiment: "rqlite-request-level-conditional-atomicity",
      target: {
        rqlite_version: TARGET_VERSION,
        deployment: "single-node loopback process inside the session-owned Agent VM",
        write_endpoint: "POST /db/execute?transaction",
        read_endpoint: "GET /db/query?level=strong",
        queued_writes: "not used",
      },
      actual_observations: {
        version: { status_build_version: build.version },
        concurrent_condition_writes: {
          operation_outcomes: concurrentOperations,
          successful_operations: concurrentSuccesses,
          conflict_operations: concurrentConflicts,
          final_slot: shared,
          durable_effect_rows: sharedEffects,
          result: "observed_one_success_one_conflict_and_one_effect",
        },
        failed_transaction: {
          error: rollbackError,
          final_slot: partial,
          operation_row: partialOperationRow,
          durable_effect_rows: partialEffects,
          result: "observed_rollback_without_partial_state_or_journal",
        },
        lost_response_recovery: {
          transport: transportObservation,
          proxy: proxyObservation,
          after_disconnect: { operation: lostAfterDisconnect, slot: lostSlotAfterDisconnect, durable_effect_rows: lostEffectsAfterDisconnect },
          after_same_operation_retry: { operation: lostAfterRetry, slot: lostSlotAfterRetry, durable_effect_rows: lostEffectsAfterRetry },
          result: "observed_recovery_without_duplicate_durable_effect",
        },
        mismatched_operation_retry: {
          error: conflictError,
          preserved_operation: lostAfterConflict,
          preserved_slot: lostSlotAfterConflict,
          durable_effect_rows: lostEffectsAfterConflict,
          result: "observed_rejection_without_mutating_original_operation",
        },
      },
      inferences: [
        "For this schema, a single non-queued transaction can combine an operation insert, a conditional state update, a trigger-backed durable effect, and operation finalization without exposing an intermediate commit to a concurrent request.",
        "The operation status predicate and operation_id-keyed effect row make a retry of the same request a read-like no-op after the original transaction committed.",
      ],
      not_proven: [
        "This is a single-node test and does not prove cluster failover, leader change, or crash-at-commit behavior for a multi-node deployment.",
        "The effects table is a database-visible surrogate. The transaction does not atomically include an external Incus/native-worker side effect; that boundary still requires the journal and worker-reconciliation contract described by the architecture.",
        "The result does not prove every Agent VM state transition can use this SQL shape; each transition still needs its own schema, preconditions, and recovery test.",
      ],
      verdict: "db_level_contract_observed_with_external_effect_scope_limit",
    };
  } finally {
    try {
      await server.stop();
    } finally {
      process.off("SIGINT", onSignal);
      process.off("SIGTERM", onSignal);
      writeFileSync(join(options.proofDir, "rqlite.log"), server.log.join(""));
    }
  }
}

async function main(): Promise<number> {
  const options = parseArgs(process.argv.slice(2));
  try {
    const evidence = await run(options);
    writeFileSync(join(options.proofDir, "evidence.json"), `${JSON.stringify(evidence, null, 2)}\n`);
    process.stdout.write(`${JSON.stringify(evidence, null, 2)}\n`);
    return 0;
  } catch (error) {
    const failure = {
      schema_version: 1,
      experiment: "rqlite-request-level-conditional-atomicity",
      verdict: "failed",
      error: error instanceof ProofFailure ? { message: error.message, detail: error.detail } : { message: String(error) },
    };
    process.stdout.write(`${JSON.stringify(failure, null, 2)}\n`);
    return 1;
  }
}

if (import.meta.main) process.exitCode = await main();
