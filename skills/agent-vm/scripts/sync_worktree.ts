#!/usr/bin/env bun
/** Git byte-path selection and bounded orchestration; never changes the lease. */
import { constants as C, closeSync, lstatSync, mkdtempSync, openSync, readlinkSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";
import { randomUUID } from "node:crypto";
import { CliError, Data, decodePath, encodePath, exclusive, fileHash, json, parseJson, Runner, runBounded, shellJoin } from "./client-io.ts";
import { Manifest, OWNER, validName } from "./guest-source.ts";
import { lstatBytes, readlinkBytes } from "./byte-fs.ts";
import { loadRelease } from "./client-release.ts";

export class SyncFailure extends Error { constructor(message: string, public code = 1) { super(message); } }
const splitNul = (b: Buffer): Buffer[] => { const result: Buffer[] = []; let start = 0; for (let i = 0; i < b.length; i++) if (b[i] === 0) { if (i > start) result.push(b.subarray(start, i)); start = i + 1; } if (start < b.length) throw new SyncFailure("Git file listing is not NUL terminated"); return result; };
export async function selectedFiles(root: string, run: Runner = runBounded): Promise<{ names: Buffer[]; manifest: Manifest }> {
  // Listing may exceed a normal management response, but remains explicitly bounded.
  const options = { stdoutLimit: 64 * 1024 * 1024, timeoutMs: 240000 };
  const modules = await run(["git", "-C", root, "ls-files", "--stage", "-z"], options);
  if (modules.code || modules.stdoutTruncated || modules.timedOut || !modules.streamCleanupVerified) throw new SyncFailure("cannot inspect Git index", modules.code || 1);
  if (splitNul(modules.stdout).some(b => b.subarray(0, 7).equals(Buffer.from("160000 ")))) throw new SyncFailure("submodules are unsupported; use explicit native upload/rsync");
  const listing = await run(["git", "-C", root, "ls-files", "-co", "--exclude-standard", "-z"], options);
  if (listing.code || listing.stdoutTruncated || listing.timedOut || !listing.streamCleanupVerified) throw new SyncFailure("cannot select Git files", listing.code || 1);
  const unique = new Map(splitNul(listing.stdout).map(b => [b.toString("hex"), b]));
  const manifest: Manifest = Object.create(null), names: Buffer[] = [];
  for (const raw of [...unique.values()].sort(Buffer.compare)) {
    const parts = decodePath(raw).split("/");
    if (parts.some(p => !p || p === "." || p === "..")) throw new SyncFailure("unsafe Git file path");
    if (parts.includes(".git")) continue;
    const name = decodePath(raw);
    if (name === OWNER) throw new SyncFailure("source uses the helper's reserved ownership filename");
    let parent = root, absent = false;
    for (const part of parts.slice(0, -1)) {
      parent += "/" + part;
      let stat; try { stat = lstatBytes(encodePath(parent)); } catch (e: any) { if (e.code === "ENOENT") { absent = true; break; } throw e; }
      if (!stat.isDirectory() || stat.isSymbolicLink()) throw new SyncFailure("source parent is not a real directory");
    }
    if (absent) continue;
    const source = Buffer.concat([Buffer.from(root + "/"), raw]);
    let stat; try { stat = lstatBytes(source); } catch (e: any) { if (e.code === "ENOENT") continue; throw e; }
    if (stat.isSymbolicLink()) manifest[name] = { link: decodePath(readlinkBytes(source)) };
    else if (stat.isFile()) manifest[name] = { sha256: fileHash(source), mode: stat.mode & 0o7777 };
    else throw new SyncFailure("unsupported source file");
    names.push(raw);
  }
  return { names, manifest };
}
let guestBundle: Promise<string> | undefined;
export function bundleGuest(): Promise<string> {
  return guestBundle ??= (async () => {
    const result = await Bun.build({ entrypoints: [join(import.meta.dir, "guest-source.ts")], target: "bun", format: "esm" });
    if (!result.success || result.outputs.length !== 1) throw new SyncFailure("cannot bundle the guest source action");
    return result.outputs[0].text();
  })();
}
export interface SyncOptions {
  cli?: string[]; ssh?: string; rsync?: string; sessionId?: string; cwd?: string; run?: Runner;
  remote?: (config: string, payload: Data) => ReturnType<Runner>;
  report?: (text: string) => void;
}
export async function sync(name: string, options: SyncOptions = {}): Promise<Data> {
  if (!validName(name)) throw new SyncFailure("--name must be a safe single segment (letters, digits, underscore, hyphen)");
  const run = options.run ?? runBounded, report = options.report ?? (s => { process.stderr.write(s); });
  const git = await run(["git", "rev-parse", "--show-toplevel"], { cwd: options.cwd });
  if (git.code || git.stdoutTruncated || git.timedOut || !git.streamCleanupVerified) throw new SyncFailure("run sync inside a Git worktree", git.code || 1);
  const rootBytes = git.stdout.subarray(0, git.stdout.length - (git.stdout.at(-1) === 10 ? 1 : 0));
  const root = decodePath(rootBytes);
  // Native subprocess argv cannot carry surrogateescape roots in Bun. Never substitute U+FFFD.
  if (!Buffer.from(root).equals(rootBytes)) throw new SyncFailure("worktree root contains unsupported byte path; use native rsync explicitly");
  const { names, manifest } = await selectedFiles(root, run);
  const info = await run([...(options.cli ?? ["agent-vm"]), "ssh-info", "--format", "ssh-config", ...(options.sessionId === undefined ? [] : ["--session-id", options.sessionId])]);
  if (info.code || info.stdoutTruncated || info.timedOut || !info.streamCleanupVerified) { report(info.stderr.toString()); throw new SyncFailure("cannot obtain an active VM SSH configuration", info.code || 1); }
  if (!info.stdout.toString().trim()) throw new SyncFailure("CLI returned empty SSH configuration");
  const tx = randomUUID().replaceAll("-", ""), base = { name, tx }, parent = `/workspace/worktrees/${name}`, stage = `${parent}/.agent-vm-src-stage.${tx}`;
  const temp = mkdtempSync(join(tmpdir(), "agent-vm-sync-")), config = join(temp, "ssh_config"), listing = join(temp, "files.nul");
  const ssh = options.ssh ?? "ssh";
  let prepared = false, committing = false;
  const remote = options.remote ?? (async (config: string, payload: Data) => {
    const bundle = await bundleGuest();
    const command = "command -v bun >/dev/null 2>&1 || { printf '%s\\n' 'guest Bun prerequisite missing' >&2; exit 127; }; command -v timeout >/dev/null 2>&1 || { printf '%s\\n' 'guest timeout prerequisite missing' >&2; exit 127; }; exec " + shellJoin(["timeout", "--signal=KILL", "180s", "bun", "-e", bundle]);
    return run([ssh, "-F", config, "-o", "BatchMode=yes", "agent-vm", command], { input: json(payload), timeoutMs: 240000 });
  });
  try {
    exclusive(config, info.stdout);
    exclusive(listing, Buffer.concat(names.flatMap(n => [n, Buffer.from([0])])));
    const begin = await remote(config, { ...base, action: "begin" });
    if (begin.code || begin.stdoutTruncated || begin.timedOut || !begin.streamCleanupVerified) { report(begin.stderr.toString()); throw new SyncFailure("remote preparation failed; existing source was retained", begin.code || 1); }
    prepared = true;
    const response = parseJson(begin.stdout.toString());
    if (response.stage !== stage || response.tx !== tx) throw new SyncFailure("remote returned an unexpected stage path");
    const transfer = await run([options.rsync ?? "rsync", "-a", "--from0", "--files-from=" + listing, "--relative", "-e", shellJoin([ssh, "-F", config, "-o", "BatchMode=yes"]), root + "/", "agent-vm:" + stage + "/"], { timeoutMs: 600000 });
    if (transfer.stderr.length) report(transfer.stderr.toString());
    if (transfer.code || transfer.timedOut || !transfer.streamCleanupVerified) throw new SyncFailure("transfer failed; existing source was retained", transfer.code || 1);
    committing = true;
    let commit;
    try { commit = await remote(config, { ...base, action: "commit", manifest }); }
    catch { throw new SyncFailure(`commit unverified; retain recovery record ${parent}/.agent-vm-sync-${tx}.json`, 75); }
    if (commit.code || commit.stdoutTruncated || commit.timedOut || !commit.streamCleanupVerified) { report(commit.stderr.toString()); throw new SyncFailure(`commit unverified (exit ${commit.code}); retain recovery record ${parent}/.agent-vm-sync-${tx}.json`, 75); }
    let ack;
    try { ack = parseJson(commit.stdout.toString()); } catch { throw new SyncFailure("commit acknowledgement unreadable; recovery material retained", 75); }
    if (ack?.committed !== true || ack.tx !== tx) throw new SyncFailure("commit acknowledgement is invalid; recovery material retained", 75);
    try {
      const finish = await remote(config, { ...base, action: "finish" });
      if (finish.code || finish.stdoutTruncated || finish.timedOut || !finish.streamCleanupVerified || parseJson(finish.stdout.toString())?.cleaned !== true) report("source committed; cleanup incomplete, recovery material retained\n");
    } catch { report("source committed; cleanup incomplete, recovery material retained\n"); }
    return { synced: true, target: `${parent}/src`, files: names.length };
  } finally {
    if (prepared && !committing) {
      try { const abort = await remote(config, { ...base, action: "abort" }); if (abort.code || abort.stdoutTruncated || abort.timedOut || !abort.streamCleanupVerified || parseJson(abort.stdout.toString())?.cleaned !== true) report("stage cleanup incomplete; recovery material retained for " + tx + "\n"); }
      catch { report("stage cleanup incomplete; recovery material retained for " + tx + "\n"); }
    }
    rmSync(temp, { recursive: true, force: true });
  }
}
export async function main(argv = process.argv.slice(2)): Promise<number> {
  try {
    const options: Record<string, string> = {};
    while (argv.length) { const key = argv.shift()!; if (!["--name", "--cli", "--session-id"].includes(key) || !argv.length) throw new SyncFailure("usage: sync_worktree.ts --name NAME [--cli PATH] [--session-id ID]", 2); options[key] = argv.shift()!; }
    const cli = options["--cli"];
    loadRelease(process.env.AGENT_VM_RELEASE_MANIFEST || join(import.meta.dir, "client-release.json"));
    const result = await sync(options["--name"], { cli: cli ? cli.endsWith(".ts") ? [process.execPath, cli] : [cli] : undefined, sessionId: options["--session-id"] });
    console.log(json(result)); return 0;
  } catch (e) { console.error(e instanceof Error ? e.message : String(e)); return e instanceof SyncFailure ? e.code : e instanceof CliError ? e.exitCode : 1; }
}
if (import.meta.main) process.exitCode = await main();
