import { afterEach, expect, test } from "bun:test";
import { mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { Client, STDERR_LIMIT, STDOUT_LIMIT, timeoutSeconds } from "../scripts/client.ts";
import { json, parseJson, readJson, runBounded, sha256 } from "../scripts/client-io.ts";
import { ACTIONS, CLIENT_DEPENDENCIES } from "../scripts/client-release.ts";
import { Fixture, response, result } from "./support.ts";

let fixtures: Fixture[] = [];
const fixture = () => { const f = new Fixture(); fixtures.push(f); return f; };
afterEach(() => { fixtures.forEach(f => f.close()); fixtures = []; });

function httpsSidecar(f: Fixture): void {
  const revision = "c".repeat(40), schema = { schema: 1, semantic_revision: 1 }, window = { schema_min: 1, schema_max: 1, semantic_min: 1, semantic_max: 1 };
  const client = readFileSync(new URL("../scripts/agent_vm.ts", import.meta.url));
  writeFileSync(join(f.root, "agent_vm.ts"), client, { mode: 0o755 });
  const dependencies = CLIENT_DEPENDENCIES.map(([name, mode]) => {
    const bytes = readFileSync(new URL(`../scripts/${name}`, import.meta.url));
    writeFileSync(join(f.root, name), bytes, { mode });
    return { name, path: name, role: "support", mode, sha256: sha256(bytes) };
  });
  const sidecar = {
    format: 2, release_id: revision, source_revision: revision, protocol: 4,
    client_artifact: { name: "agent_vm.ts", path: "agent_vm.ts", role: "client", mode: 0o755, sha256: sha256(client) },
    client_dependencies: dependencies,
    client_view: { release_id: revision, source_revision: revision, protocol: 4, transport: "https", actions: Object.fromEntries(ACTIONS.map(action => [action, { request: schema, response: schema, client_to_control: window, control_to_client: window }])) },
  };
  const path = join(f.root, "client-release.json");
  writeFileSync(path, json(sidecar), { mode: 0o600 });
  f.client.sidecar = path;
}

test("direct HTTPS sends protocol-4 JSON to the fixed Agent API with a bearer credential and TLS verification", async () => {
  const f = fixture(); httpsSidecar(f);
  f.client.env.AGENT_VM_CONTROL_HOST = "https://control.example/";
  f.client.env.AGENT_VM_TOKEN = "opaque-agent-token";
  const seen: { url?: string; init?: any } = {};
  f.client.http = async (url, init) => {
    seen.url = url; seen.init = init;
    return new Response(json(response(parseJson(String(init.body)))), { status: 200, headers: { "content-type": "application/json" } });
  };
  await f.client.request("renew", { session: "https-session", generation: 7, operationId: "https-operation", write: true });
  expect(seen.url).toBe("https://control.example/agent/v1/control");
  expect(seen.init.method).toBe("POST");
  expect(seen.init.headers).toEqual({ Accept: "application/json", Authorization: "Bearer opaque-agent-token", "Content-Type": "application/json" });
  expect(seen.init.tls).toEqual({ rejectUnauthorized: true });
  expect(parseJson(String(seen.init.body))).toMatchObject({ protocol: 4, action: "renew", session_id: "https-session", generation: 7, operation_id: "https-operation" });
  expect(JSON.stringify(readJson(f.client.operationPath("https-session", "https-operation")))).not.toContain("opaque-agent-token");
  expect(f.commands).toEqual([]);
});

test("HTTPS CA configuration is private, certificate verification cannot be disabled, and missing credentials fail before a journal", async () => {
  const f = fixture(); httpsSidecar(f);
  f.client.env.AGENT_VM_CONTROL_HOST = "https://control.example";
  f.client.env.AGENT_VM_TOKEN = "opaque-agent-token";
  const ca = join(f.root, "control-ca.pem"); writeFileSync(ca, "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n", { mode: 0o600 });
  f.client.env.AGENT_VM_CONTROL_CA = ca;
  let tls: any;
  f.client.http = async (_url, init) => { tls = init.tls; return new Response(json(response(parseJson(String(init.body)))), { status: 200 }); };
  await f.client.request("status", { session: "https-session" });
  expect(tls).toEqual({ ca: readFileSync(ca, "utf8"), rejectUnauthorized: true });

  delete f.client.env.AGENT_VM_TOKEN;
  await expect(f.client.request("status", { session: "missing-token" })).rejects.toMatchObject({ code: "config_missing" });
  expect(f.commands).toEqual([]);
});

test("HTTPS timeout retains the original operation journal for a safe retry", async () => {
  const f = fixture(); httpsSidecar(f);
  f.client.env.AGENT_VM_CONTROL_HOST = "https://control.example";
  f.client.env.AGENT_VM_TOKEN = "opaque-agent-token";
  f.client.httpsTimeoutMs = 10;
  f.client.http = async (_url, init) => await new Promise<Response>((_resolve, reject) => init.signal!.addEventListener("abort", () => reject(new Error("aborted")), { once: true }));
  await expect(f.client.request("renew", { session: "https-session", generation: 9007199254740993n, operationId: "https-operation", write: true })).rejects.toMatchObject({ code: "remote_timeout" });
  const operation = f.client.operationPath("https-session", "https-operation");
  expect(readJson(operation).outcome).toBe("pending");

  const bodies: string[] = [];
  f.client.http = async (_url, init) => {
    bodies.push(String(init.body));
    return new Response(json(response(parseJson(String(init.body)))), { status: 200 });
  };
  await f.client.request("renew", { session: "https-session", generation: 9007199254740993n, operationId: "https-operation", write: true });
  expect(bodies).toHaveLength(1);
  expect(parseJson(bodies[0])).toMatchObject({ operation_id: "https-operation", generation: 9007199254740993n });
  expect(readJson(operation).outcome).toBe("success");
});

test("HTTPS connection failure retains the original operation journal", async () => {
  const f = fixture(); httpsSidecar(f);
  f.client.env.AGENT_VM_CONTROL_HOST = "https://control.example";
  f.client.env.AGENT_VM_TOKEN = "opaque-agent-token";
  f.client.http = async () => { throw new Error("socket refused"); };
  let error: any;
  try { await f.client.request("renew", { session: "https-session", generation: 9, operationId: "connection-operation", write: true }); }
  catch (caught) { error = caught; }
  expect(error).toMatchObject({ code: "remote_failed", detail: { operation_id: "connection-operation", generation: 9, transport: "https" } });
  expect(readJson(f.client.operationPath("https-session", "connection-operation")).outcome).toBe("pending");
});

test("HTTPS response data and errors redact the bearer token before return, journaling or CLI failure", async () => {
  const f = fixture(); httpsSidecar(f);
  f.client.env.AGENT_VM_CONTROL_HOST = "https://control.example";
  f.client.env.AGENT_VM_TOKEN = "opaque-agent-token";
  f.client.http = async (_url, init) => new Response(json(response(parseJson(String(init.body)), { result: { echoed: "opaque-agent-token", "opaque-agent-token": "object-key" } })), { status: 200 });
  const returned = await f.client.request("renew", { session: "https-session", generation: 8, operationId: "echo-success", write: true });
  expect(JSON.stringify(returned)).not.toContain("opaque-agent-token");
  expect(JSON.stringify(readJson(f.client.operationPath("https-session", "echo-success")))).not.toContain("opaque-agent-token");

  f.client.http = async (_url, init) => {
    const request = parseJson(String(init.body));
    return new Response(json({ ok: false, protocol: 4, action: request.action, session_id: request.session_id, generation: request.generation ?? null, operation_id: request.operation_id, error: { code: "remote_failed", message: "server echoed opaque-agent-token", detail: { echoed: "opaque-agent-token" } } }), { status: 502, headers: { "X-Agent-VM-Exit-Code": "3" } });
  };
  let error: any;
  try { await f.client.request("renew", { session: "https-session", generation: 10, operationId: "echo-operation", write: true }); }
  catch (caught) { error = caught; }
  expect(error.message).not.toContain("opaque-agent-token");
  expect(error.exitCode).toBe(3);
  expect(JSON.stringify(error.detail)).not.toContain("opaque-agent-token");
  expect(JSON.stringify(readJson(f.client.operationPath("https-session", "echo-operation")))).not.toContain("opaque-agent-token");
});

test("HTTPS HTTP failures preserve the control CLI exit code instead of the status code", async () => {
  for (const [status, exitCode] of [[400, 2], [503, 3]] as const) {
    const f = fixture(); httpsSidecar(f);
    f.client.env.AGENT_VM_CONTROL_HOST = "https://control.example";
    f.client.env.AGENT_VM_TOKEN = "opaque-agent-token";
    f.client.http = async (_url, init) => {
      const request = parseJson(String(init.body));
      return new Response(json({ ok: false, protocol: 4, action: request.action, session_id: request.session_id, generation: request.generation ?? null, operation_id: request.operation_id, error: { code: "remote_failed", message: "control failed" } }), { status, headers: { "X-Agent-VM-Exit-Code": String(exitCode) } });
    };
    let error: any;
    try { await f.client.request("renew", { session: "https-session", generation: status, operationId: `http-${status}`, write: true }); }
    catch (caught) { error = caught; }
    expect(error).toMatchObject({ code: "remote_failed", exitCode });
  }
});

test("HTTPS response overflow reports an uncertain result and preserves the pending operation", async () => {
  const f = fixture(); httpsSidecar(f);
  f.client.env.AGENT_VM_CONTROL_HOST = "https://control.example";
  f.client.env.AGENT_VM_TOKEN = "opaque-agent-token";
  f.client.http = async () => new Response("x".repeat(STDOUT_LIMIT + 1), { status: 200 });
  let error: any;
  try { await f.client.request("renew", { session: "https-session", generation: 11, operationId: "overflow-operation", write: true }); }
  catch (caught) { error = caught; }
  expect(error).toMatchObject({ code: "remote_failed", detail: { operation_id: "overflow-operation", generation: 11, transport: "https", stdout_truncated: true } });
  expect(error.message).toContain("uncertain");
  expect(readJson(f.client.operationPath("https-session", "overflow-operation")).outcome).toBe("pending");
});

test("HTTPS response overflow reports unverified cleanup when cancellation fails", async () => {
  const f = fixture(); httpsSidecar(f);
  f.client.env.AGENT_VM_CONTROL_HOST = "https://control.example";
  f.client.env.AGENT_VM_TOKEN = "opaque-agent-token";
  f.client.http = async () => ({
    status: 200,
    body: {
      getReader: () => ({
        read: async () => ({ done: false, value: Buffer.from("x".repeat(STDOUT_LIMIT + 1)) }),
        cancel: async () => { throw new Error("cancel failed"); },
        releaseLock: () => {},
      }),
    },
  } as unknown as Response);
  let error: any;
  try { await f.client.request("renew", { session: "https-session", generation: 12, operationId: "overflow-cleanup-operation", write: true }); }
  catch (caught) { error = caught; }
  expect(error).toMatchObject({ code: "remote_failed", detail: { operation_id: "overflow-cleanup-operation", generation: 12, transport: "https", stream_cleanup_verified: false, stream_cleanup_error: "reader_cancelled_before_eof" } });
  expect(error.message).toContain("stream cleanup is not verified");
  expect(readJson(f.client.operationPath("https-session", "overflow-cleanup-operation")).outcome).toBe("pending");
});

test("D transport regression: canonical controller directly with Bun, safe quoting and budgets", async () => {
  const f = fixture();
  const budgets: number[] = [];
  f.client.run = async (argv, o) => { f.commands.push(argv); budgets.push(o!.timeoutMs!); return result(response(parseJson(String(o!.input)))); };
  (f.client.env as any).AGENT_VM_REMOTE_CONTROL = "/operator path/control;$(false).ts";
  for (const [action, fields, timeout] of [["acquire", {}, 600], ["restart", {}, 600], ["resize", { allow_restart: true }, 600], ["resize", { allow_restart: false }, 240], ["status", {}, 240]] as const) {
    await f.client.request(action, { session: "transport-test", fields });
    expect(timeoutSeconds(action, fields)).toBe(timeout);
  }
  expect(f.commands[0]).toEqual(["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "--", "agent-vm-host", "bun '/operator path/control;$(false).ts'"]);
  expect(budgets).toEqual([600000, 600000, 600000, 240000, 240000]);
  expect(STDOUT_LIMIT).toBe(2097152); expect(STDERR_LIMIT).toBe(65536);
});

test("real process pipes are concurrently drained with hard retention caps and exact exit", async () => {
  const r = await runBounded([process.execPath, "-e", 'await Promise.all([Bun.write(Bun.stdout,"x".repeat(3000000)),Bun.write(Bun.stderr,"y".repeat(100000))]);process.exitCode=23'], { stdoutLimit: 2097152, stderrLimit: 65536, timeoutMs: 3000 });
  expect(r.code).toBe(23); expect(r.stdout.length).toBe(STDOUT_LIMIT); expect(r.stderr.length).toBe(STDERR_LIMIT); expect(r.stdoutTruncated).toBe(true); expect(r.stderrTruncated).toBe(true); expect(r.streamCleanupVerified).toBe(true);
});

test("process timeout and inherited pipes terminate within one deadline without a false outcome", async () => {
  let start = performance.now();
  const timeout = await runBounded([process.execPath, "-e", "await Bun.sleep(10000)"], { timeoutMs: 100 });
  expect(timeout.timedOut).toBe(true); expect(performance.now() - start).toBeLessThan(1500);
  // A known short-lived child keeps inherited pipes open after its leader exits.
  start = performance.now();
  const pipes = await runBounded(["sh", "-c", "sleep 1 & exit 0"], { timeoutMs: 100 });
  expect(pipes.code).toBe(0); expect(pipes.timedOut).toBe(false); expect(pipes.streamCleanupVerified).toBe(false); expect(performance.now() - start).toBeLessThan(800);
});

test("fresh consumer source link runs standalone CLI through fake SSH and preserves transport exit", async () => {
  const f = fixture(), bin = join(f.root, "bin"), linked = join(f.root, "consumer"); mkdirSync(bin);
  symlinkSync(new URL("..", import.meta.url).pathname, linked, "dir");
  const server = join(f.root, "server.ts"), capture = join(f.root, "request.json");
  writeFileSync(server, `const r=JSON.parse(await Bun.stdin.text());await Bun.write(process.env.CAPTURE!,JSON.stringify({argv:process.argv.slice(2),request:r}));if(process.env.FAIL){console.error("SSH transport failed");process.exit(27)};console.log(JSON.stringify({...r,ok:true,server_time:"2026-10-25",outcome:"success",result:{},response:{schema:1,semantic_revision:1}}));`);
  writeFileSync(join(bin, "ssh"), `#!/bin/sh\nexec '${process.execPath}' '${server}' "$@"\n`, { mode: 0o755 });
  const env = { ...process.env, ...f.env, PATH: bin + ":" + process.env.PATH, CAPTURE: capture };
  const command = [process.execPath, join(linked, "scripts/agent_vm.ts"), "status", "--session-id", "explicit"];
  const good = await runBounded(command, { cwd: f.root, env }); expect(good.code).toBe(0); expect(parseJson(good.stdout.toString()).session_id).toBe("explicit");
  const seen = JSON.parse(readFileSync(capture, "utf8")); expect(seen.argv.at(-1)).toBe("bun /opt/agent-vm-native/agent_vm_control.ts"); expect(seen.request.session_id).toBe("explicit");
  const failed = await runBounded(command, { cwd: f.root, env: { ...env, FAIL: "1" } }); expect(failed.code).toBe(27); expect(failed.stdout.length).toBe(0); expect(failed.stderr.toString()).toContain('"remote_failed"');
});

test("missing SSH and invalid UTF-8 responses fail rather than fabricate results", async () => {
  const r = runBounded(["/nonexistent/agent-vm-ssh"]); await expect(r).rejects.toMatchObject({ code: "command_missing" });
  const f = fixture(); f.handler = () => result(Buffer.from([0xff])); await expect(f.client.request("status", { session: "s" })).rejects.toMatchObject({ code: "invalid_remote_response" });
});
