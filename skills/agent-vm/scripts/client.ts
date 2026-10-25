import { closeSync, fchmodSync, fsyncSync, lstatSync, openSync, readFileSync, writeFileSync, constants as C } from "node:fs";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { isIPv4 } from "node:net";
import { randomUUID } from "node:crypto";
import { Data, fail, record, integer, json, parseJson, equal, sha256, readJson, durable, fsyncDir, privateDir, Runner, runBounded, shellJoin, inherited } from "./client-io.ts";
import { PROTOCOL, capability, loadRelease, responseMetadata } from "./client-release.ts";
import { nativeCapabilities } from "./sync-native.ts";

export const STDOUT_LIMIT = 2 * 1024 * 1024, STDERR_LIMIT = 64 * 1024;
export const timeoutSeconds = (action: string, fields: Data = {}): number => ["acquire", "restart"].includes(action) || action === "resize" && fields.allow_restart === true ? 600 : 240;
export function sessionId(explicit?: string, required = true, env = process.env): string | null {
  const value = explicit ?? env.AGENT_VM_SESSION_ID;
  if (value === undefined && !required) return null;
  if (!value) fail("missing_session", "AGENT_VM_SESSION_ID must be set before a remote operation");
  if (!/^[A-Za-z0-9_.:@/-]{1,160}$/.test(value)) fail("invalid_session", "AGENT_VM_SESSION_ID contains unsupported characters");
  return value;
}
const deterministic = new Set("artifact_checksum_mismatch artifact_missing artifact_mode_mismatch artifact_path_invalid artifact_unverified bundle_invalid cache_ineligible config_invalid config_missing compatibility_action_mismatch compatibility_invalid capacity_insufficient client_sidecar_invalid draining generation_mismatch invalid_request manifest_action_unsupported manifest_format_unsupported manifest_invalid manifest_missing manifest_path_invalid manifest_self_reference manifest_unverified operation_conflict protocol_mismatch protocol_unsupported request_schema_incompatible request_semantic_incompatible response_schema_incompatible response_semantic_incompatible resource_insufficient resource_pressure resource_budget_invalid resource_unverified lease_window_short task_missing task_not_active unknown_action".split(" "));
export function uncertain(error: Data, action: string): boolean {
  const d = error.detail;
  if (error.code === "sqlite_unavailable") return !record(d) || d.state_committed !== false;
  if (record(d) && typeof d.outcome_verified === "boolean") return !d.outcome_verified;
  if (["acquire_partial", "resize_partial", "restart_partial", "cleanup_incomplete", "operation_unverified"].includes(error.code)) return true;
  if (record(d) && ["phase", "operation_phase"].some(k => typeof d[k] === "string" && ["native-generation", "native-init", "native-config", "native-stop", "native-start", "guest-readiness", "guest-post-check", "access-exposure", "port-add", "port-remove", "cache-delete"].includes(d[k]))) return true;
  if (action === "port-add" && ["generation_mismatch", "task_not_active"].includes(error.code)) return true;
  return !deterministic.has(error.code);
}
export function remoteError(raw: Buffer, request: Data): Data | null {
  const matches: Data[] = [];
  for (const line of raw.toString("utf8").split(/\r?\n/)) {
    let v; try { v = parseJson(line); } catch { continue; }
    if (!record(v) || v.ok !== false || v.protocol !== PROTOCOL || ["action", "session_id", "generation", "operation_id"].some(k => k in request && (!(k in v) || !equal(v[k], request[k])))) continue;
    if (record(v.error) && typeof v.error.code === "string" && v.error.code && typeof v.error.message === "string" && v.error.message) matches.push(v.error);
  }
  return matches.length === 1 ? matches[0] : null;
}
export interface RequestOptions { session: string | null; generation?: number | bigint; write?: boolean; operationId?: string; fields?: Data; }
export class Client {
  stateRoot: string; configRoot: string; sidecar: string;
  constructor(public env: Record<string, string | undefined> = process.env, public run: Runner = runBounded, public diagnostics: (bytes: Buffer) => void = b => { process.stderr.write(b); }) {
    this.stateRoot = join(env.XDG_STATE_HOME ?? join(homedir(), ".local/state"), "agent-vm");
    this.configRoot = join(env.XDG_CONFIG_HOME ?? join(homedir(), ".config"), "agent-vm");
    this.sidecar = env.AGENT_VM_RELEASE_MANIFEST || join(import.meta.dir, "client-release.json");
  }
  sessionDir(session: string): string { return join(this.stateRoot, "sessions", sha256(session).slice(0, 24)); }
  operationPath(session: string, op: string): string { return join(this.sessionDir(session), "operations", sha256(op) + ".json"); }
  state(session: string): Data {
    let v;
    try { v = readJson(join(this.sessionDir(session), "state.json")); } catch (e: any) { fail(e.code === "ENOENT" ? "local_state_missing" : "local_state_invalid", "local session state cannot be read; run acquire first"); }
    if (!record(v)) fail("local_state_invalid", "local session state is not an object");
    return v;
  }
  private stateWriteAllowed(session: string, current: any, candidate: Data): boolean {
    if (!record(candidate) || candidate.session_id !== session ||
      typeof candidate.task_id !== "string" || !/^avm-[a-z0-9-]+$/.test(candidate.task_id) ||
      !integer(candidate.generation) || candidate.generation < 1 ||
      "cleanup_only" in candidate && typeof candidate.cleanup_only !== "boolean") return false;
    if (current === null) return true;
    if (!record(current) || current.session_id !== session || !integer(current.generation) || current.generation < 1 ||
      typeof current.task_id !== "string" || !/^avm-[a-z0-9-]+$/.test(current.task_id)) return false;
    if (current.generation > candidate.generation) return false;
    if (equal(current.generation, candidate.generation)) {
      if (current.task_id !== candidate.task_id) return false;
      if (current.cleanup_only === false && candidate.cleanup_only === true) return false;
    }
    return true;
  }
  saveState(session: string, state: Data): boolean {
    const directory = this.sessionDir(session);
    privateDir(directory);
    const lockPath = join(directory, ".state.lock");
    const native = nativeCapabilities();
    let fd = -1;
    let locked = false;
    try {
      fd = openSync(lockPath, C.O_RDWR | C.O_CREAT | C.O_NOFOLLOW, 0o600);
      fchmodSync(fd, 0o600);
      locked = native.lock(fd);
      if (!locked) return false;
      let current: any = null;
      try { current = readJson(join(directory, "state.json")); }
      catch (error: any) { if (error.code !== "ENOENT") return false; }
      if (!this.stateWriteAllowed(session, current, state)) return false;
      durable(join(directory, "state.json"), json(state) + "\n");
      return true;
    } catch {
      return false;
    } finally {
      if (locked) native.unlock(fd);
      if (fd !== -1) closeSync(fd);
      native.close();
    }
  }
  generation(session: string, allowCleanupOnly = false): number | bigint {
    const state = this.state(session);
    if (state.cleanup_only === true && !allowCleanupOnly)
      fail("lease_unavailable", "local recovery state is cleanup-only; release or destroy the session first");
    const n = state.generation;
    if (!integer(n) || n < 1) fail("local_state_invalid", "local session state has no valid generation");
    return n;
  }
  persistRecoveryState(session: string, privateKey: string, result: any, operationId: string, expectedGeneration: number | bigint): boolean {
    if (!record(result) || result.session_id !== session || result.state === "absent" ||
      !integer(expectedGeneration) || expectedGeneration < 0 ||
      typeof result.task_id !== "string" || !/^avm-[a-z0-9-]+$/.test(result.task_id) ||
      !integer(result.generation) || result.generation < 1) return false;
    const committed = result.recovery_operation_id === operationId;
    const lastError = record(result.last_error) ? result.last_error : null;
    const matchingError = lastError?.operation_id === operationId &&
      lastError.task_id === result.task_id && equal(lastError.task_generation, result.generation);
    const cancellation = Array.isArray(lastError?.pending_acquire_operation_ids) &&
      lastError.task_id === result.task_id && equal(lastError.task_generation, result.generation) &&
      lastError.pending_acquire_operation_ids.includes(operationId);
    if (!committed && result.partial_operation_id !== operationId && !matchingError && !cancellation) return false;
    const nextGeneration = typeof expectedGeneration === "bigint" ? expectedGeneration + 1n : expectedGeneration + 1;
    if (!equal(result.generation, expectedGeneration) && !equal(result.generation, nextGeneration)) return false;
    return this.saveState(session, {
      session_id: session, task_id: result.task_id, generation: result.generation,
      private_key: privateKey,
      cleanup_only: !(committed && result.state === "active" && result.effective_lease === true),
    });
  }
  async recoverAcquireState(session: string, privateKey: string, operationId: string, expectedGeneration: number | bigint): Promise<boolean> {
    try {
      const response = await this.request("status", { session, fields: { operation_id: operationId } });
      return this.persistRecoveryState(session, privateKey, response.result, operationId, expectedGeneration);
    } catch {
      return false;
    }
  }
  async reuse(path: string, request: Data): Promise<Data> {
    for (let attempt = 0; attempt < 50; attempt++) {
      let old;
      try { old = readJson(path); } catch { if (attempt < 49) { await Bun.sleep(10); continue; } fail("local_journal_invalid", "operation journal is unreadable"); }
      if (!record(old) || !record(old.request)) fail("local_journal_invalid", "operation journal request is invalid");
      const intent = (v: Data) => Object.fromEntries(Object.entries(v).filter(([k]) => k !== "compatibility"));
      if (!equal(intent(old.request), intent(request))) fail("operation_conflict", "operation ID already describes another request");
      return old.request;
    }
    fail("local_journal_invalid", "operation journal is unreadable");
  }
  finalize(path: string, request: Data, result: Data): void {
    try { durable(path, json({ request, ...result }) + "\n"); }
    catch { fail("local_journal_update_failed", "remote operation outcome is known but the local operation journal could not be finalized", { operation_id: request.operation_id }); }
  }
  async request(action: string, options: RequestOptions): Promise<Data> {
    const { session, generation, write = false, operationId, fields = {} } = options;
    if (write && session === null) fail("missing_session", "AGENT_VM_SESSION_ID is required");
    let request: Data = { protocol: PROTOCOL, action, ...(session === null ? {} : { session_id: session }), ...(generation === undefined ? {} : { generation }), ...(write ? { operation_id: operationId || randomUUID() } : {}), ...fields };
    const [metadata, warnings] = loadRelease(this.sidecar);
    request.compatibility = capability(action, metadata);
    let op: string | undefined;
    if (write) {
      op = this.operationPath(session!, request.operation_id); privateDir(dirname(op));
      let fd: number;
      try { fd = openSync(op, C.O_WRONLY | C.O_CREAT | C.O_EXCL | C.O_NOFOLLOW, 0o600); }
      catch (e: any) { if (e.code !== "EEXIST") throw e; request = await this.reuse(op, request); fd = -1; }
      if (fd !== -1) {
        try { fchmodSync(fd, 0o600); writeFileSync(fd, json({ request, outcome: "pending" })); fsyncSync(fd); } finally { closeSync(fd); }
        fsyncDir(dirname(op));
      }
    }
    const timeout = timeoutSeconds(action, fields);
    const transport = metadata.transport ?? "legacy-bun";
    const remoteCommand = transport === "go-request"
      ? shellJoin([this.env.AGENT_VM_REMOTE_REQUEST ?? "/opt/agent-vm-native/bin/agent-vm-control", "request", "--socket", this.env.AGENT_VM_REMOTE_SOCKET ?? "/run/agent-vm-native/control.sock", "--timeout", `${timeout}s`])
      : shellJoin(["bun", this.env.AGENT_VM_REMOTE_CONTROL ?? "/opt/agent-vm-native/agent_vm_control.ts"]);
    const result = await this.run(["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "--", this.env.AGENT_VM_CONTROL_HOST ?? "agent-vm-host", remoteCommand], { input: json(request), timeoutMs: timeout * 1000, stdoutLimit: STDOUT_LIMIT, stderrLimit: STDERR_LIMIT, env: this.env });
    const detail = { operation_id: request.operation_id ?? null, stream_cleanup_verified: result.streamCleanupVerified, ...(!result.streamCleanupVerified ? { stream_cleanup_error: "reader_cancelled_before_eof" } : {}) };
    if (result.timedOut) fail("remote_timeout", "control outcome is uncertain; retry the same operation ID", detail);
    if (result.stderr.length) this.diagnostics(result.stderr);
    if (!result.streamCleanupVerified) fail("remote_failed", "control response stream cleanup is not verified", detail, result.code || 1);
    if (result.code) {
      if (result.stdoutTruncated || result.stderrTruncated) fail("remote_failed", "remote request output was truncated", { ...detail, stdout_truncated: result.stdoutTruncated, stderr_truncated: result.stderrTruncated, stdout_limit_bytes: STDOUT_LIMIT, stderr_limit_bytes: STDERR_LIMIT }, result.code);
      const error = remoteError(result.stderr, request) ?? remoteError(result.stdout, request);
      if (error) { if (op && !uncertain(error, action)) this.finalize(op, request, { outcome: "error", error }); fail(error.code, error.message, error.detail, result.code); }
      fail("remote_failed", "remote request failed; control outcome is not verified", { operation_id: request.operation_id ?? null }, result.code);
    }
    if (result.stdoutTruncated) fail("invalid_remote_response", "control response exceeded the bounded stdout limit", { stdout_truncated: true, stdout_limit_bytes: STDOUT_LIMIT });
    let response;
    try { response = parseJson(new TextDecoder("utf-8", { fatal: true }).decode(result.stdout)); } catch { fail("invalid_remote_response", "control did not return JSON", detail); }
    if (!record(response) || response.protocol !== PROTOCOL || response.action !== action || (response.session_id ?? null) !== session) fail("protocol_mismatch", "response protocol or request identity differs");
    if ("generation" in request && !equal(response.generation, request.generation)) fail("protocol_mismatch", "response generation differs");
    if ("operation_id" in request && response.operation_id !== request.operation_id) fail("protocol_mismatch", "operation identity differs");
    responseMetadata(response, request, warnings);
    if (op) this.finalize(op, request, { outcome: "success", response });
    return response;
  }
  async key(session: string): Promise<[string, string]> {
    const directory = join(this.sessionDir(session), "keys"); privateDir(directory);
    const path = join(directory, "id_ed25519");
    if (![path, path + ".pub"].every(p => { try { return lstatSync(p).isFile(); } catch { return false; } })) {
      const r = await this.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path]);
      if (r.code) fail("keygen_failed", "ssh-keygen failed", r.stderr.toString());
    }
    const fd = openSync(path, C.O_RDONLY | C.O_NOFOLLOW); try { fchmodSync(fd, 0o600); } finally { closeSync(fd); }
    const pub = readFileSync(path + ".pub", "utf8").trim(); if (!pub) fail("key_invalid", "generated public key is empty");
    return [path, pub];
  }
  writeSSH(session: string, info: Data, key: string): [string, string] {
    if (typeof info.host !== "string" || !isIPv4(info.host) || !Number.isInteger(info.port) || info.port < 1024 || info.port > 65535 || typeof info.host_key_alias !== "string" || !/^agent-vm-[a-z0-9-]+-g[1-9][0-9]*$/.test(info.host_key_alias) || typeof info.task_id !== "string" || !/^avm-[a-z0-9-]+$/.test(info.task_id) || !integer(info.generation) || info.generation < 1) fail("invalid_remote_response", "invalid VM endpoint identity");
    if (typeof info.host_key !== "string" || !/^ssh-ed25519 [A-Za-z0-9+/=]+$/.test(info.host_key)) fail("invalid_host_key", "control returned an invalid trusted host key");
    const dir = join(this.configRoot, "sessions", sha256(session).slice(0, 24)); privateDir(dir);
    const stem = join(dir, `agent-vm-${info.task_id}-g${info.generation}`), config = stem + ".ssh", hosts = stem + ".known_hosts";
    durable(hosts, `${info.host_key_alias} ${info.host_key}\n`);
    durable(config, `Host agent-vm\n  HostName ${info.host}\n  Port ${info.port}\n  User root\n  HostKeyAlias ${info.host_key_alias}\n  StrictHostKeyChecking yes\n  IdentitiesOnly yes\n  IdentityFile ${sshConfigPath(key)}\n  UserKnownHostsFile ${sshConfigPath(hosts)}\n  ControlMaster no\n  ControlPath none\n`);
    return [config, hosts];
  }
  async sshInfo(session: string): Promise<[Data, Data, string]> {
    const state = this.state(session);
    if (state.cleanup_only === true) fail("lease_unavailable", "local recovery state is cleanup-only; release or destroy the session first");
    const response = await this.request("ssh-info", { session }), info = response.result;
    if (!equal(state.task_id, info.task_id) || !equal(state.generation, info.generation)) fail("generation_mismatch", "connection material belongs to another lease; acquire the current lease first");
    if (typeof state.private_key !== "string") fail("key_missing", "private key is missing");
    try { if (!lstatSync(state.private_key).isFile()) throw 0; } catch { fail("key_missing", "private key is missing"); }
    const [config, hosts] = this.writeSSH(session, info, state.private_key);
    Object.assign(info, { identity_file: state.private_key, known_hosts_file: hosts, ssh_config: config });
    return [response, info, config];
  }
}
export function sshConfigPath(path: string): string {
  const value = resolve(path); if (/[\n\r\0]/.test(value)) fail("invalid_path", "SSH configuration path contains a control character");
  return '"' + value.replaceAll("\\", "\\\\").replaceAll('"', '\\"').replaceAll("%", "%%") + '"';
}
export { inherited };
