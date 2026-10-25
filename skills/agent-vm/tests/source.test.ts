import { afterEach, expect, test } from "bun:test";
import { constants as C, chmodSync, cpSync, existsSync, fchmodSync, lstatSync, mkdirSync, readFileSync, readdirSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { decodePath, encodePath, json, parseJson, readJson, runBounded, sha256 } from "../scripts/client-io.ts";
import { guestAction, Manifest, OWNER, walkManifest } from "../scripts/guest-source.ts";
import { nativeCapabilities } from "../scripts/sync-native.ts";
import { bundleGuest, selectedFiles, sync } from "../scripts/sync_worktree.ts";
import { Fixture, result } from "./support.ts";
import { openBytes, readlinkBytes } from "../scripts/byte-fs.ts";
import { closeSync } from "node:fs";
import { dlopen, ptr } from "bun:ffi";

let fixtures: Fixture[] = [];
const fixture = () => { const f = new Fixture(); fixtures.push(f); return f; };
afterEach(() => { fixtures.forEach(f => f.close()); fixtures = []; });
const A = "a".repeat(32), B = "b".repeat(32), D = "d".repeat(32);
const guest = (f: Fixture, action: string, tx = A, extra = {}, opts = {}) => guestAction({ name: "repo", action, tx, ...extra }, { root: join(f.root, "worktrees"), ...opts });
const parent = (f: Fixture) => join(f.root, "worktrees/repo");
const stage = (f: Fixture, tx = A) => join(parent(f), ".agent-vm-src-stage." + tx);
function populate(f: Fixture, tx: string, name: string, data: Buffer | string, mode = 0o644): Manifest {
  const path = join(stage(f, tx), name); mkdirSync(dirname(path), { recursive: true }); writeFileSync(path, data); chmodSync(path, mode);
  return walkManifest(stage(f, tx));
}
async function git(f: Fixture, ...args: string[]): Promise<void> { const r = await runBounded(["git", "-C", f.root, ...args]); expect(r.code).toBe(0); }
function byteFile(path: Buffer, data: Buffer | string, mode = 0o644): void {
  if (process.platform !== "linux") { writeFileSync(path, data, { flag: "wx", mode }); chmodSync(path, mode); return; }
  // Linux's C varargs ABI passes this mode like a fixed argument. Darwin's does
  // not; its fixture above uses valid UTF-8 through the ordinary filesystem API.
  const library = dlopen("libc.so.6", { open: { args: ["ptr", "i32", "i32"], returns: "i32" } }), name = Buffer.concat([path, Buffer.from([0])]);
  const fd = library.symbols.open(ptr(name), C.O_WRONLY | C.O_CREAT | C.O_EXCL, mode);
  try { expect(fd).toBeGreaterThanOrEqual(0); fchmodSync(fd, mode); writeFileSync(fd, data); } finally { if (fd >= 0) closeSync(fd); library.close(); }
}
function byteLink(target: Buffer, path: string): void {
  const library = dlopen(process.platform === "linux" ? "libc.so.6" : "/usr/lib/libSystem.B.dylib", { symlink: { args: ["ptr", "ptr"], returns: "i32" } });
  const from = Buffer.concat([target, Buffer.from([0])]), to = Buffer.from(path + "\0");
  try { expect(library.symbols.symlink(ptr(from), ptr(to))).toBe(0); } finally { library.close(); }
}

test("Git names retain whitespace, unicode, invalid UTF-8 bytes, deleted/ignored files and links", async () => {
  const f = fixture(); await git(f, "init", "-q");
  for (const name of ["tracked", "deleted", "space name", "new\nline", "--option", "quote'`$", "日本語", "__proto__"]) writeFileSync(join(f.root, name), name);
  writeFileSync(join(f.root, ".gitignore"), "ignored\n"); writeFileSync(join(f.root, "ignored"), "no");
  symlinkSync("/outside/not-followed", join(f.root, "link"));
  await git(f, "add", "--", "tracked", "deleted"); rmSync(join(f.root, "deleted"));
  // Linux invalid bytes use the native bindings; Darwin fixtures use valid UTF-8.
  const raw = Buffer.from(process.platform === "linux" ? [0x62, 0x61, 0x64, 0xff] : [0x62, 0x61, 0x64, 0xc3, 0xbf]); byteFile(Buffer.concat([Buffer.from(f.root + "/"), raw]), Buffer.from([0, 255]));
  const got = await selectedFiles(f.root);
  expect(got.names.some(n => n.equals(raw))).toBe(true); expect(encodePath(decodePath(raw))).toEqual(raw);
  expect(got.manifest[decodePath(raw)]).toEqual({ sha256: sha256(Buffer.from([0, 255])), mode: 0o644 });
  expect(got.manifest.link).toEqual({ link: "/outside/not-followed" }); expect(got.manifest).not.toHaveProperty("deleted"); expect(got.manifest).not.toHaveProperty("ignored"); expect(got.names.some(n => n.equals(Buffer.from(".git")))).toBe(false);
  expect(Object.hasOwn(got.manifest, "__proto__")).toBe(true);
  const back = parseJson(json(got.manifest)); expect(encodePath(Object.keys(back).find(k => k.startsWith("bad"))!)).toEqual(raw);
  expect(encodePath(decodePath(Buffer.from([0xff, 0xfe, 0xed, 0xa0, 0x80])))).toEqual(Buffer.from([0xff, 0xfe, 0xed, 0xa0, 0x80]));
});

test("symlink parent, reserved owner and submodules reject before any remote action", async () => {
  const f = fixture(); await git(f, "init", "-q"); mkdirSync(join(f.root, "dir")); writeFileSync(join(f.root, "dir/file"), "data"); await git(f, "add", "dir/file");
  rmSync(join(f.root, "dir"), { recursive: true }); symlinkSync("/tmp", join(f.root, "dir"));
  await expect(selectedFiles(f.root)).rejects.toThrow("parent"); rmSync(join(f.root, "dir"));
  writeFileSync(join(f.root, OWNER), "reserved"); await expect(selectedFiles(f.root)).rejects.toThrow("reserved"); rmSync(join(f.root, OWNER));
  await git(f, "update-index", "--add", "--cacheinfo", "160000," + "1".repeat(40) + ",module");
  const commands: string[][] = [];
  await expect(sync("repo", { cwd: f.root, run: async (argv, opts) => { commands.push(argv); return runBounded(argv, opts); } })).rejects.toThrow("submodules");
  expect(commands.every(a => a[0] === "git")).toBe(true); expect(existsSync(join(f.root, "worktrees"))).toBe(false);
});

test("binary hash, exact modes, symlink bytes and source replacement preserve adjacent results", async () => {
  const f = fixture(); await guest(f, "begin");
  const binary = Buffer.alloc(2 * 1024 * 1024 + 13, 255);
  const manifest = populate(f, A, "binary", binary, 0o751);
  const raw = Buffer.from(process.platform === "linux" ? [0x78, 0xff] : [0x78, 0xc3, 0xbf]); byteFile(Buffer.concat([Buffer.from(stage(f) + "/"), raw]), "raw");
  const target = Buffer.from(process.platform === "linux" ? [0x2e, 0x2e, 0x2f, 0xff] : [0x2e, 0x2e, 0x2f, 0xc3, 0xbf]);
  byteLink(target, join(stage(f), "link"));
  const all = walkManifest(stage(f)); await guest(f, "commit", A, { manifest: all }); await guest(f, "finish");
  expect(readFileSync(join(parent(f), "src/binary"))).toEqual(binary); expect(lstatSync(join(parent(f), "src/binary")).mode & 0o7777).toBe(0o751);
  expect(readlinkBytes(Buffer.from(join(parent(f), "src/link")))).toEqual(target);
  mkdirSync(join(parent(f), "results")); writeFileSync(join(parent(f), "results/keep"), "result");
  symlinkSync("../results", join(parent(f), "src/adjacent"));
  await guest(f, "begin", B); await guest(f, "commit", B, { manifest: populate(f, B, "new\nfile", "next") }); await guest(f, "finish", B);
  expect(existsSync(join(parent(f), "src/binary"))).toBe(false); expect(readFileSync(join(parent(f), "results/keep"), "utf8")).toBe("result"); expect(readFileSync(join(parent(f), "src/new\nfile"), "utf8")).toBe("next");
});

test("missing native capability, unowned source, symlink paths and exclusive stage fail safely", async () => {
  const f = fixture();
  await expect(guest(f, "begin", A, {}, { native: () => { throw new Error("atomic capability unavailable"); } })).rejects.toThrow("unavailable");
  expect(existsSync(join(f.root, "worktrees"))).toBe(false);
  expect(() => openBytes(Buffer.from(join(f.root, "forbidden")), C.O_WRONLY | C.O_CREAT)).toThrow("read-only open boundary");
  expect(existsSync(join(f.root, "forbidden"))).toBe(false);
  mkdirSync(join(parent(f), "src"), { recursive: true }); writeFileSync(join(parent(f), "src/keep"), "stranger");
  await expect(guest(f, "begin")).rejects.toThrow(); expect(readFileSync(join(parent(f), "src/keep"), "utf8")).toBe("stranger");
  rmSync(join(parent(f), "src"), { recursive: true }); mkdirSync(stage(f)); writeFileSync(join(stage(f), "keep"), "stranger");
  await expect(guest(f, "begin")).rejects.toThrow(); expect(readFileSync(join(stage(f), "keep"), "utf8")).toBe("stranger");
  rmSync(join(f.root, "worktrees"), { recursive: true }); mkdirSync(join(f.root, "worktrees")); symlinkSync(f.root, parent(f));
  await expect(guest(f, "begin")).rejects.toThrow("symlink");
});

test("baseline conflict serializes concurrent preparations and preserves the winning source", async () => {
  const f = fixture(); await Promise.all([guest(f, "begin", A), guest(f, "begin", B)]);
  const a = populate(f, A, "winner", "a"), b = populate(f, B, "loser", "b");
  await guest(f, "commit", A, { manifest: a }); await expect(guest(f, "commit", B, { manifest: b })).rejects.toThrow("changed");
  await guest(f, "abort", B); expect(readFileSync(join(parent(f), "src/winner"), "utf8")).toBe("a"); expect(existsSync(stage(f, B))).toBe(false);
});

test("manifest mutation and atomic exchange failures retain old source and owned recovery paths", async () => {
  const f = fixture(); await guest(f, "begin"); await guest(f, "commit", A, { manifest: populate(f, A, "old", "old") }); await guest(f, "finish");
  await guest(f, "begin", B); const manifest = populate(f, B, "next", "new");
  await expect(guest(f, "commit", B, { manifest: {} })).rejects.toThrow("differ");
  const failingNative = () => ({ ...nativeCapabilities(), rename: () => { throw new Error("atomic exchange failed"); } });
  await expect(guest(f, "commit", B, { manifest }, { native: failingNative })).rejects.toThrow("exchange");
  expect(readFileSync(join(parent(f), "src/old"), "utf8")).toBe("old"); expect(readJson(join(parent(f), `.agent-vm-sync-${B}.json`)).phase).toBe("prepared");
  await guest(f, "abort", B); expect(existsSync(stage(f, B))).toBe(false);
});

test("fsync/record fault after exchange is detected by source marker; abort retains and finish recovers", async () => {
  const f = fixture(); await guest(f, "begin"); const manifest = populate(f, A, "file", "ok");
  await expect(guest(f, "commit", A, { manifest }, { checkpoint: (p: string) => { if (p === "after-rename") throw new Error("lost ack before record fsync"); } })).rejects.toThrow("lost ack");
  const recordPath = join(parent(f), `.agent-vm-sync-${A}.json`); expect(readJson(recordPath).phase).toBe("prepared");
  const abort = await guest(f, "abort"); expect(abort).toEqual({ unverified: true, record: recordPath }); expect(existsSync(recordPath)).toBe(true); expect(readFileSync(join(parent(f), "src/file"), "utf8")).toBe("ok");
  await guest(f, "finish"); expect(existsSync(recordPath)).toBe(false);
});

test("pre-rename durability failure leaves source intact; finish refuses unconfirmed and wrong-owned stage", async () => {
  const f = fixture(); await guest(f, "begin"); const manifest = populate(f, A, "old", "old");
  await expect(guest(f, "commit", A, { manifest }, { checkpoint: (p: string) => { if (p === "before-rename") throw new Error("fsync failed"); } })).rejects.toThrow("fsync");
  await expect(guest(f, "finish")).rejects.toThrow("not confirmed"); expect(existsSync(join(parent(f), "src"))).toBe(false);
  const m = readJson(join(stage(f), OWNER)); m.tx = B; writeFileSync(join(stage(f), OWNER), json(m));
  await expect(guest(f, "abort")).rejects.toThrow("stage identity"); expect(existsSync(stage(f))).toBe(true);
});

test("first commit refuses replacing even an empty foreign directory created at rename boundary", async () => {
  const f = fixture(); await guest(f, "begin"); const manifest = populate(f, A, "file", "data");
  await expect(guest(f, "commit", A, { manifest }, { checkpoint: (p: string) => { if (p === "before-rename") mkdirSync(join(parent(f), "src")); } })).rejects.toThrow("rename failed");
  expect(readdirSync(join(parent(f), "src"))).toEqual([]); expect(existsSync(join(stage(f), "file"))).toBe(true);
});

test("bounded lock acquisition refuses contention without adopting or removing foreign paths", async () => {
  const f = fixture(); await guest(f, "begin");
  const { openSync, closeSync } = await import("node:fs");
  const fd = openSync(join(parent(f), ".agent-vm-sync.lock"), "r+"), native = nativeCapabilities(); expect(native.lock(fd)).toBe(true);
  try { await expect(guest(f, "begin", B, {}, { lockTimeoutMs: 50 })).rejects.toThrow("lock timeout"); expect(existsSync(stage(f, B))).toBe(false); }
  finally { native.unlock(fd); closeSync(fd); native.close(); }
});

test("independent processes contend through native flock, and a killed commit recovers via marker", async () => {
  const f = fixture(), root = join(f.root, "worktrees"), worker = new URL("./source-worker.ts", import.meta.url).pathname;
  const action = (action: string, tx: string, manifest?: Manifest, crash?: string) => runBounded([process.execPath, worker], { input: json({ root, payload: { name: "repo", tx, action, ...(manifest ? { manifest } : {}) }, ...(crash ? { crash } : {}) }), timeoutMs: 4000 });
  const begin = await Promise.all([action("begin", A), action("begin", B)]); expect(begin.map(r => r.code)).toEqual([0, 0]);
  const a = populate(f, A, "winner-a", "a"), b = populate(f, B, "winner-b", "b");
  const commits = await Promise.all([action("commit", A, a), action("commit", B, b)]); expect(commits.map(r => r.code).sort()).toEqual([0, 1]);
  expect(commits.find(r => r.code)!.stderr.toString()).toContain("changed");
  const winner = commits[0].code === 0 ? A : B, loser = winner === A ? B : A;
  expect((await action("finish", winner)).code).toBe(0); expect((await action("abort", loser)).code).toBe(0);
  expect((await action("begin", D)).code).toBe(0); const d = populate(f, D, "crash-data", "durable");
  expect((await action("commit", D, d, "after-rename")).code).toBe(91);
  expect(readJson(join(parent(f), `.agent-vm-sync-${D}.json`)).phase).toBe("prepared");
  const abort = await action("abort", D); expect(parseJson(abort.stdout.toString()).unverified).toBe(true);
  expect((await action("finish", D)).code).toBe(0); expect(readFileSync(join(parent(f), "src/crash-data"), "utf8")).toBe("durable");
});

test.skipIf(process.platform !== "linux")("Linux raw invalid UTF-8 names cross Git, native hash, exchange and byte cleanup", async () => {
  const f = fixture(); await git(f, "init", "-q");
  const name = Buffer.from([0x66, 0xff]), path = Buffer.concat([Buffer.from(f.root + "/"), name]);
  byteFile(path, "raw-file", 0o755);
  const selected = await selectedFiles(f.root); expect(selected.names.some(n => n.equals(name))).toBe(true);
  await guest(f, "begin"); const staged = Buffer.concat([Buffer.from(stage(f) + "/"), name]); byteFile(staged, "raw-file", 0o755);
  await guest(f, "commit", A, { manifest: selected.manifest }); await guest(f, "finish");
  await guest(f, "begin", B); await guest(f, "commit", B, { manifest: populate(f, B, "next", "next") }); await guest(f, "finish", B);
  expect(existsSync(stage(f, B))).toBe(false); expect(readFileSync(join(parent(f), "src/next"), "utf8")).toBe("next");
});

test("standalone bundled guest refuses invalid names and missing Bun before writes", async () => {
  const f = fixture(), bundle = await bundleGuest(); expect(bundle).not.toContain("python3");
  const invalid = await runBounded([process.execPath, "-e", bundle], { input: json({ name: "../escape", tx: A, action: "begin" }), timeoutMs: 3000 });
  expect(invalid.code).toBe(1); expect(invalid.stderr.toString()).toContain("unsafe workspace");
  const missing = await runBounded(["/bin/sh", "-c", "command -v bun >/dev/null 2>&1 || { printf '%s\\n' 'guest Bun prerequisite missing' >&2; exit 127; }; exec bun -e 'throw 1'"], { env: { PATH: f.root }, timeoutMs: 3000 });
  expect(missing.code).toBe(127); expect(missing.stderr.toString()).toContain("Bun prerequisite missing"); expect(readdirSync(f.root)).toEqual([]);
});

test("local sync transfers exact NUL paths, private config, no lifecycle and preserves lost-ack exit 75", async () => {
  const f = fixture(); await git(f, "init", "-q"); writeFileSync(join(f.root, "file\nname"), "data");
  let configPath = "", loseAck = false, failTransfer = false, loseCleanup = false; const commands: string[][] = [], diagnostics: string[] = [];
  const run = async (argv: string[], opts = {}) => {
    commands.push(argv);
    if (argv[0] === "candidate-cli") { expect(argv.slice(-2)).toEqual(["--session-id", "explicit"]); return result("Host agent-vm\n  StrictHostKeyChecking yes\n"); }
    if (argv[0] === "rsync") {
      const listing = argv.find(a => a.startsWith("--files-from="))!.slice(13); expect(readFileSync(listing)).toEqual(Buffer.from("file\nname\0"));
      expect(lstatSync(configPath).mode & 0o777).toBe(0o600);
      if (failTransfer) return result("", 23);
      const tx = argv.at(-1)!.match(/stage\.([a-f0-9]{32})/)![1]; cpSync(join(f.root, "file\nname"), join(stage(f, tx), "file\nname"));
      return result();
    }
    return runBounded(argv, opts);
  };
  const remote = async (config: string, payload: any) => {
    configPath = config;
    if (payload.action === "finish" && loseCleanup) return result("malformed cleanup acknowledgement");
    const value = await guestAction(payload, { root: join(f.root, "worktrees") });
    if (payload.action === "begin") value.stage = `/workspace/worktrees/repo/.agent-vm-src-stage.${payload.tx}`;
    if (payload.action === "commit" && loseAck) return result("", 255, "connection lost");
    return result(value);
  };
  const options = { cwd: f.root, run, remote, cli: ["candidate-cli"], sessionId: "explicit", report: (s: string) => { diagnostics.push(s); } };
  expect((await sync("repo", options)).synced).toBe(true); expect(existsSync(configPath)).toBe(false);
  // Ignore the isolated simulated guest files when selecting the source worktree.
  writeFileSync(join(f.root, ".git/info/exclude"), "worktrees/\n");
  loseCleanup = true; expect((await sync("repo", options)).synced).toBe(true); expect(diagnostics.join("")).toContain("source committed; cleanup incomplete"); loseCleanup = false;
  failTransfer = true; await expect(sync("repo", options)).rejects.toMatchObject({ code: 23 }); expect(existsSync(configPath)).toBe(false);
  failTransfer = false; loseAck = true; await expect(sync("repo", options)).rejects.toMatchObject({ code: 75 }); expect(existsSync(configPath)).toBe(false);
  expect(readdirSync(parent(f)).some(n => n.startsWith(".agent-vm-sync-"))).toBe(true); expect(readFileSync(join(parent(f), "src/file\nname"), "utf8")).toBe("data");
  expect(commands.filter(a => a[0] === "candidate-cli").every(a => a[1] === "ssh-info")).toBe(true);
});
