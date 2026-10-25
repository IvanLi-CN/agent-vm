import { describe, expect, spyOn, test } from "bun:test";
import { Database, SQLiteError } from "bun:sqlite";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  appendFileSync,
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  ControlError,
  JsonFloat,
  TASK_STATES,
  add,
  allocation_limit,
  canonical_json,
  compare,
  digest as request_digest,
  floor_div,
  mul,
  parse_json,
  require_int,
  sub,
  type Data,
} from "../scripts/control-types.ts";
import { StateStore } from "../scripts/control-store.ts";
import {
  object_lock_path,
  with_lock,
} from "../scripts/control-native.ts";
import * as Manifest from "../scripts/release-manifest.ts";

// Do not make these tests concurrent: native process-ownership assertions
// intentionally exercise one isolated command tree per Bun worker.
const LINUX = process.platform === "linux";
const POSIX_NATIVE = LINUX || process.platform === "darwin";
const NATIVE_URL = new URL(
  "../scripts/control-native.ts",
  import.meta.url,
).href;
const NOW = "2026-01-01T00:00:00+00:00";
const GENERATION = 9007199254740993n;
const SQLITE_MAX = 9223372036854775807n;
const HUGE = 10n ** 80n + 123n;
const sha256 = (value: string | Buffer): string =>
  createHash("sha256").update(value).digest("hex");

function caught(fn: () => unknown): unknown {
  try {
    fn();
  } catch (error) {
    return error;
  }
  throw new Error("expected an exception");
}

function controlFailure(fn: () => unknown, code: string): ControlError {
  const error = caught(fn);
  expect(error).toBeInstanceOf(ControlError);
  if (!(error instanceof ControlError)) throw error;
  expect(error.code).toBe(code);
  return error;
}

async function within<T>(
  promise: Promise<T>,
  milliseconds: number,
  label: string,
): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error(`${label} exceeded ${milliseconds}ms`)),
          milliseconds,
        );
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

async function waitFor(
  predicate: () => boolean,
  label: string,
  milliseconds = 5000,
): Promise<void> {
  const deadline = performance.now() + milliseconds;
  while (!predicate()) {
    if (performance.now() >= deadline) {
      throw new Error(`timed out waiting for ${label}`);
    }
    await Bun.sleep(10);
  }
}

type ProcessIdentity = {
  pid: number;
  start: string;
  state: string;
  parent: number;
  group: number;
  session: number;
};

function identity(pid: number): ProcessIdentity | null {
  try {
    const raw = readFileSync(`/proc/${pid}/stat`, "utf8");
    const end = raw.lastIndexOf(")");
    const fields = raw.slice(end + 2).trim().split(/\s+/);
    if (end < 0 || fields.length < 20) {
      throw new Error(`invalid /proc stat for ${pid}`);
    }
    return {
      pid,
      start: fields[19]!,
      state: fields[0]!,
      parent: Number(fields[1]),
      group: Number(fields[2]),
      session: Number(fields[3]),
    };
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT" || code === "ESRCH") return null;
    throw error;
  }
}

function recordedProcesses(root: string): ProcessIdentity[] {
  return readdirSync(root)
    .filter((name) => name.endsWith(".pid.json"))
    .map((name) =>
      JSON.parse(readFileSync(join(root, name), "utf8")) as ProcessIdentity
    );
}

function ownedProcessAlive(record: ProcessIdentity): boolean {
  const current = identity(record.pid);
  return (
    current !== null &&
    current.start === record.start &&
    current.state !== "Z" &&
    current.state !== "X"
  );
}

function killOwnedProcess(record: ProcessIdentity): void {
  if (!ownedProcessAlive(record)) return;
  try {
    process.kill(record.pid, "SIGKILL");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
  }
}

function startBun(root: string, script: string, args: string[]) {
  const child = Bun.spawn({
    cmd: [process.execPath, script, ...args],
    cwd: root,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  const done = Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]).then(([exitCode, stdout, stderr]) => ({
    exitCode,
    stdout,
    stderr,
  }));
  return { child, done };
}

type Job = ReturnType<typeof startBun>;
type Sandbox = {
  root: string;
  start(source: string, args?: string[]): Job;
};

async function sandbox<T>(
  fn: (context: Sandbox) => Promise<T> | T,
): Promise<T> {
  const root = mkdtempSync(join(tmpdir(), "agent-vm-control-kernel-"));
  chmodSync(root, 0o700);
  const jobs: Job[] = [];

  try {
    return await fn({
      root,
      start(source, args = []) {
        const script = join(root, `worker-${jobs.length}.ts`);
        writeFileSync(script, source, { mode: 0o600 });
        const job = startBun(root, script, args);
        jobs.push(job);
        return job;
      },
    });
  } finally {
    try {
      // These are exact child handles, never process-name or broad group kills.
      for (const job of jobs) {
        if (
          job.child.exitCode === null &&
          job.child.signalCode === null
        ) {
          job.child.kill("SIGKILL");
        }
      }

      if (LINUX) {
        for (const record of recordedProcesses(root)) {
          killOwnedProcess(record);
        }
      }

      await Promise.all(
        jobs.map((job) => within(job.done, 5000, "worker cleanup")),
      );

      if (LINUX) {
        // Rescan after leaders stop so late-published descendants are covered.
        const records = recordedProcesses(root);
        for (const record of records) killOwnedProcess(record);
        await waitFor(
          () => records.every((record) => !ownedProcessAlive(record)),
          "test-owned processes to stop",
        );
      }
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  }
}

async function successfulJob(job: Job, milliseconds = 10000) {
  const result = await within(job.done, milliseconds, "Bun worker");
  expect(result.stderr).toBe("");
  expect(result.exitCode).toBe(0);
  return result;
}

async function withStore(
  fn: (store: StateStore, root: string) => Promise<void> | void,
): Promise<void> {
  await sandbox(async ({ root }) => {
    const store = new StateStore(join(root, "state.sqlite3"));
    try {
      await fn(store, root);
    } finally {
      store.close();
    }
  });
}

function task(task_id = "task-a", state = "active"): Data {
  return {
    task_id,
    session_id: `session:${task_id}`,
    generation: GENERATION,
    state,
    instance_name: `agent-${task_id}`,
    vm_ip: "192.0.2.10",
    ssh_host_port: 22001,
    lease_expires_at: "2026-01-01T01:00:00+00:00",
    inactive_since: null,
    cleanup_after: null,
    created_at: NOW,
    updated_at: NOW,
    last_error: null,
    image_fingerprint: "a".repeat(64),
    resources_json: canonical_json({
      cpu: 2,
      memory_mib: 4096,
      disk_gib: HUGE,
    }),
    reservation_bytes: SQLITE_MAX,
    previous_generation: 42,
    resource_reservation_json: "{}",
  };
}

function pythonBaseline(
  source: string,
  args: string[] = [],
  input?: string,
): string {
  // Python is permitted only for independent legacy fixture/digest baselines.
  // Never import or execute the Python controller.
  const result = spawnSync("python3", ["-I", "-c", source, ...args], {
    input,
    encoding: "utf8",
    timeout: 10000,
    maxBuffer: 4 * 1024 * 1024,
  });
  expect(result.error).toBeUndefined();
  expect(result.signal).toBeNull();
  expect(result.status, result.stderr).toBe(0);
  expect(result.stderr).toBe("");
  return result.stdout;
}

const LEGACY_SQLITE_FIXTURE = String.raw`
import hashlib
import json
import sqlite3
import sys

db = sqlite3.connect(sys.argv[1])
db.executescript("""
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
    task_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL UNIQUE,
    generation INTEGER NOT NULL,
    state TEXT NOT NULL,
    instance_name TEXT NOT NULL UNIQUE,
    vm_ip TEXT,
    ssh_host_port INTEGER,
    lease_expires_at TEXT,
    inactive_since TEXT,
    cleanup_after TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_error TEXT,
    image_fingerprint TEXT NOT NULL,
    resources_json TEXT NOT NULL DEFAULT '{}',
    reservation_bytes INTEGER NOT NULL DEFAULT 0,
    previous_generation INTEGER,
    resource_reservation_json TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS ports (
    task_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    port_name TEXT NOT NULL,
    protocol TEXT NOT NULL,
    host_port INTEGER NOT NULL,
    vm_port INTEGER NOT NULL,
    device_name TEXT NOT NULL,
    state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (task_id, port_name, protocol),
    UNIQUE (host_port, protocol),
    FOREIGN KEY (task_id) REFERENCES tasks(task_id)
);
CREATE TABLE IF NOT EXISTS operations (
    operation_id TEXT PRIMARY KEY,
    action TEXT NOT NULL,
    session_id TEXT,
    generation INTEGER,
    response_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    action TEXT NOT NULL,
    task_id TEXT,
    session_id TEXT,
    generation INTEGER,
    outcome TEXT NOT NULL,
    detail_json TEXT,
    created_at TEXT NOT NULL
);
""")
now = "2026-01-01T00:00:00+00:00"
generation = 9007199254740993
request = {
    "protocol": 4,
    "action": "acquire",
    "operation_id": "legacy-operation",
    "session_id": "session:legacy",
    "generation": generation,
    "label": "\u5fc3\u7fbd \U0001f408",
    "ratio": 1.0,
    "disk_gib": 10 ** 80 + 123,
}
request_digest = hashlib.sha256(
    json.dumps(request, sort_keys=True).encode()
).hexdigest()
task = {
    "task_id": "legacy",
    "session_id": "session:legacy",
    "generation": generation,
    "state": "active",
    "instance_name": "agent-legacy",
    "vm_ip": "192.0.2.10",
    "ssh_host_port": 22001,
    "lease_expires_at": "2026-01-01T01:00:00+00:00",
    "inactive_since": None,
    "cleanup_after": None,
    "created_at": now,
    "updated_at": now,
    "last_error": None,
    "image_fingerprint": "a" * 64,
    "resources_json": json.dumps({
        "cpu": 2, "memory_mib": 4096, "disk_gib": 10 ** 80 + 123
    }, sort_keys=True),
    "reservation_bytes": 9223372036854775807,
    "previous_generation": 42,
    "resource_reservation_json": "{}",
}
db.execute(
    "INSERT INTO tasks (" + ", ".join(task) + ") VALUES (" +
    ", ".join("?" for _ in task) + ")",
    list(task.values()),
)
db.execute("INSERT INTO meta VALUES (?, ?)", ("draining", "1"))
db.execute(
    "INSERT INTO ports VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
    ("legacy", generation, "ssh", "tcp", 22001, 22, "ssh", "active", now),
)
db.execute(
    "INSERT INTO operations VALUES (?, ?, ?, ?, ?, ?)",
    ("legacy-operation", "acquire", "session:legacy", generation,
     json.dumps({"outcome": "pending", "_request_digest": request_digest},
                sort_keys=True), now),
)
db.execute(
    "INSERT INTO audit "
    "(action, task_id, session_id, generation, outcome, detail_json, created_at) "
    "VALUES (?, ?, ?, ?, ?, ?, ?)",
    ("acquire", "legacy", "session:legacy", generation, "success",
     json.dumps({"message": "\u65e7\u8bb0\u5f55"}, sort_keys=True), now),
)
db.commit()
schema = [
    [kind, name, table, " ".join(sql.split())]
    for kind, name, table, sql in db.execute(
        "SELECT type, name, tbl_name, sql FROM sqlite_master "
        "WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name"
    )
]
db.close()
print(json.dumps({
    "schema": schema,
    "request": json.dumps(request),
    "digest": request_digest,
}))
`;

function schema(store: StateStore): unknown[][] {
  return store._rows(
    "SELECT type, name, tbl_name, sql FROM sqlite_master " +
      "WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name",
  ).map((row) => [
    row.type,
    row.name,
    row.tbl_name,
    String(row.sql).replace(/\s+/g, " ").trim(),
  ]);
}

describe("control-types: legacy numeric and request digest behavior", () => {
  test("request_digest exactly matches old sorted json.dumps bytes", () => {
    const cases = [
      String.raw`{"z":1,"a":{"b":true,"a":null},"items":[3,2,1]}`,
      String.raw`{"generation":9007199254740993,"negative":-9223372036854775809}`,
      `{"huge":${HUGE},"negative":${-HUGE}}`,
      String.raw`{"value":1}`,
      String.raw`{"value":1.0}`,
      String.raw`{"value":1e0}`,
      String.raw`{"value":-0.0}`,
      String.raw`{"floats":[0.0001,0.00001,1e-7,1e15,1e16,1e23,1.2345678901234567,5e-324,1.7976931348623157e308]}`,
      String.raw`{"floats":[9007199254740993.0,NaN,Infinity,-Infinity]}`,
      String.raw`{"\u5fc3\u7fbd":"\u732b \ud83d\udc08","\u00e9":"e\u0301","\ue000":1,"\ud800\udc00":2}`,
      String.raw`{"escape":"\b\t\n\f\r\"\\/\u007f","surrogate":"\ud800"}`,
      String.raw`{"__proto__":{"polluted":true},"constructor":"literal"}`,
    ];
    const expected = JSON.parse(
      pythonBaseline(
        String.raw`
import hashlib
import json
import sys
result = []
for source in json.load(sys.stdin):
    value = json.loads(source)
    canonical = json.dumps(value, sort_keys=True)
    result.append({
        "canonical": canonical,
        "digest": hashlib.sha256(canonical.encode()).hexdigest(),
    })
print(json.dumps(result))
`,
        [],
        JSON.stringify(cases),
      ),
    ) as Array<{ canonical: string; digest: string }>;

    expect(expected).toHaveLength(cases.length);
    for (const [index, source] of cases.entries()) {
      const value = parse_json(source);
      expect(canonical_json(value)).toBe(expected[index]!.canonical);
      expect(request_digest(value)).toBe(expected[index]!.digest);
      expect(request_digest(parse_json(canonical_json(value)))).toBe(
        expected[index]!.digest,
      );
    }

    expect(request_digest(parse_json(cases[3]!))).not.toBe(
      request_digest(parse_json(cases[4]!)),
    );
    expect(request_digest(parse_json(cases[4]!))).toBe(
      request_digest(parse_json(cases[5]!)),
    );
    expect(Object.hasOwn(parse_json(cases[11]!), "__proto__")).toBe(true);
    expect(Object.hasOwn({}, "polluted")).toBe(false);
  });

  test("integer fields retain arbitrary precision and reject JSON floats", () => {
    expect(parse_json("9007199254740991")).toBe(Number.MAX_SAFE_INTEGER);
    expect(parse_json("9007199254740992")).toBe(9007199254740992n);
    expect(parse_json(String(HUGE))).toBe(HUGE);
    expect(parse_json("1.0")).toBeInstanceOf(JsonFloat);
    expect(parse_json("1e0")).toBeInstanceOf(JsonFloat);
    expect(require_int(HUGE, "generation", 0)).toBe(HUGE);

    for (const value of [
      true,
      false,
      "1",
      null,
      1.5,
      Number.MAX_SAFE_INTEGER + 1,
      parse_json("1.0"),
      parse_json("1e0"),
      parse_json("NaN"),
      parse_json("Infinity"),
    ]) {
      controlFailure(() => require_int(value, "generation"), "invalid_request");
    }
    controlFailure(() => require_int(-1, "generation", 0), "invalid_request");
    controlFailure(() => require_int(11, "port", 1, 10), "invalid_request");

    expect(add(HUGE, 1)).toBe(HUGE + 1n);
    expect(sub(HUGE, HUGE)).toBe(0);
    expect(mul(HUGE, 3)).toBe(HUGE * 3n);
    expect(compare(HUGE, HUGE - 1n)).toBe(1);
    expect(floor_div(-7, 3)).toBe(-3);
    expect(floor_div(7, -3)).toBe(-3);
    expect(floor_div(-7, -3)).toBe(2);
    expect(() => floor_div(1, 0)).toThrow(RangeError);
  });

  test("fractional overcommit uses decimal flooring without float multiplication", () => {
    expect(allocation_limit(15, parse_json("1.1"))).toBe(16);
    expect(allocation_limit(12288, parse_json("1.01"))).toBe(12410);
    expect(allocation_limit(HUGE, parse_json("1.01"))).toBe(
      HUGE * 101n / 100n,
    );

    for (const ratio of [
      true,
      "4",
      0.9,
      parse_json("NaN"),
      parse_json("Infinity"),
    ]) {
      controlFailure(
        () => allocation_limit(15, ratio, "cpu_overcommit_ratio"),
        "config_invalid",
      );
    }
  });

  test("malformed JSON is rejected instead of partially consumed", () => {
    for (const source of [
      "",
      "01",
      "1 trailing",
      "[1,]",
      '{"a":1,}',
      '{"a":}',
      '"unterminated',
    ]) {
      expect(() => parse_json(source)).toThrow(SyntaxError);
    }
  });
});

describe("control-store: legacy schema and durable transaction behavior", () => {
  test("reads a Python-created legacy database without changing its schema", async () => {
    await sandbox(({ root }) => {
      const path = join(root, "legacy.sqlite3");
      const baseline = JSON.parse(
        pythonBaseline(LEGACY_SQLITE_FIXTURE, [path]),
      ) as {
        schema: unknown[][];
        request: string;
        digest: string;
      };
      const store = new StateStore(path);
      const fresh = new StateStore(join(root, "fresh.sqlite3"));

      try {
        expect(schema(store)).toEqual(baseline.schema);
        expect(schema(fresh)).toEqual(baseline.schema);
        expect(store.get_meta("draining")).toBe("1");
        expect(store.get_task({ task_id: "legacy" })).toEqual(task("legacy"));
        expect(
          store.get_task({ session_id: "session:legacy" })?.generation,
        ).toBe(GENERATION);
        expect(store.get_ports("legacy")[0]).toMatchObject({
          generation: GENERATION,
          host_port: 22001,
          vm_port: 22,
          state: "active",
        });
        expect(request_digest(parse_json(baseline.request))).toBe(
          baseline.digest,
        );
        expect(store.operation("legacy-operation")).toEqual({
          outcome: "pending",
          _request_digest: baseline.digest,
        });
        expect(store.pending_operations()).toEqual([{
          operation_id: "legacy-operation",
          action: "acquire",
          session_id: "session:legacy",
          generation: GENERATION,
          created_at: NOW,
        }]);
        expect(
          parse_json(store._row("SELECT detail_json FROM audit")!.detail_json),
        ).toEqual({ message: "旧记录" });

        store.update_task("legacy", { state: "cached" });
        expect(store.get_task({ task_id: "legacy" })?.state).toBe("cached");
        expect(schema(store)).toEqual(baseline.schema);
        expect(statSync(path).mode & 0o777).toBe(0o600);
        expect(
          store._row("PRAGMA journal_mode")?.journal_mode,
        ).toBe("wal");
      } finally {
        fresh.close();
        store.close();
      }
    });
  });

  test("rolls back all nested task, port, journal and audit writes", async () => {
    await withStore((store) => {
      const original = new ControlError("native_failed", "native failed");
      expect(caught(() => store.transaction(() => {
        store.put_task(task());
        store.put_port({
          task_id: "task-a",
          generation: GENERATION,
          port_name: "ssh",
          protocol: "tcp",
          host_port: 22001,
          vm_port: 22,
          device_name: "ssh",
          state: "active",
          created_at: NOW,
        });
        store.record_operation("op", "acquire", "session:task-a", GENERATION, {
          outcome: "pending",
          _request_digest: "legacy-digest",
        });
        store.audit("acquire", "pending", { task_id: "task-a" });
        store.set_meta("temporary", "1");
        throw original;
      }))).toBe(original);

      expect(store.depth).toBe(0);
      expect(store.connection.inTransaction).toBe(false);
      expect(store.list_tasks()).toEqual([]);
      expect(store.get_ports("task-a")).toEqual([]);
      expect(store.operation("op")).toBeNull();
      expect(store._rows("SELECT * FROM audit")).toEqual([]);
      expect(store.get_meta("temporary")).toBeNull();

      store.transaction(() => {
        store.set_meta("outer", "1");
        store.transaction(() => store.set_meta("inner", "1"));
      });
      expect(store.get_meta("outer")).toBe("1");
      expect(store.get_meta("inner")).toBe("1");
    });
  });

  test("rejects asynchronous transaction callbacks and restores usability", async () => {
    await withStore((store) => {
      expect(() => store.transaction(() => {
        store.set_meta("temporary", "1");
        return Promise.resolve("unsupported");
      })).toThrow("transaction callback must be synchronous");
      expect(store.get_meta("temporary")).toBeNull();
      expect(store.depth).toBe(0);
      expect(store.connection.inTransaction).toBe(false);
      store.set_meta("after", "ok");
      expect(store.get_meta("after")).toBe("ok");
    });
  });

  test("a real SQLite constraint failure rolls back the outer transaction", async () => {
    await withStore((store) => {
      const error = controlFailure(() => store.transaction(() => {
        store.put_task(task());
        store.put_task({
          ...task("task-b"),
          session_id: "session:task-a",
        });
      }), "sqlite_unavailable");
      expect(error.detail).toMatchObject({
        phase: "write",
        sql_operation: "write",
        state_committed: false,
      });
      expect(error.detail.sqlite_name).toMatch(/^SQLITE_CONSTRAINT/);
      expect(store.list_tasks()).toEqual([]);
      expect(store.depth).toBe(0);
      expect(store.connection.inTransaction).toBe(false);
      store.set_meta("after", "ok");
    });
  });

  test("commit failures preserve pending journals and report unknown outcome", async () => {
    for (const commitActuallyHappened of [false, true]) {
      await withStore((store, root) => {
        store.record_operation("pending", "drain-enter", null, 0, {
          outcome: "pending",
          _request_digest: "original-digest",
        });
        const injected = caught(() =>
          store.connection.exec("SELECT * FROM missing_commit_fixture_table")
        );
        expect(injected).toBeInstanceOf(SQLiteError);
        const exec = store.connection.exec.bind(store.connection);
        const fault = spyOn(store.connection, "exec").mockImplementation(
          (...args: Parameters<typeof store.connection.exec>) => {
            if (args[0] === "COMMIT") {
              if (commitActuallyHappened) exec(...args);
              throw injected;
            }
            return exec(...args);
          },
        );

        try {
          const error = controlFailure(
            () => store.set_meta("attempted", "1"),
            "sqlite_unavailable",
          );
          expect(error.detail).toMatchObject({
            phase: "commit",
            sql_operation: "write",
            state_committed: null,
          });
        } finally {
          fault.mockRestore();
        }

        expect(store.depth).toBe(0);
        expect(store.connection.inTransaction).toBe(false);
        expect(store.operation("pending")).toEqual({
          outcome: "pending",
          _request_digest: "original-digest",
        });

        const reopened = new StateStore(join(root, "state.sqlite3"));
        try {
          expect(reopened.get_meta("attempted")).toBe(
            commitActuallyHappened ? "1" : null,
          );
          expect(reopened.operation("pending")?.outcome).toBe("pending");
        } finally {
          reopened.close();
        }
      });
    }
  });

  test("rollback failures make the connection unusable and preserve error identity", async () => {
    for (const sqliteWriteFailure of [false, true]) {
      await withStore((store) => {
        const injected = caught(() =>
          store.connection.exec("SELECT * FROM missing_rollback_fixture_table")
        );
        expect(injected).toBeInstanceOf(SQLiteError);
        const original = new ControlError("native_failed", "native failed");
        const exec = store.connection.exec.bind(store.connection);
        const fault = spyOn(store.connection, "exec").mockImplementation(
          (...args: Parameters<typeof store.connection.exec>) => {
            if (args[0] === "ROLLBACK") throw injected;
            return exec(...args);
          },
        );

        try {
          const error = controlFailure(() => store.transaction(() => {
            store.set_meta("uncommitted", "1");
            if (sqliteWriteFailure) store.connection.exec("SELECT * FROM missing_write_fixture_table");
            throw original;
          }), "sqlite_unavailable");

          expect(error.detail.state_committed).toBeNull();
          expect(error.detail.phase).toBe(
            sqliteWriteFailure ? "write" : "rollback",
          );
          if (sqliteWriteFailure) {
            expect(error.detail.rollback_error.sqlite_name).toMatch(/^SQLITE_/);
          } else {
            expect(error.detail.original_error).toEqual({
              type: "ControlError",
              message: "native failed",
            });
            expect(error.cause).toBe(original);
          }

          expect(store._unusable).toBe(true);
          expect(store.depth).toBe(0);
          expect(
            controlFailure(() => store.get_meta("uncommitted"), "sqlite_unavailable")
              .detail.state_committed,
          ).toBeNull();
          controlFailure(
            () => store.set_meta("later", "1"),
            "sqlite_unavailable",
          );
        } finally {
          fault.mockRestore();
          if (store.connection.inTransaction) exec("ROLLBACK");
        }
      });
    }
  });

  test("retains pending and unknown task states without inventing success", async () => {
    await withStore((store) => {
      store.put_task(task("unknown", "mystery"));
      store.record_operation("pending", "acquire", "session:unknown", GENERATION, {
        outcome: "pending",
        _request_digest: "original-digest",
      });
      store.record_operation("error", "release", null, null, {
        outcome: "error",
        error: { code: "native_failed", message: "native failed" },
        _request_digest: "error-digest",
      });

      // The store preserves raw state; lifecycle validation owns fail-closed
      // decisions about whether this task can be changed.
      expect(store.get_task({ task_id: "unknown" })?.state).toBe("mystery");
      expect(TASK_STATES.has("mystery")).toBe(false);
      expect(store.operation("pending")?.outcome).toBe("pending");
      expect(store.pending_operations().map((row) => row.operation_id)).toEqual([
        "pending",
      ]);

      for (const value of [
        "[]",
        "{",
        '{"outcome":"pending"}',
        '{"outcome":"future","_request_digest":"x"}',
        '{"outcome":"error","_request_digest":"x","error":{"code":"x"}}',
        '{"outcome":"success","_request_digest":"x","ok":true}',
      ]) {
        store.connection.query(
          "UPDATE operations SET response_json = ? WHERE operation_id = ?",
        ).run(value, "pending");
        controlFailure(
          () => store.operation("pending"),
          "operation_unverified",
        );
      }
      store.connection.query(
        "UPDATE operations SET response_json = ? WHERE operation_id = ?",
      ).run("{", "pending");
      controlFailure(
        () => store.pending_operations(),
        "operation_unverified",
      );
      expect(store.get_task({ task_id: "unknown" })?.state).toBe("mystery");
    });
  });

  test("corrupt database bytes produce a structured WAL initialization failure", async () => {
    await sandbox(({ root }) => {
      const path = join(root, "corrupt.sqlite3");
      writeFileSync(path, "not a sqlite database");
      const error = controlFailure(
        () => new StateStore(path),
        "sqlite_unavailable",
      );
      expect(error.detail).toMatchObject({
        phase: "wal-init",
        sql_operation: "initialize",
        state_committed: false,
      });
      expect(error.detail.sqlite_name).toMatch(/^SQLITE_/);
    });
  });

  test("an unsupported checksummed WAL fails closed during initialization", async () => {
    await sandbox(async ({ root, start }) => {
      const path = join(root, "wal.sqlite3");
      const initial = new StateStore(path);
      initial.set_meta("baseline", "ok");
      initial.close();

      const writer = start(`
        import { Database } from "bun:sqlite";
        import { writeFileSync, readFileSync } from "node:fs";
        import { join } from "node:path";
        const root = process.argv[2]!;
        const db = new Database(join(root, "wal.sqlite3"));
        db.exec("PRAGMA journal_mode=WAL");
        db.exec("PRAGMA wal_autocheckpoint=0");
        db.query("INSERT INTO meta VALUES (?, ?)").run("wal-only", "committed");
        writeFileSync(join(root, "wal.snapshot"), readFileSync(join(root, "wal.sqlite3-wal")));
        writeFileSync(join(root, "wal.ready"), "ready");
        setInterval(() => { void db.inTransaction; }, 1000);
      `, [root]);

      await waitFor(() => existsSync(join(root, "wal.ready")), "committed WAL");
      writer.child.kill("SIGKILL");
      await within(writer.done, 5000, "crashed WAL writer");

      const walPath = `${path}-wal`;
      const wal = readFileSync(join(root, "wal.snapshot"));
      expect(wal.length).toBeGreaterThan(32);
      const magic = wal.readUInt32BE(0);
      expect([0x377f0682, 0x377f0683]).toContain(magic);

      // Random WAL bytes can legitimately be ignored by SQLite recovery.
      // Preserve a valid header checksum while declaring an unsupported
      // WAL format version, which must not be treated as an empty journal.
      wal.writeUInt32BE(3007001, 4);
      let first = 0;
      let second = 0;
      const word = (offset: number): number =>
        (magic & 1) === 0
          ? wal.readUInt32LE(offset)
          : wal.readUInt32BE(offset);
      for (let offset = 0; offset < 24; offset += 8) {
        first = (first + word(offset) + second) >>> 0;
        second = (second + word(offset + 4) + first) >>> 0;
      }
      wal.writeUInt32BE(first, 24);
      wal.writeUInt32BE(second, 28);
      writeFileSync(walPath, wal);
      rmSync(`${path}-shm`, { force: true });

      let opened: StateStore | undefined;
      try {
        const error = controlFailure(() => {
          opened = new StateStore(path);
        }, "sqlite_unavailable");
        expect(["connect", "wal-init", "schema-init"]).toContain(
          error.detail.phase,
        );
        expect(error.detail.state_committed).toBe(false);
        expect(error.detail.sqlite_name).toMatch(/^SQLITE_/);
      } finally {
        opened?.close();
      }
    });
  }, 20000);
});

describe.skipIf(!POSIX_NATIVE)("native flock: Linux/macOS API behavior", () => {
  test("reentrant locking releases the exact lock after callback failure", async () => {
    await sandbox(async ({ root }) => {
      const path = object_lock_path(root, "task:心羽");
      expect(path).toBe(
        join(root, "locks", sha256("task:心羽")),
      );
      const original = new Error("callback failed");
      let failure: unknown;
      try {
        await with_lock(path, () =>
          with_lock(path, () => {
            throw original;
          }, true, true)
        );
      } catch (error) {
        failure = error;
      }
      expect(failure).toBe(original);
      expect(await with_lock(path, () => "released", false)).toBe("released");
      expect(statSync(path).mode & 0o777).toBe(0o600);
    });
  });
});

const LOCK_WORKER = `
  import { appendFileSync, existsSync, writeFileSync } from "node:fs";
  import { join } from "node:path";
  import { with_lock } from ${JSON.stringify(NATIVE_URL)};
  const [root, name, lockPath, mode] = process.argv.slice(2);
  writeFileSync(join(root, name + ".attempted"), "attempted");
  try {
    await with_lock(lockPath, async () => {
      appendFileSync(join(root, "events"), "enter:" + name + "\\n");
      writeFileSync(join(root, name + ".entered"), "entered");
      if (mode === "hold") {
        const deadline = performance.now() + 15000;
        while (!existsSync(join(root, name + ".release"))) {
          if (performance.now() > deadline) throw new Error("holder gate expired");
          await Bun.sleep(10);
        }
      }
      appendFileSync(join(root, "events"), "exit:" + name + "\\n");
    }, mode !== "probe");
    writeFileSync(join(root, name + ".result"), "ok");
  } catch (error) {
    writeFileSync(join(root, name + ".result"), error.code ?? error.message);
    if (mode !== "probe") throw error;
  }
`;

describe.skipIf(!LINUX)("isolated Linux native: cross-process flock", () => {
  test("independent Bun invocations serialize the same real lock", async () => {
    await sandbox(async ({ root, start }) => {
      const path = object_lock_path(root, "same-object");
      const first = start(LOCK_WORKER, [root, "a", path, "hold"]);
      await waitFor(() => existsSync(join(root, "a.entered")), "first holder");

      const second = start(LOCK_WORKER, [root, "b", path, "once"]);
      await waitFor(
        () => existsSync(join(root, "b.attempted")),
        "blocking contender",
      );

      const probe = start(LOCK_WORKER, [root, "probe", path, "probe"]);
      await successfulJob(probe);
      expect(readFileSync(join(root, "probe.result"), "utf8")).toBe(
        "object_busy",
      );
      expect(existsSync(join(root, "probe.entered"))).toBe(false);

      writeFileSync(join(root, "a.release"), "release");
      await successfulJob(first);
      await successfulJob(second);
      expect(readFileSync(join(root, "events"), "utf8").trim().split("\n"))
        .toEqual(["enter:a", "exit:a", "enter:b", "exit:b"]);
    });
  }, 20000);

  test("separate locks do not block each other while one holder remains active", async () => {
    await sandbox(async ({ root, start }) => {
      const first = start(LOCK_WORKER, [
        root,
        "a",
        object_lock_path(root, "object-a"),
        "hold",
      ]);
      await waitFor(() => existsSync(join(root, "a.entered")), "first holder");

      const second = start(LOCK_WORKER, [
        root,
        "b",
        object_lock_path(root, "object-b"),
        "once",
      ]);
      await successfulJob(second, 5000);
      expect(existsSync(join(root, "a.release"))).toBe(false);
      expect(readFileSync(join(root, "events"), "utf8").trim().split("\n"))
        .toEqual(["enter:a", "enter:b", "exit:b"]);

      writeFileSync(join(root, "a.release"), "release");
      await successfulJob(first);
    });
  }, 20000);
});

// Each process publishes its Linux start-time identity before doing work.
// Assertions inspect these identities before fallback cleanup can hide leaks.
const RECORD_PROCESS = `
  function recordProcess(name) {
    const raw = readFileSync("/proc/self/stat", "utf8");
    const fields = raw.slice(raw.lastIndexOf(")") + 2).trim().split(/\\s+/);
    writeFileSync(join(root, name + ".pid.json"), JSON.stringify({
      pid: process.pid,
      start: fields[19],
      state: fields[0],
      parent: Number(fields[1]),
      group: Number(fields[2]),
      session: Number(fields[3]),
    }));
  }
`;

const DESCENDANT = `
  import { readFileSync, writeFileSync } from "node:fs";
  import { join } from "node:path";
  const root = process.argv[2]!;
  ${RECORD_PROCESS}
  recordProcess("descendant");
  writeFileSync(join(root, "descendant.ready"), "ready");
  setInterval(() => {}, 1000);
`;

const RUNNER_LEADER = `
  import { spawn } from "node:child_process";
  import {
    existsSync, readFileSync, writeFileSync, writeSync,
  } from "node:fs";
  import { join } from "node:path";
  const [root, mode] = process.argv.slice(2);
  ${RECORD_PROCESS}
  recordProcess("leader");

  if (mode === "noisy" || mode === "nonzero" || mode === "noisy-timeout") {
    writeSync(1, "OUT:" + "o".repeat(1024 * 1024) + ":END");
    writeSync(2, "BEGIN:" + "e".repeat(1024 * 1024) + ":ERR");
    if (mode !== "noisy-timeout") process.exit(mode === "nonzero" ? 3 : 0);
  } else if (mode === "partial-timeout") {
    writeSync(1, "partial-out");
    writeSync(2, "partial-err");
  } else {
    const escaped = mode !== "pipes";
    const closed = mode === "orphan-closed";
    const child = spawn(
      process.execPath,
      [join(root, "descendant.ts"), root],
      {
        detached: escaped,
        stdio: closed ? "ignore" : ["ignore", 1, 2],
      },
    );
    child.on("error", (error) => {
      writeSync(2, String(error));
      process.exit(7);
    });
    child.unref();
    const deadline = performance.now() + 5000;
    while (!existsSync(join(root, "descendant.ready"))) {
      if (performance.now() > deadline) process.exit(8);
      await Bun.sleep(10);
    }
    if (mode === "orphan-pipes" || mode === "orphan-closed") {
      await Bun.sleep(200);
      process.exit(0);
    }
  }
  setInterval(() => {}, 1000);
`;

type RunnerEnvelope = {
  ok: boolean;
  code?: string;
  detail?: Data;
  result?: {
    returncode: number;
    stdout: string;
    stderr: string;
    stdout_truncated: boolean;
    stderr_truncated: boolean;
  };
};

async function runnerScenario(
  mode: string,
  options: Data,
  check: (result: RunnerEnvelope, root: string) => void | Promise<void>,
  method: "run" | "run_bounded" = "run_bounded",
): Promise<void> {
  await sandbox(async ({ root, start }) => {
    writeFileSync(join(root, "leader.ts"), RUNNER_LEADER);
    writeFileSync(join(root, "descendant.ts"), DESCENDANT);

    const worker = start(`
      import { readFileSync, writeFileSync } from "node:fs";
      import { join } from "node:path";
      import { CommandRunner } from ${JSON.stringify(NATIVE_URL)};
      const root = process.argv[2]!;
      ${RECORD_PROCESS}
      recordProcess("worker");
      let envelope;
      try {
        const result = await new CommandRunner()[${JSON.stringify(method)}](
          [process.execPath, join(root, "leader.ts"), root, ${JSON.stringify(mode)}],
          ${JSON.stringify(options)},
        );
        envelope = {
          ok: true,
          result: {
            ...result,
            stdout: result.stdout.toString("utf8"),
            stderr: result.stderr.toString("utf8"),
          },
        };
      } catch (error) {
        envelope = { ok: false, code: error.code, detail: error.detail };
      }
      writeFileSync(join(root, "runner-result.json"), JSON.stringify(envelope));
    `, [root]);

    await successfulJob(worker, 12000);
    const result = JSON.parse(
      readFileSync(join(root, "runner-result.json"), "utf8"),
    ) as RunnerEnvelope;

    const records = recordedProcesses(root);
    expect(records.length).toBeGreaterThanOrEqual(2);
    await waitFor(
      () => records.every((record) => !ownedProcessAlive(record)),
      "runner-owned processes to stop",
      2000,
    );
    // This assertion happens before sandbox's emergency cleanup.
    expect(records.filter(ownedProcessAlive)).toEqual([]);
    await check(result, root);
  });
}

function expectCleanTimeout(result: RunnerEnvelope): Data {
  expect(result.ok).toBe(false);
  expect(result.code).toBe("command_timeout");
  expect(result.detail).toMatchObject({
    reaped: true,
    pipes_drained: true,
    process_group_killed: true,
    process_group_cleanup_error: null,
  });
  return result.detail!;
}

describe.skipIf(!LINUX)("isolated Linux native: CommandRunner", () => {
  test("drains noisy pipes while retaining stdout prefix and stderr suffix", async () => {
    await runnerScenario("noisy", {
      timeout: 8,
      stdout_limit: 128,
      stderr_limit: 128,
    }, (envelope) => {
      expect(envelope.ok).toBe(true);
      expect(envelope.result).toEqual({
        returncode: 0,
        stdout: "OUT:" + "o".repeat(124),
        stderr: "e".repeat(124) + ":ERR",
        stdout_truncated: true,
        stderr_truncated: true,
      });
    });
  }, 20000);

  test("nonzero results preserve bounded output with check enabled or disabled", async () => {
    for (const check of [true, false]) {
      await runnerScenario("nonzero", {
        timeout: 8,
        check,
        stdout_limit: 128,
        stderr_limit: 128,
      }, (envelope) => {
        expect(envelope.ok).toBe(!check);
        if (check) expect(envelope.code).toBe("command_failed");
        const result = check ? envelope.detail! : envelope.result!;
        expect(result.returncode).toBe(3);
        expect(result.stdout).toBe("OUT:" + "o".repeat(124));
        expect(result.stderr).toBe("e".repeat(124) + ":ERR");
        expect(result.stdout_truncated).toBe(true);
        expect(result.stderr_truncated).toBe(true);
      });
    }
  }, 30000);

  test("ordinary run times out with partial stdout and stderr intact", async () => {
    await runnerScenario("partial-timeout", {
      timeout: 3,
    }, (envelope) => {
      const detail = expectCleanTimeout(envelope);
      expect(detail.stdout).toBe("partial-out");
      expect(detail.stderr).toBe("partial-err");
      expect(detail.stdout_truncated).toBe(false);
      expect(detail.stderr_truncated).toBe(false);
    }, "run");
  }, 20000);

  test("noisy timeout retains truncation and completes process cleanup", async () => {
    await runnerScenario("noisy-timeout", {
      timeout: 3,
      stdout_limit: 128,
      stderr_limit: 128,
    }, (envelope) => {
      const detail = expectCleanTimeout(envelope);
      expect(detail.stdout).toBe("OUT:" + "o".repeat(124));
      expect(detail.stderr).toBe("e".repeat(124) + ":ERR");
      expect(detail.stdout_truncated).toBe(true);
      expect(detail.stderr_truncated).toBe(true);
    });
  }, 20000);

  test("kills ordinary descendants holding the command pipes", async () => {
    await runnerScenario("pipes", { timeout: 3 }, (envelope, root) => {
      expectCleanTimeout(envelope);
      expect(existsSync(join(root, "descendant.ready"))).toBe(true);
      const leader = JSON.parse(
        readFileSync(join(root, "leader.pid.json"), "utf8"),
      ) as ProcessIdentity;
      const descendant = JSON.parse(
        readFileSync(join(root, "descendant.pid.json"), "utf8"),
      ) as ProcessIdentity;
      expect(descendant.group).toBe(leader.group);
    });
  }, 20000);

  test("kills setsid descendants outside the leader process group", async () => {
    await runnerScenario("escaped", { timeout: 3 }, (envelope, root) => {
      expectCleanTimeout(envelope);
      const leader = JSON.parse(
        readFileSync(join(root, "leader.pid.json"), "utf8"),
      ) as ProcessIdentity;
      const descendant = JSON.parse(
        readFileSync(join(root, "descendant.pid.json"), "utf8"),
      ) as ProcessIdentity;
      expect(descendant.group).toBe(descendant.pid);
      expect(descendant.session).toBe(descendant.pid);
      expect(descendant.group).not.toBe(leader.group);
    });
  }, 20000);

  test("kills escaped descendants holding pipes after the leader exits", async () => {
    await runnerScenario("orphan-pipes", { timeout: 3 }, (envelope, root) => {
      const detail = expectCleanTimeout(envelope);
      expect(detail.returncode).toBe(0);
      expect(existsSync(join(root, "descendant.ready"))).toBe(true);
    });
  }, 20000);

  test("detects and kills escaped descendants even after all command pipes close", async () => {
    await runnerScenario("orphan-closed", { timeout: 8 }, (envelope, root) => {
      expect(envelope.ok).toBe(false);
      expect(envelope.code).toBe("command_failed");
      expect(envelope.detail).toMatchObject({
        returncode: 0,
        reaped: true,
        pipes_drained: true,
        process_group_killed: true,
        process_group_cleanup_error: null,
      });
      expect(existsSync(join(root, "descendant.ready"))).toBe(true);
    });
  }, 20000);

  test("missing commands are structured and do not poison the runner queue", async () => {
    await sandbox(async ({ root, start }) => {
      const worker = start(`
        import { writeFileSync } from "node:fs";
        import { join } from "node:path";
        import { CommandRunner } from ${JSON.stringify(NATIVE_URL)};
        const root = process.argv[2]!;
        const runner = new CommandRunner();
        let missing;
        try {
          await runner.run([join(root, "does-not-exist")]);
        } catch (error) {
          missing = error.code;
        }
        const result = await runner.run([
          process.execPath, "-e", "process.stdout.write('ok')",
        ]);
        writeFileSync(join(root, "queue-result.json"), JSON.stringify({
          missing,
          returncode: result.returncode,
          stdout: result.stdout.toString("utf8"),
        }));
      `, [root]);
      await successfulJob(worker);
      expect(JSON.parse(
        readFileSync(join(root, "queue-result.json"), "utf8"),
      )).toEqual({
        missing: "command_missing",
        returncode: 0,
        stdout: "ok",
      });
    });
  }, 20000);
});

function makeBundle(root: string): Manifest.ReleaseManifest {
  const artifacts = Manifest.FIXED_ARTIFACTS.map(
    ([name, path, role, mode]) => {
      const bytes = Buffer.from(`${role}:${path}\n`);
      const target = join(root, path);
      mkdirSync(dirname(target), { recursive: true, mode: 0o700 });
      writeFileSync(target, bytes);
      chmodSync(target, mode);
      return { name, path, role, mode, sha256: sha256(bytes) };
    },
  );
  const manifest = Manifest.build_manifest("a".repeat(40), artifacts);
  writeFileSync(
    join(root, Manifest.MANIFEST_NAME),
    JSON.stringify(manifest),
  );
  return manifest;
}

describe("release-manifest: identity, integrity and compatibility", () => {
  test("loads and verifies the fixed TypeScript controller bundle", async () => {
    await sandbox(({ root }) => {
      const manifest = makeBundle(root);
      expect(
        Manifest.load_manifest(join(root, Manifest.MANIFEST_NAME)),
      ).toEqual(manifest);
      expect(Manifest.verify_bundle(root)).toEqual(manifest);
      expect(Object.keys(manifest.actions).sort()).toEqual(
        [...Manifest.ACTION_NAMES].sort(),
      );
      expect(manifest.artifacts.find((item) => item.role === "control")?.path)
        .toBe("agent_vm_control.ts");
      expect(manifest.artifacts.some(
        (item) => item.path === "agent_vm_control.py",
      )).toBe(false);

      const runtimePaths = Object.fromEntries(
        manifest.artifacts.map((item) => [
          item.path,
          join(root, item.path),
        ]),
      );
      expect(
        Manifest.verify_runtime_bundle(manifest, runtimePaths),
      ).toEqual(manifest);
      delete runtimePaths["control-types.ts"];
      controlFailure(
        () => Manifest.verify_runtime_bundle(manifest, runtimePaths),
        "artifact_missing",
      );
    });
  });

  test("rejects traversal, self-reference, incomplete artifacts and invalid identity", async () => {
    await sandbox(({ root }) => {
      const original = makeBundle(root);
      const cases: Array<[string, (value: Data) => void]> = [
        ["manifest_path_invalid", (value) => {
          value.artifacts[0].path = "../escape";
        }],
        ["manifest_self_reference", (value) => {
          value.artifacts[0].path = Manifest.MANIFEST_NAME;
        }],
        ["manifest_self_reference", (value) => {
          value.artifacts[0].path = Manifest.CLIENT_SIDECAR_NAME;
        }],
        ["manifest_artifact_set_invalid", (value) => {
          value.artifacts.pop();
        }],
        ["manifest_artifact_set_invalid", (value) => {
          value.artifacts[0].mode = 0o644;
        }],
        ["manifest_invalid", (value) => {
          delete value.client_view;
        }],
        ["manifest_action_set_invalid", (value) => {
          delete value.actions.status;
        }],
        ["manifest_action_unsupported", (value) => {
          value.actions["future-action"] = value.actions.status;
        }],
        ["protocol_unsupported", (value) => {
          value.protocol = 3;
        }],
        ["manifest_invalid", (value) => {
          value.source_revision = "short";
        }],
        ["manifest_invalid", (value) => {
          value.release_id = "b".repeat(40);
        }],
        ["manifest_invalid", (value) => {
          value.actions.status.client_to_control.schema_min = 2;
          value.actions.status.client_to_control.schema_max = 2;
        }],
      ];

      for (const [code, mutate] of cases) {
        const changed: Data = structuredClone(original);
        mutate(changed);
        controlFailure(() => Manifest.validate_manifest(changed), code);
      }
    });
  });

  test("verifies actual bytes and modes and rejects symlink files and parents", async () => {
    await sandbox(({ root }) => {
      const manifest = makeBundle(root);
      const client = manifest.artifacts.find(
        (item) => item.role === "client",
      )!;
      const target = join(root, client.path);
      const bytes = readFileSync(target);

      writeFileSync(target, "tampered");
      controlFailure(
        () => Manifest.verify_bundle(root, manifest),
        "artifact_checksum_mismatch",
      );
      writeFileSync(target, bytes);

      chmodSync(target, 0o600);
      controlFailure(
        () => Manifest.verify_bundle(root, manifest),
        "artifact_mode_mismatch",
      );
      chmodSync(target, client.mode);

      const savedClient = join(root, "saved-client");
      renameSync(target, savedClient);
      symlinkSync(savedClient, target);
      controlFailure(
        () => Manifest.verify_bundle(root, manifest),
        "artifact_unverified",
      );
      rmSync(target);
      renameSync(savedClient, target);

      const systemd = join(root, "systemd");
      const savedSystemd = join(root, "saved-systemd");
      renameSync(systemd, savedSystemd);
      symlinkSync(savedSystemd, systemd, "dir");
      controlFailure(
        () => Manifest.verify_bundle(root, manifest),
        "artifact_unverified",
      );
    });
  });

  test("client sidecar must match release identity, artifact and view", async () => {
    await sandbox(({ root }) => {
      const manifest = makeBundle(root);
      const sidecar = {
        format: manifest.format,
        release_id: manifest.release_id,
        source_revision: manifest.source_revision,
        protocol: manifest.protocol,
        client_artifact: manifest.artifacts.find(
          (item) => item.role === "client",
        )!,
        client_view: manifest.client_view,
      };
      const path = join(root, Manifest.CLIENT_SIDECAR_NAME);
      writeFileSync(path, JSON.stringify(sidecar));
      expect(Manifest.verify_client_sidecar(root, manifest)).toEqual(manifest);

      for (const mutate of [
        (value: Data) => { value.release_id = "b".repeat(40); },
        (value: Data) => { value.client_artifact.sha256 = "0".repeat(64); },
        (value: Data) => { delete value.client_view.actions.status; },
      ]) {
        const changed: Data = structuredClone(sidecar);
        mutate(changed);
        writeFileSync(path, JSON.stringify(changed));
        controlFailure(
          () => Manifest.verify_client_sidecar(root, manifest),
          "client_sidecar_invalid",
        );
      }
    });
  });

  test("compatibility checks both request and response schema and semantics", () => {
    const manifest = Manifest.legacy_manifest();
    const peer: Data = {
      ...Manifest.legacy_capability("status"),
      release_id: manifest.release_id,
    };
    expect(Manifest.check_compatibility(manifest, "status", peer)).toEqual([]);
    expect(
      Manifest.check_compatibility(
        manifest,
        "status",
        Manifest.legacy_capability("status"),
      )[0]?.code,
    ).toBe("compatible_release");
    expect(
      Manifest.check_compatibility(manifest, "status", null)[0]?.code,
    ).toBe("legacy_compatibility");

    const cases: Array<[string, (value: Data) => void]> = [
      ["protocol_mismatch", (value) => { value.protocol = 3; }],
      ["compatibility_action_mismatch", (value) => {
        value.action = "release";
      }],
      ["request_schema_incompatible", (value) => {
        value.request.schema = 2;
      }],
      ["request_semantic_incompatible", (value) => {
        value.request.semantic_revision = 2;
      }],
      ["response_schema_incompatible", (value) => {
        value.accepted_response.schema_min = 2;
        value.accepted_response.schema_max = 2;
      }],
      ["response_semantic_incompatible", (value) => {
        value.accepted_response.semantic_min = 2;
        value.accepted_response.semantic_max = 2;
      }],
      ["compatibility_invalid", (value) => {
        delete value.release_id;
      }],
      ["compatibility_invalid", (value) => {
        value.release_id = "not-a-revision";
      }],
    ];
    for (const [code, mutate] of cases) {
      const changed: Data = structuredClone(peer);
      mutate(changed);
      controlFailure(
        () => Manifest.check_compatibility(manifest, "status", changed),
        code,
      );
    }
    controlFailure(
      () => Manifest.legacy_capability("future-action"),
      "unknown_action",
    );
  });

  test("compatibility integers above 2^53 retain their declared range", () => {
    const manifest = Manifest.legacy_manifest();
    const maximum = 9007199254740993n;
    manifest.actions.status.client_to_control.semantic_max = maximum;
    manifest.client_view.actions.status.client_to_control.semantic_max = maximum;
    const peer: Data = Manifest.legacy_capability("status");
    peer.request.semantic_revision = maximum;
    expect(Manifest.check_compatibility(parse_json(canonical_json(manifest)), "status", peer)[0]?.code).toBe("compatible_release");
    peer.request.semantic_revision = maximum + 1n;
    controlFailure(() => Manifest.check_compatibility(manifest, "status", peer), "request_semantic_incompatible");
  });

  test("missing runtime metadata is explicit legacy compatibility; corrupt metadata fails", async () => {
    await sandbox(({ root }) => {
      const path = join(root, Manifest.MANIFEST_NAME);
      const [manifest, present, verified, warnings] =
        Manifest.load_runtime_manifest(path);
      expect(manifest).toBeNull();
      expect(present).toBe(false);
      expect(verified).toBe(true);
      expect(warnings[0]?.code).toBe("legacy_compatibility");

      const valid = makeBundle(root);
      for (const bytes of [
        Buffer.from("{"),
        Buffer.from([0xff]),
        Buffer.from("\uFEFF" + JSON.stringify(valid)),
      ]) {
        writeFileSync(path, bytes);
        controlFailure(
          () => Manifest.load_runtime_manifest(path),
          "manifest_invalid",
        );
      }

      const saved = join(root, "saved-manifest.json");
      writeFileSync(saved, JSON.stringify(valid));
      rmSync(path);
      symlinkSync(saved, path);
      controlFailure(
        () => Manifest.load_runtime_manifest(path),
        "manifest_unverified",
      );
    });
  });

  test("canonical systemd digests match the Bun launcher transition", () => {
    for (const [name, expected] of Object.entries(
      Manifest.UNIT_CONTENT_SHA256,
    )) {
      const path = fileURLToPath(new URL(
        `../assets/systemd/${name}`,
        import.meta.url,
      ));
      const content = readFileSync(path, "utf8");
      expect(sha256(content)).toBe(expected);
      const exec = Manifest.UNIT_EXEC_START[
        name as keyof typeof Manifest.UNIT_EXEC_START
      ];
      if (exec !== undefined) {
        expect(content.split("\n")).toContain(exec);
      }
    }
  });
});
