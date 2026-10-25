import { Database, SQLiteError, type SQLQueryBindings } from "bun:sqlite";
import { randomUUID } from "node:crypto";
import { chmodSync, lstatSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import {
  ControlError,
  parse_json,
  canonical_json,
  iso_time,
  is_record,
  type Data,
} from "./control-types.ts";

type Integer = number | bigint;

type FailureOptions = {
  phase: string;
  sql_operation: string;
  state_committed: boolean | null;
  rollback_error?: unknown;
};

type ConnectionOptions = {
  phase: string;
  sql_operation: string;
};

type TaskLookup = {
  session_id?: string | null;
  task_id?: string | null;
};

type AuditOptions = {
  task_id?: string | null;
  session_id?: string | null;
  generation?: Integer | null;
  detail?: unknown;
};

const PROTOCOL_VERSION = 4;
const MAX_SAFE_INTEGER = BigInt(Number.MAX_SAFE_INTEGER);
const MIN_SAFE_INTEGER = BigInt(Number.MIN_SAFE_INTEGER);
const MAX_SQLITE_INTEGER = (1n << 63n) - 1n;
const MIN_SQLITE_INTEGER = -(1n << 63n);
const STATE_EPOCH_KEY = "state_epoch";

const TASK_FIELDS = [
  "task_id",
  "session_id",
  "generation",
  "state",
  "instance_name",
  "vm_ip",
  "ssh_host_port",
  "lease_expires_at",
  "inactive_since",
  "cleanup_after",
  "created_at",
  "updated_at",
  "last_error",
  "image_fingerprint",
  "resources_json",
  "reservation_bytes",
  "previous_generation",
  "resource_reservation_json",
] as const;

const PORT_FIELDS = [
  "task_id",
  "generation",
  "port_name",
  "protocol",
  "host_port",
  "vm_port",
  "device_name",
  "state",
  "created_at",
] as const;

const SCHEMA = `
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
`;

const NATIVE_SQLITE_ERROR = Symbol("StateStore.native_sqlite_error");

class ValueError extends Error {
  override name = "ValueError";
}

class OverflowError extends RangeError {
  override name = "OverflowError";
}

function get(value: Data, key: string, fallback: any = null): any {
  return Object.hasOwn(value, key) ? value[key] : fallback;
}

function normalize_integer(value: bigint): Integer {
  return value >= MIN_SAFE_INTEGER && value <= MAX_SAFE_INTEGER
    ? Number(value)
    : value;
}

function normalize_row(row: Data): Data {
  return Object.fromEntries(
    Object.entries(row).map(([key, value]) => [
      key,
      typeof value === "bigint" ? normalize_integer(value) : value,
    ]),
  );
}

function bindings(args: readonly unknown[]): SQLQueryBindings[] {
  return args.map((value) => {
    if (typeof value === "bigint") {
      if (value < MIN_SQLITE_INTEGER || value > MAX_SQLITE_INTEGER) {
        throw new OverflowError(
          "Python int too large to convert to SQLite INTEGER",
        );
      }
      return value;
    }
    if (typeof value === "boolean") return value ? 1 : 0;
    return (value ?? null) as SQLQueryBindings;
  });
}

function error_type(error: unknown): string {
  if (error instanceof Error) {
    return error.name !== "Error"
      ? error.name
      : error.constructor.name || "Error";
  }
  return error === null ? "null" : typeof error;
}

function error_message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/**
 * Bun can throw plain Error/TypeError for database misuse, including using a
 * closed connection or supplying an unsupported binding. Tag errors only at
 * native SQLite call boundaries so callback/application errors remain distinct.
 */
function sqlite_call<T>(fn: () => T): T {
  try {
    return fn();
  } catch (error) {
    if (error instanceof Error && !(error instanceof SQLiteError)) {
      Object.defineProperty(error, NATIVE_SQLITE_ERROR, {
        value: true,
        configurable: true,
      });
    }
    throw error;
  }
}

function is_sqlite_error(error: unknown): error is Error {
  return (
    error instanceof SQLiteError ||
    (error instanceof Error &&
      (error as Error & { [NATIVE_SQLITE_ERROR]?: boolean })[
        NATIVE_SQLITE_ERROR
      ] === true)
  );
}

function sqlite_identity(error: unknown): Data {
  const fields =
    error !== null && typeof error === "object" ? (error as Data) : {};
  return {
    sqlite_code: fields.errno ?? null,
    sqlite_name:
      typeof fields.code === "string" && fields.code.startsWith("SQLITE_")
        ? fields.code
        : error_type(error),
  };
}

function python_truthy(value: unknown): boolean {
  if (value == null || value === false) return false;
  if (typeof value === "number") return value !== 0;
  if (typeof value === "bigint") return value !== 0n;
  if (typeof value === "string" || Array.isArray(value)) {
    return value.length !== 0;
  }
  if (is_record(value)) return Object.keys(value).length !== 0;
  return true;
}

function python_quote(value: string): string {
  const quote = value.includes("'") && !value.includes('"') ? '"' : "'";
  let result = quote;

  for (const character of value) {
    if (character === "\\") result += "\\\\";
    else if (character === quote) result += `\\${character}`;
    else if (character === "\t") result += "\\t";
    else if (character === "\n") result += "\\n";
    else if (character === "\r") result += "\\r";
    else if (character !== " " && /[\p{C}\p{Z}]/u.test(character)) {
      const code = character.codePointAt(0)!;
      const width = code <= 0xff ? 2 : code <= 0xffff ? 4 : 8;
      const prefix = width === 2 ? "\\x" : width === 4 ? "\\u" : "\\U";
      result += prefix + code.toString(16).padStart(width, "0");
    } else {
      result += character;
    }
  }

  return result + quote;
}

function python_repr(value: unknown): string {
  return typeof value === "string" ? python_quote(value) : python_string(value);
}

function python_string(value: unknown): string {
  if (value == null) return "None";
  if (value === true) return "True";
  if (value === false) return "False";
  if (typeof value === "string") return value;
  if (typeof value === "bigint") return value.toString();

  if (typeof value === "number") {
    if (Number.isNaN(value)) return "nan";
    if (value === Infinity) return "inf";
    if (value === -Infinity) return "-inf";
    return canonical_json(value);
  }

  if (Array.isArray(value)) {
    return `[${value.map(python_repr).join(", ")}]`;
  }

  if (is_record(value)) {
    return `{${Object.entries(value)
      .map(([key, child]) => `${python_quote(key)}: ${python_repr(child)}`)
      .join(", ")}}`;
  }

  return String(value);
}

function python_integer(value: unknown): Integer {
  if (typeof value === "bigint") return normalize_integer(value);
  if (typeof value === "boolean") return value ? 1 : 0;

  if (typeof value === "number") {
    if (Number.isNaN(value)) {
      throw new ValueError("cannot convert float NaN to integer");
    }
    if (!Number.isFinite(value)) {
      throw new OverflowError("cannot convert float infinity to integer");
    }
    return normalize_integer(BigInt(Math.trunc(value)));
  }

  const text =
    typeof value === "string"
      ? value.trim()
      : value instanceof Uint8Array
        ? new TextDecoder().decode(value).trim()
        : null;

  if (text !== null && /^[+-]?[0-9](?:_?[0-9])*$/.test(text)) {
    return normalize_integer(BigInt(text.replaceAll("_", "")));
  }

  throw new ValueError("invalid integer value");
}

function* mappings(value: unknown): Generator<Data> {
  if (is_record(value)) {
    yield value;
    for (const child of Object.values(value)) yield* mappings(child);
  } else if (Array.isArray(value)) {
    for (const child of value) yield* mappings(child);
  }
}

function deepest(value: unknown, keys: readonly string[]): string | null {
  if (is_record(value)) {
    for (const branch of [get(value, "error"), get(value, "detail")]) {
      const result = deepest(branch, keys);
      if (result !== null) return result;
    }

    for (const key of keys) {
      const child = get(value, key);
      if (child != null) return python_string(child);
    }

    for (const child of Object.values(value)) {
      const result = deepest(child, keys);
      if (result !== null) return result;
    }
  } else if (Array.isArray(value)) {
    for (const child of value) {
      const result = deepest(child, keys);
      if (result !== null) return result;
    }
  }

  return null;
}

function increment(counter: Map<string, number>, key: string): void {
  counter.set(key, (counter.get(key) ?? 0) + 1);
}

function is_thenable(value: unknown): boolean {
  return (
    value !== null &&
    (typeof value === "object" || typeof value === "function") &&
    typeof (value as { then?: unknown }).then === "function"
  );
}

export class StateStore {
  readonly path: string;
  connection!: Database;
  depth = 0;
  _unusable = false;
  private transaction_mutated = false;

  constructor(path: string) {
    this.path = path;

    const parent_path = dirname(path);
    mkdirSync(parent_path, { mode: 0o700, recursive: true });

    const parent = lstatSync(parent_path);
    if (
      !parent.isDirectory() ||
      parent.uid !== process.geteuid?.() ||
      (parent.mode & 0o077) !== 0
    ) {
      throw new ControlError(
        "state_path_unsafe",
        "state parent must be a private owned directory",
      );
    }

    try {
      if (lstatSync(path).isSymbolicLink()) {
        throw new ControlError(
          "state_path_unsafe",
          "state database cannot be a symlink",
        );
      }
    } catch (error) {
      if ((error as NodeJS.ErrnoException)?.code !== "ENOENT") throw error;
    }

    let connection: Database | null = null;
    let phase = "connect";

    try {
      connection = sqlite_call(
        () => new Database(path, { create: true, safeIntegers: true }),
      );
      this.connection = connection;
      chmodSync(path, 0o600);

      this._exec("PRAGMA busy_timeout=10000");
      phase = "wal-init";
      this._exec("PRAGMA journal_mode=WAL");
      this._exec("PRAGMA synchronous=FULL");
      this._exec("PRAGMA foreign_keys=ON");

      phase = "schema-init";
      this._schema();
    } catch (error) {
      if (connection !== null) {
        try {
          connection.close();
        } catch {
          // Preserve the initialization failure.
        }
      }

      if (!is_sqlite_error(error)) throw error;
      throw StateStore._sqlite_failure(error, {
        phase,
        sql_operation: "initialize",
        state_committed: false,
      });
    }
  }

  static _sqlite_failure(
    error: unknown,
    {
      phase,
      sql_operation,
      state_committed,
      rollback_error,
    }: FailureOptions,
  ): ControlError {
    const detail: Data = {
      phase,
      ...sqlite_identity(error),
      sql_operation,
      state_committed,
    };

    if (rollback_error != null) {
      detail.rollback_error = sqlite_identity(rollback_error);
    }

    const failure = new ControlError(
      "sqlite_unavailable",
      "state database cannot be verified",
      detail,
    );
    failure.cause = error;
    return failure;
  }

  _ensure_usable({ phase, sql_operation }: ConnectionOptions): void {
    if (this._unusable) {
      throw new ControlError(
        "sqlite_unavailable",
        "state database connection is unusable and must be reopened",
        {
          phase,
          sql_operation,
          state_committed: null,
        },
      );
    }
  }

  private _exec(sql: string): void {
    sqlite_call(() => this.connection.exec(sql));
  }

  private _run(sql: string, args: readonly unknown[] = []): void {
    // Conversion failures are application errors, not SQLite failures.
    const values = bindings(args);
    sqlite_call(() => {
      const result = this.connection.query(sql).run(...values);
      if (this.depth > 0 && result.changes > 0) {
        this.transaction_mutated = true;
      }
    });
  }

  private _state_epoch_value(value: string | null): string {
    if (value === null) return "initial";
    const text = value.trim();
    if (/^[0-9a-f]{32}$/.test(text)) return text;
    if (/^(?:0|[1-9][0-9]*)$/.test(text)) {
      if (text.length > 19) {
        throw new ControlError(
          "state_unverified",
          "state database epoch is out of range",
          { key: STATE_EPOCH_KEY },
        );
      }
      if (BigInt(text) > MAX_SQLITE_INTEGER) {
        throw new ControlError(
          "state_unverified",
          "state database epoch is out of range",
          { key: STATE_EPOCH_KEY },
        );
      }
      return text;
    }
    throw new ControlError(
      "state_unverified",
      "state database epoch is invalid",
      { key: STATE_EPOCH_KEY },
    );
  }

  private _advance_state_epoch(): void {
    const next = randomUUID().replaceAll("-", "");
    this._run(
      "INSERT INTO meta(key, value) VALUES (?, ?) " +
        "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
      [STATE_EPOCH_KEY, next],
    );
  }

  private _rollback_application(error: unknown): never {
    let rollback_error: unknown = undefined;
    try {
      this._rollback();
    } catch (failure) {
      if (!is_sqlite_error(failure)) throw failure;
      rollback_error = failure;
      this._unusable = true;
    }

    if (is_sqlite_error(error)) {
      const failure = StateStore._sqlite_failure(error, {
        phase: "write",
        sql_operation: "write",
        state_committed: rollback_error === undefined ? false : null,
        rollback_error,
      });
      throw failure;
    }

    if (rollback_error !== undefined) {
      const failure = StateStore._sqlite_failure(rollback_error, {
        phase: "rollback",
        sql_operation: "write",
        state_committed: null,
      });
      failure.detail.original_error = {
        type: error_type(error),
        message: error_message(error),
      };
      failure.cause = error;
      throw failure;
    }

    throw error;
  }

  private _commit(): void {
    // sqlite3.Connection.commit() is a no-op outside a transaction.
    if (sqlite_call(() => this.connection.inTransaction)) {
      this._exec("COMMIT");
    }
  }

  private _rollback(): void {
    // SQLite can automatically roll back on FULL, IOERR, NOMEM, or INTERRUPT.
    // Python's rollback() also succeeds when no transaction remains active.
    if (sqlite_call(() => this.connection.inTransaction)) {
      this._exec("ROLLBACK");
    }
  }

  _schema(): void {
    try {
      // Python executescript() commits a pre-existing transaction first.
      this._commit();
      this._exec(SCHEMA);
      this._commit();
    } catch (error) {
      this._rollback();
      throw error;
    }
  }

  close(): void {
    this.connection.close();
  }

  /**
   * Nested calls share the outer transaction; there are no savepoints.
   * Callbacks must complete synchronously and must not perform native waits.
   */
  transaction<T>(fn: () => T): T {
    this._ensure_usable({
      phase: "connection",
      sql_operation: "write",
    });

    const outer = this.depth === 0;

    if (outer) {
      try {
        this._exec("BEGIN IMMEDIATE");
      } catch (error) {
        if (!is_sqlite_error(error)) throw error;
        throw StateStore._sqlite_failure(error, {
          phase: "begin",
          sql_operation: "write",
          state_committed: false,
        });
      }
      this.transaction_mutated = false;
    }

    this.depth += 1;

    try {
      let result: T;

      try {
        result = fn();
        if (is_thenable(result)) {
          throw new TypeError("transaction callback must be synchronous");
        }
      } catch (error) {
        if (is_sqlite_error(error)) {
          let rollback_error: unknown = undefined;

          if (outer) {
            try {
              this._rollback();
            } catch (rollback_failure) {
              if (!is_sqlite_error(rollback_failure)) {
                throw rollback_failure;
              }
              rollback_error = rollback_failure;
              this._unusable = true;
            }
          }

          throw StateStore._sqlite_failure(error, {
            phase: "write",
            sql_operation: "write",
            state_committed: rollback_error === undefined ? false : null,
            rollback_error,
          });
        }

        if (outer) this._rollback_application(error);
        throw error;
      }

      if (outer) {
        try {
          if (this.transaction_mutated) this._advance_state_epoch();
          this._commit();
        } catch (error) {
          if (is_sqlite_error(error)) {
            let rollback_error: unknown = undefined;
            try {
              this._rollback();
            } catch (rollback_failure) {
              if (!is_sqlite_error(rollback_failure)) throw rollback_failure;
              rollback_error = rollback_failure;
              this._unusable = true;
            }

            throw StateStore._sqlite_failure(error, {
              phase: "commit",
              sql_operation: "write",
              state_committed: null,
              rollback_error,
            });
          }
          if (this.connection.inTransaction) this._rollback_application(error);
          throw error;
        }
      }

      return result;
    } finally {
      this.depth -= 1;
      if (outer) this.transaction_mutated = false;
    }
  }

  _row(query: string, args: readonly unknown[] = []): Data | null {
    this._ensure_usable({ phase: "read", sql_operation: "read" });

    try {
      const values = bindings(args);
      const row = sqlite_call(() =>
        this.connection.query<Data, SQLQueryBindings[]>(query).get(...values),
      );
      return row === null ? null : normalize_row(row);
    } catch (error) {
      if (!is_sqlite_error(error)) throw error;
      throw StateStore._sqlite_failure(error, {
        phase: "read",
        sql_operation: "read",
        state_committed: null,
      });
    }
  }

  _rows(query: string, args: readonly unknown[] = []): Data[] {
    this._ensure_usable({ phase: "read", sql_operation: "read" });

    try {
      const values = bindings(args);
      const rows = sqlite_call(() =>
        this.connection.query<Data, SQLQueryBindings[]>(query).all(...values),
      );
      return rows.map(normalize_row);
    } catch (error) {
      if (!is_sqlite_error(error)) throw error;
      throw StateStore._sqlite_failure(error, {
        phase: "read",
        sql_operation: "read",
        state_committed: null,
      });
    }
  }

  get_task({
    session_id = null,
    task_id = null,
  }: TaskLookup = {}): Data | null {
    if ((session_id === null) === (task_id === null)) {
      throw new ValueError("provide exactly one task lookup key");
    }

    return session_id !== null
      ? this._row("SELECT * FROM tasks WHERE session_id = ?", [session_id])
      : this._row("SELECT * FROM tasks WHERE task_id = ?", [task_id]);
  }

  list_tasks(): Data[] {
    return this._rows("SELECT * FROM tasks ORDER BY created_at");
  }

  put_task(task: Data): void {
    const values = TASK_FIELDS.map((field) =>
      get(
        task,
        field,
        field === "resources_json" || field === "resource_reservation_json"
          ? "{}"
          : field === "reservation_bytes"
            ? 0
            : null,
      ),
    );

    this.transaction(() => {
      this._run(
        `
        INSERT INTO tasks (${TASK_FIELDS.join(", ")})
        VALUES (${TASK_FIELDS.map(() => "?").join(", ")})
        ON CONFLICT(task_id) DO UPDATE SET
            session_id=excluded.session_id,
            generation=excluded.generation,
            state=excluded.state,
            instance_name=excluded.instance_name,
            vm_ip=excluded.vm_ip,
            ssh_host_port=excluded.ssh_host_port,
            lease_expires_at=excluded.lease_expires_at,
            inactive_since=excluded.inactive_since,
            cleanup_after=excluded.cleanup_after,
            updated_at=excluded.updated_at,
            last_error=excluded.last_error,
            image_fingerprint=excluded.image_fingerprint,
            resources_json=excluded.resources_json,
            reservation_bytes=excluded.reservation_bytes,
            previous_generation=excluded.previous_generation,
            resource_reservation_json=excluded.resource_reservation_json
        WHERE
            tasks.session_id IS NOT excluded.session_id OR
            tasks.generation IS NOT excluded.generation OR
            tasks.state IS NOT excluded.state OR
            tasks.instance_name IS NOT excluded.instance_name OR
            tasks.vm_ip IS NOT excluded.vm_ip OR
            tasks.ssh_host_port IS NOT excluded.ssh_host_port OR
            tasks.lease_expires_at IS NOT excluded.lease_expires_at OR
            tasks.inactive_since IS NOT excluded.inactive_since OR
            tasks.cleanup_after IS NOT excluded.cleanup_after OR
            tasks.updated_at IS NOT excluded.updated_at OR
            tasks.last_error IS NOT excluded.last_error OR
            tasks.image_fingerprint IS NOT excluded.image_fingerprint OR
            tasks.resources_json IS NOT excluded.resources_json OR
            tasks.reservation_bytes IS NOT excluded.reservation_bytes OR
            tasks.previous_generation IS NOT excluded.previous_generation OR
            tasks.resource_reservation_json IS NOT excluded.resource_reservation_json
        `,
        values,
      );
    });
  }

  update_task(task_id: string, changes: Data): Data {
    if (Object.keys(changes).length === 0) {
      const task = this.get_task({ task_id });
      if (task === null) {
        throw new ControlError("task_missing", `task not found: ${task_id}`);
      }
      return task;
    }

    const change_keys = Object.keys(changes);
    const change_values = Object.values(changes);
    const update_values = [...change_values, iso_time()];
    const assignments = [
      ...change_keys.map((key) => `${key} = ?`),
      "updated_at = ?",
    ].join(", ");
    const changed = change_keys
      .map((key) => `${key} IS NOT ?`)
      .join(" OR ");

    const task_update = [
      ...update_values,
      task_id,
      ...change_values,
    ];

    this.transaction(() => {
      this._run(
        `UPDATE tasks SET ${assignments} WHERE task_id = ? AND (${changed})`,
        task_update,
      );
    });

    const task = this.get_task({ task_id });
    if (task === null) {
      throw new ControlError("task_missing", `task not found: ${task_id}`);
    }
    return task;
  }

  get_ports(task_id: string, state: string | null = null): Data[] {
    return state === null
      ? this._rows(
          "SELECT * FROM ports WHERE task_id = ? ORDER BY port_name, protocol",
          [task_id],
        )
      : this._rows(
          "SELECT * FROM ports WHERE task_id = ? AND state = ? ORDER BY port_name, protocol",
          [task_id, state],
        );
  }

  get_port(
    task_id: string,
    port_name: string,
    protocol: string,
  ): Data | null {
    return this._row(
      "SELECT * FROM ports WHERE task_id = ? AND port_name = ? AND protocol = ?",
      [task_id, port_name, protocol],
    );
  }

  put_port(port: Data): void {
    this.transaction(() => {
      this._run(
        `
        INSERT INTO ports (${PORT_FIELDS.join(", ")})
        VALUES (${PORT_FIELDS.map(() => "?").join(", ")})
        ON CONFLICT(task_id, port_name, protocol) DO UPDATE SET
            generation=excluded.generation,
            host_port=excluded.host_port,
            vm_port=excluded.vm_port,
            device_name=excluded.device_name,
            state=excluded.state
        WHERE
            ports.generation IS NOT excluded.generation OR
            ports.host_port IS NOT excluded.host_port OR
            ports.vm_port IS NOT excluded.vm_port OR
            ports.device_name IS NOT excluded.device_name OR
            ports.state IS NOT excluded.state
        `,
        PORT_FIELDS.map((field) => get(port, field)),
      );
    });
  }

  update_port(
    task_id: string,
    port_name: string,
    protocol: string,
    changes: Data,
  ): void {
    if (Object.keys(changes).length === 0) return;

    const update_values = Object.values(changes);
    const update_keys = Object.keys(changes);
    const assignments = update_keys
      .map((key) => `${key} = ?`)
      .join(", ");
    const changed = update_keys
      .map((key) => `${key} IS NOT ?`)
      .join(" OR ");

    this.transaction(() => {
      this._run(
        `UPDATE ports SET ${assignments}
         WHERE task_id = ? AND port_name = ? AND protocol = ? AND (${changed})`,
        [...update_values, task_id, port_name, protocol, ...update_values],
      );
    });
  }

  remove_port(task_id: string, port_name: string, protocol: string): void {
    this.transaction(() => {
      this._run(
        "DELETE FROM ports WHERE task_id = ? AND port_name = ? AND protocol = ?",
        [task_id, port_name, protocol],
      );
    });
  }

  occupied_ports(protocol: string | null = null): Set<Integer> {
    const rows =
      protocol === null
        ? this._rows("SELECT host_port FROM ports WHERE state != 'removed'")
        : this._rows(
            "SELECT host_port FROM ports WHERE protocol = ? AND state != 'removed'",
            [protocol],
          );

    return new Set(rows.map((row) => python_integer(row.host_port)));
  }

  operation(operation_id: string): Data | null {
    const row = this._row(
      "SELECT response_json FROM operations WHERE operation_id = ?",
      [operation_id],
    );
    if (row === null) return null;

    let value: unknown;
    try {
      value = parse_json(row.response_json);
    } catch (error) {
      const failure = new ControlError(
        "operation_unverified",
        "operation journal record is invalid",
        { operation_id },
      );
      failure.cause = error;
      throw failure;
    }

    if (!is_record(value)) {
      throw new ControlError(
        "operation_unverified",
        "operation journal record is not an object",
        { operation_id },
      );
    }

    const record: Data = { ...value };
    const digest = get(record, "_request_digest");
    if (typeof digest !== "string" || digest.length === 0) {
      throw new ControlError(
        "operation_unverified",
        "operation journal digest is invalid",
        { operation_id },
      );
    }

    const outcome = get(record, "outcome");
    if (outcome === "pending") return record;

    if (outcome === "error") {
      const error = get(record, "error");
      if (
        !is_record(error) ||
        typeof get(error, "code") !== "string" ||
        error.code.length === 0 ||
        typeof get(error, "message") !== "string" ||
        error.message.length === 0
      ) {
        throw new ControlError(
          "operation_unverified",
          "operation journal error is invalid",
          { operation_id },
        );
      }
      return record;
    }

    if (outcome === "success") {
      const protocol = get(record, "protocol");
      if (
        get(record, "ok") !== true ||
        (protocol !== PROTOCOL_VERSION &&
          protocol !== BigInt(PROTOCOL_VERSION)) ||
        typeof get(record, "action") !== "string" ||
        record.action.length === 0 ||
        typeof get(record, "operation_id") !== "string" ||
        record.operation_id.length === 0 ||
        !is_record(get(record, "result"))
      ) {
        throw new ControlError(
          "operation_unverified",
          "operation journal success is invalid",
          { operation_id },
        );
      }
      return record;
    }

    throw new ControlError(
      "operation_unverified",
      "operation journal outcome is invalid",
      { operation_id },
    );
  }

  record_operation(
    operation_id: string,
    action: string,
    session_id: string | null,
    generation: Integer | null,
    response: Data,
  ): void {
    this.transaction(() => {
      this._run(
        `
        INSERT OR REPLACE INTO operations
        (operation_id, action, session_id, generation, response_json, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
        `,
        [
          operation_id,
          action,
          session_id,
          generation,
          canonical_json(response),
          iso_time(),
        ],
      );
    });
  }

  audit(
    action: string,
    outcome: string,
    {
      task_id = null,
      session_id = null,
      generation = null,
      detail = null,
    }: AuditOptions = {},
  ): void {
    this.transaction(() => {
      this._run(
        `
        INSERT INTO audit
        (action, task_id, session_id, generation, outcome, detail_json, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        `,
        [
          action,
          task_id,
          session_id,
          generation,
          outcome,
          detail === null ? null : canonical_json(detail),
          iso_time(),
        ],
      );
    });
  }

  audit_summary(): Record<string, Record<string, number>> {
    const operations = new Map<string, number>();
    const image_unpack_failures = new Map<string, number>();
    const warnings = new Map<string, number>();

    for (const row of this._rows(
      "SELECT action, outcome, detail_json FROM audit",
    )) {
      let detail: Data = {};

      if (python_truthy(row.detail_json)) {
        try {
          const parsed: unknown = parse_json(row.detail_json);
          if (is_record(parsed)) detail = parsed;
        } catch {
          detail = {};
        }
      }

      const nested = [...mappings(detail)];
      const code = deepest(detail, ["code"]) || "unknown";
      const phase = deepest(detail, ["phase", "operation_phase"]) || "unknown";

      increment(
        operations,
        `${python_string(row.action)}|${python_string(row.outcome)}|${phase}`,
      );

      for (const item of nested) {
        const warning_list = get(item, "warnings");
        if (!Array.isArray(warning_list)) continue;

        for (const warning of warning_list) {
          if (
            is_record(warning) &&
            python_truthy(get(warning, "code"))
          ) {
            increment(warnings, python_string(warning.code));
          }
        }
      }

      if (
        row.outcome === "error" &&
        ["memory_pressure", "resource_unverified", "capacity_insufficient"].includes(
          code,
        )
      ) {
        increment(warnings, code);
      }

      const image_text = nested
        .flatMap((item) =>
          ["code", "phase", "operation_phase", "message"].map((key) =>
            python_string(get(item, key, "")),
          ),
        )
        .join(" ")
        .toLowerCase();

      if (
        row.outcome === "error" &&
        image_text.includes("unpack") &&
        ["image", "rootfs"].some((token) => image_text.includes(token))
      ) {
        increment(image_unpack_failures, `${phase}|${code}`);
      }
    }

    return {
      operations: Object.fromEntries(operations),
      image_unpack_failures: Object.fromEntries(image_unpack_failures),
      warnings: Object.fromEntries(warnings),
    };
  }

  pending_operations(): Data[] {
    const pending: Data[] = [];

    for (const row of this._rows(
      "SELECT operation_id, action, session_id, generation, response_json, created_at " +
        "FROM operations ORDER BY created_at, operation_id",
    )) {
      let response: unknown;
      try {
        response = parse_json(row.response_json);
      } catch (error) {
        const failure = new ControlError(
          "operation_unverified",
          "operation journal response is malformed",
        );
        failure.cause = error;
        throw failure;
      }

      if (!is_record(response) || get(response, "outcome") !== "pending") {
        continue;
      }

      const item: Data = {
        operation_id: row.operation_id,
        action: row.action,
        session_id: row.session_id,
        generation: row.generation,
        created_at: row.created_at,
      };
      for (const key of ["task_id", "task_generation", "acquire_resources"]) {
        if (Object.hasOwn(response, key)) item[key] = response[key];
      }
      pending.push(item);
    }

    return pending;
  }

  get_meta(key: string): string | null {
    const row = this._row("SELECT value FROM meta WHERE key = ?", [key]);
    return row === null ? null : python_string(row.value);
  }

  state_epoch(): string {
    return this._state_epoch_value(this.get_meta(STATE_EPOCH_KEY));
  }

  set_meta(key: string, value: string): void {
    this.transaction(() => {
      this._run(
        "INSERT INTO meta(key, value) VALUES (?, ?) " +
          "ON CONFLICT(key) DO UPDATE SET value=excluded.value " +
          "WHERE meta.value IS NOT excluded.value",
        [key, value],
      );
    });
  }
}
