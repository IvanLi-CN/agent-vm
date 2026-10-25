import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Client } from "../scripts/client.ts";
import { Data, json, parseJson, ProcessResult, Runner } from "../scripts/client-io.ts";

export function result(stdout: any = "", code = 0, stderr: any = "", rest: Partial<ProcessResult> = {}): ProcessResult {
  return { stdout: Buffer.from(typeof stdout === "string" || Buffer.isBuffer(stdout) ? stdout : json(stdout)), stderr: Buffer.from(typeof stderr === "string" || Buffer.isBuffer(stderr) ? stderr : json(stderr)), code, stdoutTruncated: false, stderrTruncated: false, timedOut: false, streamCleanupVerified: true, ...rest };
}
export function response(request: Data, extra: Data = {}): Data {
  return { ok: true, protocol: 4, action: request.action, session_id: request.session_id ?? null, generation: request.generation ?? null, operation_id: request.operation_id ?? null, server_time: "2026-10-25T00:00:00+00:00", outcome: "success", response: { schema: 1, semantic_revision: 1 }, result: {}, ...extra };
}
export class Fixture {
  root = realpathSync(mkdtempSync(join(tmpdir(), "agent-vm-test-")));
  env = { XDG_STATE_HOME: join(this.root, "state"), XDG_CONFIG_HOME: join(this.root, "config"), AGENT_VM_RELEASE_MANIFEST: join(this.root, "absent.json"), AGENT_VM_SESSION_ID: "environment" };
  requests: Data[] = []; commands: string[][] = []; diagnostics: Buffer[] = [];
  handler: (r: Data) => ProcessResult | Promise<ProcessResult> = r => result(response(r));
  runner: Runner = async (argv, o = {}) => { this.commands.push(argv); const request = parseJson(String(o.input)); this.requests.push(request); return this.handler(request); };
  client = new Client(this.env, this.runner, b => this.diagnostics.push(b));
  close(): void { rmSync(this.root, { recursive: true, force: true }); }
  key(): string { const path = join(this.root, "private-key"); writeFileSync(path, "private-fixture", { mode: 0o600 }); return path; }
  state(session = "explicit", generation: number | bigint = 7): void { this.client.saveState(session, { session_id: session, task_id: "avm-abc", generation, private_key: this.key() }); }
  ssh(generation: number | bigint = 7): Data { return { task_id: "avm-abc", generation, host: "192.0.2.10", port: 22000, host_key_alias: `agent-vm-avm-abc-g${generation}`, host_key: "ssh-ed25519 YWJj" }; }
}
