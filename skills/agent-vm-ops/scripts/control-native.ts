import { dlopen, FFIType, ptr } from "bun:ffi";
import { spawn } from "node:child_process";
import { AsyncLocalStorage } from "node:async_hooks";
import { constants, openSync, closeSync, mkdirSync, readdirSync, readFileSync, existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { createHash } from "node:crypto";
import { ControlError, GuestWaitTimeout, parse_json, is_record, type Data } from "./control-types.ts";

let nativeLibrary: ReturnType<typeof load_libc> | undefined;
function load_libc() {
  const linux = process.platform === "linux";
  if (!linux && process.platform !== "darwin") throw new ControlError("native_unavailable", "native locks require Linux or macOS");
  const { i32, u64, ptr: pointer } = FFIType;
  const common = { flock: { args: [i32, i32], returns: i32 }, [linux ? "__errno_location" : "__error"]: { args: [], returns: pointer } };
  const symbols = linux ? { ...common, prctl: { args: [i32, u64, u64, u64, u64], returns: i32 }, pidfd_open: { args: [i32, i32], returns: i32 }, pidfd_send_signal: { args: [i32, i32, pointer, i32], returns: i32 }, waitpid: { args: [i32, pointer, i32], returns: i32 } } : common;
  return dlopen(linux ? "libc.so.6" : "/usr/lib/libSystem.B.dylib", symbols as any);
}
function libc(): any { return (nativeLibrary ??= load_libc()).symbols; }
import { toArrayBuffer } from "bun:ffi";
function native_errno(): number { const address = libc()[process.platform === "linux" ? "__errno_location" : "__error"](); return new Int32Array(toArrayBuffer(address, 0, 4))[0]; }

const lockContext = new AsyncLocalStorage<Set<string>>();
export async function with_lock<T>(path: string, callback: () => Promise<T> | T, blocking = true, reentrant = false, shared = false): Promise<T> {
  const held = lockContext.getStore();
  if (reentrant && held?.has(path)) return callback();
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  const fd = openSync(path, constants.O_RDWR | constants.O_CREAT | constants.O_NOFOLLOW, 0o600);
  try {
    for (;;) {
      // Always probe with LOCK_NB so the caller controls retry behavior and
      // can return object_busy without blocking the event loop.
      const lockFlags = (shared ? 1 : 2) | 4;
      if (libc().flock(fd, lockFlags) === 0) break;
      const code = native_errno();
      if (code !== 11 && code !== 35) throw new ControlError("lock_unavailable", "native lock cannot be verified", { errno: code });
      if (!blocking) throw new ControlError("object_busy", "resource is being changed");
      await Bun.sleep(10);
    }
    return await lockContext.run(new Set([...(held ?? []), path]), callback);
  } finally { libc().flock(fd, 8); closeSync(fd); }
}
export function object_lock_path(directory: string, identity: string): string { return join(directory, "locks", createHash("sha256").update(identity).digest("hex")); }

type Identity = { pid: number; start: string; parent: number; group: number; state: string };
function identity(pid: number): Identity | null {
  try {
    const raw = readFileSync(`/proc/${pid}/stat`, "utf8"), end = raw.lastIndexOf(")"), fields = raw.slice(end + 2).split(/\s+/);
    if (end < 0 || fields.length < 20) throw new Error("invalid stat");
    return { pid, start: fields[19], parent: Number(fields[1]), group: Number(fields[2]), state: fields[0] };
  } catch (error: any) { if (error.code === "ENOENT" || error.code === "ESRCH") return null; throw error; }
}
function process_tree(root: number): Map<number, Identity> {
  const all = new Map<number, Identity>();
  for (const entry of readdirSync("/proc")) if (/^[0-9]+$/.test(entry)) { const item = identity(Number(entry)); if (item) all.set(item.pid, item); }
  const result = new Map<number, Identity>();
  if (all.has(root)) result.set(root, all.get(root)!);
  let changed = true;
  while (changed) { changed = false; for (const item of all.values()) if (!result.has(item.pid) && result.has(item.parent)) { result.set(item.pid, item); changed = true; } }
  return result;
}
function live(item: Identity): boolean {
  const current = identity(item.pid);
  if (!current) return false;
  if (current.start !== item.start) throw new Error("process_owner_identity_unverified");
  return current.state !== "Z";
}
function signal_identity(item: Identity): void {
  const fd = libc().pidfd_open(item.pid, 0);
  if (fd < 0) { if (native_errno() === 3) return; throw new Error("pidfd_open_failed"); }
  try {
    if (!live(item)) return;
    if (libc().pidfd_send_signal(fd, 9, null, 0) < 0 && native_errno() !== 3) throw new Error("pidfd_signal_failed");
  } finally { closeSync(fd); }
}
export interface CommandResult {
  returncode: number; stdout: Buffer; stderr: Buffer;
  stdout_truncated: boolean; stderr_truncated: boolean;
  process_group_killed?: boolean; process_group_cleanup_error?: string | null; reaped?: boolean; pipes_drained?: boolean;
}
export interface CommandOptions {
  input_data?: string | Buffer | null; timeout?: number; check?: boolean;
  stdout_limit?: number; stderr_limit?: number;
}
// A one-shot owner can attribute adopted descendants only while its native
// command boundary is exclusive, including across runner instances.
let commandQueue: Promise<any> = Promise.resolve();
export class CommandRunner {
  run(argv: readonly string[], options: CommandOptions = {}): Promise<CommandResult> {
    // Keep ordinary inventory output generous, but still bounded and drained.
    return this.run_bounded(argv, { stdout_limit: 16 * 1024 * 1024, stderr_limit: 4000, ...options });
  }
  run_bounded(argv: readonly string[], options: CommandOptions = {}): Promise<CommandResult> {
    const job = commandQueue.then(() => this.execute(argv, options));
    commandQueue = job.catch(() => undefined); return job;
  }
  private async execute(argv: readonly string[], { input_data = null, timeout = 30, stdout_limit = 8192, stderr_limit = 4000, check = true }: CommandOptions): Promise<CommandResult> {
    if (!argv.length || argv.some(arg => typeof arg !== "string" || arg.includes("\0"))) throw new ControlError("invalid_request", "invalid native command");
    const linux = process.platform === "linux", tracked = new Map<number, Identity>();
    let baseline = new Map<number, Identity>(), trackingError: string | null = null;
    if (linux) {
      try { if (libc().prctl(36, 1, 0, 0, 0) !== 0) throw new Error("process_owner_boundary_unavailable"); baseline = process_tree(process.pid); }
      catch (error: any) { trackingError = error.message ?? "process_owner_boundary_unavailable"; }
    }
    if (trackingError) throw new ControlError("command_failed", "native process ownership cannot be established before spawn", {
      process_group_killed: false, process_group_cleanup_error: trackingError,
      reaped: true, pipes_drained: true, outcome_verified: true,
    });
    let stdout = Buffer.alloc(0), stderr = Buffer.alloc(0), stdout_truncated = false, stderr_truncated = false;
    let exited = false, closed = false, returncode = -1, spawnError: any = null;
    const child = spawn(argv[0], argv.slice(1), { detached: true, stdio: ["pipe", "pipe", "pipe"] });
    const completion = new Promise<void>(resolve => {
      child.once("error", error => { spawnError = error; exited = true; resolve(); });
      child.once("exit", (code, signal) => { exited = true; returncode = code ?? (signal === "SIGKILL" ? -9 : -1); });
      child.once("close", () => { closed = true; resolve(); });
    });
    child.stdout.on("data", (chunk: Buffer) => { const available = Math.max(0, stdout_limit - stdout.length); if (chunk.length > available) stdout_truncated = true; if (available) stdout = Buffer.concat([stdout, chunk.subarray(0, available)]); });
    child.stderr.on("data", (chunk: Buffer) => { const total = stderr.length + chunk.length; if (total > stderr_limit) stderr_truncated = true; stderr = Buffer.concat([stderr, chunk]).subarray(Math.max(0, total - stderr_limit)); });
    child.stdin.on("error", () => {}); child.stdin.end(input_data ?? undefined);
    const discover = () => {
      if (!linux || trackingError) return;
      try {
        for (const item of process_tree(process.pid).values()) {
          if (item.pid === process.pid) continue;
          const previous = baseline.get(item.pid) ?? tracked.get(item.pid);
          if (previous && previous.start !== item.start) throw new Error("process_owner_identity_unverified");
          if (!baseline.has(item.pid)) tracked.set(item.pid, item);
        }
      } catch (error: any) { trackingError = error.message ?? "process_owner_tree_inspection_failed"; }
    };
    const started = performance.now(); let timedOut = false;
    while (!closed && !spawnError) {
      discover();
      if (performance.now() - started >= timeout * 1000) { timedOut = true; break; }
      await Promise.race([completion, Bun.sleep(Math.min(50, Math.max(1, timeout * 1000 - (performance.now() - started))))]);
    }
    if (spawnError) {
      if (spawnError.code === "ENOENT") throw new ControlError("command_missing", `command not found: ${argv[0]}`);
      throw new ControlError("command_failed", `command could not start: ${argv[0]}`, { cause: spawnError.code ?? spawnError.name });
    }
    discover();
    let remaining: Identity[] = [];
    try { remaining = linux ? [...tracked.values()].filter(live) : []; } catch (error: any) { trackingError = error.message; }
    const leaked = remaining.some(item => item.pid !== child.pid);
    if (timedOut || leaked || trackingError) {
      let killed = !trackingError, cleanupError = trackingError;
      if (linux) {
        try {
          for (let attempt = 0; attempt < 5; attempt++) {
            discover(); if (trackingError) throw new Error(trackingError);
            const members = [...tracked.values()].filter(live);
            if (!members.length) break;
            for (const item of members) signal_identity(item);
            await Bun.sleep(50);
          }
          discover();
          if (trackingError || [...tracked.values()].some(live)) throw new Error(trackingError ?? "process_owner_members_remain");
        } catch (error: any) { killed = false; cleanupError = error.message ?? "process_owner_cleanup_failed"; }
      } else {
        try { if (child.pid) process.kill(-child.pid, "SIGKILL"); } catch (error: any) { if (error.code !== "ESRCH") { killed = false; cleanupError = error.code; } }
      }
      await Promise.race([completion, Bun.sleep(1000)]);
      // Reap only verified adopted children; the spawn owner reaps its leader.
      if (linux) {
        const status = new Int32Array(1);
        for (const item of tracked.values()) if (item.pid !== child.pid) {
          try { const current = identity(item.pid); if (current?.start === item.start && current.parent === process.pid) libc().waitpid(item.pid, ptr(status), 1); } catch { killed = false; cleanupError ??= "process_owner_reap_unverified"; }
        }
      }
      const detail = { returncode, stdout: stdout.toString("utf8"), stderr: stderr.toString("utf8"), stdout_truncated, stderr_truncated, reaped: exited, pipes_drained: closed, process_group_killed: killed, process_group_cleanup_error: cleanupError };
      if (!closed) { child.stdout.destroy(); child.stderr.destroy(); child.stdin.destroy(); child.unref(); }
      throw new ControlError(timedOut ? "command_timeout" : "command_failed", timedOut ? `command timed out: ${argv[0]}` : trackingError ? `command ownership could not be verified: ${argv[0]}` : `command left child processes behind: ${argv[0]}`, detail);
    }
    if (linux) {
      const status = new Int32Array(1);
      for (const item of tracked.values()) {
        const current = identity(item.pid);
        if (item.pid !== child.pid && current?.start === item.start && current.parent === process.pid && current.state === "Z") {
          const reaped = libc().waitpid(item.pid, ptr(status), 1);
          if (reaped !== item.pid && !(reaped < 0 && native_errno() === 10)) throw new ControlError("command_failed", "adopted descendant reaping is unverified", { reaped: false, pipes_drained: closed, process_group_killed: true });
        }
      }
    }
    const result = { returncode, stdout, stderr, stdout_truncated, stderr_truncated };
    if (check && returncode !== 0) throw new ControlError("command_failed", `command failed (${returncode}): ${argv[0]}`, { returncode, stdout: stdout.toString("utf8"), stderr: stderr.toString("utf8").trim().slice(-4000), stdout_truncated, stderr_truncated });
    return result;
  }
}

export class IncusClient {
  constructor(public runner: CommandRunner, public config: Data) {}
  get prefix(): string[] { return ["incus", "--project", String(this.config.project)]; }
  call(args: string[], options: CommandOptions & { bounded?: boolean } = {}): Promise<CommandResult> {
    return options.bounded ? this.runner.run_bounded([...this.prefix, ...args], { timeout: 60, ...options }) : this.runner.run([...this.prefix, ...args], { timeout: 60, ...options });
  }
  async json(args: string[], options: CommandOptions = {}): Promise<any> {
    const result = await this.call([...args, "--format", "json"], options);
    if (result.stdout_truncated) throw new ControlError("incus_invalid_output", "Incus JSON output was truncated");
    try { return parse_json(result.stdout); } catch { throw new ControlError("incus_invalid_output", "Incus returned invalid JSON"); }
  }
  async list_instances(): Promise<Data[]> {
    const result = await this.json(["list"]);
    if (!Array.isArray(result) || result.some(item => !is_record(item) || typeof item.name !== "string" || !item.name)) throw new ControlError("incus_invalid_output", "Incus list output is not an array");
    return result;
  }
  async query(path: string, { project = this.config.project, timeout = 30, bounded = false }: Data = {}): Promise<any> {
    const command = ["incus", "query", path + (path.includes("?") ? "&" : "?") + "project=" + encodeURIComponent(project)];
    const result = await (bounded ? this.runner.run_bounded(command, { timeout }) : this.runner.run(command, { timeout }));
    if (result.stdout_truncated) throw new ControlError("incus_invalid_output", "query output was truncated");
    try { return parse_json(result.stdout); } catch { throw new ControlError("incus_invalid_output", "query did not return JSON"); }
  }
  async instance_info(name: string, options: Data = {}): Promise<Data> {
    const value = await this.query(`/1.0/instances/${encodeURIComponent(name)}/state`, options);
    if (!is_record(value) || typeof value.status !== "string" || !value.status) throw new ControlError("incus_invalid_output", "Incus info output is not an object"); return value;
  }
  async config_show(name: string): Promise<Data> {
    const value = await this.query(`/1.0/instances/${encodeURIComponent(name)}`);
    if (!is_record(value)) throw new ControlError("incus_invalid_output", "Incus config output is not an object"); return value;
  }
  async init(image: string, name: string, metadata: Data = {}): Promise<void> {
    await this.call(["init", image, name, "--vm", "--profile", String(this.config.profile), "--config", "boot.autostart=false", ...Object.entries(metadata).flatMap(([key, value]) => ["--config", `${key}=${value}`])], { timeout: 300, bounded: true });
  }
  async start(name: string): Promise<void> { await this.call(["start", name], { timeout: 120 }); }
  async stop(name: string, force: boolean | Data = true): Promise<void> { await this.call(["stop", name, ...((typeof force === "object" ? force.force : force) ? ["--force"] : [])], { timeout: 8 }); }
  async delete(name: string): Promise<void> { await this.call(["delete", name, "--force"], { timeout: 120 }); }
  async wait(name: string, condition: string, seconds: number): Promise<void> {
    let result: CommandResult;
    try { result = await this.call(["wait", name, condition, "--timeout", String(seconds), "--interval", "1"], { timeout: seconds, check: false }); }
    catch (error) { if (error instanceof ControlError && error.code === "command_timeout") throw new GuestWaitTimeout({ transport_timeout: true }); throw error; }
    if (!result.returncode) return;
    const detail = { returncode: result.returncode, stderr: result.stderr.toString("utf8").trim().slice(-4000) };
    if (/(?:timed?\s*out|timeout)/i.test(detail.stderr)) throw new GuestWaitTimeout(detail);
    throw new ControlError("command_failed", `command failed (${result.returncode}): incus`, detail);
  }
  async config_set(name: string, key: string, value: string): Promise<void> { await this.call(["config", "set", name, `${key}=${value}`]); }
  async device_set(name: string, device: string, key: string, value: string): Promise<void> {
    const info = await this.config_show(name); await this.call(["config", "device", device in (info.devices ?? {}) ? "set" : "override", name, device, `${key}=${value}`]);
  }
  async device_add(name: string, device: string, type: string, properties: Data): Promise<void> { await this.call(["config", "device", "add", name, device, type, ...Object.entries(properties).map(([key, value]) => `${key}=${value}`)]); }
  async device_remove(name: string, device: string): Promise<void> { await this.call(["config", "device", "remove", name, device]); }
  exec(name: string, argv: string[], options: CommandOptions & { bounded?: boolean } = {}): Promise<CommandResult> { return this.call(["exec", name, "--", ...argv], options); }
  async network_forwards(): Promise<Data[]> { const value = await this.json(["network", "forward", "list", this.config.network]); if (!Array.isArray(value)) throw new ControlError("incus_invalid_output", "Incus network forwards output is not an array"); return value.filter(is_record); }
  async instance_log(name: string, { timeout = 5 }: Data = {}): Promise<string> { const result = await this.call(["info", name, "--show-log"], { bounded: true, timeout }); return result.stdout.toString("utf8") + (result.stdout_truncated ? "\n[TRUNCATED BY DIAGNOSTIC LIMIT]" : ""); }
}
