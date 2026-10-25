import { expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { existsSync, readFileSync, writeFileSync, mkdirSync, statSync, symlinkSync } from "node:fs";
import { join } from "node:path";
import { Control, request_from_stdin } from "../scripts/agent_vm_control.ts";
import { StateStore } from "../scripts/control-store.ts";
import { SystemdManager } from "../scripts/control-systemd.ts";
import { canonical_json, digest, parse_json, load_config, DEFAULT_RELEASE_FENCE_PATH, type Data } from "../scripts/control-types.ts";
import { guest_source } from "../scripts/guest-root-growth.ts";
import { Fixture, NativeCommands, KEY, expect_error } from "./control-fixture.ts";

const LINUX = process.platform === "linux";
const FIXTURE = new URL("./control-fixture.ts", import.meta.url).href;
const CONTROL = new URL("../scripts/agent_vm_control.ts", import.meta.url).href;
const STORE = new URL("../scripts/control-store.ts", import.meta.url).href;
const TYPES = new URL("../scripts/control-types.ts", import.meta.url).href;

test("configuration validates required identity and native prerequisites before opening state", async () => {
  const f = new Fixture();
  const path = join(f.root, "config.json");
  try {
    writeFileSync(path, "{}");
    expect((await expect_error("config_missing", () => load_config(path))).detail.fields).toContain("host_address");
    const valid: Data = { ...f.config, image_alias: "agent-image", release_fence_path: DEFAULT_RELEASE_FENCE_PATH };
    delete valid.required_guest_tools;
    writeFileSync(path, canonical_json(valid));
    expect(load_config(path).required_guest_tools).toContain("bun");
    expect(load_config(path).required_guest_tools).toContain("python3");
    for (const overrides of [
      { host_address: "not-an-ip" }, { vm_ip_start: "198.51.100.251", vm_ip_end: "198.51.100.20" },
      { image_fingerprint: "A".repeat(64) }, { cpu_overcommit_ratio: 0.9 },
      { required_guest_tools: ["bun", false] }, { release_fence_path: "/tmp/alternate-fence" },
    ]) {
      writeFileSync(path, canonical_json({ ...valid, ...overrides }));
      await expect_error("config_invalid", () => load_config(path));
    }
    const unopened = join(f.root, "unopened.sqlite3");
    const manifest = join(f.root, "bad-manifest.json");
    writeFileSync(manifest, "{}");
    await expect_error("manifest_invalid", () => new Control({ ...valid, state_db: unopened, release_manifest_path: manifest }));
    expect(existsSync(unopened)).toBe(false);
    expect(f.runner.calls).toEqual([]);
  } finally { f.close(); }
});

test("the shipped guest bundle runs directly from Bun stdin and rejects invalid capacity without writes", async () => {
  const child = Bun.spawn([process.execPath, "run", "-", "invalid"], {
    stdin: Buffer.from(await guest_source()), stdout: "pipe", stderr: "pipe",
  });
  const [stdout, stderr, status] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited]);
  expect(status).toBe(1);
  const report = parse_json(stdout || stderr);
  expect(report.error.phase).toBe("guest-root-growth");
  expect(report.error.capacity.verified).toBe(false);
});

test("accepted Python database reopens with exact generations and replay digests", async () => {
  const f = new Fixture();
  const path = join(f.root, "baseline.sqlite3");
  const db = new Database(path);
  db.exec(readFileSync(new URL("./fixtures/control-baseline.sql", import.meta.url), "utf8"));
  db.close();
  const store = new StateStore(path);
  try {
    const control = new Control({ ...f.config, state_db: path }, {
      store, runner: f.runner, resources: f.resources, systemd: f.systemd, clock: () => f.now,
    });
    const cases = parse_json(readFileSync(new URL("./fixtures/control-baseline.json", import.meta.url), "utf8"));
    for (const entry of cases) expect(digest(entry.request)).toBe(entry.digest);
    expect(store.get_task({ session_id: "baseline" })!.generation).toBe(9007199254740993n);
    const saved = cases.map((entry: Data) => store.operation(entry.request.operation_id));
    expect((await control.dispatch(cases[0].request)).outcome).toBe("replayed");
    await expect_error("operation_incomplete", () => control.dispatch(cases[1].request));
    await expect_error("lease_expired", () => control.dispatch(cases[2].request));
    for (let i = 0; i < cases.length; i++) expect(store.operation(cases[i].request.operation_id)).toEqual(saved[i]);
    expect(f.runner.calls).toEqual([]);
  } finally { store.close(); f.close(); }
});

test("stdin stays bounded and rejects invalid UTF-8 before control effects", async () => {
  const input = async function* (value: Buffer) { yield value; };
  await expect_error("request_too_large", () => request_from_stdin(input(Buffer.alloc(2 * 1024 * 1024 + 1, 32))));
  await expect_error("invalid_request", () => request_from_stdin(input(Buffer.from([0xff]))));
  await expect_error("invalid_request", () => request_from_stdin(input(Buffer.from("[]"))));
});

test("systemd expiry writes a private atomic timer, verifies identity and removes it", async () => {
  const f = new Fixture();
  const runner = new NativeCommands();
  const calls: string[][] = [];
  const task = "avm-0123456789abcdef", generation = 9007199254740993n;
  const expiry = "2026-10-25T08:00:00+00:00";
  const systemd = new SystemdManager(runner, f.config);
  runner.run = async argv => {
    calls.push([...argv]);
    const unit = SystemdManager.timer_unit(task, generation);
    return { returncode: 0, stdout: Buffer.from(`ActiveState=active\nFragmentPath=${join(systemd.timer_dir, unit)}\nUnitFileState=enabled\n`), stderr: Buffer.alloc(0), stdout_truncated: false, stderr_truncated: false };
  };
  try {
    const unit = await systemd.schedule_expiry(task, generation, expiry);
    const path = join(systemd.timer_dir, unit);
    expect(statSync(path).mode & 0o777).toBe(0o600);
    expect(readFileSync(path, "utf8")).toContain(`Unit=agent-vm-native-expire@${task}-g9007199254740993.service`);
    expect(await systemd.expiry_active(task, generation, expiry)).toBe(true);
    expect(await systemd.expiry_active(task, generation, "2026-10-25T09:00:00+00:00")).toBe(false);
    expect(calls.slice(0, 3).map(c => c.slice(1))).toEqual([["daemon-reload"], ["enable", "--now", unit], ["restart", unit]]);
    await systemd.remove_expiry(task, generation);
    expect(existsSync(path)).toBe(false);
    symlinkSync("missing", path);
    expect(await systemd.expiry_active(task, generation, expiry)).toBe(false);
    await expect_error("invalid_timer", () => systemd.schedule_expiry("../invalid", 1, expiry));
  } finally { f.close(); }
});

function worker(f: Fixture): string {
  const path = join(f.root, "worker.ts");
  writeFileSync(path, `
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { Control, request_from_stdin, error_envelope } from ${JSON.stringify(CONTROL)};
import { StateStore } from ${JSON.stringify(STORE)};
import { canonical_json, parse_json, ControlError } from ${JSON.stringify(TYPES)};
import { NativeCommands, FakeResources, FakeSystemd } from ${JSON.stringify(FIXTURE)};
const config = parse_json(readFileSync(process.argv[2], "utf8"));
const runner = new NativeCommands(); runner.storage_source=config.storage_path;
if (process.env.SEED) runner.instances=parse_json(readFileSync(process.env.SEED,"utf8"));
if (process.env.START_MARKER) runner.on_start=async()=>{
  writeFileSync(process.env.START_MARKER!, "ready");
  const limit=performance.now()+10000;
  while(!existsSync(process.env.START_GATE!)) { if(performance.now()>limit) throw Error("start gate timeout"); await Bun.sleep(10); }
};
const store=new StateStore(config.state_db);
const control=new Control(config,{store,runner,resources:new FakeResources(runner),systemd:new FakeSystemd(runner,config),clock:()=>new Date(process.env.NOW!)});
let request=null;
try {
  request=await request_from_stdin();
  const response=await control.dispatch(request);
  if(process.env.SAVE) writeFileSync(process.env.SAVE,canonical_json(runner.instances));
  console.log(canonical_json(response));
} catch(error) {
  if(!(error instanceof ControlError)) throw error;
  console.error(canonical_json(error_envelope(request,error.as_dict())));
  process.exitCode=2;
} finally { store.close(); }
`);
  writeFileSync(join(f.root, "config.json"), canonical_json(f.config));
  return path;
}

function launch(f: Fixture, path: string, request: Data, extra: Record<string, string> = {}) {
  const child = Bun.spawn([process.execPath, path, join(f.root, "config.json")], {
    stdin: Buffer.from(canonical_json(request)), stdout: "pipe", stderr: "pipe",
    env: { ...process.env, NOW: f.now.toISOString(), ...extra },
  });
  const result = (async () => {
    const [stdout, stderr, status] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited]);
    const output = stdout.trim() || stderr.trim();
    if (!output) throw new Error(`worker ${status} returned no response`);
    return { status, response: parse_json(output), stderr };
  })();
  return { child, result };
}

async function wait_file(path: string): Promise<void> {
  const end = performance.now() + 5000;
  while (!existsSync(path)) { if (performance.now() >= end) throw new Error(`marker timeout: ${path}`); await Bun.sleep(10); }
}

test.skipIf(!LINUX)("native multiprocess controller serializes a session and admits against durable reservations without holding global lock over native wait", async () => {
  const f = new Fixture();
  const script = worker(f), marker = join(f.root, "starting"), gate = join(f.root, "continue");
  const seed = join(f.root, "instances.json");
  const acquire = f.request("acquire", { session: "large", cpu: 16, public_key: KEY });
  const first = launch(f, script, acquire, { START_MARKER: marker, START_GATE: gate, SAVE: seed });
  let same: ReturnType<typeof launch> | undefined;
  try {
    await wait_file(marker);
    // The first process is inside the native start operation. The second must
    // promptly read its reservation, rather than waiting for its guest startup.
    const second = await launch(f, script, f.request("acquire", { session: "other", public_key: KEY })).result;
    expect(second.response.error.code).toBe("resource_insufficient");
    expect(f.store.list_tasks()).toHaveLength(1);
    expect(parse_json(f.store.list_tasks()[0].resource_reservation_json).cpu).toBe(16);
    same = launch(f, script, { protocol: 4, action: "status", session_id: "large" });
    let completed = false;
    void same.child.exited.then(() => { completed = true; });
    await Bun.sleep(100);
    expect(completed).toBe(false);
    writeFileSync(gate, "continue");
    const result = await first.result;
    expect(result.status, result.stderr).toBe(0);
    expect((await same.result).response.result.state).toBe("active");
    const task = f.store.get_task({ session_id: "large" })!;
    f.advance(1800);
    const expiry = await launch(f, script, { protocol: 4, action: "timer-expire", task_id: task.task_id, generation: task.generation }, { SEED: seed, SAVE: seed }).result;
    expect(expiry.status, expiry.stderr).toBe(0);
    expect(f.store.get_task({ task_id: task.task_id })!.state).toBe("cached");
    expect(Object.values(parse_json(readFileSync(seed, "utf8"))).map((i: any) => i.status)).toEqual(["Stopped"]);
    const stale = await launch(f, script, { protocol: 4, action: "timer-expire", task_id: task.task_id, generation: 0 }, { SEED: seed }).result;
    expect(stale.response.error.code).toBe("invalid_request");
  } finally {
    writeFileSync(gate, "continue");
    await first.result.catch(() => {});
    if (same) await same.result.catch(() => {});
    f.close();
  }
}, 20000);

test.skipIf(!LINUX)("an independent expiry closes access while another real controller waits on native startup", async () => {
  const f = new Fixture();
  const victim = await f.acquire({ session: "victim" });
  const script = worker(f), marker = join(f.root, "starting"), gate = join(f.root, "continue");
  const seed = join(f.root, "victim.json");
  writeFileSync(seed, canonical_json(f.runner.instances));
  const first = launch(f, script, f.request("acquire", { session: "slow", public_key: KEY }), { START_MARKER: marker, START_GATE: gate });
  try {
    await wait_file(marker);
    f.advance(1800);
    const expired = await launch(f, script, { protocol: 4, action: "timer-expire", task_id: victim.task_id, generation: victim.generation }, { SEED: seed, SAVE: seed }).result;
    expect(expired.status, expired.stderr).toBe(0);
    expect(f.store.get_task({ task_id: victim.task_id })!.state).toBe("cached");
    expect(f.store.get_ports(victim.task_id)).toEqual([]);
    expect(parse_json(readFileSync(seed, "utf8"))[victim.instance_name].status).toBe("Stopped");
    expect(existsSync(gate)).toBe(false);
    expect(f.store.get_task({ session_id: "slow" })!.state).toBe("preparing");
  } finally {
    writeFileSync(gate, "continue");
    await first.result.catch(() => {});
    f.close();
  }
}, 20000);

test("Bun client transports protocol requests to an actual Bun controller", async () => {
  const f = new Fixture();
  const script = worker(f);
  const { Client } = await import("../../agent-vm/scripts/client.ts");
  const { runBounded } = await import("../../agent-vm/scripts/client-io.ts");
  const client = new Client({ ...process.env, AGENT_VM_RELEASE_MANIFEST: join(f.root, "absent.json") }, async (argv, options) => {
    expect(argv[0]).toBe("ssh");
    expect(argv.at(-1)).toBe("bun /opt/agent-vm-native/agent_vm_control.ts");
    expect(options?.timeoutMs).toBe(240000);
    return runBounded([process.execPath, script, join(f.root, "config.json")], {
      ...options, env: { ...process.env, NOW: f.now.toISOString() },
    });
  }, () => {});
  try {
    const response = await client.request("status", { session: "transport-smoke" });
    expect(response.ok).toBe(true);
    expect(response.result.state).toBe("absent");
    await expect(client.request("unknown", { session: "transport-smoke" })).rejects.toMatchObject({ code: "unknown_action" });
  } finally { f.close(); }
});
