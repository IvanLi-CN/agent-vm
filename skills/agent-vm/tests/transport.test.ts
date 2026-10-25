import { afterEach, expect, test } from "bun:test";
import { mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { Client, STDERR_LIMIT, STDOUT_LIMIT, timeoutSeconds } from "../scripts/client.ts";
import { json, parseJson, runBounded } from "../scripts/client-io.ts";
import { Fixture, response, result } from "./support.ts";

let fixtures: Fixture[] = [];
const fixture = () => { const f = new Fixture(); fixtures.push(f); return f; };
afterEach(() => { fixtures.forEach(f => f.close()); fixtures = []; });

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
