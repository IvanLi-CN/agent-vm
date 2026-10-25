import * as fs from "node:fs";
import * as path from "node:path";
import { Buffer } from "node:buffer";
import {
  ControlError,
  canonical_json,
  parse_json,
  is_record,
  type Data,
} from "./control-types.ts";
import { CommandRunner } from "./control-native.ts";

const GIB = 1024n ** 3n;
const MINIMUM_FILESYSTEM_RATIO = 95n;
const COMMAND_TIMEOUT = 45;
const STDOUT_LIMIT = 8192;
const STDERR_LIMIT = 4000;
const REPORT_STDOUT_LIMIT = 1024;
const REPORT_STDERR_LIMIT = 512;

// Diagnostic identity for the JavaScript program supplied on guest stdin.
const GUEST_COMMAND = ["bun", "run", "-", "guest-root-growth"];

type Fields = Record<string, unknown>;
type NativeRunner = Pick<CommandRunner, "run_bounded">;
type NativeResult = Awaited<ReturnType<NativeRunner["run_bounded"]>>;

export interface GuestFilesystem {
  realpathSync(filename: string): string;
  readFileSync(filename: string): Buffer;
  statSync(filename: string): {
    isFile(): boolean;
  };
  accessSync(filename: string, mode: number): void;
  statfsSync(
    filename: string,
    options: { bigint: true },
  ): {
    blocks: bigint;
    bsize: bigint;
  };
}

export interface GuestRootGrowthOptions {
  runner?: NativeRunner;
  fs?: Partial<GuestFilesystem>;
  which?: (command: string) =>
    | string
    | null
    | Promise<string | null>;
  sleep?: (seconds: number) => void | Promise<void>;
  env?: {
    PATH?: string;
  };
}

export interface GuestMainOptions extends GuestRootGrowthOptions {
  stdout?: (text: string) => void | Promise<void>;
  stderr?: (text: string) => void | Promise<void>;
}

interface Layout {
  source: string;
  fstype: "ext4" | "xfs";
  disk: string;
  number: string;
  filesystem_tool: "resize2fs" | "xfs_growfs";
  filesystem_bytes: bigint;
  disk_bytes: bigint;
  partition_bytes: bigint;
}

const CLEANUP_KEYS = [
  "process_group_killed",
  "process_group_cleanup_error",
  "reaped",
  "pipes_drained",
] as const;

function text(value: unknown): string {
  if (Buffer.isBuffer(value)) {
    return value.toString("utf8");
  }
  if (value instanceof Uint8Array) {
    return Buffer.from(value).toString("utf8");
  }
  return value == null ? "" : String(value);
}

// Python slices strings by Unicode code point, rather than UTF-16 code unit.
function clip(value: unknown, limit = STDERR_LIMIT, tail = true): string {
  const characters = Array.from(text(value));
  if (limit === 0) {
    return "";
  }
  return (
    tail ? characters.slice(-limit) : characters.slice(0, limit)
  ).join("");
}

function compact_failure(
  value: Fields,
  stdout_limit: number,
  stderr_limit: number,
): Fields {
  const compacted: Fields = { ...value };
  const streams = [
    ["stdout", stdout_limit, false],
    ["stderr", stderr_limit, true],
  ] as const;

  for (const [key, limit, tail] of streams) {
    if (!(key in compacted)) {
      continue;
    }
    const raw = compacted[key];
    compacted[key] = clip(raw, limit, tail);
    compacted[`${key}_truncated`] =
      Boolean(value[`${key}_truncated`]) ||
      Buffer.byteLength(text(raw), "utf8") > limit;
  }

  if (is_record(compacted.retry_of)) {
    compacted.retry_of = compact_failure(
      compacted.retry_of,
      stdout_limit,
      stderr_limit,
    );
  }

  if (is_record(compacted.capacity)) {
    const capacity: Fields = { ...compacted.capacity };

    for (const key of ["filesystem", "source", "parent", "partition"]) {
      if (typeof capacity[key] === "string") {
        capacity[key] = clip(capacity[key], 256, false);
      }
    }

    if (is_record(capacity.tools)) {
      capacity.tools = Object.fromEntries(
        Object.entries(capacity.tools).map(([key, value]) => [
          key,
          typeof value === "string" ? clip(value, 256, false) : value,
        ]),
      );
    }

    if (Array.isArray(capacity.attempts)) {
      capacity.attempts = capacity.attempts.map((record: unknown) => {
        if (!is_record(record)) {
          return record;
        }
        const compacted_record: Fields = { ...record };
        for (const key of ["error", "refresh_error"]) {
          const error = compacted_record[key];
          if (is_record(error)) {
            compacted_record[key] = compact_failure(
              error,
              stdout_limit,
              stderr_limit,
            );
          }
        }
        return compacted_record;
      });
    }

    compacted.capacity = capacity;
  }

  return compacted;
}

function transient_failure(
  command: readonly string[],
  stdout: unknown,
  stderr: unknown,
): boolean {
  const executable = command[0];
  if (
    executable !== "growpart" &&
    executable !== "resize2fs" &&
    executable !== "xfs_growfs" &&
    executable !== "blockdev"
  ) {
    return false;
  }
  if (executable === "blockdev" && !command.includes("--rereadpt")) {
    return false;
  }

  const output = `${text(stdout)}\n${text(stderr)}`.toLowerCase();
  return [
    "device or resource busy",
    "resource temporarily unavailable",
    "old partition table",
    "re-read partition table",
    "reread partition table",
  ].some((marker) => output.includes(marker));
}

function cleanup_evidence(value: Fields): Fields {
  const result: Fields = {};
  for (const key of CLEANUP_KEYS) {
    if (
      Object.prototype.hasOwnProperty.call(value, key) &&
      value[key] !== undefined
    ) {
      result[key] = value[key];
    }
  }
  return result;
}

function result_evidence(result: NativeResult): Fields {
  return {
    returncode: result.returncode,
    stdout: text(result.stdout),
    stderr: text(result.stderr),
    stdout_truncated: result.stdout_truncated,
    stderr_truncated: result.stderr_truncated,
    ...cleanup_evidence({ ...result }),
  };
}

function native_detail(value: unknown): Fields {
  if (is_record(value)) {
    return value;
  }
  // Also accept serialized diagnostic details from injected adapters.
  if (typeof value === "string") {
    try {
      const parsed: unknown = parse_json(value);
      if (is_record(parsed)) {
        return parsed;
      }
    } catch {
      // An unstructured detail is not command-output evidence.
    }
  }
  return {};
}

function exception_name(error: unknown): string {
  if (error instanceof Error) {
    const code = (error as Error & { code?: unknown }).code;
    switch (code) {
      case "ENOENT":
        return "FileNotFoundError";
      case "EACCES":
      case "EPERM":
        return "PermissionError";
      case "ENOTDIR":
        return "NotADirectoryError";
      case "EISDIR":
        return "IsADirectoryError";
      default:
        return typeof code === "string"
          ? "OSError"
          : error.name || "Error";
    }
  }
  return "Error";
}

function named_error(name: string, message: string): Error {
  const error = new Error(message);
  error.name = name;
  return error;
}

class GuestFailure extends Error {
  readonly fields: Fields;

  constructor(
    phase: string,
    command: readonly string[] = [],
    fields: Fields = {},
  ) {
    super(phase);
    this.name = "GuestFailure";
    this.fields = {
      phase,
      command: [...command],
      returncode: null,
      stdout: "",
      stderr: "",
      transient: false,
      stdout_truncated: false,
      stderr_truncated: false,
      ...fields,
    };
  }

  get transient(): boolean {
    return this.fields.transient === true;
  }

  set_retry_of(value: Fields): void {
    this.fields.retry_of = value;
  }

  as_dict(capacity: Fields): Fields {
    return {
      ...this.fields,
      stdout: clip(this.fields.stdout, STDOUT_LIMIT, false),
      stderr: clip(this.fields.stderr, STDERR_LIMIT, true),
      capacity: { ...capacity },
    };
  }

  as_report(
    capacity: Fields,
    stdout_limit = REPORT_STDOUT_LIMIT,
    stderr_limit = REPORT_STDERR_LIMIT,
  ): Fields {
    return compact_failure(
      this.as_dict(capacity),
      stdout_limit,
      stderr_limit,
    );
  }
}

function unexpected_failure(error: unknown): GuestFailure {
  return new GuestFailure("guest-root-growth", GUEST_COMMAND, {
    returncode: 1,
    cause: exception_name(error),
  });
}

function default_filesystem(): GuestFilesystem {
  return {
    realpathSync: (filename) => fs.realpathSync(filename),
    readFileSync: (filename) => fs.readFileSync(filename),
    statSync: (filename) => fs.statSync(filename),
    accessSync: (filename, mode) => fs.accessSync(filename, mode),
    statfsSync: (filename, options) => fs.statfsSync(filename, options),
  };
}

// Match os.path.realpath's non-strict treatment of missing suffixes while
// resolving existing device symlinks through the injected filesystem.
function realpath_nonstrict(
  guest_fs: GuestFilesystem,
  filename: string,
): string {
  let candidate = path.posix.resolve(filename);
  const suffix: string[] = [];

  for (;;) {
    try {
      return path.posix.join(
        guest_fs.realpathSync(candidate),
        ...suffix,
      );
    } catch {
      const parent = path.posix.dirname(candidate);
      if (parent === candidate) {
        return path.posix.join(candidate, ...suffix);
      }
      suffix.unshift(path.posix.basename(candidate));
      candidate = parent;
    }
  }
}

function is_file(
  guest_fs: GuestFilesystem,
  filename: string,
): boolean {
  try {
    return guest_fs.statSync(filename).isFile();
  } catch {
    return false;
  }
}

function find_executable(
  guest_fs: GuestFilesystem,
  command: string,
  search_path: string,
): string | null {
  for (const directory of search_path.split(path.posix.delimiter)) {
    const filename = path.posix.join(directory, command);
    try {
      if (!guest_fs.statSync(filename).isFile()) {
        continue;
      }
      guest_fs.accessSync(filename, fs.constants.X_OK);
      return filename;
    } catch {
      // shutil.which skips inaccessible or nonexistent PATH candidates.
    }
  }
  return null;
}

function nonempty_lines(value: unknown): string[] {
  return text(value)
    .split(/\r\n|[\n\r\v\f\x1c-\x1e\x85\u2028\u2029]/u)
    .map((line) => line.trim())
    .filter((line) => line.length > 0);
}

export async function grow_guest_root(
  disk_gib: number | bigint,
  options: GuestRootGrowthOptions = {},
): Promise<Data> {
  const capacity: Fields = { verified: false };
  const attempts: Fields[] = [];

  try {
    if (
      typeof disk_gib !== "bigint" &&
      !Number.isSafeInteger(disk_gib)
    ) {
      throw named_error(
        "ValueError",
        "disk_gib must be a bigint or a safe integer",
      );
    }

    const target_bytes = BigInt(disk_gib) * GIB;
    capacity.requested_bytes = target_bytes;

    const runner = options.runner ?? new CommandRunner();
    const guest_fs: GuestFilesystem = {
      ...default_filesystem(),
      ...options.fs,
    };
    const search_path =
      (options.env ?? process.env).PATH ?? "/bin:/usr/bin";
    const which =
      options.which ??
      ((command: string) =>
        find_executable(guest_fs, command, search_path));
    const sleep =
      options.sleep ??
      ((seconds: number) =>
        new Promise<void>((resolve) => {
          setTimeout(resolve, seconds * 1000);
        }));

    function capacity_integer(
      key: string,
      fallback = 0n,
    ): bigint {
      const value = capacity[key];
      return typeof value === "bigint" ? value : fallback;
    }

    async function run(
      command: string[],
      phase: string,
      check = true,
    ): Promise<NativeResult> {
      let result: NativeResult;

      try {
        // Check locally so command failures retain both bounded streams.
        // Native timeout and containment failures still throw.
        result = await runner.run_bounded(command, {
          timeout: COMMAND_TIMEOUT,
          stdout_limit: STDOUT_LIMIT,
          stderr_limit: STDERR_LIMIT,
          check: false,
        });
      } catch (error) {
        if (!(error instanceof ControlError)) {
          throw error;
        }

        const detail = native_detail(error.detail);
        const cleanup = cleanup_evidence(detail);
        const has_cleanup_diagnostics =
          "process_group_killed" in cleanup ||
          "process_group_cleanup_error" in cleanup;

        let cause: string | undefined;
        if (error.code === "command_timeout") {
          cause = "command_timeout";
        } else if (error.code === "command_missing") {
          cause = "command_missing";
        } else if (
          error.code === "command_cleanup_failed" ||
          (error.code === "command_failed" && has_cleanup_diagnostics)
        ) {
          cause = "command_cleanup_failed";
        } else if (typeof detail.cause === "string") {
          cause = detail.cause;
        } else if (error.code !== "command_failed") {
          cause = error.code;
        }

        const stdout = text(detail.stdout);
        const stderr = text(detail.stderr);
        const fields: Fields = {
          returncode:
            typeof detail.returncode === "number"
              ? detail.returncode
              : null,
          stdout,
          stderr,
          stdout_truncated: detail.stdout_truncated === true,
          stderr_truncated: detail.stderr_truncated === true,
          transient:
            cause === undefined &&
            transient_failure(command, stdout, stderr),
          ...cleanup,
        };
        if (cause !== undefined) {
          fields.cause = cause;
        }
        throw new GuestFailure(phase, command, fields);
      }

      if (check && result.returncode !== 0) {
        throw new GuestFailure(phase, command, {
          ...result_evidence(result),
          transient: transient_failure(
            command,
            result.stdout,
            result.stderr,
          ),
        });
      }

      return result;
    }

    function one_line(
      result: NativeResult,
      phase: string,
      command: string[],
    ): string {
      const lines = nonempty_lines(result.stdout);
      if (lines.length !== 1) {
        throw new GuestFailure(phase, command, {
          ...result_evidence(result),
          cause: "ambiguous_output",
        });
      }
      return lines[0]!;
    }

    async function number_from(
      command: string[],
      phase: string,
      key: string,
    ): Promise<bigint> {
      const result = await run(command, phase);
      const raw = text(result.stdout).trim();
      if (!/^[0-9]+$/.test(raw)) {
        throw new GuestFailure(phase, command, {
          ...result_evidence(result),
          cause: "invalid_integer",
        });
      }
      const value = BigInt(raw);
      capacity[key] = value;
      return value;
    }

    function filesystem_bytes(): bigint {
      let result: bigint;
      try {
        const value = guest_fs.statfsSync("/", { bigint: true });
        result = value.blocks * value.bsize;
      } catch (error) {
        throw new GuestFailure(
          "filesystem-diagnostic",
          ["statvfs", "/"],
          { cause: exception_name(error) },
        );
      }

      if (result <= 0n) {
        throw new GuestFailure(
          "filesystem-diagnostic",
          ["statvfs", "/"],
          { cause: "non_positive_capacity" },
        );
      }

      capacity.filesystem_bytes = result;
      return result;
    }

    async function inspect_layout(): Promise<Layout> {
      const source_command = [
        "findmnt", "-n", "-o", "SOURCE", "-T", "/",
      ];
      let source = one_line(
        await run(source_command, "topology-source"),
        "topology-source",
        source_command,
      );
      source = realpath_nonstrict(guest_fs, source);

      const fstype_command = [
        "findmnt", "-n", "-o", "FSTYPE", "-T", "/",
      ];
      const fstype = one_line(
        await run(fstype_command, "topology-filesystem"),
        "topology-filesystem",
        fstype_command,
      );

      Object.assign(capacity, {
        source,
        filesystem: fstype,
      });

      if (
        !source.startsWith("/dev/") ||
        (fstype !== "ext4" && fstype !== "xfs")
      ) {
        throw new GuestFailure(
          "capability-check",
          source_command,
          {
            cause:
              "automatic growth supports direct ext4/xfs root partitions only",
          },
        );
      }

      const name = path.posix.basename(source);
      const partition_marker = `/sys/class/block/${name}/partition`;
      if (
        !/^[A-Za-z0-9_.-]+$/.test(name) ||
        !is_file(guest_fs, partition_marker)
      ) {
        throw new GuestFailure(
          "topology-check",
          ["test", "-f", partition_marker],
          { cause: "root_is_not_partition" },
        );
      }

      let marker: Buffer;
      try {
        marker = guest_fs.readFileSync(partition_marker);
      } catch (error) {
        throw new GuestFailure(
          "topology-check",
          ["cat", partition_marker],
          { cause: exception_name(error) },
        );
      }

      // Buffer's "ascii" decoder masks the high bit; reject it explicitly.
      if (marker.some((byte) => byte > 0x7f)) {
        throw named_error(
          "UnicodeDecodeError",
          "partition marker is not ASCII",
        );
      }
      const number = marker.toString("ascii").trim();
      if (!/^[0-9]+$/.test(number)) {
        throw new GuestFailure(
          "topology-check",
          ["cat", partition_marker],
          { cause: "invalid_partition_number" },
        );
      }

      const parent_command = [
        "lsblk", "-n", "-o", "PKNAME", source,
      ];
      const parent_lines = nonempty_lines(
        (await run(parent_command, "topology-parent")).stdout,
      );
      if (
        parent_lines.length !== 1 ||
        parent_lines[0]!.includes("/")
      ) {
        throw new GuestFailure("topology-check", parent_command, {
          cause: "ambiguous_root_disk",
        });
      }

      const disk = `/dev/${parent_lines[0]!}`;
      const source_type_command = [
        "lsblk", "-n", "-o", "TYPE", source,
      ];
      if (
        one_line(
          await run(source_type_command, "topology-type"),
          "topology-type",
          source_type_command,
        ) !== "part"
      ) {
        throw new GuestFailure(
          "topology-check",
          source_type_command,
          { cause: "root_is_not_partition" },
        );
      }

      const disk_type_command = [
        "lsblk", "-n", "-o", "TYPE", disk,
      ];
      if (
        one_line(
          await run(disk_type_command, "topology-type"),
          "topology-type",
          disk_type_command,
        ) !== "disk"
      ) {
        throw new GuestFailure(
          "topology-check",
          disk_type_command,
          { cause: "parent_is_not_disk" },
        );
      }

      const filesystem_tool =
        fstype === "ext4" ? "resize2fs" : "xfs_growfs";
      const required_tools = [
        "findmnt",
        "lsblk",
        "blockdev",
        "growpart",
        filesystem_tool,
      ];
      const tools: Record<string, string | null> = {};
      for (const tool of required_tools) {
        tools[tool] = await which(tool);
      }

      Object.assign(capacity, {
        source,
        filesystem: fstype,
        parent: disk,
        partition: number,
        tools,
      });

      const missing = required_tools.filter((tool) => !tools[tool]);
      if (missing.length > 0) {
        throw new GuestFailure(
          "capability-check",
          ["command", "-v", missing[0]!],
          { cause: "missing_guest_tool" },
        );
      }

      const disk_bytes = await number_from(
        ["blockdev", "--getsize64", disk],
        "capacity-diagnostic",
        "disk_bytes",
      );
      const partition_bytes = await number_from(
        ["blockdev", "--getsize64", source],
        "capacity-diagnostic",
        "partition_bytes",
      );
      const filesystem_size = filesystem_bytes();

      capacity.filesystem_threshold_bytes =
        target_bytes * MINIMUM_FILESYSTEM_RATIO / 100n;

      if (disk_bytes < target_bytes) {
        throw new GuestFailure(
          "capacity-diagnostic",
          ["blockdev", "--getsize64", disk],
          {
            returncode: 0,
            cause: "guest_virtual_disk_size_is_stale",
          },
        );
      }
      if (partition_bytes > disk_bytes) {
        throw new GuestFailure(
          "topology-check",
          ["blockdev", "--getsize64", source],
          {
            returncode: 0,
            cause: "partition_exceeds_parent_disk",
          },
        );
      }

      capacity.verified = true;
      return {
        source,
        fstype,
        disk,
        number,
        filesystem_tool,
        filesystem_bytes: filesystem_size,
        disk_bytes,
        partition_bytes,
      };
    }

    function filesystem_is_complete(): boolean {
      return (
        capacity_integer("filesystem_bytes") >=
        capacity_integer("filesystem_threshold_bytes", target_bytes)
      );
    }

    function verify_partition(layout: Layout): void {
      const required =
        target_bytes * MINIMUM_FILESYSTEM_RATIO / 100n;
      if (capacity_integer("partition_bytes") < required) {
        throw new GuestFailure(
          "partition-verify",
          ["blockdev", "--getsize64", layout.source],
          {
            returncode: 0,
            cause: "stale_partition_capacity_below_target",
            transient: true,
          },
        );
      }
    }

    async function settle_partition(layout: Layout): Promise<void> {
      const reread_command = [
        "blockdev", "--rereadpt", layout.disk,
      ];
      const reread = await run(
        reread_command,
        "partition-reread",
        false,
      );
      if (
        reread.returncode !== 0 &&
        !transient_failure(
          reread_command,
          reread.stdout,
          reread.stderr,
        )
      ) {
        throw new GuestFailure(
          "partition-reread",
          reread_command,
          result_evidence(reread),
        );
      }

      if (await which("udevadm")) {
        const settle_command = [
          "udevadm", "settle", "--timeout=5",
        ];
        const settled = await run(
          settle_command,
          "partition-settle",
          false,
        );
        if (
          settled.returncode !== 0 &&
          !transient_failure(
            settle_command,
            settled.stdout,
            settled.stderr,
          )
        ) {
          throw new GuestFailure(
            "partition-settle",
            settle_command,
            result_evidence(settled),
          );
        }
      }

      await sleep(1);
    }

    function report(
      status: "already_grown" | "grown" | "grown_after_retry",
      before: Fields,
      layout: Layout,
    ): Data {
      capacity.status = status;
      capacity.filesystem_before =
        before.filesystem_bytes ?? null;
      capacity.attempts = [...attempts];

      return {
        status,
        virtual_bytes: capacity.disk_bytes,
        partition_bytes: capacity.partition_bytes,
        filesystem_before: before.filesystem_bytes ?? null,
        filesystem_bytes: capacity.filesystem_bytes,
        filesystem_threshold_bytes:
          capacity.filesystem_threshold_bytes,
        filesystem: layout.fstype,
        source: layout.source,
        parent: layout.disk,
        partition: layout.number,
        attempts: attempts.length,
        capacity: { ...capacity },
      } as Data;
    }

    let layout = await inspect_layout();
    const before: Fields = { ...capacity };
    capacity.filesystem_before =
      before.filesystem_bytes ?? null;

    if (filesystem_is_complete()) {
      return report("already_grown", before, layout);
    }

    const filesystem_command = [
      layout.filesystem_tool,
      layout.fstype === "ext4" ? layout.source : "/",
    ];

    for (const attempt of [1, 2] as const) {
      const attempt_record: Fields = { attempt };
      attempts.push(attempt_record);
      let failure: GuestFailure | null = null;

      try {
        const growpart_command = [
          "growpart", layout.disk, layout.number,
        ];
        const grown = await run(
          growpart_command,
          "partition-grow",
          false,
        );

        if (
          grown.returncode !== 0 &&
          !(
            grown.returncode === 1 &&
            text(grown.stdout).toUpperCase().includes("NOCHANGE")
          )
        ) {
          failure = new GuestFailure(
            "partition-grow",
            growpart_command,
            {
              ...result_evidence(grown),
              transient: transient_failure(
                growpart_command,
                grown.stdout,
                grown.stderr,
              ),
            },
          );
        }

        if (failure === null) {
          layout = await inspect_layout();
          verify_partition(layout);

          if (filesystem_is_complete()) {
            return report("already_grown", before, layout);
          }

          const grown_filesystem = await run(
            filesystem_command,
            "filesystem-grow",
            false,
          );

          if (grown_filesystem.returncode !== 0) {
            failure = new GuestFailure(
              "filesystem-grow",
              filesystem_command,
              {
                ...result_evidence(grown_filesystem),
                transient: transient_failure(
                  filesystem_command,
                  grown_filesystem.stdout,
                  grown_filesystem.stderr,
                ),
              },
            );
          } else {
            layout = await inspect_layout();
            if (!filesystem_is_complete()) {
              failure = new GuestFailure(
                "filesystem-verify",
                filesystem_command,
                {
                  returncode: 0,
                  cause: "filesystem_capacity_below_target",
                },
              );
            }
          }
        }
      } catch (error) {
        if (!(error instanceof GuestFailure)) {
          throw error;
        }
        failure = error;
      }

      if (failure === null) {
        return report(
          attempt > 1 ? "grown_after_retry" : "grown",
          before,
          layout,
        );
      }

      if (attempt > 1) {
        const previous_error = attempts[attempts.length - 2]?.error;
        if (is_record(previous_error)) {
          failure.set_retry_of(previous_error);
        }
      }

      // Store a compact snapshot before adding the final attempts array
      // to capacity, avoiding recursive references.
      attempt_record.error = failure.as_report(capacity);

      if (!failure.transient || attempt === 2) {
        throw failure;
      }

      try {
        await settle_partition(layout);
        layout = await inspect_layout();
      } catch (error) {
        if (!(error instanceof GuestFailure)) {
          throw error;
        }

        const previous_error = attempt_record.error;
        if (is_record(previous_error)) {
          error.set_retry_of(previous_error);
        }
        attempt_record.refresh_error = error.as_report(capacity);

        if (!error.transient) {
          throw error;
        }
      }
    }

    throw new GuestFailure(
      "filesystem-verify",
      filesystem_command,
      { cause: "retry_exhausted" },
    );
  } catch (error) {
    capacity.attempts = [...attempts];
    const failure =
      error instanceof GuestFailure
        ? error
        : unexpected_failure(error);

    throw new ControlError(
      "guest_resize_unverified",
      "guest root filesystem growth was not verified",
      failure.as_dict(capacity) as Data,
    );
  }
}

// Preserve Python json.dumps(..., ensure_ascii=True) when measuring report
// sizes, including supplementary characters represented by surrogate pairs.
function encode_report(value: Fields): string {
  return canonical_json(value as Data).replace(
    /[\u007f-\uffff]/g,
    (character) =>
      `\\u${character.charCodeAt(0).toString(16).padStart(4, "0")}`,
  );
}

async function emit_failure(
  failure: Fields,
  stdout: (value: string) => void | Promise<void>,
  stderr: (value: string) => void | Promise<void>,
): Promise<void> {
  let encoded = encode_report({
    ok: false,
    error: failure,
  });

  async function emit_if_fits(value: string): Promise<boolean> {
    const size = Buffer.byteLength(value, "utf8");
    if (size <= STDERR_LIMIT) {
      await stderr(`${value}\n`);
      return true;
    }
    if (size <= STDOUT_LIMIT) {
      await stdout(`${value}\n`);
      return true;
    }
    return false;
  }

  if (await emit_if_fits(encoded)) {
    return;
  }

  for (const [stdout_limit, stderr_limit] of [
    [1024, 512],
    [512, 256],
    [256, 128],
    [128, 64],
  ] as const) {
    encoded = encode_report({
      ok: false,
      error: compact_failure(
        failure,
        stdout_limit,
        stderr_limit,
      ),
    });

    if (await emit_if_fits(encoded)) {
      return;
    }
  }

  // Match the original final fallback: keep the structured report intact.
  await stdout(`${encoded}\n`);
}

function write_stream(
  stream: NodeJS.WritableStream,
  value: string,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    stream.write(value, (error?: Error | null) => {
      if (error) {
        reject(error);
      } else {
        resolve();
      }
    });
  });
}

export async function guest_main(
  args: string[],
  options: GuestMainOptions = {},
): Promise<number> {
  const stdout =
    options.stdout ??
    ((value: string) => write_stream(process.stdout, value));
  const stderr =
    options.stderr ??
    ((value: string) => write_stream(process.stderr, value));

  let result: Data;
  try {
    const argument = args[0];
    if (argument === undefined) {
      throw named_error("IndexError", "missing disk_gib argument");
    }

    const raw = argument.trim();
    if (!/^[+-]?[0-9]+$/.test(raw)) {
      throw named_error("ValueError", "invalid disk_gib argument");
    }

    // Parse directly as bigint; never round a CLI capacity through Number.
    result = await grow_guest_root(BigInt(raw), options);
  } catch (error) {
    const failure =
      error instanceof ControlError && is_record(error.detail)
        ? error.detail
        : unexpected_failure(error).as_dict({
            verified: false,
            attempts: [],
          });

    await emit_failure(failure, stdout, stderr);
    return 1;
  }

  await stdout(`${encode_report(result as Fields)}\n`);
  return 0;
}

let bundled_source: Promise<string> | undefined;

/** Bundle the same guest action shipped in the verified release, in memory. */
export function guest_source(): Promise<string> {
  return bundled_source ??= (async () => {
    const result = await Bun.build({ entrypoints: [import.meta.path], target: "bun" });
    if (!result.success || result.outputs.length !== 1) {
      throw new ControlError("guest_resize_unverified", "guest disk action cannot be bundled", { phase: "capability-check" });
    }
    return result.outputs[0]!.text();
  })();
}

if (import.meta.main) {
  process.exitCode = await guest_main(process.argv.slice(2));
}
