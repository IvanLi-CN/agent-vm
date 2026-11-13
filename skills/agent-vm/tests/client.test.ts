import { afterEach, expect, test } from "bun:test";
import { chmodSync, lstatSync, mkdirSync, readFileSync, readdirSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { parseArgs, parseSize, dispatch } from "../scripts/agent_vm.ts";
import { attachDiagnosis, sessionId, uncertain } from "../scripts/client.ts";
import { CliError, equal, FloatToken, json, parseJson, readJson, sha256 } from "../scripts/client-io.ts";
import { ACTIONS, CLIENT_DEPENDENCIES, capability, loadRelease } from "../scripts/client-release.ts";
import { Fixture, response, result } from "./support.ts";

let fixtures: Fixture[] = [];
const fixture = () => { const f = new Fixture(); fixtures.push(f); return f; };
afterEach(() => { fixtures.forEach(f => f.close()); fixtures = []; });
const selected = (args: string[]) => parseArgs(["--session-id", "explicit", ...args]);

test("session precedence at every CLI nesting depth; exec remainder is opaque", () => {
  for (const args of [["--session-id", "before", "status"], ["status", "--session-id", "after"], ["admin", "--session-id", "nested", "session", "list"], ["admin", "drain", "status", "--session-id", "deep"]]) expect(sessionId(parseArgs(args).options["session-id"], true, { AGENT_VM_SESSION_ID: "env" })).not.toBe("env");
  expect(sessionId(undefined, true, { AGENT_VM_SESSION_ID: "env" })).toBe("env");
  expect(parseArgs(["exec", "--session-id", "explicit", "--", "printf", "--session-id", "guest"]).rest).toEqual(["--", "printf", "--session-id", "guest"]);
  expect(() => sessionId("", true, { AGENT_VM_SESSION_ID: "env" })).toThrow("must be set");
  expect(() => sessionId("bad\nidentity")).toThrow("unsupported");
});

test("missing identity and malformed CLI fail before keys, journals or remote writes", async () => {
  const f = fixture(); delete (f.env as any).AGENT_VM_SESSION_ID;
  await expect(dispatch(parseArgs(["acquire"]), f.client)).rejects.toMatchObject({ code: "missing_session" });
  await expect(f.client.request("acquire", { session: null, write: true })).rejects.toMatchObject({ code: "missing_session" });
  expect(f.commands).toEqual([]);
  expect(readdirSync(f.root)).toEqual([]);
  for (const argv of [["update"], ["port"], ["resize", "--live", "--allow-restart"], ["status", "--wat", "x"], ["status", "--__proto__", "x"], ["sync"], ["upload", "one"], ["port", "add", "web", "--guest-port", "NaN"]]) expect(() => parseArgs(argv)).toThrow();
  await expect(dispatch(selected(["sync", "--name", "../escape"]), f.client)).rejects.toMatchObject({ code: "invalid_name" });
});

test("scoped reconciliation pins identity and dry run has no operation intent or acquire", async () => {
  const f = fixture();
  const args = ["admin", "reconcile-session", "--task-id", "avm-target", "--generation", "2", "--pending-operation-id", "pending-target"];
  await dispatch(selected(args), f.client);
  expect(f.requests[0]).toMatchObject({ action: "admin-reconcile-session", session_id: "explicit", task_id: "avm-target", generation: 2, pending_operation_id: "pending-target", apply: false });
  expect(f.requests[0].operation_id).toBeUndefined();
  await dispatch(selected([...args, "--apply", "--operation-id", "precise-recovery"]), f.client);
  expect(f.requests[1]).toMatchObject({ action: "admin-reconcile-session", generation: 2, apply: true, operation_id: "precise-recovery" });
  expect(f.requests.map(r => r.action)).toEqual(["admin-reconcile-session", "admin-reconcile-session"]);
  await expect(dispatch(selected(["admin", "reconcile-session", "--apply"]), f.client)).rejects.toMatchObject({ code: "invalid_request" });
  expect(f.requests).toHaveLength(2);
});

test("global reconcile and gc previews are read-only while apply journals", async () => {
  const f = fixture(); f.state();
  await dispatch(selected(["admin", "reconcile"]), f.client);
  await dispatch(selected(["admin", "gc"]), f.client);
  expect(f.requests.slice(0, 2)).toMatchObject([{ action: "admin-reconcile", apply: false }, { action: "admin-gc", apply: false }]);
  expect(f.requests[0].operation_id).toBeUndefined();
  expect(f.requests[1].operation_id).toBeUndefined();
  expect(() => readdirSync(join(f.client.sessionDir("explicit"), "operations"))).toThrow();
  await dispatch(selected(["admin", "reconcile", "--apply", "--operation-id", "global-recovery"]), f.client);
  expect(f.requests[2]).toMatchObject({ action: "admin-reconcile", apply: true, operation_id: "global-recovery" });
  expect(readJson(f.client.operationPath("explicit", "global-recovery")).outcome).toBe("success");
});

test("remote matching error envelope is rendered once and keeps non-JSON diagnostics", async () => {
  const f = fixture(); f.state();
  f.handler = r => r.action === "renew" ? result("", 2, json({ ...r, ok: false, error: { code: "draining", message: "drain" } }) + "\nwarning: bounded peer diagnostic\n") : result(response(r));
  await expect(dispatch(selected(["renew"]), f.client)).rejects.toMatchObject({ code: "draining", detail: { operation_id: expect.any(String) } });
  expect(f.diagnostics).toEqual([Buffer.from("warning: bounded peer diagnostic\n")]);
});

test("mutation failures receive bounded status diagnosis through the shared path", async () => {
  const f = fixture(); f.state();
  f.handler = r => r.action === "renew"
    ? result("", 2, { ...r, ok: false, error: { code: "native_unavailable", message: "native status unavailable" } })
    : result(response(r, { result: { session_id: "explicit", task_id: "avm-abc", generation: 7, state: "active", effective_lease: true, native_status: "unknown", verified: false } }));
  await expect(dispatch(selected(["renew"]), f.client)).rejects.toMatchObject({ code: "native_unavailable", detail: { diagnosis_completeness: "full", status: { task_id: "avm-abc", native_status: "unknown", verified: false } } });
  expect(f.requests.map(r => r.action)).toEqual(["renew", "status"]);
});

test("acquire startup recovery adds bounded status diagnosis and preserves pending safety", async () => {
  const f = fixture();
  f.client.key = async () => [f.key(), "ssh-ed25519 YWJj fixture"];
  f.handler = r => {
    if (r.action === "status") return result(response(r, { result: { session_id: "explicit", task_id: "avm-target", generation: 3, state: "preparing", effective_lease: false, lease_expires_at: "2026-11-13T01:00:00Z", native_status: "unknown", guest_available: null, verified: false } }));
    if (r.action === "acquire") return result({ ...r, ok: false, error: { code: "startup_reconcile_required", message: "pending operation recovery remains uncertain", detail: { operation_id: r.operation_id, outcome_verified: false } } }, 2);
    return result(response(r));
  };
  await expect(dispatch(selected(["acquire"]), f.client)).rejects.toMatchObject({ code: "startup_reconcile_required", detail: { diagnosis_completeness: "full", status: { task_id: "avm-target", state: "preparing" }, next_action: expect.stringContaining("reconcile") } });
  expect(f.requests.map(r => r.action)).toEqual(["status", "acquire", "status"]);
  expect(f.requests.filter(r => r.action === "admin-reconcile-session")).toHaveLength(0);
});

test("explain adds bounded metrics and read-only recovery preview", async () => {
  const f = fixture();
  f.client.key = async () => [f.key(), "ssh-ed25519 YWJj fixture"];
  f.handler = r => {
    if (r.action === "status") return result(response(r, { result: { session_id: "explicit", task_id: "avm-target", generation: 3, state: "preparing", effective_lease: false, verified: false } }));
    if (r.action === "acquire") return result({ ...r, ok: false, error: { code: "startup_reconcile_required", message: "recovery required", detail: { operation_id: r.operation_id, outcome_verified: false } } }, 2);
    if (r.action === "admin-metrics") return result(response(r, { result: { admission_ready: false, pending_operations: [{ operation_id: "redact-me" }], warnings: ["bounded"] } }));
    if (r.action === "admin-reconcile-session") return result(response(r, { result: { dry_run: true, action: "close-expired-and-retain-cache" } }));
    return result(response(r));
  };
  await expect(dispatch(selected(["acquire", "--explain"]), f.client)).rejects.toMatchObject({ detail: { metrics_summary: { admission_ready: false, pending_operations: { item_count: 1 } }, recovery_preview: { dry_run: true } } });
  expect(f.requests.map(r => r.action)).toEqual(["status", "acquire", "status", "admin-metrics", "admin-reconcile-session"]);
  expect(f.requests.find(r => r.action === "admin-reconcile-session")?.apply).toBe(false);
});

test("legacy peer diagnosis is explicitly partial and never claims recovery", async () => {
  const f = fixture();
  f.handler = r => r.action === "status" ? result(response(r, { result: {} })) : result({ ...r, ok: false, error: { code: "startup_reconcile_required", message: "recovery required", detail: { outcome_verified: false } } }, 2);
  await expect(attachDiagnosis(new CliError("startup_reconcile_required", "recovery required"), f.client, "explicit", "pending", false)).resolves.toMatchObject({ detail: { diagnosis_completeness: "partial", status: {} } });
});

test("diagnosis bounds non-object details and remote messages", async () => {
  const f = fixture();
  f.handler = r => r.action === "status" ? result(response(r, { result: {} })) : result({ ...r, ok: false, error: { code: "native_unavailable", message: "m".repeat(2048), detail: "d".repeat(2048) } }, 2);
  const error = await attachDiagnosis(new CliError("native_unavailable", "m".repeat(2048), ["raw", { nested: "value" }]), f.client, "explicit", undefined, false);
  expect(error.message.length).toBeLessThanOrEqual(513);
  expect(error.detail).toMatchObject({ original_error_detail: ["raw", { nested: "value" }] });
  expect(JSON.stringify(error.detail).length).toBeLessThan(5000);
});

test("stale lease diagnosis retains authoritative blocker and bounded status", async () => {
  const f = fixture();
  f.handler = r => r.action === "status" ? result(response(r, { result: { task_id: "avm-target", generation: 2, state: "active", lease_expires_at: "2026-11-13T01:00:00Z", effective_lease: true } })) : result({ ...r, ok: false, error: { code: "stale_target", message: "reconciliation refuses an unexpired lease", detail: { diagnosis: { recovery_allowed: false, recovery_blocker: "lease_unexpired", lease_expires_at: "2026-11-13T01:00:00Z" } } } }, 2);
  await expect(dispatch(selected(["admin", "reconcile-session", "--task-id", "avm-target", "--generation", "2", "--pending-operation-id", "pending"]), f.client)).rejects.toMatchObject({ code: "stale_target", detail: { diagnosis: { recovery_allowed: false, recovery_blocker: "lease_unexpired" }, diagnosis_completeness: "full" } });
});

test("resource canonicalization, live fencing and native command selection", async () => {
  expect(parseSize("12GiB", true)).toBe(12288); expect(parseSize("512M", true)).toBe(512); expect(parseSize("40G", false)).toBe(40);
  expect(() => parseSize("12GB", true)).toThrow(); expect(parseSize("9007199254740993GiB", false)).toBe(9007199254740993n);
  const f = fixture(); f.state();
  for (const argv of [["resize", "--live", "--cpu", "2"], ["resize", "--live", "--memory", "4G", "--disk", "40G"]]) await expect(dispatch(selected(argv), f.client)).rejects.toMatchObject({ code: "invalid_request" });
  expect(f.commands).toHaveLength(0);
  await dispatch(selected(["resize", "--live", "--memory", "4G", "--operation-id", "resize"]), f.client);
  expect(f.requests[0]).toMatchObject({ action: "resize", generation: 7, session_id: "explicit", memory_mib: 4096, live_memory: true, allow_restart: false, operation_id: "resize" });
});

test("old state namespaces, exact large generations, operation journal retry and conflict", async () => {
  const f = fixture(), generation = 9007199254740993n;
  f.state("explicit", generation);
  expect(f.client.generation("explicit")).toBe(generation);
  expect(f.client.sessionDir("explicit")).toEndWith(sha256("explicit").slice(0, 24));
  const op = f.client.operationPath("explicit", "retry"); mkdirSync(join(f.client.sessionDir("explicit"), "operations"));
  const old = { protocol: 4, action: "renew", generation, session_id: "explicit", operation_id: "retry", compatibility: { ...capability("renew", { actions: {} }), release_id: "a".repeat(40) } };
  writeFileSync(op, json({ request: old, outcome: "pending" }));
  await f.client.request("renew", { session: "explicit", generation, operationId: "retry", write: true });
  expect(f.requests[0]).toEqual(old); expect(readJson(op).outcome).toBe("success"); expect(lstatSync(op).mode & 0o777).toBe(0o600);
  await expect(f.client.request("release", { session: "explicit", generation, operationId: "retry", write: true })).rejects.toMatchObject({ code: "operation_conflict" });
  expect(f.commands).toHaveLength(1);
});

test("exclusive journal creators reuse the same request without truncating pending intent", async () => {
  const f = fixture();
  await Promise.all([1, 2].map(() => f.client.request("acquire", { session: "s", generation: 0, operationId: "same", write: true })));
  expect(f.requests).toHaveLength(2); expect(f.requests[0]).toEqual(f.requests[1]);
  expect(readJson(f.client.operationPath("s", "same")).outcome).toBe("success");
});

test("strict JSON numeric identity, unicode, escaped strings and prototype keys", () => {
  const v = parseJson('{"generation":9007199254740993,"float":1.0,"str":"\\u005f_agent_vm_number__0","__proto__":{"safe":true},"x":"a\\\"b"}');
  expect(v.generation).toBe(9007199254740993n); expect(v.float).toBeInstanceOf(FloatToken); expect(equal(v.float, 1)).toBe(false); expect(v.str).toBe("__agent_vm_number__0"); expect(Object.hasOwn(v, "__proto__")).toBe(true); expect(({} as any).safe).toBeUndefined();
  expect(equal(parseJson(json(v)), v)).toBe(true);
  for (const invalid of ['{"x":01}', '{"x":1,}', '1 2', '[1,]', 'NaN']) expect(() => parseJson(invalid)).toThrow();
  expect(capability("__proto__", { actions: {}, manifest_present: false })).toMatchObject({ action: "__proto__", legacy: true, request: { schema: 1 } });
});

for (const [label, patch, code] of [
  ["wrong generation", { generation: true }, "protocol_mismatch"], ["wrong session", { session_id: "other" }, "protocol_mismatch"], ["missing envelope", { ok: false }, "invalid_remote_response"], ["array outcome", { outcome: [] }, "invalid_remote_response"], ["malformed warning", { warnings: "oops" }, "invalid_remote_response"], ["other warning action", { warnings: [{ code: "x", message: "x", action: "other" }] }, "invalid_remote_response"], ["schema drift", { response: { schema: 2, semantic_revision: 1 } }, "response_schema_incompatible"], ["semantic drift", { response: { schema: 1, semantic_revision: 2 } }, "response_semantic_incompatible"], ["release mismatch", { release: { release_id: "a".repeat(40), source_revision: "b".repeat(40), protocol: 4 } }, "invalid_remote_response"],
] as const) test(`success envelope rejects ${label} and leaves intent pending`, async () => {
  const f = fixture(); f.handler = r => result(response(r, patch));
  await expect(f.client.request("renew", { session: "s", generation: 1, operationId: "test", write: true })).rejects.toMatchObject({ code });
  expect(readJson(f.client.operationPath("s", "test")).outcome).toBe("pending");
});

for (const [label, change, outcome] of [
  ["verified preflight", {}, "error"], ["maintenance admission fence", { error: { code: "maintenance_unhealthy", message: "maintenance" } }, "error"], ["unrelated", { session_id: "other" }, "pending"], ["bool generation", { generation: true }, "pending"], ["fractional generation", { generation: new FloatToken("1.0") }, "pending"], ["unknown code", { error: { code: "internal_error", message: "unknown" } }, "pending"], ["native phase", { error: { code: "generation_mismatch", message: "native phase", detail: { phase: "native-start" } } }, "pending"],
] as const) test(`error envelope ${label} preserves journal outcome and exact exit`, async () => {
  const f = fixture(); f.handler = r => result("", 23, { ...r, ok: false, error: { code: "resource_pressure", message: "preflight" }, ...change });
  await expect(f.client.request("renew", { session: "s", generation: 1, operationId: "test", write: true })).rejects.toMatchObject({ exitCode: 23 });
  expect(readJson(f.client.operationPath("s", "test")).outcome).toBe(outcome);
});

test("duplicate errors, huge diagnostics, timeout and lost streams never finalize journals", async () => {
  const f = fixture();
  for (const [i, handler] of [
    (r: any) => { const err = json({ ...r, ok: false, error: { code: "draining", message: "drain" } }); return result("", 2, err + "\n" + err); },
    (r: any) => { const err = json({ ...r, ok: false, error: { code: "draining", message: "drain" } }); return result(err, 2, err); },
    () => result("", 255, "x", { stderrTruncated: true }),
    () => result("", 0, "", { timedOut: true, streamCleanupVerified: false }),
    () => result("", 0, "", { streamCleanupVerified: false }),
    () => result("{", 0, "", { stdoutTruncated: true }),
  ].entries()) {
    f.handler = handler; await expect(f.client.request("release", { session: "s", generation: 1, operationId: String(i), write: true })).rejects.toBeInstanceOf(CliError);
    expect(readJson(f.client.operationPath("s", String(i))).outcome).toBe("pending");
  }
});

test("legacy responses have explicit warnings; deterministic and sqlite failures are classified", async () => {
  const f = fixture(); f.handler = r => { const v = response(r); delete v.outcome; delete v.response; return result(v); };
  const v = await f.client.request("status", { session: "s" });
  expect(v.warnings.map((v: any) => v.code)).toEqual(expect.arrayContaining(["legacy_compatibility", "legacy_response_outcome", "legacy_response_compatibility", "legacy_control_release"]));
  expect(uncertain({ code: "generation_mismatch" }, "port-add")).toBe(true);
  expect(uncertain({ code: "generation_mismatch", detail: { outcome_verified: true } }, "port-add")).toBe(false);
  expect(uncertain({ code: "sqlite_unavailable", detail: { state_committed: null } }, "renew")).toBe(true);
  expect(uncertain({ code: "sqlite_unavailable", detail: { state_committed: false } }, "renew")).toBe(false);
  expect(uncertain({ code: "command_failed", detail: { phase: [] } }, "renew")).toBe(true);
});

test("sidecar shape, complete actions, exact client artifact, checksum and mode fail closed", async () => {
  const f = fixture(), client = join(f.root, "agent_vm.ts"), path = join(f.root, "client-release.json");
  writeFileSync(client, "client", { mode: 0o755 });
  const schema = { schema: 1, semantic_revision: 1 }, window = { schema_min: 1, schema_max: 1, semantic_min: 1, semantic_max: 1 };
  const dependencies = CLIENT_DEPENDENCIES.map(([name, mode]) => { writeFileSync(join(f.root, name), name, { mode }); return { name, path: name, role: "support", mode, sha256: sha256(name) }; });
  const sidecar = { format: 1, protocol: 4, release_id: "a".repeat(40), source_revision: "a".repeat(40), client_artifact: { name: "agent_vm.ts", path: "agent_vm.ts", role: "client", mode: 0o755, sha256: sha256("client") }, client_dependencies: dependencies, client_view: { release_id: "a".repeat(40), source_revision: "a".repeat(40), protocol: 4, actions: Object.fromEntries(ACTIONS.map(a => [a, { request: schema, response: schema, client_to_control: window, control_to_client: window }])) } };
  writeFileSync(path, json(sidecar)); expect(loadRelease(path)[0].artifacts_verified).toBe(true);
  for (const change of [(v: any) => { delete v.client_view.actions.status; }, (v: any) => { v.client_artifact.path = "../agent_vm.ts"; }, (v: any) => { v.client_artifact.sha256 = "0".repeat(64); }, (v: any) => { v.client_view.actions.status.request.schema = 2; },
    (v: any) => { delete v.client_dependencies; v.client_artifact.name = v.client_artifact.path = "agent_vm.py"; },
    (v: any) => { v.client_dependencies.pop(); }, (v: any) => { v.client_dependencies[0] = v.client_dependencies[1]; },
    (v: any) => { v.client_dependencies[0].path = "../escape.ts"; }, (v: any) => { v.client_dependencies[0].mode = 0o777; },
    (v: any) => { v.client_dependencies[0].sha256 = "0".repeat(64); }]) {
    const copy = structuredClone(sidecar); change(copy); writeFileSync(path, json(copy)); f.client.sidecar = path;
    await expect(f.client.request("status", { session: "s" })).rejects.toBeInstanceOf(CliError); expect(f.commands).toHaveLength(0);
  }
  writeFileSync(path, json(sidecar)); chmodSync(client, 0o644); expect(() => loadRelease(path)).toThrow("mode differs"); chmodSync(client, 0o755);
  writeFileSync(join(f.root, "guest-source.ts"), "tampered"); expect(() => loadRelease(path)).toThrow("checksum differs");
});

test("format-2 client view selects the fixed Go request adapter", async () => {
  const f = fixture(), client = join(f.root, "agent_vm.ts"), path = join(f.root, "client-release.json");
  writeFileSync(client, "client", { mode: 0o755 });
  const schema = { schema: 1, semantic_revision: 1 }, window = { schema_min: 1, schema_max: 1, semantic_min: 1, semantic_max: 1 };
  const dependencies = CLIENT_DEPENDENCIES.map(([name, mode]) => { writeFileSync(join(f.root, name), name, { mode }); return { name, path: name, role: "support", mode, sha256: sha256(name) }; });
  const sidecar = { format: 2, protocol: 4, release_id: "b".repeat(40), source_revision: "b".repeat(40), client_artifact: { name: "agent_vm.ts", path: "agent_vm.ts", role: "client", mode: 0o755, sha256: sha256("client") }, client_dependencies: dependencies, client_view: { release_id: "b".repeat(40), source_revision: "b".repeat(40), protocol: 4, transport: "go-request", actions: Object.fromEntries(ACTIONS.map(a => [a, { request: schema, response: schema, client_to_control: window, control_to_client: window }])) } };
  writeFileSync(path, json(sidecar)); f.client.sidecar = path;
  await f.client.request("status", { session: "s" });
  expect(f.commands[0]).toEqual(["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "--", "agent-vm-host", "/opt/agent-vm-native/bin/agent-vm-control request --socket /run/agent-vm-native/control.sock --timeout 240s"]);
});

test("pending complete-package installation fails before legacy fallback, SSH or journal writes", async () => {
  const f = fixture();
  f.client.sidecar = join(f.root, "client-release.json");
  writeFileSync(join(f.root, ".agent-vm-client-install.json"), "{}", { mode: 0o600 });
  await expect(f.client.request("acquire", { session: "s", write: true })).rejects.toMatchObject({ code: "local_client_install_pending" });
  expect(f.commands).toEqual([]);
  expect(readdirSync(f.root)).toEqual([".agent-vm-client-install.json"]);
});

test("generation-bound SSH material is strict, private, atomic and rejects stale identity", async () => {
  const f = fixture(); f.state(); f.handler = r => result(response(r, { result: f.ssh() }));
  const [, info, config] = await f.client.sshInfo("explicit");
  const text = readFileSync(config, "utf8");
  expect(text).toContain("StrictHostKeyChecking yes"); expect(text).toContain("IdentitiesOnly yes"); expect(text).toContain("HostKeyAlias agent-vm-avm-abc-g7");
  for (const p of [config, info.known_hosts_file, join(f.client.sessionDir("explicit"), "state.json")]) expect(lstatSync(p).mode & 0o777).toBe(0o600);
  f.handler = r => result(response(r, { result: f.ssh(8) })); await expect(f.client.sshInfo("explicit")).rejects.toMatchObject({ code: "generation_mismatch" });
  for (const patch of [{ host: "x\nProxyCommand bad" }, { task_id: "../bad" }, { generation: true }, { port: 1 }, { host_key: "ssh-rsa YWJj" }]) expect(() => f.client.writeSSH("explicit", { ...f.ssh(), ...patch }, f.key())).toThrow();
});

test("exec, transfer, export and SSH preserve argv, archive stdout and native exit semantics", async () => {
  const f = fixture(); f.state(); f.handler = r => result(response(r, { result: f.ssh() }));
  const commands: string[][] = [], native = async (argv: string[]) => { commands.push(argv); return 37; };
  expect(await dispatch(selected(["exec", "--", "printf", "a'`$\n"]), f.client, native)).toBe(37);
  expect(commands[0].at(-1)).toBe("printf 'a'\"'\"'`$\n'");
  expect(() => selected(["upload", "--local", "/tmp/x"])).toThrow();
  expect(await dispatch(selected(["download", "/remote with space", "local"]), f.client, native)).toBe(37);
  expect(commands[1].slice(-2)).toEqual(["agent-vm:/remote with space", expect.stringContaining("local")]);
  expect(await dispatch(selected(["export", "/"]), f.client, native)).toBe(37); expect(commands[2].at(-1)).toBe("tar -C / -cf - -- .");
  await expect(dispatch(selected(["export", "relative"]), f.client, native)).rejects.toMatchObject({ code: "invalid_path" });
  expect(f.requests.every(r => r.action === "ssh-info")).toBe(true);
});

test("lifecycle and admin commands retain action/generation/operation scope with sessionless metrics", async () => {
  const f = fixture(); f.state();
  for (const argv of [["status"], ["renew", "--operation-id", "renew"], ["release", "--operation-id", "release"], ["port", "add", "web", "--guest-port", "8080", "--protocol", "udp"], ["port", "list"], ["port", "remove", "web"], ["admin", "status"], ["admin", "session", "list"], ["admin", "drain", "enter"], ["admin", "reconcile"], ["admin", "gc", "--apply"]]) await dispatch(selected(argv), f.client);
  expect(f.requests.map(r => r.action)).toEqual(["status", "renew", "release", "port-add", "port-list", "port-remove", "admin-status", "admin-session-list", "drain-enter", "admin-reconcile", "admin-gc"]);
  expect(f.requests[3]).toMatchObject({ generation: 7, vm_port: 8080, port_protocol: "udp" });
  expect(f.requests[8]).toMatchObject({ generation: 0 });
  await dispatch(parseArgs(["admin", "metrics"]), f.client); expect(f.requests.at(-1)).not.toHaveProperty("session_id"); expect(f.requests.at(-1)).not.toHaveProperty("operation_id");
  await expect(dispatch(selected(["admin", "metrics"]), f.client)).rejects.toMatchObject({ code: "invalid_request" });
});

test("local recovery state is locked, monotonic and cleanup-only safe", () => {
  const f = fixture();
  f.state("explicit", 7);
  expect(f.client.saveState("explicit", {
    session_id: "explicit", task_id: "avm-abc", generation: 7,
    private_key: f.key(), cleanup_only: false,
  })).toBe(true);
  expect(f.client.saveState("explicit", {
    session_id: "explicit", task_id: "avm-abc", generation: 7,
    private_key: f.key(), cleanup_only: true,
  })).toBe(false);
  expect(f.client.generation("explicit")).toBe(7);
  expect(() => f.client.generation("explicit", false)).not.toThrow();
  expect(f.client.saveState("explicit", {
    session_id: "explicit", task_id: "avm-def", generation: 8,
    private_key: f.key(), cleanup_only: true,
  })).toBe(true);
  expect(() => f.client.generation("explicit")).toThrow("cleanup-only");
  expect(f.client.generation("explicit", true)).toBe(8);
  expect(f.client.saveState("explicit", {
    session_id: "explicit", task_id: "avm-abc", generation: 8,
    private_key: f.key(), cleanup_only: false,
  })).toBe(false);
});

test("authoritative cached status marks local generation stale before later mutation", async () => {
  const f = fixture(); f.state("explicit", 7);
  f.handler = request => result(response(request, {
    result: { session_id: "explicit", task_id: "avm-abc", generation: 7, state: "cached", effective_lease: false, guest_available: false, lease_expires_at: null, pending_operation_count: 0, pending_operations_complete: true },
  }));
  await dispatch(selected(["status"]), f.client);
  expect(f.client.state("explicit")).toMatchObject({ generation: 7, task_id: "avm-abc", cleanup_only: true, stale: true });
  expect(() => f.client.generation("explicit")).toThrow("cleanup-only");
  expect(f.client.generation("explicit", true)).toBe(7);
});

test("diagnosis status also reconciles authoritative stale local state", async () => {
  const f = fixture(); f.state("explicit", 7);
  f.handler = request => result(response(request, {
    result: { session_id: "explicit", task_id: "avm-abc", generation: 7, state: "cached", effective_lease: false, guest_available: false, pending_operation_count: 0, pending_operations_complete: true },
  }));
  await attachDiagnosis(new CliError("startup_reconcile_required", "recovery required"), f.client, "explicit", "pending", false);
  expect(f.client.state("explicit")).toMatchObject({ cleanup_only: true, stale: true });
});

test("recovery state requires an exact committed or partial operation proof", async () => {
  const f = fixture();
  const key = f.key();
  const active = { session_id: "explicit", task_id: "avm-abc", generation: 8, state: "active", effective_lease: true, recovery_operation_id: "acquire-op" };
  expect(f.client.persistRecoveryState("explicit", key, active, "acquire-op", 7)).toBe(true);
  expect(f.client.state("explicit")).toMatchObject({ generation: 8, cleanup_only: false });

  const partial = { session_id: "explicit", task_id: "avm-abc", generation: 9, state: "cached", effective_lease: false, partial_operation_id: "partial-op" };
  expect(f.client.persistRecoveryState("explicit", key, partial, "partial-op", 8)).toBe(true);
  expect(f.client.state("explicit")).toMatchObject({ generation: 9, cleanup_only: true });
  expect(f.client.persistRecoveryState("explicit", key, { ...partial, task_id: "avm-other" }, "partial-op", 9)).toBe(false);

  const commandCount = f.commands.length;
  await expect(f.client.sshInfo("explicit")).rejects.toMatchObject({ code: "lease_unavailable" });
  expect(f.commands).toHaveLength(commandCount);

  f.handler = request => result(response(request, {
    result: { ...partial, generation: 10, partial_operation_id: "remote-op" },
  }));
  expect(await f.client.recoverAcquireState("explicit", key, "remote-op", 9)).toBe(true);
  expect(f.requests.at(-1)).toMatchObject({ action: "status", operation_id: "remote-op" });
  expect(f.client.state("explicit")).toMatchObject({ generation: 10, cleanup_only: true });

  f.handler = request => result(response(request, {
    operation_id: "different-op",
    result: { ...partial, generation: 11, partial_operation_id: "remote-op" },
  }));
  expect(await f.client.recoverAcquireState("explicit", key, "remote-op", 10)).toBe(false);
  expect(f.client.state("explicit")).toMatchObject({ generation: 10, cleanup_only: true });
});
