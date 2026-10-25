// Keep these tests sequential: the settlement test temporarily replaces Bun.sleep.

import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { writeFileSync } from "node:fs";
import { Control } from "../scripts/agent_vm_control.ts";
import {
  canonical_json,
  parse_json,
  type Data,
} from "../scripts/control-types.ts";
import {
  Fixture,
  ControlError,
  GIB,
  KEY,
  PROTOCOL_VERSION,
  command_result,
  expect_error,
  type FixtureOptions,
} from "./control-fixture.ts";

function scenario(
  name: string,
  body: (f: Fixture) => Promise<void>,
  options: FixtureOptions = {},
): void {
  test(name, async () => {
    const f = new Fixture(options);
    try {
      await body(f);
    } finally {
      f.close();
    }
  });
}

function task(f: Fixture, session = "thread-a"): Data {
  const value = f.store.get_task({ session_id: session });
  expect(value).not.toBeNull();
  return value!;
}

function instance(f: Fixture, session = "thread-a"): Data {
  return f.runner.instances[task(f, session).instance_name];
}

function changes(f: Fixture, since = 0): string[][] {
  return f.runner.calls.slice(since).flatMap(([argv]) => {
    if (argv.slice(0, 3).join(" ") !== "incus --project agents") return [];
    const command = argv.slice(3);
    if (["init", "start", "stop", "delete"].includes(command[0])) {
      return [command];
    }
    if (
      command[0] === "config" &&
      (
        command[1] === "set" ||
        command[1] === "device" &&
        ["add", "set", "override", "remove"].includes(command[2])
      )
    ) {
      return [command];
    }
    return [];
  });
}

function restarted(f: Fixture): Control {
  return new Control(f.config, {
    runner: f.runner,
    store: f.store,
    resources: f.resources,
    systemd: f.systemd,
    clock: () => f.now,
  });
}

async function cache(f: Fixture, session: string): Promise<Data> {
  await f.acquire({ session });
  await f.call("release", { session });
  return task(f, session);
}

function timer(f: Fixture, value: Data): Promise<Data> {
  return f.control.dispatch({
    protocol: PROTOCOL_VERSION,
    action: "timer-expire",
    task_id: value.task_id,
    generation: value.generation,
  });
}

// Independent encoding for the flat, primitive-valued requests used here.
// Include request_id: rebuilding a request is not an exact replay.
function exact_digest(request: Data): string {
  const text = "{" + Object.keys(request).sort()
    .map(key => `${JSON.stringify(key)}: ${JSON.stringify(request[key])}`)
    .join(", ") + "}";
  return createHash("sha256").update(text).digest("hex");
}

function balloon_timeout(target = 2048, current = 3482): ControlError {
  return new ControlError("command_failed", "fixture balloon timeout", {
    returncode: 1,
    stderr:
      `Error: Failed updating memory limit: Failed setting memory to ${target}MiB ` +
      `(currently ${current}MiB) as it was taking too long`,
  });
}

scenario("protocol guards preserve old journals before native observation", async f => {
  const request = f.request("acquire", { public_key: KEY, protocol: 1 });
  const saved = {
    outcome: "pending",
    _request_digest: exact_digest(request),
  };
  f.store.record_operation(
    request.operation_id, "acquire", request.session_id, 0, saved,
  );

  await expect_error("protocol_mismatch", () => f.control.dispatch(request));
  expect(f.runner.calls).toEqual([]);
  expect(f.store.list_tasks()).toEqual([]);
  expect(f.store.operation(request.operation_id)).toEqual(saved);

  await expect_error("operation_conflict", () =>
    f.control.dispatch({ ...request, protocol: PROTOCOL_VERSION }));
  expect(f.runner.calls).toEqual([]);
  expect(f.store.operation(request.operation_id)).toEqual(saved);
});

scenario("malformed requests fail before native effects or allocation", async f => {
  await expect_error("invalid_request", () => f.control.dispatch([]));
  await expect_error("unknown_action", () =>
    f.control.dispatch({ protocol: PROTOCOL_VERSION, action: "unknown" }));

  const request = f.request("acquire", { public_key: KEY });
  delete request.generation;
  await expect_error("invalid_request", () => f.control.dispatch(request));

  expect(f.runner.calls).toEqual([]);
  expect(f.store.list_tasks()).toEqual([]);
  expect(f.store.operation(request.operation_id)).toBeNull();
});

scenario("legacy numeric protocol equality does not coerce string or boolean identity", async f => {
  expect((await f.control.dispatch(parse_json('{"protocol":4.0,"action":"status","session_id":"thread-a"}'))).result.state).toBe("absent");
  const calls = f.runner.calls.length;
  for (const protocol of ["4", true, null, 5]) {
    await expect_error("protocol_mismatch", () => f.control.dispatch({ protocol, action: "status", session_id: "thread-a" }));
  }
  expect(f.runner.calls).toHaveLength(calls);
});

scenario("missing guest Bun blocks disk action and SSH exposure without a Python fallback", async f => {
  f.config.required_guest_tools = ["sshd", "docker"]; // Old explicit config still requires Bun.
  f.runner.on_run = argv => {
    if (argv.includes("exec") && argv.at(-1)?.includes("missing_tool=")) {
      expect(argv.at(-1)).toContain("for tool in bun ");
      return command_result({
        returncode: 1,
        stdout: "missing_tool=bun\nmissing_compose=true\n",
      });
    }
  };
  const error = await expect_error("command_failed", () => f.acquire());
  expect(error.detail.stdout).toContain("missing_tool=bun");
  expect(error.detail.stdout).toContain("missing_compose=true");
  expect(f.runner.calls.some(([argv]) => argv.includes("exec") && argv.includes("bun"))).toBe(false);
  expect(f.runner.calls.some(([argv]) => argv.includes("authorized_keys"))).toBe(false);
  expect(f.store.get_ports(task(f).task_id)).toEqual([]);
  expect(instance(f).status).toBe("Stopped");
  expect(task(f).state).toBe("cached");
});

scenario("admin metrics reports a stable durable observation window", async f => {
  const result = await f.call("admin-metrics", { session: null });

  expect(result.observation_consistent).toBe(true);
  expect(result.observation_attempts).toBe(1);
  expect(result.observation_epoch).toBe("initial");
  expect(result.default_admission).not.toBeNull();
  expect(result.warnings).not.toContainEqual({
    code: "observation_inconsistent",
    message: "durable state changed while collecting metrics",
  });
});

scenario("admin metrics retries when durable state changes during native sampling", async f => {
  let probes = 0;
  f.resources.on_memory = async () => {
    probes += 1;
    if (probes === 1) f.store.set_meta("observation-race", "1");
    return f.resources.memory_value;
  };

  const result = await f.call("admin-metrics", { session: null });

  expect(probes).toBeGreaterThanOrEqual(2);
  expect(result.observation_consistent).toBe(true);
  expect(result.observation_attempts).toBe(2);
  expect(result.observation_epoch).toBe(f.store.state_epoch());
});

scenario("admin metrics bounds an unstable observation and reports a warning", async f => {
  let probes = 0;
  f.resources.on_memory = async () => {
    probes += 1;
    f.store.set_meta(`observation-race-${probes}`, "1");
    return f.resources.memory_value;
  };

  const result = await f.call("admin-metrics", { session: null });

  expect(probes).toBeGreaterThanOrEqual(3);
  expect(result.observation_consistent).toBe(false);
  expect(result.observation_attempts).toBe(3);
  expect(result.warnings).toContainEqual({
    code: "observation_inconsistent",
    message: "durable state changed while collecting metrics",
  });
});

scenario("activation fence prevents drain exit and leaves admission closed", async f => {
  await f.call("drain-enter", { session: null });
  writeFileSync(f.config.release_fence_path, "activation");
  await expect_error("release_activation_in_progress", () => f.call("drain-exit", { session: null }));
  expect(f.store.get_meta("draining")).toBe("1");
});

for (const [label, error, deferred] of [
  ["deterministic init failure", new ControlError("command_failed", "unpack failed"), false],
  ["timeout with confirmed process cleanup", new ControlError("command_timeout", "init timeout", {
    reaped: true, pipes_drained: true, process_group_killed: true, process_group_cleanup_error: null,
  }), true],
  ["unverified init process cleanup", new ControlError("command_failed", "cleanup unverified", {
    process_group_killed: false, process_group_cleanup_error: "process_owner_boundary_unavailable",
  }), true],
] as const) {
  scenario(`${label} preserves the appropriate absence and journal fence`, async f => {
    const request = f.request("acquire", { public_key: KEY });
    f.control.incus.init = async () => { throw error; };
    await expect_error(error.code, () => f.control.dispatch(request));
    expect(task(f).state).toBe(deferred ? "closing" : "absent");
    expect(task(f).reservation_bytes > 0).toBe(deferred);
    expect(f.store.operation(request.operation_id)!.outcome).toBe(deferred ? "pending" : "error");
    if (deferred) {
      expect(parse_json(task(f).last_error).native_init_absence_retry).toBe(true);
      f.advance(2);
      const result = await f.call("admin-reconcile", { session: null, apply: true });
      expect(result.dry_run).toBe(false);
      if (error.detail?.process_group_killed === false) {
        expect(task(f).state).toBe("closing");
        expect(task(f).reservation_bytes).toBeGreaterThan(0);
        expect(f.store.operation(request.operation_id)!.outcome).toBe("pending");
      } else {
        expect(task(f).state).toBe("absent");
        expect(task(f).reservation_bytes).toBe(0);
        expect(f.store.operation(request.operation_id)!.outcome).toBe("error");
      }
    }
  });
}

scenario("pre-native project guards do not consume or reclaim cache", async f => {
  const cached = await cache(f, "cache");
  for (const key of ["limits.instances", "limits.virtual-machines"]) {
    f.runner.project_limits = { [key]: "8" };
    const before = f.runner.calls.length;
    await expect_error("project_instance_limits_conflict", () =>
      f.acquire({ session: `blocked-${key}` }));
    expect(changes(f, before)).toEqual([]);
    expect(f.store.get_task({ session_id: `blocked-${key}` })).toBeNull();
    expect(task(f, "cache")).toEqual(cached);
    expect(instance(f, "cache").status).toBe("Stopped");
  }
});

scenario("active reuse preserves allocation and deadline without implicit restart", async f => {
  const request = f.request("acquire", {
    public_key: KEY, cpu: 8, memory_mib: 12288, disk_gib: 40,
  });
  const first = await f.control.dispatch(request);
  expect(first.result.resources).toEqual({
    cpu: 8, memory_mib: 12288, disk_gib: 40,
  });
  expect(first.result.guest_filesystem.virtual_bytes).toBe(40 * GIB);

  f.advance(20);
  const before = f.runner.calls.length;
  const replay = await f.control.dispatch(request);
  const reused = await f.acquire();
  expect(replay.outcome).toBe("replayed");
  expect(replay.server_time).not.toBe(first.server_time);
  expect(replay.result.lease_expires_at).toBe(first.result.lease_expires_at);
  expect(reused.lease_expires_at).toBe(first.result.lease_expires_at);
  expect(reused.generation).toBe(1);
  await expect_error("resource_config_mismatch", () => f.acquire({ cpu: 4 }));
  await expect_error("resource_config_mismatch", () => f.acquire({ disk_gib: 20 }));
  expect(changes(f, before)).toEqual([]);

  instance(f).status = "Stopped";
  expect((await f.call("status")).guest_available).toBe(false);
  expect((await f.acquire()).effective_lease).toBe(true);
  expect(changes(f, before)).toEqual([]);
});

scenario("release is lazy and reacquire advances generation while retaining sizes", async f => {
  const first = await f.acquire({ cpu: 4, memory_mib: 8192, disk_gib: 40 });
  await f.call("port-add", { name: "web", vm_port: 8080 });
  f.advance(300);
  const released = await f.call("release");
  expect(released.state).toBe("cached");
  expect(released.ports).toEqual([]);
  expect(released.lease_expires_at).toBeNull();
  expect(instance(f).status).toBe("Stopped");

  f.advance(3600);
  const again = await f.call("release");
  expect(again.inactive_since).toBe(released.inactive_since);
  expect(again.cleanup_after).toBe(released.cleanup_after);

  await expect_error("resize_not_growth", () => f.acquire({ disk_gib: 20 }));
  const resumed = await f.acquire();
  expect(resumed.task_id).toBe(first.task_id);
  expect(resumed.instance_name).toBe(first.instance_name);
  expect(resumed.generation).toBe(2);
  expect(resumed.resources).toEqual(first.resources);
  expect(resumed.inactive_since).toBeNull();
  expect(resumed.ports.map((p: Data) => p.port_name)).toEqual(["ssh"]);
});

scenario("failed cached resume preserves cache age", async f => {
  const cached = await cache(f, "cache");
  f.advance(3600);
  f.runner.failure = async argv => argv[3] === "start";
  await expect_error("command_failed", () => f.acquire({ session: "cache" }));
  expect(task(f, "cache").inactive_since).toBe(cached.inactive_since);
  expect(instance(f, "cache").status).toBe("Stopped");
  f.runner.failure = null;
  const resumed = await f.acquire({ session: "cache" });
  expect(resumed.task_id).toBe(cached.task_id);
  expect(resumed.generation).toBeGreaterThan(cached.generation);
});

scenario("resize plans do not mutate; explicit disk growth preserves the lease", async f => {
  const first = await f.acquire({ disk_gib: 30 });
  const durable = task(f);
  const before = f.runner.calls.length;

  const plan = await f.call("resize", { cpu: 3, disk_gib: 40 });
  expect(plan.applied).toBe(false);
  expect(plan.plan).toBe("restart-required-for-resource-change");
  expect(task(f)).toEqual(durable);
  expect(changes(f, before)).toEqual([]);

  await expect_error("resize_not_growth", () =>
    f.call("resize", { disk_gib: 20, allow_restart: true }));
  expect(task(f)).toEqual(durable);
  expect(changes(f, before)).toEqual([]);

  const grown = await f.call("resize", { disk_gib: 40, allow_restart: true });
  expect(grown.applied).toBe(true);
  expect(grown.resources.disk_gib).toBe(40);
  expect(grown.guest_filesystem.capacity.verified).toBe(true);
  expect(grown.guest_filesystem.virtual_bytes).toBe(40 * GIB);
  expect(grown.lease_expires_at).toBe(first.lease_expires_at);
  expect(changes(f, before).filter(c => c[0] === "stop")).toHaveLength(1);
  expect(changes(f, before).filter(c => c[0] === "start")).toHaveLength(1);
});

scenario("same-size disk verification succeeds without restarting", async f => {
  const first = await f.acquire();
  f.runner.guest_growth_results.push(
    f.guest_growth_result({ status: "already_grown", attempts: 0 }),
  );
  const before = f.runner.calls.length;
  const result = await f.call("resize", { disk_gib: 20 });
  expect(result.applied).toBe(true);
  expect(result.guest_filesystem.status).toBe("already_grown");
  expect(result.lease_expires_at).toBe(first.lease_expires_at);
  expect(changes(f, before)).toEqual([]);
});

for (const failure of ["native", "guest", "malformed", "truncated", "transport"]) {
  scenario(`disk growth ${failure} failure closes access without claiming success`, async f => {
    const first = await f.acquire();
    if (failure === "native") {
      f.runner.failure = async argv => argv.includes("size=40GiB");
    } else if (failure === "guest") {
      f.runner.guest_growth_results.push(
        f.guest_growth_failure({ disk_gib: 40 }),
      );
    } else if (failure === "malformed") {
      const report = f.guest_growth_result({ disk_gib: 40 });
      const payload = parse_json(report.stdout);
      payload.capacity.parent = "/dev/foreign";
      f.runner.guest_growth_results.push(
        command_result({ stdout: canonical_json(payload) }),
      );
    } else if (failure === "truncated") {
      f.runner.guest_growth_results.push(command_result({
        ...f.guest_growth_result({ disk_gib: 40 }),
        stdout_truncated: true,
      }));
    } else {
      f.runner.guest_growth_results.push(
        new ControlError("command_timeout", "fixture guest transport timeout"),
      );
    }

    const error = await expect_error("resize_partial", () =>
      f.call("resize", { disk_gib: 40, allow_restart: true }));
    expect(error.detail.cleanup.ok).toBe(true);
    expect(task(f).state).toBe("cached");
    expect(task(f).lease_expires_at).toBeNull();
    expect(task(f).ssh_host_port).toBeNull();
    expect(f.store.get_ports(first.task_id)).toEqual([]);
    expect(instance(f).status).toBe("Stopped");
    expect(parse_json(task(f).last_error).code).toBe("resize_partial");
  });
}

scenario("acquire rejects unverified guest growth before exposing SSH", async f => {
  f.runner.guest_growth_results.push(f.guest_growth_failure());
  await expect_error("guest_resize_unverified", () => f.acquire());
  expect(task(f).state).toBe("cached");
  expect(task(f).lease_expires_at).toBeNull();
  expect(f.store.get_ports(task(f).task_id)).toEqual([]);
  expect(instance(f).status).toBe("Stopped");
  expect(parse_json(task(f).last_error).code).toBe("acquire_partial");
});

scenario("live unchanged is a plan; shrink preserves boot, ports, disk and deadline", async f => {
  const first = await f.acquire();
  const durable = task(f);
  const before = f.runner.calls.length;
  const unchanged = await f.call("resize", {
    memory_mib: 4096, live_memory: true,
  });
  expect(unchanged).toMatchObject({ applied: false, plan: "unchanged" });
  expect(task(f)).toEqual(durable);
  expect(changes(f, before)).toEqual([]);

  f.resources.on_memory = async () => {
    throw new ControlError("resource_unverified", "fixture metrics unavailable");
  };
  const shrunk = await f.call("resize", {
    memory_mib: 2048, live_memory: true,
  });
  expect(shrunk.resources).toEqual({ cpu: 2, memory_mib: 2048, disk_gib: 20 });
  expect(shrunk.ports).toEqual(first.ports);
  expect(shrunk.lease_expires_at).toBe(first.lease_expires_at);
  expect(shrunk.verification.guest_boot_id)
    .toBe("00000000-0000-4000-8000-000000000001");
  expect(changes(f, before)).toEqual([
    ["config", "set", first.instance_name, "limits.memory=2048MiB"],
  ]);
  await expect_error("resource_unverified", () =>
    f.call("resize", { memory_mib: 3072, live_memory: true }));
});

scenario("live growth reserves maximum before acknowledgement and ignores memory pressure", async f => {
  await f.acquire();
  f.resources.memory_value.available_bytes = 0;
  const observed: number[] = [];
  f.runner.on_config_set = async () => {
    observed.push((await f.control._allocation_totals()).memory_mib);
    expect(parse_json(task(f).resources_json).memory_mib).toBe(4096);
  };
  const grown = await f.call("resize", { memory_mib: 8192, live_memory: true });
  expect(observed).toEqual([8192]);
  expect(grown.resources.memory_mib).toBe(8192);
  expect(grown.resource_reservation).toEqual({});
  expect(grown.verification.configured_memory_mib).toBe(8192);

  f.config.memory_budget_mib = 4096;
  f.config.memory_overcommit_ratio = 1;
  const before = f.runner.calls.length;
  await expect_error("resource_insufficient", () =>
    f.call("resize", { memory_mib: 12288, live_memory: true }));
  expect(changes(f, before)).toEqual([]);
  expect(parse_json(task(f).resources_json).memory_mib).toBe(8192);
});

scenario("another session cannot spend an unconfirmed live reduction", async f => {
  await f.acquire();
  f.config.memory_budget_mib = 4096;
  f.config.memory_overcommit_ratio = 1;
  f.runner.on_config_set = async () => {
    expect((await f.control._allocation_totals()).memory_mib).toBe(4096);
    await expect_error("resource_insufficient", () =>
      f.acquire({ session: "other", memory_mib: 2048 }));
  };
  await f.call("resize", { memory_mib: 2048, live_memory: true });
  expect(f.store.get_task({ session_id: "other" })).toBeNull();
  f.runner.on_config_set = null;
  await f.acquire({ session: "other", memory_mib: 2048 });
  expect((await f.control._allocation_totals()).memory_mib).toBe(4096);
});

scenario("partial live growth retains maximum reservation until explicit restart recovery", async f => {
  const first = await f.acquire();
  f.runner.on_config_set = async () => {
    throw new ControlError("command_timeout", "fixture update lost acknowledgement");
  };
  const request = f.request("resize", { memory_mib: 8192, live_memory: true });
  const before = f.runner.calls.length;
  await expect_error("resize_partial", () => f.control.dispatch(request));
  expect(instance(f).status).toBe("Running");
  expect(parse_json(task(f).resources_json).memory_mib).toBe(4096);
  expect((await f.control._allocation_totals()).memory_mib).toBe(8192);
  expect(f.store.get_ports(first.task_id)).toEqual(first.ports);
  expect(changes(f, before).some(c => ["stop", "start", "delete"].includes(c[0])))
    .toBe(false);
  expect(f.store.operation(request.operation_id)?._request_digest)
    .toBe(exact_digest(request));

  const calls = f.runner.calls.length;
  await expect_error("operation_incomplete", () => f.control.dispatch(request));
  expect(f.runner.calls).toHaveLength(calls);
  await expect_error("resize_incomplete", () =>
    f.call("resize", { memory_mib: 4096, live_memory: true }));

  f.runner.on_config_set = null;
  expect((await f.call("resize", { memory_mib: 4096 })).applied).toBe(false);
  const reserved: number[] = [];
  f.runner.on_config_set = async () => {
    reserved.push((await f.control._allocation_totals()).memory_mib);
  };
  const recovered = await f.call("resize", {
    memory_mib: 4096, allow_restart: true,
  });
  expect(reserved).toEqual([8192, 8192]);
  expect(recovered.resource_reservation).toEqual({});
  expect(recovered.resources.memory_mib).toBe(4096);
  expect(recovered.lease_expires_at).toBe(first.lease_expires_at);
});

scenario("boot discontinuity and native drift cannot reduce durable allocation", async f => {
  await f.acquire();
  instance(f).config["limits.memory"] = "3072MiB";
  const before = f.runner.calls.length;
  await expect_error("resize_unverified", () =>
    f.call("resize", { memory_mib: 2048, live_memory: true }));
  expect(changes(f, before)).toEqual([]);

  instance(f).config["limits.memory"] = "4096MiB";
  f.runner.on_config_set = async () => {
    instance(f).boot_id = "00000000-0000-4000-8000-000000000002";
  };
  await expect_error("resize_partial", () =>
    f.call("resize", { memory_mib: 2048, live_memory: true }));
  expect(instance(f).status).toBe("Running");
  expect((await f.control._allocation_totals()).memory_mib).toBe(4096);
  expect(parse_json(task(f).resources_json).memory_mib).toBe(4096);
});

// A single sequential test owns the process-wide Bun.sleep replacement.
test("balloon settlement retries once and rechecks every guard during the wait", async () => {
  const original_sleep = Bun.sleep;
  const cases = [
    "success", "second-failure", "boot", "configuration",
    "reservation", "expiry", "drain", "window",
  ];
  try {
    for (const boundary of cases) {
      const f = new Fixture();
      try {
        const first = await f.acquire();
        const before = f.runner.calls.length;
        let attempts = 0;
        let waits = 0;
        let during_wait: (() => Promise<void>) | undefined;

        f.runner.on_config_set = async (name, key) => {
          expect(key).toBe("limits.memory");
          attempts++;
          expect((await f.control._allocation_totals()).memory_mib).toBe(4096);
          if (attempts === 1 || boundary === "second-failure") {
            f.runner.instances[name].config["limits.memory"] = "4096MiB";
            during_wait = async () => {
              if (boundary === "boot") {
                instance(f).boot_id = "00000000-0000-4000-8000-000000000002";
              } else if (boundary === "configuration") {
                instance(f).config["limits.memory"] = "2048MiB";
              } else if (boundary === "reservation") {
                f.store.update_task(first.task_id, { resource_reservation_json: "{}" });
              } else if (boundary === "expiry") {
                f.advance(1800);
                await f.control._expire_preempt(first);
              } else if (boundary === "drain") {
                await f.call("drain-enter", { session: null });
              } else if (boundary === "window") {
                f.advance(1741);
              }
            };
            throw balloon_timeout();
          }
        };

        Bun.sleep = (async (milliseconds: number) => {
            expect(milliseconds).toBe(45_000);
            waits++;
            expect(during_wait).toBeDefined();
            await during_wait!();
            if (!["expiry", "window"].includes(boundary)) f.advance(45);
          }) as typeof Bun.sleep;

        if (boundary === "success") {
          const result = await f.call("resize", {
            memory_mib: 2048, live_memory: true,
          });
          expect(result.applied).toBe(true);
          expect(result.native_attempts.map((a: Data) => a.ok)).toEqual([false, true]);
          expect(result.resources.memory_mib).toBe(2048);
          expect(result.lease_expires_at).toBe(first.lease_expires_at);
        } else {
          await expect_error("resize_partial", () =>
            f.call("resize", { memory_mib: 2048, live_memory: true }));
          expect(parse_json(task(f).resources_json).memory_mib).toBe(4096);
        }
        expect(waits).toBe(1);
        expect(attempts).toBe(
          ["success", "second-failure"].includes(boundary) ? 2 : 1,
        );
        expect(changes(f, before).some(c => ["start", "delete"].includes(c[0])))
          .toBe(false);
        if (boundary !== "expiry") {
          expect(instance(f).status).toBe("Running");
          expect(changes(f, before).some(c => c[0] === "stop")).toBe(false);
        } else {
          expect(instance(f).status).toBe("Stopped");
          expect(task(f).state).toBe("closing");
        }
      } finally {
        Bun.sleep = original_sleep;
        f.close();
      }
    }
  } finally {
    Bun.sleep = original_sleep;
  }
});

scenario("invalid live modes, drain and short windows cannot change the guest", async f => {
  await f.acquire();
  const before = f.runner.calls.length;
  for (const fields of [
    { cpu: 2 },
    { disk_gib: 20 },
    { memory_mib: 2048, cpu: 2 },
    { memory_mib: 2048, allow_restart: true },
    { memory_mib: 2048, live_memory: "yes" },
  ]) {
    await expect_error("invalid_request", () =>
      f.call("resize", { live_memory: true, ...fields }));
  }
  await f.call("drain-enter", { session: null });
  await expect_error("draining", () =>
    f.call("resize", { memory_mib: 2048, live_memory: true }));
  expect(changes(f, before)).toEqual([]);
  expect(instance(f).config["limits.memory"]).toBe("4096MiB");
});

scenario("restart and live resize require their full remaining lease windows", async f => {
  await f.acquire();
  f.advance(1501); // 299 seconds remain; restart requires 300.
  const before = f.runner.calls.length;
  await expect_error("lease_window_short", () => f.call("restart"));
  await expect_error("lease_window_short", () =>
    f.call("resize", { cpu: 3, allow_restart: true }));
  f.advance(120); // 179 seconds remain; live adjustment requires 180.
  await expect_error("lease_window_short", () =>
    f.call("resize", { memory_mib: 2048, live_memory: true }));
  expect(changes(f, before)).toEqual([]);
});

scenario("expiry during live update cannot commit or reopen the active lease", async f => {
  const first = await f.acquire();
  f.runner.on_config_set = async () => {
    f.advance(1800);
    await f.control._expire_preempt(first);
  };
  await expect_error("resize_partial", () =>
    f.call("resize", { memory_mib: 8192, live_memory: true }));
  expect(task(f).state).toBe("closing");
  expect(instance(f).status).toBe("Stopped");
  expect((await f.call("status")).effective_lease).toBe(false);
  expect((await f.control._allocation_totals()).memory_mib).toBe(8192);
});

scenario("early expiry rearms; expired and stale timers cannot damage a new generation", async f => {
  const first = await f.acquire();
  expect((await timer(f, first)).result.reason).toBe("not-expired");
  expect([...f.systemd.scheduled.values()]).toContain(first.lease_expires_at);

  f.advance(1800);
  await expect_error("lease_expired", () => f.call("renew"));
  await expect_error("lease_expired", () => f.call("ssh-info"));
  await timer(f, first);
  expect(task(f).state).toBe("cached");
  expect(task(f).inactive_since).toBe(first.lease_expires_at);
  expect(instance(f).status).toBe("Stopped");

  const resumed = await f.acquire();
  const durable = task(f);
  const before = f.runner.calls.length;
  expect((await timer(f, first)).result.reason).toBe("stale-generation");
  expect(task(f)).toEqual(durable);
  expect(resumed.generation).toBe(2);
  expect(changes(f, before)).toEqual([]);
});

scenario("drain preserves active access while blocking admission, renewal and restart", async f => {
  const first = await f.acquire();
  await f.call("drain-enter", { session: null });
  const before = f.runner.calls.length;
  await expect_error("draining", () => f.acquire({ session: "new" }));
  await expect_error("draining", () => f.call("renew"));
  await expect_error("draining", () => f.call("restart"));
  expect((await f.acquire()).generation).toBe(first.generation);
  expect((await f.call("ssh-info")).port).toBe(first.ssh_host_port);
  expect(changes(f, before)).toEqual([]);
  expect((await f.call("release")).state).toBe("cached");
});

scenario("GC previews all eligible caches; age GC uses the exact threshold", async f => {
  const old = await cache(f, "old");
  f.advance(86400);
  const young = await cache(f, "young");
  const preview = await f.call("admin-gc", { session: null });
  expect(preview).toMatchObject({
    dry_run: true, selection: "eligible", minimum_idle_seconds: 0,
  });
  expect(preview.candidates.map((c: Data) => c.task_id).sort())
    .toEqual([old.task_id, young.task_id].sort());
  expect(Object.keys(f.runner.instances)).toHaveLength(2);

  const aged = await f.call("timer-age-gc", { session: null });
  expect(aged.minimum_idle_seconds).toBe(86400);
  expect(aged.candidates.map((c: Data) => c.task_id)).toEqual([old.task_id]);
  expect(task(f, "old").state).toBe("absent");
  expect(task(f, "young").state).toBe("cached");

  await f.call("admin-gc", { session: null, apply: true });
  expect(task(f, "young").state).toBe("absent");
});

scenario("GC protects active, transitional, unknown and unexpectedly exposed objects", async f => {
  const active = await f.acquire({ session: "active" });
  instance(f, "active").status = "Stopped";
  const closing = await cache(f, "closing");
  const exposed = await cache(f, "exposed");
  f.store.update_task(closing.task_id, { state: "closing" });
  instance(f, "exposed").expanded_devices.unaccounted = { type: "proxy" };
  f.runner.instances.foreign = { name: "foreign", status: "Stopped", config: {} };

  const before = f.runner.calls.length;
  expect((await f.call("admin-gc", { session: null, apply: true })).candidates)
    .toEqual([]);
  expect(changes(f, before)).toEqual([]);
  expect(f.runner.instances[active.instance_name]).toBeDefined();
  expect(f.runner.instances[exposed.instance_name]).toBeDefined();
  expect(f.runner.instances.foreign).toBeDefined();
  expect((await f.call("status", { session: "active" })).effective_lease).toBe(true);
});

scenario("GC preview and old successful replay cannot delete a later generation", async f => {
  await cache(f, "cache");
  const preview = await f.call("admin-gc", { session: null });
  expect(preview.candidates).toHaveLength(1);
  await f.acquire({ session: "cache" });
  expect((await f.call("admin-gc", { session: null, apply: true })).candidates)
    .toEqual([]);

  await f.call("release", { session: "cache" });
  const request = f.request("admin-gc", { session: null, apply: true });
  await f.control.dispatch(request);
  await cache(f, "cache");
  const before = f.runner.calls.length;
  await expect_error("operation_unverified", () => f.control.dispatch(request));
  expect(changes(f, before)).toEqual([]);
  expect(instance(f, "cache")).toBeDefined();
});

scenario("foreign ownership and failed deletion retain durable ownership", async f => {
  const cached = await cache(f, "cache");
  instance(f, "cache").config["user.agent-vm.generation"] = "999";
  let result = await f.call("admin-gc", { session: null, apply: true });
  expect(result.candidates[0].error.code).toBe("ownership_mismatch");
  expect(task(f, "cache").state).toBe("cached");

  instance(f, "cache").config["user.agent-vm.generation"] = String(cached.generation);
  f.runner.failure = async argv => argv[3] === "delete";
  result = await f.call("admin-gc", { session: null, apply: true });
  expect(result.candidates[0].error.code).toBe("command_failed");
  expect(task(f, "cache").state).toBe("deleting");
  expect(parse_json(task(f, "cache").last_error).detail.outcome_verified).toBe(false);

  f.runner.failure = null;
  await f.call("admin-reconcile", { session: null, apply: true });
  expect(task(f, "cache").state).toBe("absent");
  expect(task(f, "cache").last_error).toBeNull();
  expect(f.runner.instances[cached.instance_name]).toBeUndefined();
});

scenario("destroy retries a deleting cache with the same operation", async f => {
  const cached = await cache(f, "cache");
  const request = f.request("destroy", { session: "cache", operation: "destroy-cache" });
  f.runner.failure = async argv => argv[3] === "delete";
  await expect_error("command_failed", () => f.control.dispatch(request));
  expect(task(f, "cache").state).toBe("deleting");
  expect(f.store.operation("destroy-cache")?.outcome).toBe("pending");

  f.runner.failure = null;
  await f.control.dispatch(request);
  expect(task(f, "cache").state).toBe("absent");
  expect(f.store.operation("destroy-cache")?.outcome).toBe("success");
  expect(f.runner.instances[cached.instance_name]).toBeUndefined();
});

scenario("unconfirmed native deletion never frees durable ownership", async f => {
  const cached = await cache(f, "cache");
  f.runner.on_run = async argv => {
    if (argv[3] === "delete") return command_result();
  };
  const result = await f.call("admin-gc", { session: null, apply: true });
  expect(result.candidates[0].error.code).toBe("cleanup_incomplete");
  expect(task(f, "cache").state).toBe("deleting");
  expect(f.runner.instances[cached.instance_name]).toBeDefined();
});

scenario("reconcile preview is inert and apply repairs only owned cache state", async f => {
  const active = await f.acquire({ session: "active" });
  await cache(f, "cache");
  instance(f, "cache").status = "Running";
  f.runner.instances.foreign = { name: "foreign", status: "Running", config: {} };
  const before = f.runner.calls.length;
  const preview = await f.call("admin-reconcile", { session: null });
  expect(preview.dry_run).toBe(true);
  expect(changes(f, before)).toEqual([]);
  expect(instance(f, "cache").status).toBe("Running");

  await f.call("admin-reconcile", { session: null, apply: true });
  expect(instance(f, "cache").status).toBe("Stopped");
  expect(instance(f, "active").status).toBe("Running");
  expect(task(f, "active").lease_expires_at).toBe(active.lease_expires_at);
  expect(f.runner.instances.foreign.status).toBe("Running");
});

scenario("native cleanup failure stays closed until reconciliation succeeds", async f => {
  const first = await f.acquire();
  f.runner.failure = async argv => argv[0] === "conntrack";
  await expect_error("cleanup_incomplete", () => f.call("release"));
  expect(task(f).state).toBe("closing");
  expect(instance(f).status).toBe("Stopped");
  expect(task(f).lease_expires_at).toBeNull();
  expect(f.store.get_ports(first.task_id).length).toBeGreaterThan(0);
  expect(f.systemd.retries.length).toBeGreaterThan(0);

  f.runner.failure = null;
  await f.call("admin-reconcile", { session: null, apply: true });
  expect(task(f).state).toBe("cached");
  expect(f.store.get_ports(first.task_id)).toEqual([]);
});

scenario("operation digest includes request identity and pending replay has no effects", async f => {
  await f.acquire();
  const request = f.request("port-add", { name: "web", vm_port: 8080 });
  const crash = new Error("fixture process loss");
  f.runner.on_run = async argv => {
    if (
      argv[3] === "config" && argv[4] === "device" &&
      argv[5] === "add"
    ) throw crash;
  };
  await expect(f.control.dispatch(request)).rejects.toBe(crash);
  const saved = f.store.operation(request.operation_id);
  expect(saved).toEqual({
    outcome: "pending", _request_digest: exact_digest(request),
  });

  f.runner.on_run = null;
  const next = restarted(f);
  const before = f.runner.calls.length;
  await expect_error("operation_incomplete", () => next.dispatch(request));
  await expect_error("operation_conflict", () =>
    next.dispatch({ ...request, vm_port: 8081 }));
  await expect_error("operation_conflict", () =>
    next.dispatch(f.request("port-add", {
      operation: request.operation_id, name: "web", vm_port: 8080,
    })));
  expect(changes(f, before)).toEqual([]);
  expect(f.store.operation(request.operation_id)).toEqual(saved);
});

scenario("renewal committed before a lost response replays after controller restart exactly once", async f => {
  const first = await f.acquire();
  const request = f.request("renew", { operation: "renew-once" });
  f.systemd.on_schedule = async () => {
    throw new ControlError("timer_failed", "fixture lost renewal response");
  };
  await expect_error("timer_failed", () => f.control.dispatch(request));

  const committed = task(f).lease_expires_at;
  expect(Date.parse(committed) - Date.parse(first.lease_expires_at)).toBe(900_000);
  const saved = f.store.operation(request.operation_id)!;
  expect(saved.outcome).toBe("success");
  expect(saved._request_digest).toBe(exact_digest(request));
  expect(saved.result.lease_expires_at).toBe(committed);
  expect(saved.response).toEqual({ schema: 1, semantic_revision: 1 });

  f.systemd.on_schedule = null;
  f.advance(10);
  const next = restarted(f);
  const replay = await next.dispatch(request);
  expect(replay.outcome).toBe("replayed");
  expect(replay.result.lease_expires_at).toBe(committed);
  expect(task(f).lease_expires_at).toBe(committed);
  expect([...f.systemd.scheduled.values()]).toContain(committed);
  expect(f.store.operation(request.operation_id)).toEqual(saved);

  await expect_error("operation_conflict", () =>
    next.dispatch(f.request("renew", { operation: request.operation_id })));
  expect(task(f).lease_expires_at).toBe(committed);
});

scenario("status proves committed and pending acquire recovery identities", async f => {
  const active = await f.acquire();
  const committed = f.request("status");
  committed.operation_id = "op-1";
  const committed_result = (await f.control.dispatch(committed)).result;
  expect(committed_result.recovery_operation_id).toBe("op-1");

  const pending_request = f.request("acquire", {
    operation: "pending-status",
    generation: active.generation,
  });
  f.store.record_operation(
    pending_request.operation_id,
    "acquire",
    "thread-a",
    active.generation,
    {
      outcome: "pending",
      _request_digest: exact_digest(pending_request),
      task_id: active.task_id,
      task_generation: active.generation,
      acquire_resources: active.resources,
    },
  );
  const pending_status = f.request("status");
  pending_status.operation_id = "pending-status";
  expect((await f.control.dispatch(pending_status)).result.partial_operation_id)
    .toBe("pending-status");
});

scenario("active acquire converges matching old pending operations and preserves conflicts", async f => {
  const active = await f.acquire();
  const old_request = f.request("acquire", {
    operation: "old-acquire",
    generation: 0,
  });
  f.store.record_operation(
    old_request.operation_id,
    "acquire",
    "thread-a",
    0,
    {
      outcome: "pending",
      _request_digest: exact_digest(old_request),
      acquire_resources: active.resources,
    },
  );

  const before = f.runner.calls.length;
  const reused = await f.acquire();
  expect(reused.effective_lease).toBe(true);
  expect(changes(f, before)).toEqual([]);
  expect(f.store.operation("old-acquire")?.outcome).toBe("success");

  const conflicting = f.request("acquire", {
    operation: "conflicting-acquire",
    generation: active.generation,
  });
  f.store.record_operation(
    conflicting.operation_id,
    "acquire",
    "thread-a",
    active.generation,
    {
      outcome: "pending",
      _request_digest: exact_digest(conflicting),
      task_id: active.task_id,
      task_generation: active.generation,
      acquire_resources: { cpu: 8, memory_mib: 4096, disk_gib: 20 },
    },
  );
  await expect_error("operation_conflict", () => f.acquire());
  expect(f.store.operation("conflicting-acquire")?.outcome).toBe("pending");
  await f.call("release");
  expect(f.store.operation("conflicting-acquire")?.error.code).toBe("lease_released");
});

scenario("release and expiry cancel every matching pending acquire and retain the marker", async f => {
  const active = await f.acquire();
  for (const operation of ["pending-one", "pending-two"]) {
    const request = f.request("acquire", {
      operation,
      generation: active.generation,
    });
    f.store.record_operation(
      operation,
      "acquire",
      "thread-a",
      active.generation,
      {
        outcome: "pending",
        _request_digest: exact_digest(request),
        task_id: active.task_id,
        task_generation: active.generation,
        acquire_resources: active.resources,
      },
    );
  }

  const released = await f.call("release");
  expect(released.state).toBe("cached");
  expect(parse_json(task(f).last_error)).toMatchObject({
    pending_acquire_cancellation: true,
    pending_acquire_operation_ids: ["pending-one", "pending-two"],
  });
  expect(f.store.operation("pending-one")?.error.code).toBe("lease_released");
  expect(f.store.operation("pending-two")?.error.code).toBe("lease_released");

  const next = await f.acquire();
  const pending = f.request("acquire", {
    operation: "expiry-pending",
    generation: next.generation,
  });
  f.store.record_operation(
    pending.operation_id,
    "acquire",
    "thread-a",
    next.generation,
    {
      outcome: "pending",
      _request_digest: exact_digest(pending),
      task_id: next.task_id,
      task_generation: next.generation,
      acquire_resources: next.resources,
    },
  );
  f.advance(1800);
  await timer(f, next);
  expect(task(f).state).toBe("cached");
  expect(f.store.operation("expiry-pending")?.error.code).toBe("lease_released");
  expect(parse_json(task(f).last_error)).toMatchObject({
    pending_acquire_cancellation: true,
    pending_acquire_operation_ids: ["expiry-pending"],
  });
});
