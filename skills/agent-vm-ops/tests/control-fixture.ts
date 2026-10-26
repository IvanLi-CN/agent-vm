// skills/agent-vm-ops/tests/control-fixture.ts
//
// Offline native-boundary fixture. This does not constitute real Incus acceptance.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  chmodSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import {
  basename,
  dirname,
  isAbsolute,
  join,
  relative,
  resolve,
} from "node:path";

import { Control } from "../scripts/agent_vm_control.ts";
import {
  CommandRunner,
  type CommandOptions,
  type CommandResult,
} from "../scripts/control-native.ts";
import { CTResources } from "../scripts/control-observation.ts";
import { StateStore } from "../scripts/control-store.ts";
import { SystemdManager } from "../scripts/control-systemd.ts";
import {
  ControlError,
  GIB,
  PROTOCOL_VERSION,
  canonical_json,
  parse_json,
  type Data,
  type Integer,
} from "../scripts/control-types.ts";
import {
  FIXED_ARTIFACTS,
  MANIFEST_NAME,
  build_manifest,
} from "../scripts/release-manifest.ts";

export { ControlError, GIB, PROTOCOL_VERSION, StateStore };

export const KEY =
  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest fixture";

type Awaitable<T> = T | Promise<T>;
type InputData = CommandOptions["input_data"];
type Hook<Args extends unknown[], Result = void> =
  (...args: Args) => Awaitable<Result>;

export function command_result({
  returncode = 0,
  stdout = "",
  stderr = "",
  stdout_truncated = false,
  stderr_truncated = false,
  ...cleanup
}: Omit<Partial<CommandResult>, "stdout" | "stderr"> & {
  stdout?: string | Buffer;
  stderr?: string | Buffer;
} = {}): CommandResult {
  return {
    returncode,
    stdout: Buffer.from(stdout),
    stderr: Buffer.from(stderr),
    stdout_truncated,
    stderr_truncated,
    ...cleanup,
  };
}

function split_setting(value: string): [string, string] {
  const index = value.indexOf("=");
  assert(index > 0, `Invalid native setting: ${value}`);
  return [value.slice(0, index), value.slice(index + 1)];
}

function growth_tools(): Data {
  return {
    findmnt: "/usr/bin/findmnt",
    lsblk: "/usr/bin/lsblk",
    blockdev: "/usr/sbin/blockdev",
    growpart: "/usr/bin/growpart",
    resize2fs: "/usr/sbin/resize2fs",
  };
}

export interface GuestGrowthOptions {
  disk_gib?: number;
  status?: string;
  attempts?: number;
  returncode?: number;
  stderr?: string | Buffer;
}

export function guest_growth_result({
  disk_gib = 20,
  status = "grown",
  attempts = 1,
  returncode = 0,
  stderr = "",
}: GuestGrowthOptions = {}): CommandResult {
  const size = disk_gib * GIB;
  const filesystem = size - 1_000_000;
  const threshold = Math.floor(size * 95 / 100);
  const filesystem_before =
    status === "already_grown" && attempts === 0
      ? filesystem
      : threshold - 1_000_000;

  const failure_capacity: Data = {
    requested_bytes: size,
    disk_bytes: size,
    partition_bytes: size,
    filesystem_bytes: filesystem,
    filesystem_before,
    filesystem_threshold_bytes: threshold,
    filesystem: "ext4",
    source: "/dev/sda2",
    parent: "/dev/sda",
    partition: "2",
    tools: growth_tools(),
    verified: true,
  };

  const attempt_records: Data[] = Array.from(
    { length: attempts },
    (_, index) => ({ attempt: index + 1 }),
  );

  if (
    ["grown_after_retry", "already_grown"].includes(status) &&
    attempts === 2
  ) {
    attempt_records[0].error = {
      phase: "filesystem-grow",
      command: ["resize2fs", "/dev/sda2"],
      returncode: 1,
      stdout: "",
      stderr: "resize2fs: Device or resource busy",
      capacity: failure_capacity,
      transient: true,
      stdout_truncated: false,
      stderr_truncated: false,
    };
  }

  const payload: Data = {
    status,
    virtual_bytes: size,
    partition_bytes: size,
    filesystem_before,
    filesystem_bytes: filesystem,
    filesystem_threshold_bytes: threshold,
    filesystem: "ext4",
    source: "/dev/sda2",
    parent: "/dev/sda",
    partition: "2",
    attempts,
    capacity: {
      status,
      ...failure_capacity,
      attempts: attempt_records,
    },
  };

  return command_result({
    returncode,
    stdout: returncode === 0 ? canonical_json(payload) : "",
    stderr,
  });
}

export function guest_growth_failure({
  disk_gib = 20,
  phase = "filesystem-grow",
  command = ["resize2fs", "/dev/sda2"],
  returncode = 1,
  stderr = "resize2fs: transient guest failure",
}: {
  disk_gib?: number;
  phase?: string;
  command?: string[];
  returncode?: number;
  stderr?: string;
} = {}): CommandResult {
  const size = disk_gib * GIB;
  const capacity: Data = {
    requested_bytes: size,
    disk_bytes: size,
    partition_bytes: size,
    filesystem_bytes: 10 * GIB,
    filesystem_before: 10 * GIB,
    filesystem_threshold_bytes: Math.floor(size * 95 / 100),
    filesystem: "ext4",
    source: "/dev/sda2",
    parent: "/dev/sda",
    partition: "2",
    tools: growth_tools(),
    verified: true,
  };

  const error: Data = {
    phase,
    command: [...command],
    returncode,
    stdout: "",
    stderr,
    capacity,
    transient: false,
    stdout_truncated: false,
    stderr_truncated: false,
  };

  const report_error = {
    ...error,
    capacity: {
      ...capacity,
      attempts: [{ attempt: 1, error }],
    },
  };

  return command_result({
    returncode,
    stderr: canonical_json({ ok: false, error: report_error }) + "\n",
  });
}

export type NativeCall = [
  argv: string[],
  input_data: InputData,
  timeout: number,
];

export interface NativeInvocation {
  argv: string[];
  options: CommandOptions;
  bounded: boolean;
}

export class NativeCommands extends CommandRunner {
  instances: Record<string, Data> = {};
  calls: NativeCall[] = [];
  invocations: NativeInvocation[] = [];

  free = 80 * GIB;
  proxy_inventory: Data[] = [];
  forwards: Data[] = [];
  image_available = true;
  image_type = "container";
  proxy_allowed = true;
  project_limits: Data = {};
  storage_source: string | null = null;

  guest_growth_results: Array<CommandResult | Error> = [];

  host_ports_stdout: string | Buffer = "";
  conntrack_stdout: string | Buffer = "";
  systemctl_stdout: string | Buffer = "";

  failure: Hook<
    [argv: string[], options: CommandOptions],
    boolean | ControlError | void
  > | null = null;

  // A general boundary hook can return a complete result or throw an error.
  // Returning undefined continues through the modeled native protocol.
  on_run: Hook<
    [argv: string[], options: CommandOptions],
    CommandResult | void
  > | null = null;

  on_start: Hook<[name: string]> | null = null;
  on_version: Hook<[]> | null = null;
  on_list_instances: Hook<[]> | null = null;
  on_wait: Hook<[name: string, condition: string]> | null = null;
  on_config_set: Hook<
    [name: string, key: string, value: string]
  > | null = null;

  async run(
    argv: readonly string[],
    options: CommandOptions = {},
  ): Promise<CommandResult> {
    return this.execute_fake(argv, options, false);
  }

  async run_bounded(
    argv: readonly string[],
    options: CommandOptions = {},
  ): Promise<CommandResult> {
    // Preserve injected truncation flags rather than deriving them from the
    // supplied limits; malformed/truncated-report tests inject these directly.
    return this.execute_fake(argv, options, true);
  }

  private async execute_fake(
    argv: readonly string[],
    options: CommandOptions,
    bounded: boolean,
  ): Promise<CommandResult> {
    const args = [...argv];
    const normalized: CommandOptions = {
      input_data: null,
      timeout: 30,
      check: true,
      ...options,
    };

    this.calls.push([
      args,
      normalized.input_data,
      normalized.timeout!,
    ]);
    this.invocations.push({
      argv: [...args],
      options: { ...normalized },
      bounded,
    });

    const failure = await this.failure?.(args, normalized);
    if (failure instanceof ControlError) throw failure;
    if (failure) {
      throw new ControlError(
        "command_failed",
        "injected native failure",
      );
    }

    const injected = await this.on_run?.(args, normalized);
    const result = injected ??
      await this.native_command(args, normalized);

    if (normalized.check !== false && result.returncode !== 0) {
      throw new ControlError(
        "command_failed",
        `command failed (${result.returncode}): ${args[0]}`,
        {
          returncode: result.returncode,
          stdout: result.stdout.toString("utf8"),
          stderr: result.stderr.toString("utf8"),
          stdout_truncated: result.stdout_truncated,
          stderr_truncated: result.stderr_truncated,
        },
      );
    }

    return result;
  }

  private instance(name: string): Data {
    const item = this.instances[name];
    if (!item) {
      throw new ControlError("command_failed", "instance absent");
    }
    return item;
  }

  private async native_command(
    args: string[],
    _options: CommandOptions,
  ): Promise<CommandResult> {
    assert(args.length > 0, "Empty native command");

    if (args[0] === "conntrack") {
      return command_result({ stdout: this.conntrack_stdout });
    }
    if (args[0] === "ss") {
      return command_result({ stdout: this.host_ports_stdout });
    }
    if (args[0] === "systemctl") {
      return command_result({ stdout: this.systemctl_stdout });
    }

    if (args[0] === "incus" && args[1] === "version") {
      await this.on_version?.();
      return command_result({ stdout: "7.5.1" });
    }

    assert.equal(args[0], "incus", `Unexpected command: ${args}`);

    let command: string[];
    if (args[1] === "query") {
      command = args.slice(1);
    } else {
      assert.deepEqual(
        args.slice(0, 3),
        ["incus", "--project", "agents"],
      );
      command = args.slice(3);
    }

    const verb = command[0];

    if (verb === "query") {
      if (args[1] !== "query") {
        throw new ControlError(
          "command_failed",
          "Incus query rejects global --project",
        );
      }
      const value = await this.query_value(command[1]);
      return command_result({ stdout: canonical_json(value) });
    }

    if (verb === "list") {
      await this.on_list_instances?.();
      return command_result({
        stdout: canonical_json(Object.values(this.instances)),
      });
    }

    if (verb === "init") {
      const [, _image, name] = command;
      assert(!this.instances[name], "duplicate init");

      const config: Data = {};
      for (let index = 1; index < command.length; index++) {
        if (command[index - 1] === "--config") {
          const [key, value] = split_setting(command[index]);
          config[key] = value;
        }
      }

      this.instances[name] = {
        name,
        status: "Stopped",
        config,
        devices: {},
        expanded_devices: {
          eth0: { type: "nic", network: "agentsbr0" },
          root: { type: "disk", size: "20GiB" },
        },
      };
      return command_result();
    }

    if (["start", "stop", "delete"].includes(verb)) {
      const name = command[1];
      const item = this.instance(name);

      if (verb === "delete") {
        delete this.instances[name];
        this.free += 20 * GIB;
      } else {
        item.status = verb === "start" ? "Running" : "Stopped";
        item.agent_ready = false;
        if (verb === "start") {
          const size = String(item.expanded_devices.root.size);
          assert(/^\d+GiB$/.test(size), `Invalid disk size: ${size}`);
          item.guest_disk_bytes = Number(size.slice(0, -3)) * GIB;
          await this.on_start?.(name);
        }
      }
      return command_result();
    }

    if (verb === "wait") {
      const [, name, condition] = command;
      this.instance(name);
      await this.on_wait?.(name, condition);
      if (condition === "agent") {
        this.instance(name).agent_ready = true;
      }
      return command_result();
    }

    if (command[0] === "config" && command[1] === "show") {
      return command_result({
        stdout: canonical_json(this.instance(command[2])),
      });
    }

    if (command[0] === "config" && command[1] === "set") {
      const [, , name, setting] = command;
      const [key, value] = split_setting(setting);
      this.instance(name).config[key] = value;
      await this.on_config_set?.(name, key, value);
      return command_result();
    }

    if (command[0] === "config" && command[1] === "device") {
      const [, , operation, name, device] = command;
      const item = this.instance(name);

      if (operation === "remove") {
        delete item.devices[device];
        delete item.expanded_devices[device];
      } else {
        assert(
          ["add", "set", "override"].includes(operation),
          `Unexpected device operation: ${operation}`,
        );
        const fields = command.slice(5);
        const base: Data = {
          ...(item.expanded_devices[device] ?? {}),
        };
        if (operation === "add") base.type = fields.shift();
        for (const field of fields) {
          const [key, value] = split_setting(field);
          base[key] = value;
        }
        item.devices[device] = base;
        item.expanded_devices[device] = { ...base };
      }

      return command_result();
    }

    if (
      command[0] === "network" &&
      command[1] === "forward" &&
      command[2] === "list"
    ) {
      return command_result({
        stdout: canonical_json(this.forwards),
      });
    }

    if (verb === "info" && command.includes("--show-log")) {
      this.instance(command[1]);
      return command_result();
    }

    if (verb === "exec") {
      const item = this.instance(command[1]);
      if (!item.agent_ready) {
        throw new ControlError(
          "command_failed",
          "VM agent isn't currently running",
        );
      }

      const separator = command.indexOf("--");
      assert(separator >= 0, "Incus exec requires --");
      const guest = command.slice(separator + 1);

      if (
        guest.includes("cat /etc/ssh/ssh_host_ed25519_key.pub") ||
        (
          guest.length === 2 &&
          guest[0] === "cat" &&
          guest[1] === "/etc/ssh/ssh_host_ed25519_key.pub"
        )
      ) {
        return command_result({ stdout: KEY });
      }

      if (
        guest.length === 2 &&
        guest[0] === "cat" &&
        guest[1] === "/proc/sys/kernel/random/boot_id"
      ) {
        return command_result({
          stdout:
            item.boot_id ??
            "00000000-0000-4000-8000-000000000001",
        });
      }

      // Guest growth is a Bun program supplied on stdin or via -e.
      // No Python controller or Python growth-program bridge is involved.
      const bun_growth =
        basename(guest[0] ?? "") === "bun" &&
        (
          guest[1] === "-" ||
          guest[1] === "-e" ||
          guest[1] === "--eval" ||
          (guest[1] === "run" && guest[2] === "-")
        );

      if (bun_growth) {
        if (this.guest_growth_results.length > 0) {
          const response = this.guest_growth_results.shift()!;
          if (response instanceof Error) throw response;
          return command_result(response);
        }

        const requested_gib = Number(guest.at(-1));
        assert(
          Number.isSafeInteger(requested_gib) && requested_gib > 0,
          "Guest growth requires a final integer disk capacity",
        );

        const size = Number(item.guest_disk_bytes);
        if (size < requested_gib * GIB) {
          throw new ControlError(
            "command_failed",
            "guest virtual disk size is stale",
          );
        }

        const response = guest_growth_result({
          disk_gib: requested_gib,
        });
        const payload = parse_json(response.stdout);
        payload.virtual_bytes = size;
        payload.partition_bytes = size;
        payload.filesystem_bytes = size - 1_000_000;
        payload.capacity.disk_bytes = size;
        payload.capacity.partition_bytes = size;
        payload.capacity.filesystem_bytes = size - 1_000_000;

        return command_result({
          stdout: canonical_json(payload),
        });
      }

      // The original fake accepts readiness/tool checks, SSH installation,
      // and other ordinary guest setup commands once the agent is ready.
      return command_result();
    }

    assert.fail(`Unexpected Incus command: ${canonical_json(command)}`);
  }

  private async query_value(target: string): Promise<unknown> {
    const url = new URL(target, "http://fixture.invalid");
    const path = url.pathname;
    const project = url.searchParams.get("project") ?? "agents";

    if (path === "/1.0/projects") {
      return [{ name: "agents" }, { name: "default" }];
    }
    if (path === "/1.0/projects/agents") {
      return {
        config: {
          ...this.project_limits,
          "restricted.devices.proxy":
            this.proxy_allowed ? "allow" : "block",
        },
      };
    }
    if (path === "/1.0/storage-pools/agents") {
      return {
        name: "agents",
        driver: "dir",
        config: { source: this.storage_source },
      };
    }
    if (path === "/1.0/network-acls/agent-vm-isolation") {
      return parse_json(
        readFileSync(
          new URL("../assets/network-acl.json", import.meta.url),
        ),
      );
    }
    if (path.startsWith("/1.0/images/")) {
      if (!this.image_available) {
        throw new ControlError("command_failed", "image absent");
      }
      return {
        fingerprint: decodeURIComponent(path.split("/").at(-1)!),
        type: this.image_type,
      };
    }
    if (path === "/1.0/instances") {
      return project === "agents"
        ? Object.values(this.instances)
        : this.proxy_inventory;
    }
    if (path === "/1.0/networks") {
      return [{
        managed: true,
        type: "bridge",
        name: "agentsbr0",
      }];
    }
    if (path.endsWith("/forwards")) return this.forwards;
    if (path.endsWith("/leases")) return [];

    if (path.startsWith("/1.0/instances/")) {
      const name = decodeURIComponent(path.split("/")[3]);
      const item = this.instance(name);
      if (!path.endsWith("/state")) return item;
      return {
        status: item.status,
        network: {
          eth0: {
            addresses: [{
              family: "inet",
              address: item.expanded_devices.eth0["ipv4.address"],
            }],
          },
        },
      };
    }

    assert.fail(`Unexpected Incus query: ${target}`);
  }
}

export class FakeResources {
  cpu_value = 16;

  memory_value: Data = {
    scope: "ct-memory",
    sources: ["lxcfs"],
    limit_bytes: 32 * GIB,
    available_bytes: 24 * GIB,
    threshold_bytes: Math.floor((32 * GIB + 9) / 10),
  };

  cpu_pressure_value: Data = {
    scope: "ct-cpu-pressure",
    sources: ["cgroupfs"],
    mountpoint: "/sys/fs/cgroup",
    mount_root: "/",
    filesystem: "cgroup2",
    cgroup_path: "/",
    metric: "/sys/fs/cgroup/cpu.pressure",
    some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
    full: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
  };

  text_values: Record<string, string> = {
    "/proc/sys/net/ipv4/ip_local_port_range": "32768 60999",
    "/sys/fs/cgroup/cpu.max": "max 100000",
    "/sys/fs/cgroup/memory.max": "max",
  };

  disk_total = 100 * GIB;
  conntrack_path: string | null = "/usr/bin/conntrack";

  on_cpu: Hook<[], number> | null = null;
  on_memory: Hook<[], Data> | null = null;
  on_cpu_pressure: Hook<[], Data> | null = null;
  on_disk_usage: Hook<
    [path: string],
    { total: number; used: number; free: number }
  > | null = null;
  on_read_text: Hook<[path: string], string> | null = null;
  on_which: Hook<[command: string], string | null> | null = null;

  constructor(public readonly runner: NativeCommands) {
  }

  async cpu(): Promise<number> {
    return this.on_cpu ? await this.on_cpu() : this.cpu_value;
  }

  async memory(): Promise<Data> {
    return this.on_memory
      ? await this.on_memory()
      : this.memory_value;
  }

  async cpu_pressure(): Promise<Data> {
    return this.on_cpu_pressure
      ? await this.on_cpu_pressure()
      : this.cpu_pressure_value;
  }

  // These host-observation seams replace the old fixture's process-wide
  // Path.read_text, shutil.disk_usage, shutil.which, and cpu_count patches.
  async read_text(path: string): Promise<string> {
    if (this.on_read_text) return await this.on_read_text(path);
    if (Object.hasOwn(this.text_values, path)) {
      return this.text_values[path];
    }
    return readFileSync(path, "utf8");
  }

  async disk_usage(
    path: string,
  ): Promise<{ total: number; used: number; free: number }> {
    if (this.on_disk_usage) return await this.on_disk_usage(path);
    return {
      total: this.disk_total,
      used: this.disk_total - this.runner.free,
      free: this.runner.free,
    };
  }

  async which(command: string): Promise<string | null> {
    return this.on_which
      ? await this.on_which(command)
      : this.conntrack_path;
  }

  async cpu_count(): Promise<number> {
    return 16;
  }
}

export class FakeSystemd extends SystemdManager {
  readonly scheduled = new Map<string, string>();
  readonly removed: string[] = [];
  readonly retries: Array<{
    task_id: string;
    generation: Integer;
  }> = [];

  unit_active = true;
  service_ready = true;
  service_asset_verified = true;

  on_schedule: Hook<
    [task_id: string, generation: Integer, expires_at: string]
  > | null = null;
  on_remove: Hook<
    [task_id: string, generation: Integer]
  > | null = null;
  on_retry: Hook<
    [task_id: string, generation: Integer]
  > | null = null;

  constructor(runner: NativeCommands, config: Data) {
    super(runner, config, { enabled: false });
  }

  async schedule_expiry(
    task_id: string,
    generation: Integer,
    expires_at: string,
  ): Promise<string> {
    await this.on_schedule?.(task_id, generation, expires_at);
    const unit = SystemdManager.timer_unit(task_id, generation);
    this.scheduled.set(unit, expires_at);
    return unit;
  }

  async remove_expiry(
    task_id: string,
    generation: Integer,
  ): Promise<string> {
    await this.on_remove?.(task_id, generation);
    const unit = SystemdManager.timer_unit(task_id, generation);
    this.scheduled.delete(unit);
    this.removed.push(unit);
    return unit;
  }

  async expiry_active(
    task_id: string,
    generation: Integer,
    expires_at: string,
  ): Promise<boolean> {
    // Match the original disabled SystemdManager behavior.
    if (!this.enabled) return true;
    const unit = SystemdManager.timer_unit(task_id, generation);
    return this.scheduled.get(unit) === expires_at;
  }

  async retry_cleanup(
    task_id: string,
    generation: Integer,
  ): Promise<void> {
    await this.on_retry?.(task_id, generation);
    this.retries.push({ task_id, generation });
  }

  async enabled_unit_active(_unit: string): Promise<boolean> {
    return this.unit_active;
  }

  async enabled_service_ready(_unit: string): Promise<boolean> {
    return this.service_ready;
  }

  async service_asset_active(
    _unit: string,
    _required_lines: readonly string[],
  ): Promise<boolean> {
    return this.service_asset_verified;
  }

  async enable_periodic_units(): Promise<void> {}
}

export interface RequestOptions extends Data {
  session?: string | null;
  operation?: string | null;
}

export interface FixtureOptions {
  config?: Data;
  runner?: NativeCommands;
  resources?: FakeResources;
  systemd?: FakeSystemd;
  now?: Date;
}

function remove_owned_temp(
  root: string,
  parent: string,
  identity: { dev: number; ino: number },
): void {
  assert.equal(dirname(root), parent);
  assert(basename(root).startsWith("agent-vm-control-"));
  assert.notEqual(root, parent);

  let current: ReturnType<typeof lstatSync>;
  try {
    current = lstatSync(root);
  } catch (error: any) {
    if (error.code === "ENOENT") return;
    throw error;
  }

  assert(current.isDirectory(), "Fixture root is no longer a directory");
  assert(!current.isSymbolicLink(), "Fixture root became a symlink");
  assert.equal(current.dev, identity.dev, "Fixture root device changed");
  assert.equal(current.ino, identity.ino, "Fixture root inode changed");
  assert.equal(realpathSync(root), root);

  // Only the exact directory returned by mkdtempSync is removed.
  rmSync(root, { recursive: true, force: false });
}

export class Fixture {
  readonly root: string;
  now: Date;
  config: Data;
  readonly runner: NativeCommands;
  readonly store: StateStore;
  readonly resources: FakeResources;
  readonly systemd: FakeSystemd;
  readonly control: Control;
  readonly close: () => void;

  counter = 0;

  static guest_growth_result = guest_growth_result;
  static guest_growth_failure = guest_growth_failure;

  readonly guest_growth_result = guest_growth_result;
  readonly guest_growth_failure = guest_growth_failure;
  readonly error = expect_error;

  constructor(options: FixtureOptions = {}) {
    const parent = realpathSync(tmpdir());
    const root = mkdtempSync(join(parent, "agent-vm-control-"));
    const identity = lstatSync(root);
    let store: StateStore | undefined;

    this.root = root;
    this.now = new Date(
      options.now?.getTime() ?? Date.UTC(2026, 9, 4),
    );

    // Preserve the original Fixture defaults exactly.
    this.config = {
      project: "agents",
      profile: "agent-vm",
      image_fingerprint: "a".repeat(64),
      network: "agentsbr0",
      network_acl: "agent-vm-isolation",
      pool: "agents",
      host_address: "198.51.100.19",
      port_start: 20000,
      port_end: 20030,
      vm_ip_start: "198.51.100.20",
      vm_ip_end: "198.51.100.250",
      gateway_ip: "198.51.100.1",
      initial_lease_seconds: 1800,
      renew_seconds: 900,
      max_lease_seconds: 7200,
      cache_seconds: 86400,
      create_cost_bytes: 4 * GIB,
      resume_cost_bytes: GIB,
      resize_cost_bytes: GIB,
      storage_path: root,
      cpu_budget: 4,
      memory_budget_mib: 16384,
      required_guest_tools: ["bun", "python3", "sshd", "rsync", "docker"],
      state_db: join(root, "state.sqlite3"),
      timer_dir: join(root, "systemd"),
      release_fence_path: join(root, "release-activation.fence"),
      release_manifest_path: join(root, "absent-release-manifest.json"),
      ...options.config,
    };

    try {
      this.runner = options.runner ?? new NativeCommands();
      this.runner.storage_source = this.config.storage_path;
      store = new StateStore(this.config.state_db);
      this.store = store;

      this.resources =
        options.resources ?? new FakeResources(this.runner);
      this.systemd =
        options.systemd ?? new FakeSystemd(this.runner, this.config);

      this.control = new Control(this.config, {
        runner: this.runner,
        store: this.store,
        resources: this.resources,
        systemd: this.systemd,
        clock: () => this.now,
      });
      this.config = this.control.config;
    } catch (error) {
      try {
        store?.close();
        remove_owned_temp(root, parent, identity);
      } catch (cleanup_error) {
        throw new AggregateError(
          [error, cleanup_error],
          "Fixture construction and cleanup failed",
        );
      }
      throw error;
    }

    let closed = false;
    const owned_store = this.store;

    this.close = () => {
      if (closed) return;
      owned_store.close();
      remove_owned_temp(root, parent, identity);
      closed = true;
    };
  }

  advance(seconds: number): void {
    this.now = new Date(this.now.getTime() + seconds * 1000);
  }

  request(
    action: string,
    {
      session = "thread-a",
      operation = null,
      ...fields
    }: RequestOptions = {},
  ): Data {
    this.counter += 1;
    const task = session === null
      ? null
      : this.store.get_task({ session_id: session });

    const value: Data = {
      protocol: PROTOCOL_VERSION,
      action,
      request_id: `request-${this.counter}`,
    };

    if (action !== "admin-metrics" && session !== null) {
      value.session_id = session;
    }

    if (task && Control.SESSION_ACTIONS.has(action)) {
      value.task_id = task.task_id;
    }

    if (action === "port-list") {
      value.generation = task?.generation ?? 0;
    }

    if (Control.WRITE_ACTIONS.has(action)) {
      value.operation_id = operation ?? `op-${this.counter}`;
      value.generation = task?.generation ?? 0;
    }

    // Last-wins overrides intentionally permit malformed protocol, identity,
    // generation, compatibility, and operation fields in negative tests.
    return { ...value, ...fields };
  }

  preflight(request: Data): Data[] {
    return this.control._preflight_release(request);
  }

  async call(
    action: string,
    options: RequestOptions = {},
  ): Promise<Data> {
    const response = await this.control.dispatch(
      this.request(action, options),
    );
    return response.result;
  }

  async acquire(options: RequestOptions = {}): Promise<Data> {
    return this.call("acquire", {
      public_key: KEY,
      ...options,
    });
  }

  runtime_manifest(
    root: string,
    revision: string,
    {
      client = Buffer.from("client\n"),
      control = Buffer.from("control\n"),
    }: {
      client?: string | Buffer;
      control?: string | Buffer;
    } = {},
  ) {
    const destination = resolve(root);
    const suffix = relative(this.root, destination);
    assert(
      suffix !== ".." &&
      !suffix.startsWith("../") &&
      !isAbsolute(suffix),
      "Runtime fixture must remain inside its temporary root",
    );

    const artifacts = [...FIXED_ARTIFACTS]
      .sort(([left], [right]) =>
        left < right ? -1 : left > right ? 1 : 0
      )
      .map(([name, path, role, mode]) => {
        const content = Buffer.from(
          role === "client"
            ? client
            : role === "control"
              ? control
              : `${role}:${path}\n`,
        );
        const target = join(destination, path);
        mkdirSync(dirname(target), { recursive: true });
        writeFileSync(target, content);
        chmodSync(target, mode);
        return {
          name,
          path,
          role,
          sha256: createHash("sha256").update(content).digest("hex"),
          mode,
        };
      });

    const manifest = build_manifest(revision, artifacts);
    const manifest_path = join(destination, MANIFEST_NAME);
    writeFileSync(
      manifest_path,
      canonical_json(manifest),
      "utf8",
    );
    return [manifest, manifest_path] as const;
  }
}

export function fixture(options: FixtureOptions = {}): Fixture {
  return new Fixture(options);
}

export async function expect_error(
  code: string,
  operation: () => Awaitable<unknown>,
): Promise<ControlError> {
  try {
    await operation();
  } catch (error) {
    assert(error instanceof ControlError, "Expected ControlError");
    assert.equal(error.code, code);
    return error;
  }
  assert.fail(`Expected ControlError(${code})`);
}
