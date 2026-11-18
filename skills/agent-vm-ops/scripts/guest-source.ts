import { constants as C, closeSync, fstatSync, fsyncSync, lstatSync, mkdirSync, openSync, readdirSync, readlinkSync, realpathSync, rmSync, unlinkSync } from "node:fs";
import { join, resolve } from "node:path";
import { Data, decodePath, durable, encodePath, equal, exclusive, fileHash, fsyncDir, json, lexists, readJson, record, parseJson } from "./client-io.ts";
import { nativeCapabilities } from "./sync-native.ts";
import { lstatBytes, openBytes, readdirBytes, readlinkBytes, removeTreeBytes } from "./byte-fs.ts";

export const OWNER = ".agent-vm-sync-owner.json", TAG = "agent-vm-source-v1";
export const validName = (v: any): v is string => typeof v === "string" && /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/.test(v);
export type Manifest = Record<string, { link: string } | { sha256: string; mode: number }>;
export function directory(path: string, create = false): void {
  if (resolve(path) !== path) throw new Error("non-canonical directory");
  let current = "/";
  for (const part of path.split("/").slice(1)) {
    if (!part) continue;
    current = join(current, part);
    let stat;
    try { stat = lstatSync(current); } catch (e: any) {
      if (e.code !== "ENOENT" || !create) throw e;
      try { mkdirSync(current, { mode: 0o700 }); } catch (e: any) { if (e.code !== "EEXIST") throw e; }
      stat = lstatSync(current);
    }
    if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error("non-directory or symlink: " + current);
  }
  if (realpathSync(path) !== path) throw new Error("non-canonical directory");
}
export function walkManifest(root: string, flush = false): Manifest {
  const manifest: Manifest = Object.create(null);
  function visit(relative: Buffer): void {
    const path = Buffer.concat([Buffer.from(root + "/"), relative]);
    const stat = lstatBytes(path);
    if (stat.isDirectory() && !stat.isSymbolicLink()) {
      for (const name of readdirBytes(path)) {
        const child = relative.length ? Buffer.concat([relative, Buffer.from("/"), name]) : name;
        if (!relative.length && name.equals(Buffer.from(OWNER))) continue;
        visit(child);
      }
      if (flush) fsyncDir(path);
    } else if (stat.isSymbolicLink()) manifest[decodePath(relative)] = { link: decodePath(readlinkBytes(path)) };
    else if (stat.isFile()) {
      manifest[decodePath(relative)] = { sha256: fileHash(path), mode: stat.mode & 0o7777 };
      if (flush) { const fd = openBytes(path, C.O_RDONLY | C.O_NOFOLLOW); try { fsyncSync(fd); } finally { closeSync(fd); } }
    } else throw new Error("unsupported staged file: " + decodePath(relative));
  }
  visit(Buffer.alloc(0));
  if (flush) { const fd = openSync(join(root, OWNER), C.O_RDONLY | C.O_NOFOLLOW); try { fsyncSync(fd); } finally { closeSync(fd); } }
  return manifest;
}
export interface GuestOptions {
  root?: string;
  native?: typeof nativeCapabilities;
  // Fault injection is module-only: production stdin cannot choose roots or hooks.
  checkpoint?: (phase: string) => void;
  lockTimeoutMs?: number;
}
export async function guestAction(r: Data, options: GuestOptions = {}): Promise<Data> {
  if (!record(r) || !validName(r.name) || typeof r.tx !== "string" || !/^[0-9a-f]{32}$/.test(r.tx) || !["begin", "commit", "abort", "finish"].includes(r.action)) throw new Error("unsafe workspace, transaction or action");
  const { name, tx, action } = r;
  // Native capability resolution precedes even begin's parent creation.
  const native = (options.native ?? nativeCapabilities)();
  let lock = -1;
  const parent = join(options.root ?? "/workspace/worktrees", name), src = join(parent, "src"), stage = join(parent, ".agent-vm-src-stage." + tx), recordPath = join(parent, ".agent-vm-sync-" + tx + ".json");
  const checkpoint = options.checkpoint ?? (() => {});
  const owned = (path: string): Data => {
    directory(path);
    const m = readJson(join(path, OWNER));
    if (!record(m) || m.helper !== TAG || m.name !== name || typeof m.tx !== "string" || !/^[0-9a-f]{32}$/.test(m.tx)) throw new Error("source is not owned by this helper");
    return m;
  };
  const identity = (path: string): any[] | null => { if (!lexists(path)) return null; const m = owned(path), s = lstatSync(path, { bigint: true }); return [s.dev, s.ino, m.tx]; };
  const save = (path: string, value: Data) => { durable(path, json(value)); fsyncDir(parent); };
  try {
    directory(parent, action === "begin");
    lock = openSync(join(parent, ".agent-vm-sync.lock"), C.O_RDWR | C.O_CREAT | C.O_NOFOLLOW | C.O_NONBLOCK, 0o600);
    if (!fstatSync(lock).isFile()) throw new Error("invalid lock");
    const deadline = performance.now() + (options.lockTimeoutMs ?? 60000);
    while (!native.lock(lock)) { if (performance.now() >= deadline) throw new Error("source transaction lock timeout"); await Bun.sleep(25); }
    if (action === "begin") {
      const baseline = identity(src);
      if (lexists(recordPath)) throw new Error("transaction already exists; retain recovery material");
      mkdirSync(stage, { mode: 0o700 });
      save(join(stage, OWNER), { helper: TAG, name, tx });
      exclusive(recordPath, json({ phase: "prepared", baseline, tx })); fsyncDir(parent);
      checkpoint("prepared");
      return { stage, tx };
    }
    const state = readJson(recordPath);
    if (!record(state) || state.tx !== tx || !["prepared", "committed"].includes(state.phase) || !(state.baseline === null || Array.isArray(state.baseline) && state.baseline.length === 3 && typeof state.baseline[2] === "string")) throw new Error("invalid transaction record");
    if (action === "commit") {
      if (state.phase !== "prepared") throw new Error("transaction is not prepared");
      if (!equal(identity(src), state.baseline)) throw new Error("source changed during transfer; retry with a new transaction");
      if (owned(stage).tx !== tx) throw new Error("wrong stage owner");
      if (!record(r.manifest) || !equal(walkManifest(stage, true), r.manifest)) throw new Error("staged files differ from source manifest");
      checkpoint("before-rename");
      native.rename(Buffer.from(stage), Buffer.from(src), state.baseline !== null);
      fsyncDir(parent);
      checkpoint("after-rename");
      state.phase = "committed"; save(recordPath, state);
      checkpoint("committed");
      return { committed: true, tx, target: src };
    }
    const committed = lexists(src) && owned(src).tx === tx;
    if (action === "abort" && (committed || state.phase === "committed")) return { unverified: true, record: recordPath };
    if (action === "finish" && !committed) throw new Error("commit is not confirmed; retaining recovery material");
    if (lexists(stage)) {
      const expected = committed ? state.baseline : null;
      if (committed ? !expected || !equal(identity(stage), expected) : owned(stage).tx !== tx) throw new Error("stage identity differs; retaining recovery material");
      removeTreeBytes(Buffer.from(stage)); fsyncDir(parent);
    }
    unlinkSync(recordPath); fsyncDir(parent);
    return { cleaned: true };
  } finally { if (lock !== -1) { native.unlock(lock); closeSync(lock); } native.close(); }
}

if (import.meta.main) {
  try {
    const chunks: Buffer[] = []; let length = 0;
    for await (const chunk of Bun.stdin.stream()) { length += chunk.length; if (length > 32 * 1024 * 1024) throw new Error("source transaction request exceeds 32 MiB"); chunks.push(Buffer.from(chunk)); }
    const r = parseJson(new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks)));
    console.log(json(await guestAction(r)));
  } catch (e) { console.error(json({ ok: false, error: { code: "source_transaction_failed", message: e instanceof Error ? e.message : String(e) } })); process.exitCode = 1; }
}
