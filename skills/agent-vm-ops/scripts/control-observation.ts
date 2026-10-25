import { readFileSync } from "node:fs";
import { availableParallelism } from "node:os";
import {
  ControlError,
  integer,
  is_record,
  narrow,
  type Data,
} from "./control-types.ts";

type PSIFields = {
  avg10: number;
  avg60: number;
  avg300: number;
  total: number;
};

type CPUPressure = {
  some: PSIFields;
  full: PSIFields;
};

type LxcfsMemory = {
  MemTotal: bigint;
  MemAvailable: bigint;
};

const PSI_FIELDS = ["avg10", "avg60", "avg300", "total"] as const;
const CGROUP_ROOT = "/sys/fs/cgroup";
const GIB_BYTES = 1024n ** 3n;

// Preserve Python's default decimal-integer conversion bound.
const MAX_INTEGER_DIGITS = 4300;

class PSIValueError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ValueError";
  }
}

const DECIMAL_FLOAT =
  /^[+-]?(?:(?:[0-9](?:_?[0-9])*)(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.(?:[0-9](?:_?[0-9])*))(?:[eE][+-]?(?:[0-9](?:_?[0-9])*))?$/;

function psi_float(value: unknown, message: string): number {
  if (typeof value === "number" || value instanceof Number) {
    return Number(value);
  }

  if (typeof value === "bigint") {
    const result = Number(value);
    if (!Number.isFinite(result)) throw new PSIValueError(message);
    return result;
  }

  if (Buffer.isBuffer(value)) value = value.toString("utf8");
  if (typeof value !== "string") throw new PSIValueError(message);

  const text = value.trim();
  if (/^[+-]?(?:inf(?:inity)?|nan)$/i.test(text)) {
    if (/nan$/i.test(text)) return NaN;
    return text.startsWith("-") ? -Infinity : Infinity;
  }
  if (!text || !DECIMAL_FLOAT.test(text)) {
    throw new PSIValueError(message);
  }
  return Number(text.replaceAll("_", ""));
}

function validate_psi_range(name: string, number: number): void {
  if (
    !Number.isFinite(number) ||
    number < 0 ||
    (name.startsWith("avg") && number > 100)
  ) {
    throw new PSIValueError("CPU PSI value is out of range");
  }
}

function fields(line: string): string[] {
  const text = line.trim();
  return text ? text.split(/\s+/) : [];
}

function lines(raw: string): string[] {
  return raw.split(/\r\n|[\n\r\v\f\u001c-\u001e\u0085\u2028\u2029]/);
}

function minimum(a: bigint, b: bigint): bigint {
  return a < b ? a : b;
}

export function normalize_cpu_psi(value: unknown): CPUPressure {
  if (
    !is_record(value) ||
    !Object.hasOwn(value, "some") ||
    !Object.hasOwn(value, "full")
  ) {
    throw new PSIValueError("CPU PSI data is missing");
  }

  const result = {} as CPUPressure;
  for (const scope of ["some", "full"] as const) {
    const values: unknown = value[scope];
    if (
      !is_record(values) ||
      Object.keys(values).length !== PSI_FIELDS.length ||
      !PSI_FIELDS.every(name => Object.hasOwn(values, name))
    ) {
      throw new PSIValueError("CPU PSI fields are incomplete");
    }

    const normalized = {} as PSIFields;
    for (const name of PSI_FIELDS) {
      const number = psi_float(values[name], "CPU PSI value is invalid");
      validate_psi_range(name, number);
      normalized[name] = number;
    }
    result[scope] = normalized;
  }

  if (PSI_FIELDS.some(name => result.full[name] > result.some[name])) {
    throw new PSIValueError("CPU PSI full pressure exceeds some pressure");
  }
  return result;
}

export function parse_cpu_psi(raw: string): CPUPressure {
  const result: Data = Object.create(null);

  for (const line of lines(raw)) {
    const parts = fields(line);
    if (!parts.length) continue;

    const scope = parts[0]!;
    if (scope !== "some" && scope !== "full") {
      throw new PSIValueError("unknown CPU PSI line");
    }
    if (Object.hasOwn(result, scope)) {
      throw new PSIValueError("duplicate CPU PSI line");
    }

    const values: Record<string, number> = Object.create(null);
    for (const field of parts.slice(1)) {
      const separator = field.indexOf("=");
      const name = separator < 0 ? field : field.slice(0, separator);
      if (separator < 0 || Object.hasOwn(values, name)) {
        throw new PSIValueError("invalid CPU PSI field");
      }

      const value = field.slice(separator + 1);
      if (name === "total" && !/^[0-9]+$/.test(value)) {
        throw new PSIValueError("invalid CPU PSI total");
      }

      const number = psi_float(value, "invalid CPU PSI value");
      validate_psi_range(name, number);
      values[name] = number;
    }

    if (!Object.hasOwn(values, "avg10")) {
      throw new PSIValueError("CPU PSI avg10 is missing");
    }
    result[scope] = values;
  }

  return normalize_cpu_psi(result);
}

export class CTResources {
  constructor(public readonly config: Data = {}) {}

  static _read(path: string): string {
    try {
      return readFileSync(path, "utf8");
    } catch {
      throw new ControlError(
        "resource_unverified",
        "CT resource metric is unavailable",
        { metric: path },
      );
    }
  }

  _read(path: string): string {
    return CTResources._read(path);
  }

  static _unsigned(
    value: string,
    metric: string,
    positive = false,
  ): bigint {
    const text = value.trim();
    if (!/^[0-9]+$/.test(text) || text.length > MAX_INTEGER_DIGITS) {
      throw new ControlError(
        "resource_unverified",
        "CT resource metric is invalid",
        { metric },
      );
    }

    let number: bigint;
    try {
      number = BigInt(text);
    } catch {
      throw new ControlError(
        "resource_unverified",
        "CT resource metric is invalid",
        { metric },
      );
    }

    if (positive && number === 0n) {
      throw new ControlError(
        "resource_unverified",
        "CT capacity must be positive",
        { metric },
      );
    }
    return number;
  }

  cpu(): number {
    // Node's native implementation observes Linux scheduler affinity.
    const affinity = availableParallelism();
    if (!Number.isSafeInteger(affinity) || affinity < 0) {
      throw new ControlError(
        "resource_unverified",
        "CT resource metric is invalid",
        { metric: "CPU affinity" },
      );
    }

    let capacity = integer(affinity);
    const [, paths] = this._cgroup_metric_paths("cpu.max");
    for (const path of paths) {
      const parts = fields(this._read(path));
      if (parts.length !== 2) {
        throw new ControlError(
          "resource_unverified",
          "invalid CT CPU quota",
        );
      }

      const period = CTResources._unsigned(
        parts[1]!,
        "cpu.max period",
        true,
      );
      if (parts[0] !== "max") {
        const quota = CTResources._unsigned(
          parts[0]!,
          "cpu.max quota",
          true,
        );
        capacity = minimum(capacity, quota / period);
      }
    }

    // Capacity remains bounded by the safe native affinity count.
    return Number(capacity);
  }

  _verify_cgroup2_mount(): void {
    const matches: Array<[string, string]> = [];

    for (const line of lines(this._read("/proc/self/mountinfo"))) {
      const separator = line.indexOf(" - ");
      if (separator < 0) continue;

      const left = fields(line.slice(0, separator));
      const right = fields(line.slice(separator + 3));
      if (
        left.length >= 5 &&
        right.length > 0 &&
        left[3] === "/" &&
        left[4] === CGROUP_ROOT
      ) {
        matches.push([right[0]!, left[3]]);
      }
    }

    if (
      matches.length !== 1 ||
      matches[0]![0] !== "cgroup2" ||
      matches[0]![1] !== "/"
    ) {
      throw new ControlError(
        "resource_unverified",
        "CT cgroup v2 mount cannot be verified",
        {
          metric: "/proc/self/mountinfo",
          mountpoint: CGROUP_ROOT,
        },
      );
    }
  }

  current_cgroup_path(): string {
    const entries = lines(this._read("/proc/self/cgroup")).filter(
      line => line.trim() !== "",
    );
    const detail = { metric: "/proc/self/cgroup" };

    if (entries.length !== 1) {
      throw new ControlError(
        "resource_unverified",
        "CT cgroup path cannot be verified",
        detail,
      );
    }

    const line = entries[0]!;
    const first = line.indexOf(":");
    const second = first < 0 ? -1 : line.indexOf(":", first + 1);
    if (
      first < 0 ||
      second < 0 ||
      line.slice(0, first) !== "0" ||
      line.slice(first + 1, second) !== ""
    ) {
      throw new ControlError(
        "resource_unverified",
        "CT cgroup path is not unified",
        detail,
      );
    }

    const path = line.slice(second + 1);
    if (
      path !== "/" &&
      (
        !/^\/[^\x00/]+(?:\/[^\x00/]+)*$/.test(path) ||
        path.split("/").some(part => part === "." || part === "..")
      )
    ) {
      throw new ControlError(
        "resource_unverified",
        "CT cgroup path is invalid",
        detail,
      );
    }
    return path;
  }

  _cgroup_metric_path(metric: string): [string, string] {
    const [path, paths] = this._cgroup_metric_paths(metric);
    return [path, paths[0]!];
  }

  _cgroup_metric_paths(metric: string): [string, string[]] {
    this._verify_cgroup2_mount();
    const path = this.current_cgroup_path();
    const components = path === "/" ? [] : path.slice(1).split("/");
    const paths: string[] = [];

    for (let depth = components.length; depth >= 0; depth--) {
      const ancestor = depth
        ? "/" + components.slice(0, depth).join("/")
        : "";
      paths.push(`${CGROUP_ROOT}${ancestor}/${metric}`);
    }
    return [path, paths];
  }

  cpu_pressure(): Data {
    const [cgroup_path, pressure_path] =
      this._cgroup_metric_path("cpu.pressure");

    let values: CPUPressure;
    try {
      values = parse_cpu_psi(this._read(pressure_path));
    } catch (error) {
      if (!(error instanceof PSIValueError)) throw error;
      throw new ControlError(
        "resource_unverified",
        "CT CPU pressure metric is invalid",
        { metric: pressure_path },
      );
    }

    return {
      scope: "ct-cpu-pressure",
      sources: ["cgroupfs"],
      mountpoint: CGROUP_ROOT,
      mount_root: "/",
      filesystem: "cgroup2",
      cgroup_path,
      metric: pressure_path,
      ...values,
    };
  }

  _lxcfs_meminfo(): LxcfsMemory | null {
    // Read meminfo only after verifying its virtualization boundary.
    const mounts: Array<[string, string]> = [];
    for (const line of lines(this._read("/proc/self/mountinfo"))) {
      const separator = line.indexOf(" - ");
      if (separator < 0) continue;

      const left = fields(line.slice(0, separator));
      const right = fields(line.slice(separator + 3));
      if (
        left.length >= 6 &&
        right.length >= 2 &&
        left[4] === "/proc/meminfo"
      ) {
        mounts.push([right[0]!, right[1]!]);
      }
    }

    if (
      mounts.length !== 1 ||
      mounts[0]![0] !== "fuse.lxcfs" ||
      mounts[0]![1] !== "lxcfs"
    ) {
      return null;
    }

    const values: Partial<LxcfsMemory> = {};
    for (const line of lines(this._read("/proc/meminfo"))) {
      const separator = line.indexOf(":");
      if (separator < 0) continue;

      const name = line.slice(0, separator);
      if (name !== "MemTotal" && name !== "MemAvailable") continue;

      const parts = fields(line.slice(separator + 1));
      if (
        Object.hasOwn(values, name) ||
        parts.length !== 2 ||
        parts[1] !== "kB"
      ) {
        throw new ControlError(
          "resource_unverified",
          "invalid LXCfs memory metric",
          { metric: name },
        );
      }
      values[name] = CTResources._unsigned(parts[0]!, name) * 1024n;
    }

    const total = values.MemTotal;
    const available = values.MemAvailable;
    if (
      total === undefined ||
      available === undefined ||
      total <= 0n ||
      available < 0n ||
      available > total
    ) {
      throw new ControlError(
        "resource_unverified",
        "LXCfs memory metrics are missing or inconsistent",
      );
    }
    return { MemTotal: total, MemAvailable: available };
  }

  _optional_read(path: string): string | null {
    try {
      return this._read(path);
    } catch (error) {
      if (
        error instanceof ControlError &&
        error.code === "resource_unverified"
      ) {
        return null;
      }
      throw error;
    }
  }

  _memory_events(): Data | null {
    const raw = this._optional_read(`${CGROUP_ROOT}/memory.events`);
    if (raw === null) return null;

    const values: Data = {};
    for (const line of lines(raw)) {
      const parts = fields(line);
      const name = parts[0];
      if (
        parts.length !== 2 ||
        (name !== "oom" &&
          name !== "oom_kill" &&
          name !== "oom_group_kill")
      ) {
        continue;
      }
      if (Object.hasOwn(values, name)) return null;

      try {
        values[name] = narrow(
          CTResources._unsigned(parts[1]!, `memory.events.${name}`),
        );
      } catch (error) {
        // Optional OOM counters never invalidate verified capacity.
        if (error instanceof ControlError) return null;
        throw error;
      }
    }
    return Object.keys(values).length ? values : null;
  }

  memory(): Data {
    // Memory intentionally observes the visible CT root cgroup.
    const raw_limit = this._read(`${CGROUP_ROOT}/memory.max`).trim();
    const lxcfs = this._lxcfs_meminfo();
    const sources: string[] = [];
    let current: bigint | null = null;
    let limit: bigint;
    let available: bigint;

    if (raw_limit === "max") {
      if (lxcfs === null) {
        throw new ControlError(
          "resource_unverified",
          "unlimited CT cgroup requires a verified LXCfs memory view",
        );
      }
      limit = lxcfs.MemTotal;
      available = lxcfs.MemAvailable;

      const raw_current =
        this._optional_read(`${CGROUP_ROOT}/memory.current`);
      if (raw_current !== null) {
        current = CTResources._unsigned(raw_current, "memory.current");
        if (current > limit) {
          throw new ControlError(
            "resource_unverified",
            "CT memory accounting exceeds its LXCfs capacity",
          );
        }
      }
    } else {
      limit = CTResources._unsigned(raw_limit, "memory.max", true);
      current = CTResources._unsigned(
        this._read(`${CGROUP_ROOT}/memory.current`),
        "memory.current",
      );

      if (current > limit) {
        throw new ControlError(
          "resource_unverified",
          "CT memory accounting exceeds its cgroup limit",
        );
      }

      const entries = lines(this._read(`${CGROUP_ROOT}/memory.stat`))
        .map(fields)
        .filter(parts => parts[0] === "inactive_file");

      if (entries.length !== 1 || entries[0]!.length !== 2) {
        throw new ControlError(
          "resource_unverified",
          "inactive_file memory metric is missing or duplicated",
        );
      }

      const inactive = CTResources._unsigned(
        entries[0]![1]!,
        "inactive_file",
      );
      if (inactive > current) {
        throw new ControlError(
          "resource_unverified",
          "CT memory accounting is inconsistent",
        );
      }

      const headroom = limit - current + inactive;
      available = minimum(limit, headroom > 0n ? headroom : 0n);
      sources.push("cgroup-v2");

      if (lxcfs !== null) {
        limit = minimum(limit, lxcfs.MemTotal);
        if (current > limit) {
          throw new ControlError(
            "resource_unverified",
            "CT memory accounting exceeds its effective capacity",
          );
        }
        available = minimum(
          minimum(available, lxcfs.MemAvailable),
          limit,
        );
      }
    }

    if (lxcfs !== null) sources.push("lxcfs");
    const tenth = (limit + 9n) / 10n;

    return {
      scope: "ct-memory",
      sources,
      limit_bytes: narrow(limit),
      available_bytes: narrow(available),
      current_bytes: current === null ? null : narrow(current),
      threshold_bytes: narrow(tenth > GIB_BYTES ? tenth : GIB_BYTES),
      oom_events: this._memory_events(),
    };
  }
}
