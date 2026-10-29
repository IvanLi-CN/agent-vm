import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

export type Data = Record<string, any>;
export type Integer = number | bigint;
export const PROTOCOL_VERSION = 4;
export const SCHEMA_VERSION = 1;
export const SEMANTIC_REVISION = 1;
export const GIB = 1024 ** 3;
export const MEMORY_SETTLE_SECONDS = 45;
export const GUEST_READINESS_BUDGET_SECONDS = 300;
export const DEFAULT_RESOURCES = { cpu: 2, memory_mib: 4096, disk_gib: 20 };
export const DEFAULT_CONFIG_PATH = "/etc/agent-vm-native/config.json";
export const DEFAULT_TIMER_DIR = "/etc/systemd/system";
export const DEFAULT_RELEASE_FENCE_PATH = "/run/agent-vm-native/release-activation.fence";
export const TASK_ID_RE = /^[a-z0-9][a-z0-9-]{0,47}$/;
export const SESSION_ID_RE = /^[A-Za-z0-9_.:@/-]{1,160}$/;
export const PORT_NAME_RE = /^[a-z][a-z0-9-]{0,30}$/;
export const TASK_STATES = new Set(["absent", "cached", "preparing", "active", "closing", "deleting", "failed"]);
export const RESOURCE_KEYS = ["cpu", "memory_mib", "disk_gib"];

export class ControlError extends Error {
  constructor(public code: string, message: string, public detail?: any) { super(message); this.name = "ControlError"; }
  as_dict(): Data { return { code: this.code, message: this.message, ...(this.detail === undefined || this.detail === null ? {} : { detail: this.detail }) }; }
}
export class GuestWaitTimeout extends ControlError {
  constructor(detail?: any) { super("guest_wait_timeout", "Incus readiness wait expired", detail); }
}

// JSON's numeric spelling is significant: Python rejects 1.0 for an integer
// field and hashes it differently from 1. Preserve that distinction on input.
export class JsonFloat extends Number {}
export function is_record(value: any): value is Data {
  return value !== null && typeof value === "object" && !Array.isArray(value) && !(value instanceof Number) && !(value instanceof Date) && !Buffer.isBuffer(value);
}
export function narrow(value: bigint): Integer {
  return value <= BigInt(Number.MAX_SAFE_INTEGER) && value >= BigInt(Number.MIN_SAFE_INTEGER) ? Number(value) : value;
}
export function integer(value: any): bigint {
  if (typeof value === "bigint") return value;
  if (typeof value === "number" && Number.isSafeInteger(value)) return BigInt(value);
  throw new ControlError("invalid_request", "value must be an integer");
}
export const add = (a: Integer, b: Integer): Integer => narrow(integer(a) + integer(b));
export const sub = (a: Integer, b: Integer): Integer => narrow(integer(a) - integer(b));
export const mul = (a: Integer, b: Integer): Integer => narrow(integer(a) * integer(b));
export function floor_div(a: Integer, b: Integer): Integer {
  const x = integer(a), y = integer(b);
  if (y === 0n) throw new RangeError("division by zero");
  const quotient = x / y;
  return narrow(quotient - (x % y !== 0n && (x < 0n) !== (y < 0n) ? 1n : 0n));
}
export function compare(a: Integer, b: Integer): number { const x = integer(a), y = integer(b); return x < y ? -1 : x > y ? 1 : 0; }
export function require_int(value: any, field: string, minimum?: Integer | null, maximum?: Integer | null): Integer {
  if (typeof value !== "bigint" && (typeof value !== "number" || !Number.isSafeInteger(value))) throw new ControlError("invalid_request", `${field} must be an integer`);
  if (minimum != null && compare(value, minimum) < 0) throw new ControlError("invalid_request", `${field} must be >= ${minimum}`);
  if (maximum != null && compare(value, maximum) > 0) throw new ControlError("invalid_request", `${field} must be <= ${maximum}`);
  return value;
}
export function require_string(value: any, field: string, pattern?: RegExp | null): string {
  if (typeof value !== "string" || !value) throw new ControlError("invalid_request", `${field} must be a non-empty string`);
  if (pattern) {
    pattern.lastIndex = 0;
    const match = pattern.exec(value);
    if (!match || match.index !== 0 || match[0].length !== value.length) throw new ControlError("invalid_request", `${field} contains unsupported characters`);
  }
  return value;
}
export const require_nonempty_string = require_string;

export function parse_json(input: string | Buffer): any {
  const source = String(input); let position = 0;
  const fail = (): never => { throw new SyntaxError(`invalid JSON at ${position}`); };
  function space() { while (/[\t\n\r ]/.test(source[position] ?? "x")) position++; }
  function value(): any {
    space(); const start = position, ch = source[position++];
    if (ch === '"') {
      let escaped = false;
      while (position < source.length) {
        const next = source[position++];
        if (next === '"' && !escaped) return JSON.parse(source.slice(start, position));
        if (next === "\\" && !escaped) escaped = true; else escaped = false;
      }
      return fail();
    }
    if (ch === "[") {
      const items: any[] = []; space(); if (source[position] === "]") { position++; return items; }
      for (;;) { items.push(value()); space(); const separator = source[position++]; if (separator === "]") return items; if (separator !== ",") return fail(); }
    }
    if (ch === "{") {
      const result: Data = {}; space(); if (source[position] === "}") { position++; return result; }
      for (;;) {
        space(); if (source[position] !== '"') return fail();
        const key = value(); space(); if (source[position++] !== ":") return fail();
        Object.defineProperty(result, key, { value: value(), enumerable: true, configurable: true, writable: true });
        space(); const separator = source[position++]; if (separator === "}") return result; if (separator !== ",") return fail();
      }
    }
    position = start;
    for (const [literal, result] of [["true", true], ["false", false], ["null", null]] as const) {
      if (source.startsWith(literal, position)) { position += literal.length; return result; }
    }
    for (const literal of ["-Infinity", "Infinity", "NaN"]) {
      if (source.startsWith(literal, position)) { position += literal.length; return new JsonFloat(Number(literal)); }
    }
    const token = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(source.slice(position))?.[0];
    if (!token) return fail(); position += token.length;
    return /[.eE]/.test(token) ? new JsonFloat(Number(token)) : narrow(BigInt(token));
  }
  const result = value(); space(); if (position !== source.length) fail(); return result;
}

function quote(value: string): string {
  return JSON.stringify(value).replace(/[\u007f-\uffff]/g, ch => "\\u" + ch.charCodeAt(0).toString(16).padStart(4, "0"));
}
export function float_repr(value: number): string {
  if (!Number.isFinite(value)) return Number.isNaN(value) ? "NaN" : value < 0 ? "-Infinity" : "Infinity";
  if (Object.is(value, -0)) return "-0.0";
  if (value === 0) return "0.0";
  const negative = value < 0 ? "-" : "", raw = Math.abs(value).toString().toLowerCase();
  const [mantissa, power] = raw.split("e");
  const point = mantissa.indexOf(".");
  const all = mantissa.replace(".", "");
  const leading = all.search(/[1-9]/);
  const digits = all.slice(leading).replace(/0+$/, "");
  const exponent = (power === undefined ? 0 : Number(power)) + (point === -1 ? mantissa.length : point) - leading - 1;
  if (exponent < -4 || exponent >= 16) {
    const fraction = digits.length > 1 ? "." + digits.slice(1) : "";
    return negative + digits[0] + fraction + "e" + (exponent < 0 ? "-" : "+") + String(Math.abs(exponent)).padStart(2, "0");
  }
  const offset = exponent + 1;
  const fixed = offset <= 0 ? "0." + "0".repeat(-offset) + digits : offset >= digits.length ? digits + "0".repeat(offset - digits.length) + ".0" : digits.slice(0, offset) + "." + digits.slice(offset);
  return negative + fixed;
}
function compare_unicode(a: string, b: string): number {
  const x = Array.from(a, ch => ch.codePointAt(0)!), y = Array.from(b, ch => ch.codePointAt(0)!);
  for (let i = 0; i < Math.min(x.length, y.length); i++) if (x[i] !== y[i]) return x[i] - y[i];
  return x.length - y.length;
}
export function canonical_json(value: any, compact = false): string {
  const comma = compact ? "," : ", ", colon = compact ? ":" : ": ";
  function encode(item: any): string {
    if (item === null) return "null";
    if (typeof item === "string") return quote(item);
    if (typeof item === "bigint") return String(item);
    if (typeof item === "boolean") return String(item);
    if (item instanceof Number) return float_repr(Number(item));
    if (typeof item === "number") return Number.isSafeInteger(item) && !Object.is(item, -0) ? String(item) : float_repr(item);
    if (Array.isArray(item)) return "[" + item.map(encode).join(comma) + "]";
    if (!is_record(item)) throw new TypeError("value is not JSON serializable");
    return "{" + Object.keys(item).sort(compare_unicode).map(key => quote(key) + colon + encode(item[key])).join(comma) + "}";
  }
  return encode(value);
}
export const digest = (value: any): string => createHash("sha256").update(canonical_json(value)).digest("hex");
export function iso_time(value = new Date()): string { return value.toISOString().replace(/\.\d{3}Z$/, "+00:00"); }
export function parse_time(value: any): Date | null {
  if (!value) return null;
  if (typeof value !== "string") throw new ControlError("invalid_time", "timestamp must be a string");
  const parsed = new Date(value);
  if (!Number.isFinite(parsed.getTime())) throw new ControlError("invalid_time", `invalid timestamp: ${value}`);
  return parsed;
}
export function resource_mapping(raw: any, label: string, { allow_empty = false } = {}): Data {
  if (typeof raw !== "string") throw new Error(label);
  const result = parse_json(raw);
  if (!is_record(result) || !(allow_empty && Object.keys(result).length === 0 || Object.keys(result).length === 3 && RESOURCE_KEYS.every(key => key in result))) throw new Error(label);
  for (const value of Object.values(result)) require_int(value, label, 0);
  return result;
}
export function decimal_ratio(value: any, name: string): { numerator: bigint; denominator: bigint } {
  if (!(typeof value === "number" || typeof value === "bigint" || value instanceof JsonFloat) || !Number.isFinite(Number(value)) || Number(value) < 1) throw new ControlError("config_invalid", `${name} must be a finite number at least one`);
  const raw = value instanceof JsonFloat || typeof value === "number" && !Number.isInteger(value) ? float_repr(Number(value)) : String(value);
  const [mantissa, exponent = "0"] = raw.toLowerCase().split("e");
  const fraction = mantissa.split(".")[1]?.length ?? 0;
  const scale = fraction - Number(exponent);
  const digits = BigInt(mantissa.replace(".", ""));
  return scale >= 0 ? { numerator: digits, denominator: 10n ** BigInt(scale) } : { numerator: digits * 10n ** BigInt(-scale), denominator: 1n };
}
export function allocation_limit(base: Integer, ratio: any, name = "ratio"): Integer {
  const { numerator, denominator } = decimal_ratio(ratio, name); return narrow(integer(base) * numerator / denominator);
}
export function ipv4_to_int(value: string): number {
  const parts = value.split(".");
  if (parts.length !== 4 || parts.some(part => !/^(0|[1-9][0-9]{0,2})$/.test(part) || Number(part) > 255)) throw new ControlError("config_invalid", `invalid IPv4 address: ${value}`);
  return parts.reduce((result, part) => result * 256 + Number(part), 0);
}
export const int_to_ipv4 = (value: number): string => [24, 16, 8, 0].map(shift => (value >>> shift) & 255).join(".");
export function load_config(path = DEFAULT_CONFIG_PATH): Data {
  let raw: any;
  try { raw = parse_json(readFileSync(path)); } catch (error: any) { throw new ControlError(error.code === "ENOENT" ? "config_missing" : "config_invalid", `${error.code === "ENOENT" ? "configuration not found" : "cannot read configuration"}: ${path}`); }
  if (!is_record(raw)) throw new ControlError("config_invalid", "configuration root must be an object");
  if ("backing_probes" in raw) throw new ControlError("config_invalid", "external storage probes are unsupported; remove backing_probes");
  const config: Data = { initial_lease_seconds: 1800, renew_seconds: 900, max_lease_seconds: 7200, cache_seconds: 86400, cpu_budget: 0, memory_budget_mib: 0, cpu_overcommit_ratio: 4, memory_overcommit_ratio: 4, cpu_startup_pressure_full_avg10_percent: 20, create_cost_bytes: 4 * GIB, resume_cost_bytes: GIB, resize_cost_bytes: GIB, required_guest_tools: ["bun", "python3", "sshd", "rsync", "docker", "growpart"], timer_dir: DEFAULT_TIMER_DIR, release_fence_path: DEFAULT_RELEASE_FENCE_PATH, ...raw };
  const required = ["project", "profile", "image_alias", "image_fingerprint", "network", "network_acl", "pool", "host_address", "port_start", "port_end", "vm_ip_start", "vm_ip_end", "gateway_ip", "storage_path", "state_db", "timer_dir"];
  const missing = required.filter(key => raw[key] === undefined || raw[key] === null || raw[key] === "");
  if (missing.length) throw new ControlError("config_missing", "required environment configuration is missing", { fields: missing });
  for (const key of ["project", "profile", "image_alias", "image_fingerprint", "network", "network_acl", "pool", "storage_path", "state_db", "timer_dir", "release_fence_path"]) require_string(config[key], key);
  if (!/^[0-9a-f]{64}$/.test(config.image_fingerprint)) throw new ControlError("config_invalid", "image_fingerprint must be a 64-character lowercase hex fingerprint");
  require_int(config.port_start, "port_start", 1024, 65535); require_int(config.port_end, "port_end", config.port_start, 65535);
  for (const key of ["initial_lease_seconds", "renew_seconds", "max_lease_seconds", "cache_seconds"]) require_int(config[key], key, 1);
  if (compare(config.renew_seconds, config.max_lease_seconds) > 0) throw new ControlError("config_invalid", "renew_seconds exceeds max_lease_seconds");
  for (const key of ["cpu_budget", "memory_budget_mib", "create_cost_bytes", "resume_cost_bytes", "resize_cost_bytes"]) require_int(config[key], key, 0);
  for (const key of ["cpu_overcommit_ratio", "memory_overcommit_ratio"]) decimal_ratio(config[key], key);
  const threshold = config.cpu_startup_pressure_full_avg10_percent;
  if (!(typeof threshold === "number" || typeof threshold === "bigint" || threshold instanceof JsonFloat) || !Number.isFinite(Number(threshold)) || Number(threshold) < 0 || Number(threshold) > 100) throw new ControlError("config_invalid", "cpu_startup_pressure_full_avg10_percent must be a finite percentage between zero and 100");
  config.cpu_startup_pressure_full_avg10_percent = Number(threshold);
  for (const key of ["host_address", "vm_ip_start", "vm_ip_end", "gateway_ip"]) { require_string(config[key], key); ipv4_to_int(config[key]); }
  if (ipv4_to_int(config.vm_ip_start) > ipv4_to_int(config.vm_ip_end)) throw new ControlError("config_invalid", "vm_ip_start must not exceed vm_ip_end");
  if (!Array.isArray(config.required_guest_tools) || !config.required_guest_tools.every((item: any) => typeof item === "string" && item)) throw new ControlError("config_invalid", "required_guest_tools must be a list of strings");
  // Bun is required by the managed disk action even for an older explicit list.
  config.required_guest_tools = Array.from(new Set(["bun", ...config.required_guest_tools]));
  require_string(config.network_acl, "network_acl", PORT_NAME_RE);
  if (config.release_fence_path !== DEFAULT_RELEASE_FENCE_PATH) throw new ControlError("config_invalid", "release_fence_path must use the fixed activation fence");
  return config;
}
