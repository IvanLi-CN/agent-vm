import { expect, test } from "bun:test";
import { availableParallelism } from "node:os";

import { Control } from "../scripts/agent_vm_control.ts";
import { with_lock } from "../scripts/control-native.ts";
import { CTResources } from "../scripts/control-observation.ts";
import {
  allocation_limit,
  canonical_json,
  iso_time,
  parse_json,
  type Data,
} from "../scripts/control-types.ts";
import {
  ControlError,
  Fixture,
  GIB,
  StateStore,
  command_result,
  expect_error,
  type FixtureOptions,
} from "./control-fixture.ts";

// Offline native-boundary coverage; this does not establish Incus acceptance.
// Exercise reservations directly where transitional inventory would otherwise
// enter the 30-second admission retry loop. No memory-settlement paths are used.
const TIMEOUT = 4000;
const CGROUP = "/sys/fs/cgroup";
const CGROUP_MOUNT =
  "42 35 0:28 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n";
const LXCFS_MOUNT =
  "12 1 0:9 /meminfo /proc/meminfo rw - fuse.lxcfs lxcfs rw\n";
const PSI =
  "some avg10=12.34 avg60=5.00 avg300=1.00 total=100\n" +
  "full avg10=7.89 avg60=3.00 avg300=0.50 total=50\n";

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
  }, TIMEOUT);
}

class ObservedResources extends CTResources {
  readonly reads: string[] = [];

  constructor(readonly values: Record<string, string>) {
    super();
  }

  override _read(path: string): string {
    this.reads.push(path);
    if (!Object.hasOwn(this.values, path)) {
      throw new ControlError(
        "resource_unverified",
        "fixture metric missing",
        { metric: path },
      );
    }
    return this.values[path]!;
  }
}

function cpu_metrics(
  values: Record<string, string> = {},
): Record<string, string> {
  return {
    "/proc/self/mountinfo": CGROUP_MOUNT,
    "/proc/self/cgroup": "0::/\n",
    [`${CGROUP}/cpu.max`]: "max 100000",
    [`${CGROUP}/cpu.pressure`]: PSI,
    ...values,
  };
}

function memory_metrics(
  values: Record<string, string> = {},
): Record<string, string> {
  return {
    "/proc/self/mountinfo": "",
    [`${CGROUP}/memory.max`]: String(4 * GIB),
    [`${CGROUP}/memory.current`]: String(3 * GIB),
    [`${CGROUP}/memory.stat`]: "inactive_file 0\n",
    ...values,
  };
}

function task(f: Fixture, session: string): Data {
  const value = f.store.get_task({ session_id: session });
  expect(value).not.toBeNull();
  return value!;
}

async function cache(f: Fixture, session: string): Promise<Data> {
  await f.acquire({ session });
  await f.call("release", { session });
  return task(f, session);
}

function candidate(control: Control, session: string, now: Date): Data {
  return {
    ...control._base_task(session, 1),
    lease_expires_at: iso_time(new Date(now.getTime() + 1800_000)),
  };
}

function mutations(f: Fixture, since: number): string[][] {
  return f.runner.calls.slice(since).flatMap(([argv]) => {
    if (argv.slice(0, 3).join(" ") !== "incus --project agents") {
      return [];
    }
    const command = argv.slice(3);
    if (["init", "start", "stop", "delete"].includes(command[0]!)) {
      return [command];
    }
    if (
      command[0] === "config" &&
      (
        command[1] === "set" ||
        command[1] === "device" &&
        ["add", "set", "override", "remove"].includes(command[2]!)
      )
    ) {
      return [command];
    }
    return [];
  });
}

function deletions(f: Fixture, since: number): string[] {
  return mutations(f, since)
    .filter(command => command[0] === "delete")
    .map(command => command[1]!);
}

function pressure(f: Fixture, full: number): void {
  f.resources.cpu_pressure_value.some.avg10 = full;
  f.resources.cpu_pressure_value.full.avg10 = full;
}

test("CPU capacity floors quotas and observes every visible ancestor", () => {
  const observed = new ObservedResources(cpu_metrics({
    "/proc/self/cgroup": "0::/system.slice/agent-vm\n",
    [`${CGROUP}/system.slice/agent-vm/cpu.max`]: "max 100000",
    [`${CGROUP}/system.slice/cpu.max`]: "150000 100000",
    [`${CGROUP}/cpu.max`]: "750000 100000",
  }));

  expect(observed.cpu()).toBe(Math.min(availableParallelism(), 1));
  expect(observed.reads).toEqual([
    "/proc/self/mountinfo",
    "/proc/self/cgroup",
    `${CGROUP}/system.slice/agent-vm/cpu.max`,
    `${CGROUP}/system.slice/cpu.max`,
    `${CGROUP}/cpu.max`,
  ]);

  const fractional = new ObservedResources(cpu_metrics({
    [`${CGROUP}/cpu.max`]: "99999 100000",
  }));
  expect(fractional.cpu()).toBe(0);
}, TIMEOUT);

for (const quota of [
  "",
  "max",
  "max 0",
  "0 100000",
  "-1 100000",
  "1.5 100000",
  "100000 100000 extra",
  `${"9".repeat(4301)} 100000`,
]) {
  test(`CPU rejects malformed quota ${quota.slice(0, 30)}`, async () => {
    const observed = new ObservedResources(cpu_metrics({
      [`${CGROUP}/cpu.max`]: quota,
    }));
    await expect_error("resource_unverified", () => observed.cpu());
  }, TIMEOUT);
}

for (const [label, mount, path] of [
  ["missing mount", "", "0::/\n"],
  ["wrong filesystem", CGROUP_MOUNT.replace("cgroup2", "tmpfs"), "0::/\n"],
  ["subtree mount", CGROUP_MOUNT.replace("/ /sys", "/nested /sys"), "0::/\n"],
  ["duplicate mount", CGROUP_MOUNT + CGROUP_MOUNT, "0::/\n"],
  ["legacy hierarchy", CGROUP_MOUNT, "1:cpu:/slice\n"],
  ["mixed hierarchy", CGROUP_MOUNT, "0::/\n1:cpu:/slice\n"],
  ["relative path", CGROUP_MOUNT, "0::slice\n"],
  ["parent traversal", CGROUP_MOUNT, "0::/slice/../other\n"],
  ["empty component", CGROUP_MOUNT, "0::/slice//other\n"],
] as const) {
  test(`CPU and PSI fail closed for ${label}`, async () => {
    for (const metric of ["cpu", "cpu_pressure"] as const) {
      const observed = new ObservedResources(cpu_metrics({
        "/proc/self/mountinfo": mount,
        "/proc/self/cgroup": path,
      }));
      await expect_error("resource_unverified", () => observed[metric]());
      expect(observed.reads.some(value => value.endsWith("/cpu.max"))).toBe(false);
      expect(observed.reads.some(value => value.endsWith("/cpu.pressure"))).toBe(false);
    }
  }, TIMEOUT);
}

test("a missing ancestor quota cannot fall back to a child or host capacity", async () => {
  const observed = new ObservedResources(cpu_metrics({
    "/proc/self/cgroup": "0::/slice/agent-vm\n",
    [`${CGROUP}/slice/agent-vm/cpu.max`]: "max 100000",
  }));
  await expect_error("resource_unverified", () => observed.cpu());
  expect(observed.reads.at(-1)).toBe(`${CGROUP}/slice/cpu.max`);
}, TIMEOUT);

test("PSI reads only the verified current cgroup and preserves scope", () => {
  const observed = new ObservedResources(cpu_metrics({
    "/proc/self/cgroup": "0::/slice/agent-vm\n",
    [`${CGROUP}/slice/agent-vm/cpu.pressure`]: PSI,
  }));
  expect(observed.cpu_pressure()).toEqual({
    scope: "ct-cpu-pressure",
    sources: ["cgroupfs"],
    mountpoint: CGROUP,
    mount_root: "/",
    filesystem: "cgroup2",
    cgroup_path: "/slice/agent-vm",
    metric: `${CGROUP}/slice/agent-vm/cpu.pressure`,
    some: { avg10: 12.34, avg60: 5, avg300: 1, total: 100 },
    full: { avg10: 7.89, avg60: 3, avg300: 0.5, total: 50 },
  });
  expect(observed.reads).toEqual([
    "/proc/self/mountinfo",
    "/proc/self/cgroup",
    `${CGROUP}/slice/agent-vm/cpu.pressure`,
  ]);
}, TIMEOUT);

for (const [label, raw] of [
  ["missing full", PSI.split("\n")[0]!],
  ["duplicate scope", PSI + PSI.split("\n")[0]!],
  ["unknown scope", PSI + "other avg10=0 avg60=0 avg300=0 total=0\n"],
  ["duplicate field", PSI.replace("avg10=12.34", "avg10=12.34 avg10=1")],
  ["missing field", PSI.replace("avg60=5.00 ", "")],
  ["extra field", PSI.replace("total=100", "total=100 extra=0")],
  ["fractional total", PSI.replace("total=100", "total=1.5")],
  ["non-finite average", PSI.replace("avg10=12.34", "avg10=NaN")],
  ["negative average", PSI.replace("avg10=12.34", "avg10=-1")],
  ["excess average", PSI.replace("avg10=12.34", "avg10=101")],
  ["contradictory full", PSI.replace("avg10=7.89", "avg10=13")],
] as const) {
  test(`PSI rejects ${label}`, async () => {
    const observed = new ObservedResources(cpu_metrics({
      [`${CGROUP}/cpu.pressure`]: raw,
    }));
    const error = await expect_error(
      "resource_unverified",
      () => observed.cpu_pressure(),
    );
    expect(error.detail.metric).toBe(`${CGROUP}/cpu.pressure`);
  }, TIMEOUT);
}

test("finite memory reclaims inactive_file and conservatively bounds LXCfs", () => {
  const values = memory_metrics({
    [`${CGROUP}/memory.max`]: String(16 * GIB),
    [`${CGROUP}/memory.current`]: String(10 * GIB),
    [`${CGROUP}/memory.stat`]: `anon 1\ninactive_file ${4 * GIB}\n`,
    "/proc/self/mountinfo": LXCFS_MOUNT,
    "/proc/meminfo":
      `MemTotal: ${12 * GIB / 1024} kB\n` +
      `MemAvailable: ${3 * GIB / 1024} kB\n`,
  });

  const observed = new ObservedResources(values);
  expect(observed.memory()).toMatchObject({
    limit_bytes: 12 * GIB,
    available_bytes: 3 * GIB,
    current_bytes: 10 * GIB,
    sources: ["cgroup-v2", "lxcfs"],
  });

  const without_lxcfs = new ObservedResources({
    ...values,
    "/proc/self/mountinfo": "",
  });
  expect(without_lxcfs.memory()).toMatchObject({
    limit_bytes: 16 * GIB,
    available_bytes: 10 * GIB,
    sources: ["cgroup-v2"],
  });
  expect(without_lxcfs.reads).not.toContain("/proc/meminfo");
}, TIMEOUT);

test("unlimited memory requires verified LXCfs before reading meminfo", async () => {
  const values = {
    [`${CGROUP}/memory.max`]: "max",
    "/proc/self/mountinfo": LXCFS_MOUNT,
    "/proc/meminfo": "MemTotal: 25165824 kB\nMemAvailable: 16777216 kB\n",
  };
  const observed = new ObservedResources(values);
  expect(observed.memory()).toMatchObject({
    limit_bytes: 24 * GIB,
    available_bytes: 16 * GIB,
    current_bytes: null,
    sources: ["lxcfs"],
  });
  expect(observed.reads.indexOf("/proc/self/mountinfo"))
    .toBeLessThan(observed.reads.indexOf("/proc/meminfo"));

  for (const mount of [
    "",
    LXCFS_MOUNT.replace("fuse.lxcfs", "proc"),
    LXCFS_MOUNT + LXCFS_MOUNT,
  ]) {
    const invalid = new ObservedResources({
      ...values,
      "/proc/self/mountinfo": mount,
    });
    await expect_error("resource_unverified", () => invalid.memory());
    expect(invalid.reads).not.toContain("/proc/meminfo");
  }
}, TIMEOUT);

for (const [label, overrides] of [
  ["zero limit", { [`${CGROUP}/memory.max`]: "0" }],
  ["negative current", { [`${CGROUP}/memory.current`]: "-1" }],
  ["current above limit", { [`${CGROUP}/memory.current`]: String(5 * GIB) }],
  ["missing inactive_file", { [`${CGROUP}/memory.stat`]: "anon 1\n" }],
  ["duplicate inactive_file", {
    [`${CGROUP}/memory.stat`]: "inactive_file 1\ninactive_file 2\n",
  }],
  ["inactive_file above current", {
    [`${CGROUP}/memory.stat`]: `inactive_file ${4 * GIB}\n`,
  }],
  ["oversized integer", { [`${CGROUP}/memory.current`]: "9".repeat(4301) }],
  ["invalid LXCfs units", {
    "/proc/self/mountinfo": LXCFS_MOUNT,
    "/proc/meminfo": "MemTotal: 4194304 MB\nMemAvailable: 1048576 kB\n",
  }],
  ["duplicate LXCfs metric", {
    "/proc/self/mountinfo": LXCFS_MOUNT,
    "/proc/meminfo":
      "MemTotal: 4194304 kB\nMemTotal: 4194304 kB\nMemAvailable: 1048576 kB\n",
  }],
  ["available above LXCfs total", {
    "/proc/self/mountinfo": LXCFS_MOUNT,
    "/proc/meminfo": "MemTotal: 1 kB\nMemAvailable: 2 kB\n",
  }],
] satisfies Array<[string, Record<string, string>]>) {
  test(`memory rejects ${label}`, async () => {
    const observed = new ObservedResources(memory_metrics(overrides));
    await expect_error("resource_unverified", () => observed.memory());
  }, TIMEOUT);
}

for (const limit of ["max", String(16 * GIB)]) {
  test(`memory ${limit} rejects current above effective LXCfs capacity`, async () => {
    const observed = new ObservedResources(memory_metrics({
      [`${CGROUP}/memory.max`]: limit,
      [`${CGROUP}/memory.current`]: String(14 * GIB),
      [`${CGROUP}/memory.stat`]: `inactive_file ${GIB}\n`,
      "/proc/self/mountinfo": LXCFS_MOUNT,
      "/proc/meminfo":
        `MemTotal: ${12 * GIB / 1024} kB\nMemAvailable: 1048576 kB\n`,
    }));
    await expect_error("resource_unverified", () => observed.memory());
  }, TIMEOUT);
}

test("memory arithmetic remains exact above 2^53 and rounds the threshold upward", () => {
  const limit = 9007199254740993n;
  const observed = new ObservedResources(memory_metrics({
    [`${CGROUP}/memory.max`]: String(limit),
    [`${CGROUP}/memory.current`]: "3",
    [`${CGROUP}/memory.stat`]: "inactive_file 1\n",
  }));
  expect(observed.memory()).toMatchObject({
    limit_bytes: limit,
    available_bytes: 9007199254740991,
    current_bytes: 3,
    threshold_bytes: 900719925474100,
  });

  expect(new ObservedResources(memory_metrics({
    [`${CGROUP}/memory.current`]: "0",
  })).memory()).toMatchObject({
    available_bytes: 4 * GIB,
    threshold_bytes: GIB,
  });
}, TIMEOUT);

test("optional OOM counters retain exact integers without invalidating capacity", () => {
  const observed = new ObservedResources(memory_metrics({
    [`${CGROUP}/memory.events`]:
      "low 0\noom 9007199254740993\noom_kill 1\noom_group_kill 0\n",
  }));
  expect(observed.memory().oom_events).toEqual({
    oom: 9007199254740993n,
    oom_kill: 1,
    oom_group_kill: 0,
  });

  for (const events of ["oom invalid\n", "oom 1\noom 2\n"]) {
    const invalid = new ObservedResources(memory_metrics({
      [`${CGROUP}/memory.events`]: events,
    }));
    expect(invalid.memory()).toMatchObject({
      limit_bytes: 4 * GIB,
      available_bytes: GIB,
      oom_events: null,
    });
  }
}, TIMEOUT);

scenario("overcommit policy uses exact decimal floors, including large integers", async f => {
  Object.assign(f.config, { cpu_budget: 15, memory_budget_mib: 12288 });
  expect((await f.control._resource_policy()).limits).toEqual({
    cpu: 60,
    memory_mib: 49152,
  });

  Object.assign(f.config, {
    cpu_overcommit_ratio: 1.1,
    memory_overcommit_ratio: 1.01,
  });
  expect((await f.control._resource_policy()).limits).toEqual({
    cpu: 16,
    memory_mib: 12410,
  });

  const base = 9007199254740993n;
  f.config.memory_budget_mib = base;
  f.resources.memory_value.limit_bytes = base * 1048576n;
  expect((await f.control._resource_policy()).limits.memory_mib)
    .toBe(9097271247288402n);

  expect(allocation_limit(base, 1.1)).toBe(9907919180215092n);
  expect(allocation_limit(15, 9007199254740993n))
    .toBe(135107988821114895n);
  expect(allocation_limit(3, 1e21)).toBe(3000000000000000000000n);

  for (const ratio of [true, "4", 0.9, NaN, Infinity, -Infinity]) {
    await expect_error(
      "config_invalid",
      () => allocation_limit(15, ratio),
    );
  }
});

scenario("the base budget must fit CT capacity, while the logical ceiling may exceed it", async f => {
  f.config.cpu_budget = 16;
  expect((await f.control._resource_policy()).limits.cpu).toBe(64);
  f.config.cpu_budget = 17;
  const before = f.runner.calls.length;
  await expect_error("resource_budget_invalid", () => f.acquire());
  expect(f.store.list_tasks()).toEqual([]);
  expect(mutations(f, before)).toEqual([]);
});

scenario("separate controls and DB connections atomically enforce the shared ceiling", async f => {
  Object.assign(f.config, {
    cpu_overcommit_ratio: 1,
    memory_overcommit_ratio: 1,
  });
  const stores: StateStore[] = [];

  // Both requests reach observation before either can commit a reservation.
  // This bounded barrier does not run inside an admission or SQLite lock.
  let arrivals = 0;
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  const deadline = setTimeout(release, 500);

  try {
    f.resources.on_cpu = async () => {
      arrivals += 1;
      if (arrivals === 2) release();
      await gate;
      return 16;
    };

    const controls = ["one", "two"].map(() => {
      const store = new StateStore(f.config.state_db);
      stores.push(store);
      return new Control(f.config, {
        store,
        runner: f.runner,
        resources: f.resources,
        systemd: f.systemd,
        clock: () => f.now,
      });
    });
    expect(stores[0]).not.toBe(stores[1]);

    const outcomes = await Promise.allSettled(controls.map((control, index) =>
      control._reserve_resources(
        candidate(control, `concurrent-${index}`, f.now),
        { cpu: 3, memory_mib: 4096, disk_gib: 20 },
      )
    ));

    expect(arrivals).toBe(2);
    expect(outcomes.filter(result => result.status === "fulfilled")).toHaveLength(1);
    const failures = outcomes.filter(result => result.status === "rejected");
    expect(failures).toHaveLength(1);
    const failure = failures[0]!;
    if (failure.status !== "rejected") throw new Error("Expected rejection");
    expect(failure.reason).toBeInstanceOf(ControlError);
    expect(failure.reason.code).toBe("resource_insufficient");
    expect(failure.reason.detail.projected.cpu).toBe(6);

    expect(f.store.list_tasks()).toHaveLength(1);
    const reserved = f.store.list_tasks()[0]!;
    f.store.update_task(reserved.task_id, {
      state: "closing",
      resources_json: canonical_json({
        cpu: 2,
        memory_mib: 6144,
        disk_gib: 20,
      }),
    });
    expect(f.control._allocation_totals()).toEqual({
      cpu: 3,
      memory_mib: 6144,
    });
    expect(f.control._allocation_totals(reserved.task_id)).toEqual({
      cpu: 0,
      memory_mib: 0,
    });
    expect(f.runner.instances).toEqual({});
  } finally {
    clearTimeout(deadline);
    release();
    f.resources.on_cpu = null;
    for (const store of stores) store.close();
  }
});

scenario("unaccounted running inventory immediately denies resource reservation", async f => {
  f.runner.instances.foreign = {
    name: "foreign",
    status: "Running",
    config: {},
    devices: {},
    expanded_devices: {},
  };
  const before = f.runner.calls.length;
  await expect_error("resource_unverified", () =>
    f.control._reserve_resources(
      candidate(f.control, "new", f.now),
      { cpu: 2, memory_mib: 4096, disk_gib: 20 },
    )
  );
  expect(f.store.list_tasks()).toEqual([]);
  expect(mutations(f, before)).toEqual([]);
  expect(f.runner.instances.foreign.status).toBe("Running");
});

scenario("native allocation drift denies admission without changing the guest", async f => {
  const active = await f.acquire();
  const native = f.runner.instances[active.instance_name]!;

  for (const [object, field, value] of [
    [native.config, "limits.cpu", "8"],
    [native.config, "limits.memory", "8192MiB"],
    [native.expanded_devices.root, "size", "40GiB"],
  ] satisfies Array<[Data, string, string]>) {
    const original = object[field];
    object[field] = value;
    try {
      const diagnostics = await f.control._resource_diagnostics();
      expect(diagnostics.verified).toBe(false);
      expect(diagnostics.error.code).toBe("resource_unverified");

      const before = f.runner.calls.length;
      await expect_error("resource_unverified", () =>
        f.control._reserve_resources(
          candidate(f.control, "new", f.now),
          { cpu: 2, memory_mib: 4096, disk_gib: 20 },
        )
      );
      expect(mutations(f, before)).toEqual([]);
      expect(f.store.list_tasks()).toHaveLength(1);
      expect(native.status).toBe("Running");
    } finally {
      object[field] = original;
    }
  }
});

scenario("corrupt durable resources keep diagnostics structured and deny reservation", async f => {
  const active = await f.acquire();
  const original = task(f, "thread-a");

  for (const update of [
    { resources_json: '{"cpu":2,"memory_mib":4096,"disk_gib":-1}' },
    { resource_reservation_json: "[]" },
    { resource_reservation_json: '{"cpu":3}' },
    { reservation_bytes: -1 },
    { state: "unknown" },
  ]) {
    f.store.update_task(active.task_id, update);
    try {
      const diagnostics = await f.control._resource_diagnostics();
      expect(diagnostics.verified).toBe(false);
      expect(diagnostics.error.code).toBe("resource_unverified");
      const before = f.runner.calls.length;
      await expect_error("resource_unverified", () =>
        f.control._reserve_resources(
          candidate(f.control, "new", f.now),
          { cpu: 2, memory_mib: 4096, disk_gib: 20 },
        )
      );
      expect(mutations(f, before)).toEqual([]);
    } finally {
      f.store.put_task(original);
    }
  }
});

scenario("resource failures preserve independently verified memory pressure", async f => {
  f.resources.memory_value.available_bytes = 0;
  f.runner.project_limits = { "limits.cpu": "16" };

  const diagnostics = await f.control._resource_diagnostics();
  expect(diagnostics.verified).toBe(false);
  expect(diagnostics.error.code).toBe("project_resource_limits_conflict");
  expect(diagnostics.memory_pressure).toBe(true);
  expect(diagnostics.memory.available_bytes).toBe(0);

  f.resources.on_memory = () => {
    throw new ControlError("resource_unverified", "missing memory metric");
  };
  const missing = await f.control._resource_diagnostics();
  expect(missing.verified).toBe(false);
  expect(missing.memory_pressure).toBeNull();
  expect(missing.error.code).toBe("resource_unverified");
});

scenario("capacity reclaim deletes oldest caches until the target watermark is met", async f => {
  const active = await f.acquire({ session: "active" });
  const oldest = await cache(f, "oldest");
  f.advance(1);
  const next = await cache(f, "next");
  f.advance(1);
  const spare = await cache(f, "spare");

  f.runner.free = 8 * GIB;
  const before = f.runner.calls.length;
  expect(await f.control._reclaim_capacity(4 * GIB))
    .toEqual([oldest.task_id, next.task_id]);
  expect(deletions(f, before)).toEqual([
    oldest.instance_name,
    next.instance_name,
  ]);
  expect(task(f, "oldest").state).toBe("absent");
  expect(task(f, "next").state).toBe("absent");
  expect(task(f, "spare").state).toBe("cached");
  expect(f.runner.instances[spare.instance_name]).toBeDefined();
  expect(f.runner.instances[active.instance_name].status).toBe("Running");
  expect(f.runner.free).toBe(48 * GIB);
});

scenario("the low watermark is inclusive and pending disk reservations count", async f => {
  const cached = await cache(f, "cache");
  f.runner.free = 19 * GIB;
  let before = f.runner.calls.length;
  expect(await f.control._reclaim_capacity(4 * GIB)).toEqual([]);
  expect(deletions(f, before)).toEqual([]);

  f.store.update_task(cached.task_id, { reservation_bytes: GIB });
  before = f.runner.calls.length;
  expect(await f.control._reclaim_capacity(4 * GIB))
    .toEqual([cached.task_id]);
  expect(deletions(f, before)).toEqual([cached.instance_name]);
});

scenario("capacity reclaim excludes the requesting cache and never deletes active leases", async f => {
  const active = await f.acquire({ session: "active" });
  const own = await cache(f, "own");
  f.advance(1);
  const other = await cache(f, "other");

  // An unexpectedly stopped guest still has an active durable lease.
  f.runner.instances[active.instance_name].status = "Stopped";
  f.runner.free = 8 * GIB;
  const before = f.runner.calls.length;

  expect(await f.control._reclaim_capacity(4 * GIB, own.task_id))
    .toEqual([other.task_id]);
  expect(deletions(f, before)).toEqual([other.instance_name]);
  expect(task(f, "active").state).toBe("active");
  expect(task(f, "own").state).toBe("cached");
  expect(f.runner.instances[active.instance_name]).toBeDefined();
  expect(f.runner.instances[own.instance_name]).toBeDefined();
});

scenario("capacity reclaim skips busy and newly exposed caches", async f => {
  const busy = await cache(f, "busy");
  f.advance(1);
  const exposed = await cache(f, "exposed");
  f.runner.instances[exposed.instance_name].expanded_devices.proxy = {
    type: "proxy",
  };
  f.runner.free = 8 * GIB;
  const before = f.runner.calls.length;

  await f.control._object_lock("session:busy", async () => {
    const error = await expect_error(
      "capacity_insufficient",
      () => f.control._reclaim_capacity(4 * GIB),
    );
    expect(error.detail.reclaimed).toEqual([]);
  });

  expect(deletions(f, before)).toEqual([]);
  expect(task(f, "busy").state).toBe("cached");
  expect(task(f, "exposed").state).toBe("cached");
  expect(f.runner.instances[busy.instance_name]).toBeDefined();
});

scenario("failed capacity deletion preserves ownership and does not continue deleting", async f => {
  const first = await cache(f, "first");
  f.advance(1);
  const later = await cache(f, "later");
  f.runner.free = 8 * GIB;
  f.runner.failure = argv => argv[3] === "delete";
  const before = f.runner.calls.length;

  const error = await expect_error(
    "reclaim_failed",
    () => f.control._reclaim_capacity(4 * GIB),
  );
  expect(error.detail.code).toBe("command_failed");
  expect(error.detail.operation_phase).toBe("cache-delete");
  expect(error.detail.outcome_verified).toBe(false);
  expect(deletions(f, before)).toEqual([first.instance_name]);
  expect(task(f, "first").state).toBe("deleting");
  expect(parse_json(task(f, "first").last_error)).toMatchObject({
    code: "command_failed",
    detail: { operation_phase: "cache-delete", outcome_verified: false },
  });
  expect(task(f, "later").state).toBe("cached");
  expect(f.runner.instances[first.instance_name]).toBeDefined();
  expect(f.runner.instances[later.instance_name]).toBeDefined();
  expect(f.runner.free).toBe(8 * GIB);
});

scenario("unconfirmed capacity deletion cannot claim reclaimed storage", async f => {
  const cached = await cache(f, "cache");
  f.runner.free = 8 * GIB;
  f.runner.on_run = argv => {
    if (argv[3] === "delete") return command_result();
  };

  const error = await expect_error(
    "reclaim_failed",
    () => f.control._reclaim_capacity(4 * GIB),
  );
  expect(error.detail.code).toBe("cleanup_incomplete");
  expect(task(f, "cache").state).toBe("deleting");
  expect(f.runner.instances[cached.instance_name]).toBeDefined();
  expect(f.runner.free).toBe(8 * GIB);
});

scenario("native cache deletion releases the global lock before awaiting Incus", async f => {
  await cache(f, "cache");
  f.runner.free = 8 * GIB;
  let checked = false;
  f.runner.on_run = async argv => {
    if (argv[3] === "delete") {
      // Nonblocking acquisition makes a lock regression fail immediately.
      await with_lock(f.control.lock_path, () => {
        checked = true;
      }, false);
    }
  };

  expect(await f.control._reclaim_capacity(4 * GIB)).toHaveLength(1);
  expect(checked).toBe(true);
});

scenario("logical quota denial precedes disk reclamation", async f => {
  const cached = await cache(f, "cache");
  Object.assign(f.config, {
    cpu_overcommit_ratio: 1,
    memory_overcommit_ratio: 1,
  });
  f.runner.free = 8 * GIB;
  const before = f.runner.calls.length;

  const error = await expect_error("resource_insufficient", () =>
    f.acquire({ session: "too-large", cpu: 8 })
  );
  expect(error.detail.dimensions).toContain("cpu");
  expect(mutations(f, before)).toEqual([]);
  expect(task(f, "cache").state).toBe("cached");
  expect(f.runner.instances[cached.instance_name]).toBeDefined();
});

scenario("CPU startup pressure is inclusive and only gates physical oversubscription", async f => {
  Object.assign(f.config, {
    cpu_budget: 15,
    cpu_overcommit_ratio: 4,
    cpu_startup_pressure_full_avg10_percent: 20,
  });
  pressure(f, 20);

  const active = await f.acquire({ session: "base", cpu: 16 });
  const before = f.runner.calls.length;
  const error = await expect_error("resource_pressure", () =>
    f.acquire({ session: "blocked", cpu: 2 })
  );
  expect(error.detail).toMatchObject({
    allocated_cpu: 16,
    requested_cpu: 2,
    projected_cpu: 18,
    ct_capacity_cpu: 16,
    physically_oversubscribed: true,
    threshold_full_avg10_percent: 20,
  });
  expect(mutations(f, before)).toEqual([]);
  expect(f.runner.instances[active.instance_name].status).toBe("Running");

  pressure(f, 19.99);
  expect((await f.acquire({ session: "below", cpu: 2 })).state).toBe("active");
});

scenario("restart pressure denial occurs before stopping an existing guest", async f => {
  f.config.cpu_budget = 15;
  const active = await f.acquire({ session: "base", cpu: 16 });
  await f.acquire({ session: "other", cpu: 2 });
  pressure(f, 20);
  const before = f.runner.calls.length;

  await expect_error("resource_pressure", () =>
    f.call("restart", { session: "base" })
  );
  expect(mutations(f, before)).toEqual([]);
  expect(f.runner.instances[active.instance_name].status).toBe("Running");
});

scenario("cached resume pressure projects the requested CPU rather than its old size", async f => {
  f.config.cpu_overcommit_ratio = 8;
  await f.acquire({ session: "active", cpu: 11 });
  const cached = await f.acquire({ session: "cache", cpu: 8 });
  await f.call("release", { session: "cache" });
  pressure(f, 20);

  const resumed = await f.acquire({ session: "cache", cpu: 4 });
  expect(resumed.resources.cpu).toBe(4);
  expect(f.runner.instances[cached.instance_name].config["limits.cpu"]).toBe("4");
  expect(f.runner.instances[cached.instance_name].status).toBe("Running");
});

scenario("unverified or nonlocal PSI blocks startup without reservation or native effects", async f => {
  const original = f.resources.cpu_pressure_value;
  for (const override of [
    { scope: "host-cpu-pressure" },
    { sources: ["procfs"] },
    { cgroup_path: "/slice", metric: `${CGROUP}/cpu.pressure` },
    { full: { avg10: 0 } },
  ]) {
    f.resources.cpu_pressure_value = { ...original, ...override };
    const before = f.runner.calls.length;
    await expect_error("resource_unverified", () =>
      f.control._reserve_resources(
        candidate(f.control, "unverified", f.now),
        { cpu: 2, memory_mib: 4096, disk_gib: 20 },
      )
    );
    expect(f.store.list_tasks()).toEqual([]);
    expect(mutations(f, before)).toEqual([]);
  }
});

scenario("memory pressure warns without blocking logically available new demand", async f => {
  Object.assign(f.config, {
    memory_budget_mib: 32768,
    memory_overcommit_ratio: 2,
  });
  f.resources.memory_value.available_bytes = 0;
  const admitted = await f.acquire();

  expect(admitted.state).toBe("active");
  const diagnostics = await f.control._resource_diagnostics();
  expect(diagnostics.verified).toBe(true);
  expect(diagnostics.memory_pressure).toBe(true);
  expect(diagnostics.remaining.memory_mib).toBe(61440);
});

scenario("failed observations preserve existing lease access but deny cached startup", async f => {
  const active = await f.acquire();
  f.resources.on_memory = () => {
    throw new ControlError("resource_unverified", "missing memory metric");
  };
  f.resources.on_cpu_pressure = () => {
    throw new ControlError("resource_unverified", "missing PSI metric");
  };

  const status = await f.call("status");
  expect(status.effective_lease).toBe(true);
  expect(status.admission.verified).toBe(false);

  const before = f.runner.calls.length;
  const reused = await f.acquire();
  expect(reused.generation).toBe(active.generation);
  expect(reused.lease_expires_at).toBe(active.lease_expires_at);
  expect(mutations(f, before)).toEqual([]);

  await f.call("renew");
  expect((await f.call("ssh-info")).generation).toBe(active.generation);
  expect((await f.call("release")).state).toBe("cached");

  const after_release = f.runner.calls.length;
  await expect_error("resource_unverified", () => f.acquire());
  expect(mutations(f, after_release)).toEqual([]);
  expect(task(f, "thread-a").state).toBe("cached");
});
