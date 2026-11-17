import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Client } from "../scripts/client.ts";
import { Data, json, parseJson, ProcessResult, Runner } from "../scripts/client-io.ts";
import { ACTIONS, CLIENT_DEPENDENCIES } from "../scripts/client-release.ts";
import { sha256 } from "../scripts/client-io.ts";

export function result(stdout: any = "", code = 0, stderr: any = "", rest: Partial<ProcessResult> = {}): ProcessResult {
  return { stdout: Buffer.from(typeof stdout === "string" || Buffer.isBuffer(stdout) ? stdout : json(stdout)), stderr: Buffer.from(typeof stderr === "string" || Buffer.isBuffer(stderr) ? stderr : json(stderr)), code, stdoutTruncated: false, stderrTruncated: false, timedOut: false, streamCleanupVerified: true, ...rest };
}
export function response(request: Data, extra: Data = {}): Data {
  const release = { release_id: "a".repeat(40), source_revision: "a".repeat(40), protocol: 4, version: "1.0.0", manifest_present: true, artifacts_verified: true };
  return { ok: true, protocol: 4, action: request.action, session_id: request.session_id ?? null, generation: request.generation ?? null, operation_id: request.operation_id ?? null, server_time: "2026-10-25T00:00:00+00:00", outcome: "success", response: { schema: 1, semantic_revision: 1 }, release, result: {}, ...extra };
}
export class Fixture {
  root = realpathSync(mkdtempSync(join(tmpdir(), "agent-vm-test-")));
  env = { XDG_STATE_HOME: join(this.root, "state"), XDG_CONFIG_HOME: join(this.root, "config"), AGENT_VM_RELEASE_MANIFEST: join(this.root, "client-release.json"), AGENT_VM_SESSION_ID: "environment" };
  requests: Data[] = []; commands: string[][] = []; diagnostics: Buffer[] = [];
  handler: (r: Data) => ProcessResult | Promise<ProcessResult> = r => result(response(r));
  runner: Runner = async (argv, o = {}) => { this.commands.push(argv); const request = parseJson(String(o.input)); this.requests.push(request); return this.handler(request); };
  client: Client;
  constructor() {
    mkdirSync(join(this.root, "config", "agent-vm"), { recursive: true });
    mkdirSync(join(this.root, "state"), { recursive: true });
    const revision = "a".repeat(40), schema = { schema: 1, semantic_revision: 1 }, window = { schema_min: 1, schema_max: 1, semantic_min: 1, semantic_max: 1 };
    const client = readFileSync(new URL("../scripts/agent_vm.ts", import.meta.url));
    writeFileSync(join(this.root, "agent_vm.ts"), client, { mode: 0o755 });
    const dependencies = CLIENT_DEPENDENCIES.map(([name, mode]) => { const bytes = readFileSync(new URL(`../scripts/${name}`, import.meta.url)); writeFileSync(join(this.root, name), bytes, { mode }); return { name, path: name, role: "support", mode, sha256: sha256(bytes) }; });
    writeFileSync(this.env.AGENT_VM_RELEASE_MANIFEST, json({ format: 4, version: "1.0.0", source_revision: revision, protocol: 4, client_artifact: { name: "agent_vm.ts", path: "agent_vm.ts", role: "client", mode: 0o755, sha256: sha256(client) }, client_dependencies: dependencies, client_view: { version: "1.0.0", source_revision: revision, protocol: 4, transport: "go-request", actions: Object.fromEntries(ACTIONS.map(action => [action, { request: schema, response: schema, client_to_control: window, control_to_client: window }])) } }), { mode: 0o600 });
    writeFileSync(join(this.root, "config", "agent-vm", "config.toml"), `default_host = "fixture"\n[hosts.fixture]\ntransport = "ssh"\ncontrol_host = "fixture-host"\nremote_request = "/opt/agent-vm-native/bin/agent-vm-server"\nremote_socket = "/run/agent-vm-native/control.sock"\n`, { mode: 0o600 });
    this.client = new Client(this.env, this.runner, b => this.diagnostics.push(b));
  }
  close(): void { rmSync(this.root, { recursive: true, force: true }); }
  key(): string { const path = join(this.root, "private-key"); writeFileSync(path, "private-fixture", { mode: 0o600 }); return path; }
  state(session = "explicit", generation: number | bigint = 7): void { this.client.saveState(session, { session_id: session, task_id: "avm-abc", generation, private_key: this.key() }); }
  ssh(generation: number | bigint = 7): Data { return { task_id: "avm-abc", generation, host: "192.0.2.10", port: 22000, host_key_alias: `agent-vm-avm-abc-g${generation}`, host_key: "ssh-ed25519 YWJj" }; }
}
