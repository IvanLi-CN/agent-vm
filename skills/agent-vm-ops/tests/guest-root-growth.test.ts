import { describe, expect, test } from "bun:test";
import { Buffer } from "node:buffer";
import {
  grow_guest_root,
  guest_main,
  type GuestFilesystem,
  type GuestRootGrowthOptions,
} from "../scripts/guest-root-growth.ts";
import type { CommandResult } from "../scripts/control-native.ts";
import {
  ControlError,
  is_record,
  parse_json,
  type Data,
} from "../scripts/control-types.ts";

const GIB = 1024n ** 3n;
const SOURCE = "/dev/sda2";
const DISK = "/dev/sda";
const MARKER = "/sys/class/block/sda2/partition";
const GROW = ["growpart", DISK, "2"];
const REREAD = ["blockdev", "--rereadpt", DISK];
const SETTLE = ["udevadm", "settle", "--timeout=5"];

interface Step {
  returncode?: number;
  stdout?: string;
  stderr?: string;
  stdout_truncated?: boolean;
  stderr_truncated?: boolean;
  partition?: bigint;
  filesystem?: bigint;
  error?: ControlError;
}

interface FixtureOptions {
  disk_gib?: number | bigint;
  disk_bytes?: bigint;
  partition_bytes?: bigint;
  filesystem_bytes?: bigint;
  filesystem_type?: string;
  source_output?: string;
  parent_output?: string;
  source_type?: string;
  parent_type?: string;
  marker_exists?: boolean;
  marker_text?: string;
  missing_tools?: string[];
  udevadm?: boolean;
  source_result?: Partial<CommandResult>;
  growpart?: Step[];
  resize?: Step[];
  reread?: Step[];
  settle?: Step[];
}

function result(
  stdout = "",
  stderr = "",
  returncode = 0,
): CommandResult {
  return {
    returncode,
    stdout: Buffer.from(stdout),
    stderr: Buffer.from(stderr),
    stdout_truncated: false,
    stderr_truncated: false,
  };
}

function fixture(config: FixtureOptions = {}) {
  const disk_gib = config.disk_gib ?? 20n;
  const target = BigInt(disk_gib) * GIB;
  const threshold = target * 95n / 100n;
  const state = {
    disk: config.disk_bytes ?? target,
    partition: config.partition_bytes ?? target / 2n,
    filesystem: config.filesystem_bytes ?? target / 4n,
  };
  const calls: string[][] = [];
  const lookups: string[] = [];
  const sleeps: number[] = [];
  const scripts = {
    growpart: [...(config.growpart ?? [{ partition: target }])],
    resize: [...(config.resize ?? [{ filesystem: threshold }])],
    reread: [...(config.reread ?? [{ partition: target }])],
    settle: [...(config.settle ?? [{}])],
  };

  function perform(kind: keyof typeof scripts): CommandResult {
    const step = scripts[kind].shift();
    if (step === undefined) {
      throw new Error(`Unexpected extra ${kind} invocation`);
    }
    if (step.error !== undefined) {
      throw step.error;
    }
    if (step.partition !== undefined) {
      state.partition = step.partition;
    }
    if (step.filesystem !== undefined) {
      state.filesystem = step.filesystem;
    }
    return {
      ...result(
        step.stdout ?? "",
        step.stderr ?? "",
        step.returncode ?? 0,
      ),
      stdout_truncated: step.stdout_truncated ?? false,
      stderr_truncated: step.stderr_truncated ?? false,
    };
  }

  // Every filesystem method is injected; no host filesystem fallback is used.
  const fs: GuestFilesystem = {
    realpathSync(filename) {
      return filename;
    },
    statSync(filename) {
      return {
        isFile: () =>
          filename === MARKER && config.marker_exists !== false,
      };
    },
    readFileSync(filename) {
      if (filename !== MARKER) {
        throw new Error(`Unexpected filesystem read: ${filename}`);
      }
      return Buffer.from(config.marker_text ?? "2\n");
    },
    accessSync(filename) {
      throw new Error(`Unexpected executable access: ${filename}`);
    },
    statfsSync(filename, options) {
      expect(filename).toBe("/");
      expect(options).toEqual({ bigint: true });
      return { blocks: state.filesystem, bsize: 1n };
    },
  };

  const options: GuestRootGrowthOptions = {
    fs,
    async which(command) {
      lookups.push(command);
      if (config.missing_tools?.includes(command)) {
        return null;
      }
      if (command === "udevadm" && !config.udevadm) {
        return null;
      }
      return `/usr/sbin/${command}`;
    },
    sleep(seconds) {
      sleeps.push(seconds);
    },
    runner: {
      async run_bounded(command, command_options) {
        calls.push([...command]);
        expect(command_options).toEqual({
          timeout: 45,
          stdout_limit: 8192,
          stderr_limit: 4000,
          check: false,
        });

        // This adapter never launches commands and rejects unknown argv.
        switch (command.join(" ")) {
          case "findmnt -n -o SOURCE -T /":
            return {
              ...result(config.source_output ?? `${SOURCE}\n`),
              ...config.source_result,
            };
          case "findmnt -n -o FSTYPE -T /":
            return result(`${config.filesystem_type ?? "ext4"}\n`);
          case `lsblk -n -o PKNAME ${SOURCE}`:
            return result(config.parent_output ?? "sda\n");
          case `lsblk -n -o TYPE ${SOURCE}`:
            return result(`${config.source_type ?? "part"}\n`);
          case `lsblk -n -o TYPE ${DISK}`:
            return result(`${config.parent_type ?? "disk"}\n`);
          case `blockdev --getsize64 ${DISK}`:
            return result(`${state.disk}\n`);
          case `blockdev --getsize64 ${SOURCE}`:
            return result(`${state.partition}\n`);
          case `growpart ${DISK} 2`:
            return perform("growpart");
          case `resize2fs ${SOURCE}`:
          case "xfs_growfs /":
            return perform("resize");
          case `blockdev --rereadpt ${DISK}`:
            return perform("reread");
          case "udevadm settle --timeout=5":
            return perform("settle");
          default:
            throw new Error(`Unexpected command: ${command.join(" ")}`);
        }
      },
    },
  };

  return {
    disk_gib,
    target,
    threshold,
    state,
    fs,
    options,
    calls,
    lookups,
    sleeps,
  };
}

type Fixture = ReturnType<typeof fixture>;

function commands(f: Fixture, executable: string): string[][] {
  return f.calls.filter((command) => command[0] === executable);
}

function effects(f: Fixture): string[][] {
  return f.calls.filter((command) =>
    ["growpart", "resize2fs", "xfs_growfs", "udevadm"].includes(
      command[0] ?? "",
    ) ||
    (command[0] === "blockdev" && command[1] === "--rereadpt")
  );
}

async function failure(f: Fixture): Promise<Data> {
  try {
    await grow_guest_root(f.disk_gib, f.options);
  } catch (error) {
    if (!(error instanceof ControlError)) {
      throw error;
    }
    expect(error.code).toBe("guest_resize_unverified");
    if (!is_record(error.detail)) {
      throw new Error("Expected structured guest growth failure");
    }
    return error.detail;
  }
  throw new Error("Expected guest growth to fail");
}

async function main(f: Fixture, args = [String(f.disk_gib)]) {
  const stdout: string[] = [];
  const stderr: string[] = [];
  const code = await guest_main(args, {
    ...f.options,
    stdout(value) {
      stdout.push(value);
    },
    stderr(value) {
      stderr.push(value);
    },
  });
  const out = stdout.join("");
  const err = stderr.join("");

  expect(Number(out !== "") + Number(err !== "")).toBe(1);
  const raw = err || out;
  expect(raw.endsWith("\n")).toBe(true);
  expect(raw.split("\n")).toHaveLength(2);

  // JSON.parse would round capacities larger than Number.MAX_SAFE_INTEGER.
  const payload: unknown = parse_json(raw);
  if (!is_record(payload)) {
    throw new Error("Expected one structured guest report");
  }

  return { code, stdout: out, stderr: err, raw, payload };
}

describe("grow_guest_root", () => {
  for (const filesystem of ["ext4", "xfs"] as const) {
    test(`grows ${filesystem} using its matching filesystem tool`, async () => {
      const f = fixture({ filesystem_type: filesystem });
      const report = await grow_guest_root(f.disk_gib, f.options);
      const filesystem_command = filesystem === "ext4"
        ? ["resize2fs", SOURCE]
        : ["xfs_growfs", "/"];

      expect(report).toMatchObject({
        status: "grown",
        attempts: 1,
        virtual_bytes: f.target,
        partition_bytes: f.target,
        filesystem_before: f.target / 4n,
        filesystem_bytes: f.threshold,
        filesystem_threshold_bytes: f.threshold,
        filesystem,
        source: SOURCE,
        parent: DISK,
        partition: "2",
      });
      expect(report.capacity.verified).toBe(true);
      expect(report.capacity.attempts).toEqual([{ attempt: 1 }]);
      expect(effects(f)).toEqual([GROW, filesystem_command]);
      expect(f.sleeps).toEqual([]);
      expect(f.lookups).toContain(filesystem_command[0]);
      expect(commands(
        f,
        filesystem === "ext4" ? "xfs_growfs" : "resize2fs",
      )).toEqual([]);
    });
  }

  test("is idempotent at the floored 95 percent threshold", async () => {
    const target = 21n * GIB;
    const threshold = target * 95n / 100n;
    expect(target * 95n % 100n).not.toBe(0n);

    const f = fixture({
      disk_gib: 21,
      partition_bytes: target,
      filesystem_bytes: threshold,
    });
    const report = await grow_guest_root(f.disk_gib, f.options);

    expect(report.status).toBe("already_grown");
    expect(report.attempts).toBe(0);
    expect(report.filesystem_before).toBe(threshold);
    expect(report.filesystem_bytes).toBe(threshold);
    expect(report.filesystem_threshold_bytes).toBe(threshold);
    expect(report.capacity.attempts).toEqual([]);
    expect(effects(f)).toEqual([]);
  });

  test("accepts partition and filesystem capacity exactly at 95 percent", async () => {
    const target = 21n * GIB;
    const threshold = target * 95n / 100n;
    const f = fixture({
      disk_gib: 21n,
      growpart: [{ partition: threshold }],
      resize: [{ filesystem: threshold }],
    });
    const report = await grow_guest_root(f.disk_gib, f.options);

    expect(report.status).toBe("grown");
    expect(report.partition_bytes).toBe(threshold);
    expect(report.filesystem_bytes).toBe(threshold);
    expect(effects(f)).toEqual([GROW, ["resize2fs", SOURCE]]);
  });

  test("rejects a filesystem one byte below 95 percent after resize", async () => {
    const target = 21n * GIB;
    const threshold = target * 95n / 100n;
    const f = fixture({
      disk_gib: 21n,
      partition_bytes: target,
      filesystem_bytes: threshold - 1n,
      resize: [{ filesystem: threshold - 1n }],
    });
    const error = await failure(f);

    expect(error).toMatchObject({
      phase: "filesystem-verify",
      command: ["resize2fs", SOURCE],
      returncode: 0,
      cause: "filesystem_capacity_below_target",
      transient: false,
    });
    expect(error.capacity.filesystem_bytes).toBe(threshold - 1n);
    expect(error.capacity.filesystem_threshold_bytes).toBe(threshold);
    expect(error.capacity.attempts).toHaveLength(1);
    expect(effects(f)).toEqual([GROW, ["resize2fs", SOURCE]]);
  });

  test("preserves exact bigint capacities above the safe integer range", async () => {
    const disk_gib = 9007199254740993n;
    const target = disk_gib * GIB;
    const threshold = target * 95n / 100n;
    const f = fixture({
      disk_gib,
      disk_bytes: target + 17n,
      partition_bytes: target - 3n,
      filesystem_bytes: threshold,
    });
    const report = await grow_guest_root(disk_gib, f.options);

    expect(report.status).toBe("already_grown");
    expect(report.virtual_bytes).toBe(target + 17n);
    expect(report.partition_bytes).toBe(target - 3n);
    expect(report.filesystem_bytes).toBe(threshold);
    expect(report.filesystem_threshold_bytes).toBe(threshold);
    expect(report.capacity.requested_bytes).toBe(target);
    expect(report.capacity.disk_bytes).toBe(target + 17n);
    expect(report.capacity.partition_bytes).toBe(target - 3n);
    expect(effects(f)).toEqual([]);
  });

  test("multiplies bigint filesystem blocks without Number conversion", async () => {
    const blocks = 9007199254740993n;
    const bsize = 4096n;
    const f = fixture();
    f.fs.statfsSync = (filename, options) => {
      expect(filename).toBe("/");
      expect(options).toEqual({ bigint: true });
      return { blocks, bsize };
    };

    const report = await grow_guest_root(f.disk_gib, f.options);

    expect(report.filesystem_bytes).toBe(blocks * bsize);
    expect(report.filesystem_before).toBe(blocks * bsize);
    expect(effects(f)).toEqual([]);
  });

  for (const filesystem of ["ext4", "xfs"] as const) {
    const filesystem_tool = filesystem === "ext4"
      ? "resize2fs"
      : "xfs_growfs";

    for (const missing of [
      "findmnt",
      "lsblk",
      "blockdev",
      "growpart",
      filesystem_tool,
    ]) {
      test(`missing ${missing} prevents ${filesystem} growth writes`, async () => {
        const f = fixture({
          filesystem_type: filesystem,
          missing_tools: [missing],
          partition_bytes: 20n * GIB,
          filesystem_bytes: 20n * GIB,
        });
        const error = await failure(f);

        expect(error).toMatchObject({
          phase: "capability-check",
          command: ["command", "-v", missing],
          cause: "missing_guest_tool",
          transient: false,
        });
        expect(error.capacity.tools[missing]).toBeNull();
        expect(error.capacity.attempts).toEqual([]);
        expect(commands(f, "blockdev")).toEqual([]);
        expect(effects(f)).toEqual([]);
      });
    }
  }

  const unsupported: Array<{
    name: string;
    config: FixtureOptions;
    phase: string;
    cause: string;
  }> = [
    {
      name: "unsupported filesystem",
      config: { filesystem_type: "btrfs" },
      phase: "capability-check",
      cause: "automatic growth supports direct ext4/xfs root partitions only",
    },
    {
      name: "non-device root",
      config: { source_output: "/overlay\n" },
      phase: "capability-check",
      cause: "automatic growth supports direct ext4/xfs root partitions only",
    },
    {
      name: "missing partition marker",
      config: { marker_exists: false },
      phase: "topology-check",
      cause: "root_is_not_partition",
    },
    {
      name: "invalid partition number",
      config: { marker_text: "2 extra\n" },
      phase: "topology-check",
      cause: "invalid_partition_number",
    },
    {
      name: "non-partition root type",
      config: { source_type: "lvm" },
      phase: "topology-check",
      cause: "root_is_not_partition",
    },
    {
      name: "non-disk parent",
      config: { parent_type: "crypt" },
      phase: "topology-check",
      cause: "parent_is_not_disk",
    },
    {
      name: "multiple root sources",
      config: { source_output: "/dev/sda2\n/dev/sda3\n" },
      phase: "topology-source",
      cause: "ambiguous_output",
    },
    {
      name: "multiple parent disks",
      config: { parent_output: "sda\nsdb\n" },
      phase: "topology-check",
      cause: "ambiguous_root_disk",
    },
    {
      name: "partition larger than parent",
      config: { partition_bytes: 20n * GIB + 1n },
      phase: "topology-check",
      cause: "partition_exceeds_parent_disk",
    },
  ];

  for (const scenario of unsupported) {
    test(`rejects ${scenario.name} before growth writes`, async () => {
      const f = fixture(scenario.config);
      const error = await failure(f);

      expect(error.phase).toBe(scenario.phase);
      expect(error.cause).toBe(scenario.cause);
      expect(error.capacity.verified).toBe(false);
      expect(error.capacity.attempts).toEqual([]);
      expect(effects(f)).toEqual([]);
    });
  }

  test("rejects a virtual disk one byte below target before accepting an already grown filesystem", async () => {
    const f = fixture({
      disk_bytes: 20n * GIB - 1n,
      filesystem_bytes: 20n * GIB,
    });
    const error = await failure(f);

    expect(error).toMatchObject({
      phase: "capacity-diagnostic",
      command: ["blockdev", "--getsize64", DISK],
      returncode: 0,
      cause: "guest_virtual_disk_size_is_stale",
      transient: false,
    });
    expect(error.capacity.disk_bytes).toBe(f.target - 1n);
    expect(error.capacity.verified).toBe(false);
    expect(effects(f)).toEqual([]);
  });

  for (const busy_reread of [false, true]) {
    test(`refreshes a stale partition and retries once with ${busy_reread ? "busy" : "successful"} reread`, async () => {
      const f = fixture({
        growpart: [{}, {}],
        reread: [{
          returncode: busy_reread ? 1 : 0,
          stderr: busy_reread ? "Device or resource busy\n" : "",
          partition: 20n * GIB,
        }],
        udevadm: true,
      });
      const report = await grow_guest_root(f.disk_gib, f.options);

      expect(report.status).toBe("grown_after_retry");
      expect(report.attempts).toBe(2);
      expect(report.capacity.attempts[0].error).toMatchObject({
        phase: "partition-verify",
        cause: "stale_partition_capacity_below_target",
        transient: true,
      });
      expect(report.capacity.attempts[0].error.capacity.partition_bytes)
        .toBe(f.target / 2n);
      expect(effects(f)).toEqual([
        GROW,
        REREAD,
        SETTLE,
        GROW,
        ["resize2fs", SOURCE],
      ]);
      expect(f.sleeps).toEqual([1]);
    });
  }

  test("stops after two transient growpart failures and one refresh", async () => {
    const busy = {
      returncode: 1,
      stderr: "growpart: Device or resource busy\n",
    };
    const f = fixture({
      growpart: [busy, busy],
      reread: [{
        returncode: 1,
        stderr: "blockdev: Device or resource busy\n",
      }],
    });
    const error = await failure(f);

    expect(error).toMatchObject({
      phase: "partition-grow",
      command: GROW,
      transient: true,
    });
    expect(error.retry_of).toEqual(
      error.capacity.attempts[0].error,
    );
    expect(error.capacity.attempts.map((attempt: Data) => attempt.attempt))
      .toEqual([1, 2]);
    expect(error.capacity.attempts[1].error.retry_of).toEqual(
      error.capacity.attempts[0].error,
    );
    expect(effects(f)).toEqual([GROW, REREAD, GROW]);
    expect(f.sleeps).toEqual([1]);
  });

  test("never resizes a partition remaining one byte below 95 percent", async () => {
    const target = 21n * GIB;
    const threshold = target * 95n / 100n;
    const f = fixture({
      disk_gib: 21n,
      partition_bytes: threshold - 1n,
      growpart: [{}, {}],
      reread: [{}],
    });
    const error = await failure(f);

    expect(error.phase).toBe("partition-verify");
    expect(error.cause).toBe("stale_partition_capacity_below_target");
    expect(error.transient).toBe(true);
    expect(error.capacity.partition_bytes).toBe(threshold - 1n);
    expect(error.capacity.attempts).toHaveLength(2);
    expect(error.retry_of.phase).toBe("partition-verify");
    expect(effects(f)).toEqual([GROW, REREAD, GROW]);
    expect(f.sleeps).toEqual([1]);
  });

  test("links a different second failure to the original stale partition", async () => {
    const f = fixture({
      growpart: [{}, {}],
      resize: [{
        returncode: 1,
        stderr: "resize2fs: Device or resource busy\n",
      }],
    });
    const error = await failure(f);

    expect(error).toMatchObject({
      phase: "filesystem-grow",
      command: ["resize2fs", SOURCE],
      transient: true,
    });
    expect(error.retry_of).toEqual(
      error.capacity.attempts[0].error,
    );
    expect(error.retry_of).toMatchObject({
      phase: "partition-verify",
      cause: "stale_partition_capacity_below_target",
    });
    expect(error.capacity.attempts).toHaveLength(2);
    expect(error.capacity.attempts[1].error.retry_of).toEqual(
      error.retry_of,
    );
    expect(effects(f)).toEqual([
      GROW,
      REREAD,
      GROW,
      ["resize2fs", SOURCE],
    ]);
  });

  test("accepts completion observed after a transient filesystem failure", async () => {
    const threshold = 20n * GIB * 95n / 100n;
    const f = fixture({
      partition_bytes: 20n * GIB,
      growpart: [{}, {}],
      resize: [{
        returncode: 1,
        stderr: "resize2fs: Device or resource busy\n",
        filesystem: threshold,
      }],
    });
    const report = await grow_guest_root(f.disk_gib, f.options);

    expect(report.status).toBe("already_grown");
    expect(report.attempts).toBe(2);
    expect(report.filesystem_bytes).toBe(threshold);
    expect(commands(f, "resize2fs")).toHaveLength(1);
    expect(commands(f, "growpart")).toHaveLength(2);
    expect(f.sleeps).toEqual([1]);
  });

  test("accepts growpart NOCHANGE when the partition is already sufficient", async () => {
    const f = fixture({
      partition_bytes: 20n * GIB,
      growpart: [{ returncode: 1, stdout: "NOCHANGE: partition 2\n" }],
    });
    const report = await grow_guest_root(f.disk_gib, f.options);

    expect(report.status).toBe("grown");
    expect(effects(f)).toEqual([GROW, ["resize2fs", SOURCE]]);
  });

  test("fails closed on a permanent growpart error", async () => {
    const f = fixture({
      growpart: [{
        returncode: 2,
        stderr: "growpart: invalid geometry\n",
      }],
    });
    const error = await failure(f);

    expect(error.phase).toBe("partition-grow");
    expect(error.transient).toBe(false);
    expect(error.returncode).toBe(2);
    expect(error.capacity.attempts).toHaveLength(1);
    expect(effects(f)).toEqual([GROW]);
    expect(f.sleeps).toEqual([]);
  });

  test("preserves refresh failure provenance and stops before another attempt", async () => {
    const f = fixture({
      growpart: [{}],
      udevadm: true,
      settle: [{
        returncode: 1,
        stderr: "udevadm: settle failed\n",
      }],
    });
    const error = await failure(f);

    expect(error.phase).toBe("partition-settle");
    expect(error.command).toEqual(SETTLE);
    expect(error.retry_of.phase).toBe("partition-verify");
    expect(error.capacity.attempts).toHaveLength(1);
    expect(error.capacity.attempts[0].refresh_error.retry_of).toEqual(
      error.capacity.attempts[0].error,
    );
    expect(effects(f)).toEqual([GROW, REREAD, SETTLE]);
    expect(f.sleeps).toEqual([]);
  });
});

describe("guest_main reports", () => {
  test("emits successful exact bigint CLI capacities on stdout", async () => {
    const disk_gib = 9007199254740993n;
    const target = disk_gib * GIB;
    const threshold = target * 95n / 100n;
    const f = fixture({
      disk_gib,
      disk_bytes: target + 17n,
      partition_bytes: target - 3n,
      filesystem_bytes: threshold,
    });
    const report = await main(f);

    expect(report.code).toBe(0);
    expect(report.stderr).toBe("");
    expect(report.payload.status).toBe("already_grown");
    expect(report.payload.virtual_bytes).toBe(target + 17n);
    expect(report.payload.partition_bytes).toBe(target - 3n);
    expect(report.payload.filesystem_bytes).toBe(threshold);
    expect(report.payload.filesystem_threshold_bytes).toBe(threshold);
    expect(report.payload.capacity.requested_bytes).toBe(target);
    expect(report.stdout).toContain(`"requested_bytes": ${target}`);
    expect(effects(f)).toEqual([]);
  });

  for (const code of [
    "command_timeout",
    "command_failed",
    "command_cleanup_failed",
  ]) {
    for (const serialized of [false, true]) {
      test(`preserves ${code} cleanup fields from ${serialized ? "serialized" : "object"} details`, async () => {
        const cleanup = {
          process_group_killed: false,
          process_group_cleanup_error: "PermissionError: denied",
          reaped: true,
          pipes_drained: false,
        };
        const detail = {
          returncode: -9,
          stdout: "partial stdout",
          stderr: "Device or resource busy",
          stdout_truncated: false,
          stderr_truncated: false,
          ...cleanup,
        };
        const f = fixture({
          growpart: [{
            error: new ControlError(
              code,
              "Injected native failure",
              serialized ? JSON.stringify(detail) : detail,
            ),
          }],
        });
        const report = await main(f);
        const error = report.payload.error;

        expect(report.code).toBe(1);
        expect(report.payload.ok).toBe(false);
        expect(error).toMatchObject({
          phase: "partition-grow",
          command: GROW,
          cause: code === "command_timeout"
            ? "command_timeout"
            : "command_cleanup_failed",
          returncode: -9,
          stdout: "partial stdout",
          stderr: "Device or resource busy",
          stdout_truncated: false,
          stderr_truncated: false,
          transient: false,
          ...cleanup,
        });
        expect(error.capacity.attempts[0].error).toMatchObject(cleanup);
        expect(effects(f)).toEqual([GROW]);
        expect(f.sleeps).toEqual([]);
      });
    }
  }

  test("emits a short failure on stderr and preserves result cleanup fields", async () => {
    const cleanup = {
      process_group_killed: true,
      process_group_cleanup_error: null,
      reaped: false,
      pipes_drained: true,
    };
    const f = fixture({
      source_result: {
        returncode: 2,
        stdout: Buffer.from("partial stdout"),
        stderr: Buffer.from("partial stderr"),
        ...cleanup,
      },
    });
    const report = await main(f);

    expect(report.code).toBe(1);
    expect(report.stdout).toBe("");
    expect(Buffer.byteLength(report.stderr)).toBeLessThanOrEqual(4001);
    expect(report.payload.error).toMatchObject({
      phase: "topology-source",
      returncode: 2,
      stdout: "partial stdout",
      stderr: "partial stderr",
      stdout_truncated: false,
      stderr_truncated: false,
      ...cleanup,
    });
    expect(effects(f)).toEqual([]);
  });

  test("preserves output below the capture limit by moving the report to stdout", async () => {
    const source_output = `${SOURCE}\n${"x".repeat(5000)}\n`;
    const f = fixture({ source_output });
    const report = await main(f);

    expect(report.code).toBe(1);
    expect(report.stderr).toBe("");
    expect(Buffer.byteLength(report.stdout)).toBeGreaterThan(4000);
    expect(Buffer.byteLength(report.stdout)).toBeLessThanOrEqual(8193);
    expect(report.payload.error.phase).toBe("topology-source");
    expect(report.payload.error.cause).toBe("ambiguous_output");
    expect(report.payload.error.stdout).toBe(source_output);
    expect(report.payload.error.stdout_truncated).toBe(false);
    expect(effects(f)).toEqual([]);
  });

  test("compacts oversized diagnostics using the stdout head and stderr tail", async () => {
    const captured_stdout = `HEAD${"a".repeat(8188)}`;
    const captured_stderr = `${"b".repeat(3996)}TAIL`;
    const cleanup = {
      process_group_killed: false,
      process_group_cleanup_error: "cleanup denied",
      reaped: true,
      pipes_drained: false,
    };
    const f = fixture({
      source_result: {
        returncode: 2,
        stdout: Buffer.from(captured_stdout),
        stderr: Buffer.from(captured_stderr),
        stdout_truncated: true,
        stderr_truncated: true,
        ...cleanup,
      },
    });
    const report = await main(f);
    const error = report.payload.error;

    expect(report.code).toBe(1);
    expect(report.stdout).toBe("");
    expect(Buffer.byteLength(report.stderr)).toBeLessThanOrEqual(4001);
    expect(error.stdout).toBe(captured_stdout.slice(0, 1024));
    expect(error.stderr).toBe(captured_stderr.slice(-512));
    expect(error.stdout_truncated).toBe(true);
    expect(error.stderr_truncated).toBe(true);
    expect(error).toMatchObject(cleanup);
    expect(effects(f)).toEqual([]);
  });

  test("bounds nested retry diagnostics while retaining both failures", async () => {
    const first_stdout = `FIRST-HEAD${"a".repeat(8182)}`;
    const second_stdout = `SECOND-HEAD${"b".repeat(8181)}`;
    const first_stderr =
      `Device or resource busy\n${"a".repeat(3900)}FIRST-TAIL`;
    const second_stderr =
      `Device or resource busy\n${"b".repeat(3900)}SECOND-TAIL`;
    const f = fixture({
      growpart: [
        {
          returncode: 1,
          stdout: first_stdout,
          stderr: first_stderr,
          stdout_truncated: true,
          stderr_truncated: true,
        },
        {
          returncode: 1,
          stdout: second_stdout,
          stderr: second_stderr,
          stdout_truncated: true,
          stderr_truncated: true,
        },
      ],
    });
    const report = await main(f);
    const error = report.payload.error;
    const attempts = error.capacity.attempts;

    expect(report.code).toBe(1);
    expect(report.payload.ok).toBe(false);
    expect(Buffer.byteLength(report.stdout)).toBeLessThanOrEqual(8193);
    expect(Buffer.byteLength(report.stderr)).toBeLessThanOrEqual(4001);
    expect(error.phase).toBe("partition-grow");
    expect(error.transient).toBe(true);
    expect(attempts).toHaveLength(2);
    expect(attempts.map((attempt: Data) => attempt.attempt))
      .toEqual([1, 2]);

    for (const evidence of [
      error,
      error.retry_of,
      attempts[0].error,
      attempts[1].error,
      attempts[1].error.retry_of,
    ]) {
      expect(evidence.phase).toBe("partition-grow");
      expect(evidence.command).toEqual(GROW);
      expect(evidence.stdout_truncated).toBe(true);
      expect(evidence.stderr_truncated).toBe(true);
      expect(Array.from(evidence.stdout).length).toBeLessThanOrEqual(1024);
      expect(Array.from(evidence.stderr).length).toBeLessThanOrEqual(512);
    }

    expect(error.stdout.startsWith("SECOND-HEAD")).toBe(true);
    expect(error.stderr.endsWith("SECOND-TAIL")).toBe(true);
    expect(error.retry_of.stdout.startsWith("FIRST-HEAD")).toBe(true);
    expect(error.retry_of.stderr.endsWith("FIRST-TAIL")).toBe(true);
    expect(attempts[0].error.stderr.endsWith("FIRST-TAIL")).toBe(true);
    expect(attempts[1].error.stderr.endsWith("SECOND-TAIL")).toBe(true);
    expect(effects(f)).toEqual([GROW, REREAD, GROW]);
    expect(f.sleeps).toEqual([1]);
  });
});
