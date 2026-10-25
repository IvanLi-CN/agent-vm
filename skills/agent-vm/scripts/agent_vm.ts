#!/usr/bin/env bun
/** Protocol-4 request client. Control effects belong to the canonical Bun controller. */
import { readFileSync } from "node:fs";
import { resolve, join } from "node:path";
import { Client, inherited, sessionId } from "./client.ts";
import { CliError, Data, fail, integer, json, readJson, shellJoin } from "./client-io.ts";
import { PROTOCOL, VERSION, loadRelease } from "./client-release.ts";
import { sync, SyncFailure } from "./sync_worktree.ts";
import { validName } from "./guest-source.ts";

const HELP = `Usage: agent-vm [--session-id ID] COMMAND [OPTIONS]
Commands: version, help, doctor, status, acquire, renew, release, destroy, restart,
  resize, ssh-info, ssh, exec -- ARGV, sync --name NAME, upload SOURCE DESTINATION,
  download SOURCE DESTINATION, export PATH, port add|list|remove,
  admin status|metrics|session list|drain enter|status|exit|reconcile|gc.
Mutation options: --operation-id ID. Resource options: --cpu N --memory SIZE --disk SIZE.
Resize: --live (memory only) or --allow-restart. Use COMMAND --help for options.
`;
const paths: Record<string, string[]> = {
  version: [], help: [], doctor: [], status: [],
  acquire: ["ssh-host-port", "cpu", "memory", "disk", "operation-id"],
  renew: ["operation-id"], release: ["operation-id"], destroy: ["operation-id"], restart: ["operation-id"],
  resize: ["cpu", "memory", "disk", "live", "allow-restart", "operation-id"],
  "ssh-info": ["format"], ssh: [], exec: [], sync: ["name"], upload: [], download: [], export: [],
  "port add": ["guest-port", "protocol", "host-port", "operation-id"], "port list": [], "port remove": ["protocol", "operation-id"],
  "admin status": [], "admin metrics": [], "admin session list": [],
  "admin drain enter": [], "admin drain status": [], "admin drain exit": [],
  "admin reconcile": ["apply", "operation-id"], "admin gc": ["apply", "operation-id"],
};
export interface Args { command: string; options: Data; positional: string[]; rest: string[]; }
export function parseArgs(argv: string[]): Args {
  const words: string[] = [], options: Data = Object.create(null), rest: string[] = [];
  const flags = new Set(["live", "allow-restart", "apply"]);
  for (let i = 0; i < argv.length; i++) {
    const token = argv[i];
    if (token === "--") { rest.push(...argv.slice(i)); break; }
    if (token === "--version") return { command: "short-version", options, positional: [], rest };
    if (token === "--help" || token === "-h") return { command: "help", options, positional: [], rest };
    if (token.startsWith("--")) {
      const [key, ...equals] = token.slice(2).split("=");
      if (flags.has(key)) { if (equals.length) fail("invalid_request", `--${key} takes no value`); options[key] = true; }
      else { const value = equals.length ? equals.join("=") : argv[++i]; if (value === undefined || value.startsWith("--")) fail("invalid_request", `--${key} requires a value`); options[key] = value; }
    } else if (token.startsWith("-")) fail("invalid_request", `unsupported option: ${token}`);
    else words.push(token);
  }
  const match = Object.keys(paths).sort((a, b) => b.split(" ").length - a.split(" ").length).find(p => p.split(" ").every((word, i) => words[i] === word));
  if (!match) fail("unknown_command", "unknown or incomplete command");
  for (const key of Object.keys(options)) if (key !== "session-id" && !paths[match].includes(key)) fail("invalid_request", `unsupported option --${key} for ${match}`);
  const positional = words.slice(match.split(" ").length), count = ["upload", "download"].includes(match) ? 2 : ["export", "port add", "port remove"].includes(match) ? 1 : 0;
  if (!["ssh"].includes(match) && positional.length !== count) fail("invalid_request", "unexpected or missing positional arguments");
  if (match === "sync" && !options.name || match === "port add" && !options["guest-port"]) fail("invalid_request", "required command option is missing");
  if (options.live && options["allow-restart"]) fail("invalid_request", "--live and --allow-restart are mutually exclusive");
  if (options.format && !["json", "ssh-config"].includes(options.format) || options.protocol && !["tcp", "udp"].includes(options.protocol)) fail("invalid_request", "unsupported option value");
  for (const key of ["cpu", "ssh-host-port", "guest-port", "host-port"]) if (options[key] !== undefined) {
    if (!/^[+-]?\d+$/.test(options[key])) fail("invalid_request", `--${key} must be an integer`);
    const n = BigInt(options[key]); options[key] = n <= BigInt(Number.MAX_SAFE_INTEGER) && n >= BigInt(Number.MIN_SAFE_INTEGER) ? Number(n) : n;
  }
  if (rest.length && !["exec", "ssh"].includes(match)) fail("invalid_request", "unexpected command remainder");
  return { command: match, options, positional, rest };
}
export function parseSize(value: string, memory: boolean): number | bigint {
  const m = (memory ? /^(\d+)(MiB|GiB|M|G)$/i : /^(\d+)(GiB|G)$/i).exec(value);
  if (!m) fail("invalid_size", memory ? "memory must use MiB, GiB, M or G" : "disk must use GiB or G");
  const n = BigInt(m[1]) * (memory && /^g/i.test(m[2]) ? 1024n : 1n);
  return n <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(n) : n;
}
export async function dispatch(args: Args, client = new Client(), native = inherited): Promise<Data | string | number> {
  const { command: cmd, options: o, positional: pos, rest } = args;
  if (cmd === "help") return HELP;
  if (cmd === "short-version") return `agent-vm ${VERSION}\n`;
  if (cmd === "version") { const [release, warnings] = loadRelease(client.sidecar); return { version: VERSION, protocol: PROTOCOL, release, warnings }; }
  if (cmd === "admin metrics") { if (o["session-id"] !== undefined) fail("invalid_request", "admin metrics does not accept --session-id"); return client.request("admin-metrics", { session: null }); }
  const session = sessionId(o["session-id"], !["doctor", "admin metrics"].includes(cmd), client.env);
  const mutate = async (action: string, fields: Data = {}, generation?: number | bigint): Promise<Data> => client.request(action, { session, generation: generation ?? (["acquire", "drain-enter", "drain-exit", "admin-reconcile", "admin-gc"].includes(action) ? 0 : client.generation(session!)), write: true, operationId: o["operation-id"], fields });
  const resources = (): Data => ({ ...(o.cpu === undefined ? {} : { cpu: o.cpu }), ...(o.memory === undefined ? {} : { memory_mib: parseSize(o.memory, true) }), ...(o.disk === undefined ? {} : { disk_gib: parseSize(o.disk, false) }) });
  if (["doctor", "status"].includes(cmd)) return client.request(cmd, { session });
  if (cmd === "acquire") {
    const [key, publicKey] = await client.key(session!);
    let generation = (await client.request("status", { session })).result.generation;
    if (!integer(generation) || generation < 0) fail("invalid_remote_response", "status has no valid generation");
    if (o["operation-id"]) {
      try { generation = readJson(client.operationPath(session!, o["operation-id"])).request.generation; }
      catch (e: any) { if (e.code !== "ENOENT") fail("local_journal_invalid", "operation journal is unreadable"); }
    }
    const response = await mutate(cmd, { public_key: publicKey, ...resources(), ...(o["ssh-host-port"] === undefined ? {} : { ssh_host_port: o["ssh-host-port"] }) }, generation);
    const current = "current_state" in response ? response.current_state : response.result;
    if (!current?.effective_lease) fail("lease_unavailable", "replayed allocation no longer has an active lease; inspect status");
    client.saveState(session!, { session_id: session, task_id: current.task_id, generation: current.generation, private_key: key });
    return response;
  }
  if (["renew", "release", "destroy", "restart"].includes(cmd)) return mutate(cmd);
  if (cmd === "resize") {
    if (o.live && (o.memory === undefined || o.cpu !== undefined || o.disk !== undefined)) fail("invalid_request", "--live requires only --memory; CPU/disk changes use --allow-restart");
    return mutate(cmd, { allow_restart: !!o["allow-restart"], live_memory: !!o.live, ...resources() });
  }
  if (cmd.startsWith("port ")) {
    const action = cmd.slice(5), generation = client.generation(session!);
    if (action === "list") return client.request("port-list", { session, generation });
    return mutate("port-" + action, { name: pos[0], port_protocol: o.protocol ?? "tcp", ...(action === "add" ? { vm_port: o["guest-port"], ...(o["host-port"] === undefined ? {} : { host_port: o["host-port"] }) } : {}) }, generation);
  }
  if (cmd.startsWith("admin ")) {
    const action = cmd.slice(6);
    if (action === "status" || action === "session list") return client.request(action === "status" ? "admin-status" : "admin-session-list", { session });
    if (action.startsWith("drain ")) { const drain = action.slice(6); return drain === "status" ? client.request("drain-status", { session }) : mutate("drain-" + drain, {}, 0); }
    return mutate("admin-" + action, { apply: !!o.apply }, 0);
  }
  if (cmd === "sync") {
    if (!validName(o.name)) fail("invalid_name", "--name must be one safe path segment");
    return sync(o.name, { cli: [process.execPath, join(import.meta.dir, "agent_vm.ts")], sessionId: session! });
  }
  if (cmd === "exec" && (rest[0] !== "--" || rest.length < 2 || rest.some(v => v.includes("\0")))) fail("invalid_request", "exec requires -- followed by a program and its arguments");
  if (["upload", "download", "export"].includes(cmd)) {
    const remote = cmd === "upload" ? pos[1] : pos[0];
    if (!remote.startsWith("/") || remote.includes("\0")) fail("invalid_path", "remote path must be absolute");
  }
  const [response, , config] = await client.sshInfo(session!);
  if (cmd === "ssh-info") return o.format === "ssh-config" ? readFileSync(config, "utf8") : response;
  if (cmd === "ssh") return native(["ssh", "-F", config, "agent-vm", ...pos, ...rest.slice(rest[0] === "--" ? 1 : 0)]);
  if (cmd === "exec") return native(["ssh", "-F", config, "-o", "BatchMode=yes", "--", "agent-vm", shellJoin(rest.slice(1))]);
  if (cmd === "export") return native(["ssh", "-F", config, "-o", "BatchMode=yes", "--", "agent-vm", shellJoin(["tar", "-C", "/", "-cf", "-", "--", pos[0].replace(/^\/+/, "") || "."])]);
  if (cmd === "upload" || cmd === "download") {
    const local = resolve(cmd === "upload" ? pos[0] : pos[1]), remote = "agent-vm:" + (cmd === "upload" ? pos[1] : pos[0]);
    return native(["scp", "-F", config, "-o", "BatchMode=yes", "--", ...(cmd === "upload" ? [local, remote] : [remote, local])]);
  }
  fail("unknown_command", "unknown command");
}
export async function main(argv = process.argv.slice(2)): Promise<number> {
  try { const result = await dispatch(parseArgs(argv)); if (typeof result === "number") return result; process.stdout.write(typeof result === "string" ? result : json(result) + "\n"); return 0; }
  catch (e) {
    const error = e instanceof CliError ? e : new CliError("client_failed", e instanceof Error ? e.message : String(e), undefined, e instanceof SyncFailure ? e.code : 3);
    process.stderr.write(json({ ok: false, error: error.asDict() }) + "\n"); return error.exitCode;
  }
}
if (import.meta.main) process.exitCode = await main();
