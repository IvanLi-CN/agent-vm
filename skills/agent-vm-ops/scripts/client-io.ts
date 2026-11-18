import { constants as C, openSync, closeSync, fsyncSync, fchmodSync, fstatSync, readFileSync, writeFileSync, renameSync, unlinkSync, mkdirSync, lstatSync, readSync } from "node:fs";
import { dirname, join } from "node:path";
import { createHash, randomUUID } from "node:crypto";
import { openBytes } from "./byte-fs.ts";

export type Data = Record<string, any>;
export const record = (v: any): v is Data => v !== null && typeof v === "object" && !Array.isArray(v) && !(v instanceof FloatToken);
export class CliError extends Error {
  constructor(public code: string, message: string, public detail?: any, public exitCode = 2) { super(message); }
  asDict(): Data { return { code: this.code, message: this.message, ...(this.detail === undefined ? {} : { detail: this.detail }) }; }
}
export function fail(code: string, message: string, detail?: any, exitCode = 2): never { throw new CliError(code, message, detail, exitCode); }
export const sha256 = (value: string | Uint8Array): string => createHash("sha256").update(value).digest("hex");
export const quote = (value: string): string => /^[A-Za-z0-9_@%+=:,./-]+$/.test(value) ? value : "'" + value.replaceAll("'", "'\"'\"'") + "'";
export const shellJoin = (argv: string[]): string => argv.map(quote).join(" ");
export const integer = (v: any): boolean => typeof v === "bigint" || typeof v === "number" && Number.isSafeInteger(v);

// Retain integer tokens exactly and distinguish 1 from 1.0 for identity checks.
export class FloatToken { constructor(public token: string) {} }
export function parseJson(text: string): any {
  let pos = 0;
  const invalid = (): never => { throw new SyntaxError(`invalid JSON at ${pos}`); };
  const space = () => { while (/[\t\n\r ]/.test(text[pos] ?? "x")) pos++; };
  function value(): any {
    space(); const start = pos, ch = text[pos++];
    if (ch === '"') {
      let escaped = false;
      while (pos < text.length) { const next = text[pos++]; if (next === '"' && !escaped) return JSON.parse(text.slice(start, pos)); escaped = next === "\\" && !escaped; }
      return invalid();
    }
    if (ch === "[") {
      const result: any[] = []; space(); if (text[pos] === "]") { pos++; return result; }
      for (;;) { result.push(value()); space(); const c = text[pos++]; if (c === "]") return result; if (c !== ",") return invalid(); }
    }
    if (ch === "{") {
      const result: Data = {}; space(); if (text[pos] === "}") { pos++; return result; }
      for (;;) { space(); if (text[pos] !== '"') return invalid(); const key = value(); space(); if (text[pos++] !== ":") return invalid(); Object.defineProperty(result, key, { value: value(), enumerable: true, configurable: true, writable: true }); space(); const c = text[pos++]; if (c === "}") return result; if (c !== ",") return invalid(); }
    }
    pos = start;
    for (const [literal, result] of [["true", true], ["false", false], ["null", null]] as const) if (text.startsWith(literal, pos)) { pos += literal.length; return result; }
    const token = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(text.slice(pos))?.[0];
    if (!token) return invalid(); pos += token.length;
    if (/[.eE]/.test(token)) return new FloatToken(token);
    const n = BigInt(token); return n >= BigInt(Number.MIN_SAFE_INTEGER) && n <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(n) : n;
  }
  const result = value(); space(); if (pos !== text.length) invalid(); return result;
}
export function json(value: any): string {
  if (value instanceof FloatToken) return value.token;
  if (typeof value === "bigint") return String(value);
  if (Array.isArray(value)) return "[" + value.map(json).join(",") + "]";
  if (record(value)) return "{" + Object.keys(value).sort().map(k => JSON.stringify(k) + ":" + json(value[k])).join(",") + "}";
  return JSON.stringify(value);
}
export const equal = (a: any, b: any): boolean => json(a ?? null) === json(b ?? null);
export function readJson(path: string): any {
  const fd = openSync(path, C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK);
  try { if (!fstatSync(fd).isFile()) throw new Error("metadata is not a regular file"); return parseJson(new TextDecoder("utf-8", { fatal: true }).decode(readFileSync(fd))); } finally { closeSync(fd); }
}
export function fsyncDir(path: string | Buffer): void {
  const fd = Buffer.isBuffer(path) ? openBytes(path, C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW) : openSync(path, C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW);
  try { fsyncSync(fd); } finally { closeSync(fd); }
}
export function exclusive(path: string, content: string | Buffer, mode = 0o600): void {
  const fd = openSync(path, C.O_WRONLY | C.O_CREAT | C.O_EXCL | C.O_NOFOLLOW, mode);
  try { fchmodSync(fd, mode); writeFileSync(fd, content); fsyncSync(fd); } finally { closeSync(fd); }
}
export function durable(path: string, content: string, mode = 0o600): void {
  const tmp = join(dirname(path), "." + path.split("/").at(-1) + "." + randomUUID());
  exclusive(tmp, content, mode);
  try { renameSync(tmp, path); fsyncDir(dirname(path)); } finally { try { unlinkSync(tmp); } catch (e: any) { if (e.code !== "ENOENT") throw e; } }
}
export function privateDir(path: string): void { mkdirSync(path, { recursive: true, mode: 0o700 }); }
export function lexists(path: string | Buffer): boolean { try { lstatSync(path); return true; } catch (e: any) { if (e.code === "ENOENT") return false; throw e; } }
export function fileHash(path: string | Buffer): string {
  const fd = Buffer.isBuffer(path) ? openBytes(path, C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK) : openSync(path, C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK);
  try {
    if (!fstatSync(fd).isFile()) throw new Error("source is not a regular file");
    const h = createHash("sha256"), block = Buffer.alloc(1024 * 1024);
    for (;;) { const n = readSync(fd, block, 0, block.length, null); if (!n) break; h.update(block.subarray(0, n)); }
    return h.digest("hex");
  } finally { closeSync(fd); }
}

// POSIX filenames use UTF-8 + surrogateescape, matching the predecessor's JSON.
export function decodePath(bytes: Buffer): string {
  let out = "";
  for (let i = 0; i < bytes.length;) {
    const b = bytes[i], n = b < 128 ? 1 : b >= 194 && b <= 223 ? 2 : b >= 224 && b <= 239 ? 3 : b >= 240 && b <= 244 ? 4 : 0;
    const part = bytes.subarray(i, i + n);
    try { if (!n || part.length !== n) throw 0; out += new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(part); i += n; }
    catch { out += String.fromCharCode(0xdc00 + b); i++; }
  }
  return out;
}
export function encodePath(text: string): Buffer {
  const parts: Buffer[] = [];
  for (const ch of text) { const n = ch.codePointAt(0)!; parts.push(n >= 0xdc80 && n <= 0xdcff ? Buffer.from([n - 0xdc00]) : Buffer.from(ch)); }
  return Buffer.concat(parts);
}

export interface ProcessResult { code: number; stdout: Buffer; stderr: Buffer; stdoutTruncated: boolean; stderrTruncated: boolean; timedOut: boolean; streamCleanupVerified: boolean; }
export interface ProcessOptions { input?: string | Buffer; timeoutMs?: number; stdoutLimit?: number; stderrLimit?: number; cwd?: string; env?: Record<string, string | undefined>; }
export type Runner = (argv: string[], options?: ProcessOptions) => Promise<ProcessResult>;
export const runBounded: Runner = async (argv, options = {}) => {
  let p;
  try { p = Bun.spawn(argv, { stdin: options.input === undefined ? "ignore" : new Blob([typeof options.input === "string" ? options.input : new Uint8Array(options.input)]), stdout: "pipe", stderr: "pipe", cwd: options.cwd, env: options.env }); }
  catch (e: any) { if (e.code === "ENOENT") fail("command_missing", `command not found: ${argv[0]}`); throw e; }
  const streams = [p.stdout, p.stderr], limits = [options.stdoutLimit ?? 2 * 1024 * 1024, options.stderrLimit ?? 64 * 1024];
  const output = [Buffer.alloc(0), Buffer.alloc(0)], eof = [false, false], readers = streams.map(s => s.getReader());
  const drain = readers.map(async (reader, i) => {
    try { for (;;) { const item = await reader.read(); if (item.done) { eof[i] = true; break; } const room = limits[i] + 1 - output[i].length; if (room > 0) output[i] = Buffer.concat([output[i], item.value.subarray(0, room)]); } } catch { /* stream is unverified */ }
  });
  let timer: ReturnType<typeof setTimeout>;
  let deadline = false;
  await Promise.race([Promise.all([p.exited, ...drain]), new Promise<void>(resolve => { timer = setTimeout(() => { deadline = true; resolve(); }, options.timeoutMs ?? 240000); })]);
  clearTimeout(timer!);
  const streamCleanupVerified = eof.every(Boolean);
  const timedOut = deadline && p.exitCode === null;
  if (timedOut) p.kill("SIGKILL");
  if (deadline) for (const reader of readers) void reader.cancel().catch(() => {});
  const code = await p.exited;
  return { code, stdout: output[0].subarray(0, limits[0]), stderr: output[1].subarray(0, limits[1]), stdoutTruncated: output[0].length > limits[0], stderrTruncated: output[1].length > limits[1], timedOut, streamCleanupVerified };
};
export async function inherited(argv: string[]): Promise<number> {
  try { return await Bun.spawn(argv, { stdin: "inherit", stdout: "inherit", stderr: "inherit" }).exited; }
  catch (e: any) { if (e.code === "ENOENT") fail("command_missing", `command not found: ${argv[0]}`); throw e; }
}
