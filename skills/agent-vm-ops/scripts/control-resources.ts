import { randomBytes } from "node:crypto";
import { readFile, statfs } from "node:fs/promises";
import { availableParallelism } from "node:os";
import { posix } from "node:path";
import { performance } from "node:perf_hooks";
import { ControlCore } from "./control-core.ts";
import { normalize_cpu_psi, parse_cpu_psi } from "./control-observation.ts";
import {
  ControlError, GuestWaitTimeout, GIB, GUEST_READINESS_BUDGET_SECONDS,
  PORT_NAME_RE, TASK_STATES, JsonFloat,
  add, sub, mul, compare, integer, narrow, floor_div, allocation_limit,
  decimal_ratio, canonical_json, parse_json, resource_mapping,
  require_int, require_string, is_record, iso_time, parse_time,
  ipv4_to_int, int_to_ipv4,
  type Data, type Integer,
} from "./control-types.ts";

const TRANSITIONAL_ADMISSION_ERROR_MESSAGE = "transitional native instance blocks admission";
const TRANSITIONAL_ADMISSION_WAIT_SECONDS = 30;
const TRANSITIONAL_ADMISSION_POLL_SECONDS = 0.25;
const GUEST_READINESS_DIAGNOSTIC_SECONDS = 120;
const DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS = 5;
const DIAGNOSTIC_LOG_LIMIT = 8192;
const DIAGNOSTIC_LOG_TRUNCATION_MARKER = "\n[TRUNCATED BY DIAGNOSTIC LIMIT]";
const RESOURCE_DIMENSIONS = ["cpu", "memory_mib"] as const;
const PSI_FIELDS = ["avg10", "avg60", "avg300", "total"] as const;

const monotonic = (): number => performance.now() / 1000;
const rounded = (value: number): number => Math.round(value * 1000) / 1000;
const elapsed = (started: number): number => rounded(Math.max(0, monotonic() - started));
const equal = (a: any, b: any): boolean => canonical_json(a) === canonical_json(b);
const maximum = (a: Integer, b: Integer): Integer => compare(a, b) >= 0 ? a : b;
const minimum = (a: Integer, b: Integer): Integer => compare(a, b) <= 0 ? a : b;
const sum = (values: Integer[]): Integer => values.reduce<Integer>((a, b) => add(a, b), 0);
const quote = (value: string): string => encodeURIComponent(value).replace(
  /[!'()*]/g, ch => `%${ch.charCodeAt(0).toString(16).toUpperCase()}`,
);
const shell_quote = (value: string): string =>
  value === "" ? "''" : /^[A-Za-z0-9_/@%+=:,.-]+$/.test(value)
    ? value : "'" + value.replaceAll("'", "'\"'\"'") + "'";
const lines = (value: string): string[] =>
  value.split(/\r\n|[\n\r\v\f\u001c-\u001e\u0085\u2028\u2029]/);
const words = (value: string): string[] => value.trim() ? value.trim().split(/\s+/) : [];
const detail_of = (error: ControlError): Data => is_record(error.detail) ? { ...error.detail } : {};
const unverified = (message: string): Data => ({
  verified: false, error: { code: "resource_unverified", message },
});

function native_integer(value: any): Integer {
  if (typeof value === "bigint") return narrow(value);
  if (typeof value === "number" && Number.isSafeInteger(value)) return value;
  if (typeof value === "string" && /^[+-]?[0-9](?:_?[0-9])*$/.test(value.trim()))
    return narrow(BigInt(value.trim().replaceAll("_", "")));
  throw new TypeError("invalid integer");
}

function percentage(value: any, name: string): number {
  if (
    !(typeof value === "number" || typeof value === "bigint" || value instanceof JsonFloat) ||
    !Number.isFinite(Number(value)) || Number(value) < 0 || Number(value) > 100
  ) throw new ControlError("config_invalid", `${name} must be a finite percentage between zero and 100`);
  return Number(value);
}

export class ResourceControl extends ControlCore {
  [method: string]: any;

  async _read_text(path: string): Promise<string> {
    return String(this.resources.read_text
      ? await this.resources.read_text(path)
      : await readFile(path, "utf8"));
  }

  async _disk_usage(path: string): Promise<Data> {
    if (this.resources.disk_usage) return await this.resources.disk_usage(path);
    const usage = await statfs(path, { bigint: true });
    return {
      total: narrow(usage.blocks * usage.bsize),
      free: narrow(usage.bavail * usage.bsize),
    };
  }

  async _which(command: string): Promise<string | null> {
    return this.resources.which
      ? await this.resources.which(command)
      : Bun.which(command);
  }

  async _cpu_count(): Promise<Integer> {
    return this.resources.cpu_count
      ? await this.resources.cpu_count()
      : availableParallelism();
  }

  _task_for_session(session_id: string): Data {
    const task = this.store.get_task({ session_id });
    if (!task || task.state === "absent")
      throw new ControlError("task_missing", "no VM lease exists for this session");
    this._validate_task_state(task);
    return task;
  }

  _validate_task_state(task: Data): void {
    if (!TASK_STATES.has(task.state))
      throw new ControlError(
        "resource_unverified", "task state cannot be verified; reconciliation is required",
        { task_id: task.task_id, state: task.state },
      );
  }

  _check_generation(task: Data, request: Data): void {
    const generation = require_int(request.generation, "generation", 0);
    if (compare(generation, task.generation) !== 0)
      throw new ControlError(
        "generation_mismatch", "request generation does not match current task generation",
        { current_generation: narrow(integer(task.generation)) },
      );
  }

  _base_task(session_id: string, generation: Integer): Data {
    const now = iso_time(this.now()), task_id = "avm-" + randomBytes(8).toString("hex");
    return {
      task_id, session_id, generation, state: "preparing",
      instance_name: `agent-vm-${task_id}`,
      vm_ip: null, ssh_host_port: null, lease_expires_at: null,
      inactive_since: null, cleanup_after: null,
      created_at: now, updated_at: now, last_error: null,
      image_fingerprint: this.config.image_fingerprint,
    };
  }

  async _ownership(task: Data): Promise<Data> {
    const info = await this.incus.config_show(String(task.instance_name));
    if (!is_record(info))
      throw new ControlError("ownership_mismatch", "native instance inventory is not an object; refusing mutation");
    const { config, devices, expanded_devices } = info;
    const expected: Data = {
      "user.agent-vm.managed": "native-v1",
      "user.agent-vm.task": task.task_id,
      "user.agent-vm.session": task.session_id,
      "user.agent-vm.generation": String(task.generation),
    };
    const generations = new Set([String(task.generation)]);
    if (["preparing", "closing"].includes(task.state) && task.previous_generation != null)
      generations.add(String(task.previous_generation));
    if (
      !is_record(config) || !is_record(devices) || !is_record(expanded_devices) ||
      Object.values(devices).some(device => !is_record(device)) ||
      Object.values(expanded_devices).some(device => !is_record(device)) ||
      Object.entries(expected).some(([key, value]) =>
        key !== "user.agent-vm.generation" && config[key] !== value) ||
      !generations.has(config["user.agent-vm.generation"])
    ) throw new ControlError(
      "ownership_mismatch", "native instance identity differs from durable ownership; refusing mutation",
    );
    return info;
  }

  _native_operation_current(task: Data): Data {
    const current = this.store.get_task({ task_id: task.task_id });
    const expiry = current ? parse_time(current.lease_expires_at) : null;
    if (
      !current || compare(current.generation, task.generation) !== 0 ||
      current.instance_name !== task.instance_name ||
      !["preparing", "active"].includes(current.state) || !expiry || expiry <= this.now()
    ) throw new ControlError("lease_expired", "native operation was revoked before its effect");
    return current;
  }

  async _start_instance(task: Data): Promise<void> {
    await this._native_lock(task, async () => {
      const current = this._native_operation_current(task);
      await this._ownership(current);
      await this._network_guard();
      await this._verify_network_device(current);
      await this.incus.start(current.instance_name);
    });
  }

  async _stop_instance(task: Data): Promise<void> {
    await this._native_lock(task, async () => {
      const current = this._native_operation_current(task);
      await this._ownership(current);
      await this.incus.stop(current.instance_name, { force: true });
    });
  }

  _state_summary(task: Data): Data {
    this._validate_task_state(task);
    const result: Data = {
      task_id: task.task_id, session_id: task.session_id,
      generation: narrow(integer(task.generation)),
      state: task.state, instance_name: task.instance_name,
      vm_ip: task.vm_ip, ssh_host_port: task.ssh_host_port,
      lease_expires_at: task.lease_expires_at,
      inactive_since: task.inactive_since, cleanup_after: task.cleanup_after,
    };
    if (task.last_error) {
      try {
        const error = parse_json(task.last_error);
        if (!is_record(error)) throw new TypeError("invalid task error");
        result.last_error = { ...error };
      } catch {
        throw new ControlError("resource_unverified", "task error state cannot be verified");
      }
    }
    result.ports = this.store.get_ports(task.task_id);
    try {
      result.resources = resource_mapping(task.resources_json, "resources");
      result.resource_reservation = resource_mapping(
        task.resource_reservation_json, "reservation", { allow_empty: true },
      );
    } catch {
      if (
        task.state !== "absent" ||
        !["", "{}"].includes(String(task.resources_json ?? "").trim()) ||
        !["", "{}"].includes(String(task.resource_reservation_json ?? "").trim())
      ) throw new ControlError("resource_unverified", "task resource state cannot be verified");
      result.resources = null;
      result.resource_reservation = null;
      result.resource_state = {
        verified: false,
        error: { code: "resource_unverified", message: "absent task has no durable resource allocation" },
      };
    }
    const expiry = parse_time(task.lease_expires_at);
    result.effective_lease = task.state === "active" && expiry !== null && expiry > this.now();
    return result;
  }

  async _runtime_summary(task: Data): Promise<Data> {
    const result = this._state_summary(task);
    result.guest_available = false;
    if (task.state === "absent") {
      result.native_status = "absent";
      return result;
    }
    try {
      await this._ownership(task);
      result.native_status = (await this.incus.instance_info(task.instance_name)).status;
      result.guest_available = result.effective_lease && result.native_status.toLowerCase() === "running";
      if (result.effective_lease && !result.guest_available)
        result.guest_error = {
          code: "guest_unavailable", cause: "unknown",
          message: "effective lease has no running guest; inspect before explicit restart",
        };
    } catch (error) {
      if (!(error instanceof ControlError)) throw error;
      result.native_status = "unverified";
      result.guest_error = error.as_dict();
    }
    return result;
  }

  _requested_resources(request: Data, base: Data, { active_reuse = false }: Data = {}): Data {
    const target = { ...base };
    for (const [key, low, high] of [
      ["cpu", 1, 16], ["memory_mib", 512, 65536], ["disk_gib", 20, 4096],
    ] as const) {
      if (request[key] != null) target[key] = require_int(request[key], key, low, high);
    }
    if (!active_reuse && compare(target.disk_gib, base.disk_gib) < 0)
      throw new ControlError(
        "resize_not_growth", "virtual disk cannot shrink",
        { current: { ...base }, requested: target },
      );
    return target;
  }

  async _ensure_capacity_verified(): Promise<Data> {
    try {
      const pool = await this.incus.query(
        "/1.0/storage-pools/" + quote(String(this.config.pool)), { project: "default" },
      );
      if (!is_record(pool)) throw new TypeError("pool response is not an object");
      if (pool.name != null && pool.name !== this.config.pool)
        throw new TypeError("pool name does not match configuration");
      if (pool.driver !== "dir")
        throw new ControlError(
          "resource_unverified", "configured Incus pool is not a directory pool",
          { pool: this.config.pool, driver: pool.driver },
        );
      const source = is_record(pool.config) ? pool.config.source : null;
      const configured_path = posix.normalize(String(this.config.storage_path));
      if (typeof source !== "string" || !source || posix.normalize(source) !== configured_path)
        throw new ControlError(
          "resource_unverified", "Incus pool source differs from configured storage path",
          { pool: this.config.pool, pool_source: source, storage_path: configured_path },
        );
    } catch (error) {
      if (error instanceof ControlError) throw error;
      throw new ControlError(
        "resource_unverified", "Incus pool source cannot be verified", { pool: this.config.pool },
      );
    }
    let usage: Data;
    try { usage = await this._disk_usage(String(this.config.storage_path)); }
    catch { throw new ControlError("resource_unverified", "cannot measure CT Incus pool filesystem"); }
    let total: Integer, free: Integer;
    try {
      total = narrow(integer(usage.total));
      free = narrow(integer(usage.free));
      if (compare(total, 0) <= 0 || compare(free, 0) < 0 || compare(free, total) > 0)
        throw new TypeError("invalid capacity");
    } catch {
      throw new ControlError("resource_unverified", "invalid CT filesystem capacity");
    }
    const layer: Data = { label: "ct-filesystem", total, free, kind: "data" };
    layer.low = maximum(
      native_integer(this.config.disk_low_bytes ?? 10 * GIB), floor_div(mul(total, 15), 100),
    );
    layer.target = maximum(
      native_integer(this.config.disk_target_bytes ?? 20 * GIB), floor_div(mul(total, 25), 100),
    );
    if (compare(layer.target, total) >= 0 || compare(layer.low, layer.target) >= 0)
      throw new ControlError("watermarks_unreachable", "configure reachable CT storage watermarks", layer);
    return { scope: "ct-filesystem", observed_at: iso_time(this.now()), layers: [layer] };
  }

  async _storage_headroom(request_cost: Integer = 0): Promise<Data> {
    return { ...await this._ensure_capacity_verified(), request_cost };
  }

  _durable_reservation_bytes(value: any): Integer {
    if (value == null) return 0;
    try {
      const result = narrow(integer(value));
      if (compare(result, 0) < 0) throw new TypeError("negative reservation");
      return result;
    } catch {
      throw new TypeError("reservation_bytes must be a non-negative integer");
    }
  }

  _space_sufficient(
    facts: Data, cost: Integer, threshold: string, exclude_task_id: string | null = null,
  ): boolean {
    let reserved: Integer = 0;
    try {
      for (const task of this.store.list_tasks())
        if (task.task_id !== exclude_task_id)
          reserved = add(reserved, this._durable_reservation_bytes(task.reservation_bytes));
    } catch {
      throw new ControlError("resource_unverified", "durable disk reservation is malformed");
    }
    const demand = add(cost, reserved);
    return facts.layers.every((layer: Data) => compare(sub(layer.free, demand), layer[threshold]) >= 0);
  }

  async _eligible_cache(snapshot: Data): Promise<Data> {
    const task = this.store.get_task({ task_id: snapshot.task_id });
    if (!task || !["cached", "deleting"].includes(task.state) || !task.inactive_since)
      throw new ControlError("cache_ineligible", "not an eligible stopped cache");
    if (compare(task.generation, snapshot.generation) !== 0 || task.session_id !== snapshot.session_id)
      throw new ControlError("cache_ineligible", "cache ownership or generation changed");
    const expiry = parse_time(task.lease_expires_at);
    if (expiry && expiry > this.now())
      throw new ControlError("cache_ineligible", "cache still has an effective deadline");
    const ownership = await this._ownership(task);
    if (Object.values(ownership.expanded_devices ?? {}).some((device: any) => device.type === "proxy"))
      throw new ControlError("cache_ineligible", "cache still has native proxy mappings");
    const info = await this.incus.instance_info(task.instance_name);
    if (String(info.status ?? "").toLowerCase() !== "stopped" || this.store.get_ports(task.task_id).length)
      throw new ControlError("cache_ineligible", "cache is running or has unreclaimed mappings");
    return task;
  }

  async _delete_cache(task: Data): Promise<void> {
    task = await this._eligible_cache(task);
    await this._global_lock(() => this.store.update_task(task.task_id, { state: "deleting" }));
    try {
      await this.incus.delete(task.instance_name);
      if ((await this.incus.list_instances()).some(item => item.name === task.instance_name))
        throw new ControlError("cleanup_incomplete", "native deletion is not confirmed");
    } catch (error) {
      if (!(error instanceof ControlError)) throw error;
      const annotated = new ControlError(error.code, error.message, {
        ...detail_of(error), operation_phase: "cache-delete", outcome_verified: false,
      });
      this.store.update_task(task.task_id, { last_error: canonical_json(annotated.as_dict()) });
      throw annotated;
    }
    await this._global_lock(() => this.store.update_task(task.task_id, {
      state: "absent", vm_ip: null, ssh_host_port: null, lease_expires_at: null,
      inactive_since: null, cleanup_after: null,
      resources_json: canonical_json({ cpu: 0, memory_mib: 0, disk_gib: 0 }),
      resource_reservation_json: "{}", reservation_bytes: 0, last_error: null,
    }));
  }

  async _reclaim_capacity(request_cost: Integer, exclude_task_id: string | null = null): Promise<string[]> {
    let facts = await this._storage_headroom(request_cost);
    if (await this._global_lock(() => this._space_sufficient(facts, request_cost, "low", exclude_task_id)))
      return [];
    const reclaimed: string[] = [];
    const candidates = this.store.list_tasks()
      .filter(task => task.state === "cached" && task.inactive_since && task.task_id !== exclude_task_id)
      .sort((a, b) => a.inactive_since < b.inactive_since ? -1 : a.inactive_since > b.inactive_since ? 1 : 0);
    for (const task of candidates) {
      facts = await this._storage_headroom(request_cost);
      if (await this._global_lock(() => this._space_sufficient(facts, request_cost, "target", exclude_task_id)))
        break;
      try {
        await this._object_lock("session:" + task.session_id, () => this._delete_cache(task), false);
        reclaimed.push(task.task_id);
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        if (["object_busy", "cache_ineligible"].includes(error.code)) continue;
        throw new ControlError("reclaim_failed", "exact cache reclamation failed", {
          ...error.as_dict(), operation_phase: "cache-delete", outcome_verified: false,
        });
      }
    }
    facts = await this._storage_headroom(request_cost);
    await this._global_lock(() => {
      if (!this._space_sufficient(facts, request_cost, "low", exclude_task_id))
        throw new ControlError(
          "capacity_insufficient", "capacity remains insufficient after eligible reclaim",
          { storage: facts, reclaimed },
        );
    });
    return reclaimed;
  }

  async _resource_policy({ memory_observation = null }: Data = {}): Promise<Data> {
    const base = {
      cpu: require_int(this.config.cpu_budget ?? 0, "cpu_budget", 1),
      memory_mib: require_int(this.config.memory_budget_mib ?? 0, "memory_budget_mib", 512),
    };
    const ratios: Data = {
      cpu: this.config.cpu_overcommit_ratio ?? 4,
      memory_mib: this.config.memory_overcommit_ratio ?? 4,
    };
    decimal_ratio(ratios.cpu, "cpu_overcommit_ratio");
    decimal_ratio(ratios.memory_mib, "memory_overcommit_ratio");
    const cpu = await this.resources.cpu();
    const memory = memory_observation ?? await this.resources.memory();
    const capacity: Data = { cpu, memory_mib: floor_div(memory.limit_bytes, 1024 ** 2) };
    if (RESOURCE_DIMENSIONS.some(key => compare(base[key], capacity[key]) > 0))
      throw new ControlError(
        "resource_budget_invalid", "base budget exceeds CT-visible capacity",
        { base, ct_capacity: capacity },
      );
    const project = await this.incus.query(
      "/1.0/projects/" + quote(this.config.project), { project: "default" },
    );
    if (!is_record(project) || !is_record(project.config))
      throw new ControlError("resource_unverified", "project resource configuration cannot be inspected");
    const conflicts = Object.fromEntries(
      ["limits.cpu", "limits.memory"].filter(key => project.config[key]).map(key => [key, project.config[key]]),
    );
    if (Object.keys(conflicts).length)
      throw new ControlError(
        "project_resource_limits_conflict",
        "project aggregate quotas count stopped caches; authorized deployment must remove them",
        { project: this.config.project, conflicting_limits: conflicts },
      );
    const count_limits = this._instance_count_limits(project.config);
    if (Object.keys(count_limits).length)
      throw new ControlError(
        "project_instance_limits_conflict",
        "project instance quotas include stopped caches; authorized deployment must remove them",
        {
          project: this.config.project, conflicting_limits: count_limits,
          instance_count: (await this.incus.list_instances()).length,
        },
      );
    return {
      base,
      ratios: Object.fromEntries(Object.entries(ratios).map(([key, value]) => [key, Number(value)])),
      limits: {
        cpu: allocation_limit(base.cpu, ratios.cpu, "cpu_overcommit_ratio"),
        memory_mib: allocation_limit(base.memory_mib, ratios.memory_mib, "memory_overcommit_ratio"),
      },
      ct_capacity: capacity,
      memory: { ...memory, observed_at: iso_time(this.now()) },
    };
  }

  async _cpu_pressure_observation(): Promise<Data> {
    let observation: any;
    try {
      if (typeof this.resources.cpu_pressure !== "function")
        return unverified("CT CPU pressure observation is unavailable");
      observation = await this.resources.cpu_pressure();
    } catch (error) {
      if (!(error instanceof ControlError)) throw error;
      return { verified: false, error: error.as_dict() };
    }
    if (!is_record(observation)) return unverified("CT CPU pressure observation is not an object");
    if (
      observation.scope !== "ct-cpu-pressure" ||
      !equal(observation.sources ?? null, ["cgroupfs"]) ||
      observation.mountpoint !== "/sys/fs/cgroup" ||
      observation.mount_root !== "/" || observation.filesystem !== "cgroup2"
    ) return unverified("CT CPU pressure observation is not CT-local");
    const path = observation.cgroup_path;
    if (
      typeof path !== "string" ||
      path !== "/" && (
        !/^\/[^\x00/]+(?:\/[^\x00/]+)*$/.test(path) ||
        path.split("/").some(part => part === "." || part === "..")
      )
    ) return unverified("CT CPU pressure cgroup path is invalid");
    if (observation.metric !== "/sys/fs/cgroup" + (path === "/" ? "" : path) + "/cpu.pressure")
      return unverified("CT CPU pressure metric path is not current-cgroup local");
    try {
      return {
        ...observation, ...normalize_cpu_psi(observation),
        verified: true, observed_at: iso_time(this.now()),
      };
    } catch {
      return unverified("CT CPU pressure observation is malformed");
    }
  }

  async _cpu_startup_pressure_diagnostics({
    allocated_cpu, requested_cpu, capacity_cpu, observation = null,
  }: Data): Promise<Data> {
    observation ??= await this._cpu_pressure_observation();
    const threshold = percentage(
      this.config.cpu_startup_pressure_full_avg10_percent ?? 20,
      "cpu_startup_pressure_full_avg10_percent",
    );
    const projected = add(allocated_cpu, requested_cpu);
    const detail: Data = {
      threshold_full_avg10_percent: threshold,
      allocated_cpu, requested_cpu, projected_cpu: projected,
      ct_capacity_cpu: capacity_cpu,
      physically_oversubscribed: compare(projected, capacity_cpu) > 0,
      startup_blocked: false,
    };
    if (!observation.verified)
      return { ...detail, verified: false, error: observation.error };
    let full_avg10: number, some_avg10: number;
    try {
      const full = observation.full, some = observation.some;
      if (!is_record(full) || !is_record(some)) throw new TypeError("incomplete pressure");
      for (const name of PSI_FIELDS) {
        if (!(name in full) || !(name in some)) throw new TypeError("missing pressure field");
        const a = Number(full[name]), b = Number(some[name]);
        if (!Number.isFinite(a) || !Number.isFinite(b) || a > b)
          throw new TypeError("invalid pressure");
      }
      full_avg10 = Number(full.avg10);
      some_avg10 = Number(some.avg10);
    } catch {
      return { ...detail, ...unverified("CT CPU pressure observation is incomplete") };
    }
    return {
      ...detail, verified: true,
      full_avg10_percent: full_avg10, some_avg10_percent: some_avg10,
      startup_blocked: detail.physically_oversubscribed && full_avg10 >= threshold,
    };
  }

  async _enforce_cpu_startup_pressure(options: Data): Promise<Data> {
    const detail = await this._cpu_startup_pressure_diagnostics(options);
    if (!detail.verified)
      throw new ControlError("resource_unverified", "CT CPU pressure observation is not verified", detail);
    if (detail.startup_blocked)
      throw new ControlError("resource_pressure", "CPU scheduler pressure blocks VM startup", detail);
    return detail;
  }

  _allocation_breakdown(exclude_task_id: string | null = null): Data {
    const totals: Data = Object.fromEntries(RESOURCE_DIMENSIONS.map(key => [
      key, { allocated: 0, reserved: 0, allocated_and_reserved: 0 },
    ]));
    for (const other of this.store.list_tasks()) {
      const state = other.state;
      if (other.task_id === exclude_task_id || ["absent", "cached"].includes(state)) continue;
      if (!["preparing", "active", "closing", "deleting", "failed"].includes(state))
        throw new ControlError("resource_unverified", "task state cannot be verified", {
          task_id: other.task_id, state,
        });
      try {
        const value = resource_mapping(other.resources_json, "resources");
        const reservation = resource_mapping(
          other.resource_reservation_json, "reservation", { allow_empty: true },
        );
        for (const key of RESOURCE_DIMENSIONS) {
          const allocated = value[key], reserved = reservation[key] ?? 0;
          require_int(allocated, key, 0);
          require_int(reserved, key, 0);
          totals[key].allocated = add(totals[key].allocated, allocated);
          totals[key].reserved = add(totals[key].reserved, reserved);
          totals[key].allocated_and_reserved = add(
            totals[key].allocated_and_reserved, maximum(allocated, reserved),
          );
        }
      } catch {
        throw new ControlError("resource_unverified", "occupied object lacks verified resource sizes");
      }
    }
    return totals;
  }

  _allocation_totals(exclude_task_id: string | null = null): Data {
    return Object.fromEntries(Object.entries(this._allocation_breakdown(exclude_task_id))
      .map(([key, value]) => [key, (value as Data).allocated_and_reserved]));
  }

  async _running_inventory_guard(): Promise<void> {
    const instances = await this.incus.list_instances();
    const records = new Map(this.store.list_tasks().map(task => [task.instance_name, task]));
    for (const record of records.values()) {
      this._validate_task_state(record);
      if (record.state !== "absent") continue;
      if (instances.some(instance => instance.name === record.instance_name))
        throw new ControlError(
          "resource_unverified", "absent task still has a native instance; reconciliation is required",
          { task_id: record.task_id },
        );
      if (this.store.get_ports(record.task_id).length)
        throw new ControlError(
          "cleanup_incomplete", "absent task still has port mappings; reconciliation is required",
          { task_id: record.task_id },
        );
      let resources: Data, reservation: Data, reservation_bytes: Integer;
      try {
        resources = resource_mapping(record.resources_json, "resources");
        reservation = resource_mapping(record.resource_reservation_json, "reservation", { allow_empty: true });
        reservation_bytes = this._durable_reservation_bytes(record.reservation_bytes);
      } catch {
        throw new ControlError(
          "resource_unverified", "absent task resource state cannot be verified; reconciliation is required",
          { task_id: record.task_id },
        );
      }
      if (
        !equal(resources, { cpu: 0, memory_mib: 0, disk_gib: 0 }) ||
        Object.keys(reservation).length || compare(reservation_bytes, 0) !== 0
      ) throw new ControlError(
        "resource_unverified", "absent task resource state is not reconciled; reconciliation is required",
        { task_id: record.task_id },
      );
    }
    for (const instance of instances) {
      const record = records.get(instance.name);
      if (String(instance.status ?? "").toLowerCase() === "stopped") {
        if (record && !["absent", "cached"].includes(record.state)) {
          if (record.state !== "active")
            throw new ControlError("resource_unverified", TRANSITIONAL_ADMISSION_ERROR_MESSAGE);
          await this._verify_native_allocation(record);
        }
        continue;
      }
      if (!record || ["absent", "cached"].includes(record.state))
        throw new ControlError("resource_unverified", "unaccounted running native instance blocks admission");
      if (record.state !== "active")
        throw new ControlError("resource_unverified", TRANSITIONAL_ADMISSION_ERROR_MESSAGE);
      await this._verify_native_allocation(record);
    }
  }

  async _resource_diagnostics({
    global_locked = false, capacity_observation = null,
  }: Data = {}): Promise<Data> {
    let memory: Data | null = null, memory_error: ControlError | null = null;
    try { memory = await this.resources.memory(); }
    catch (error) {
      if (!(error instanceof ControlError)) throw error;
      memory_error = error;
    }
    const cpu_pressure = await this._cpu_pressure_observation();
    let policy: Data | null = null;
    try {
      if (memory_error) throw memory_error;
      policy = await this._resource_policy({ memory_observation: memory });
      await this._running_inventory_guard();
      const capacity = capacity_observation ?? await this._storage_headroom(0);
      const read = () => this.store.transaction(() => ({
        breakdown: this._allocation_breakdown(),
        disk: this._disk_admission_diagnostics(capacity),
      }));
      const { breakdown, disk } = global_locked ? read() : await this._global_lock(read);
      if (!disk.verified)
        throw new ControlError("resource_unverified", "disk admission state is not verified", disk.error);
      const column = (name: string): Data => Object.fromEntries(
        Object.entries(breakdown).map(([key, value]) => [key, (value as Data)[name]]),
      );
      const allocated = column("allocated"), reserved = column("reserved");
      const allocated_and_reserved = column("allocated_and_reserved");
      return {
        verified: true, ...policy, allocated, reserved, allocated_and_reserved,
        remaining: Object.fromEntries(RESOURCE_DIMENSIONS.map(key => [
          key, sub(policy!.limits[key], allocated_and_reserved[key]),
        ])),
        memory_pressure: compare(policy.memory.available_bytes, policy.memory.threshold_bytes) < 0,
        cpu_pressure, disk_admission: disk,
      };
    } catch (error) {
      if (!(error instanceof ControlError)) throw error;
      const result: Data = {
        verified: false, error: error.as_dict(),
        base: null, ratios: null, limits: null, ct_capacity: null,
        memory: memory ? { ...memory, observed_at: iso_time(this.now()) } : null,
        memory_pressure: null, cpu_pressure,
        allocated: null, reserved: null, allocated_and_reserved: null,
        remaining: null, disk_admission: null,
      };
      if (policy) Object.assign(result, policy);
      if (memory)
        result.memory_pressure = compare(memory.available_bytes, memory.threshold_bytes) < 0;
      return result;
    }
  }

  _disk_admission_diagnostics(capacity: any): Data {
    if (!is_record(capacity) || !Array.isArray(capacity.layers))
      return unverified("disk capacity is unavailable");
    try {
      const layers = capacity.layers;
      if (!layers.length) throw new TypeError("empty capacity");
      let allocated: Integer = 0, reserved: Integer = 0;
      let allocated_and_reserved: Integer = 0, durable_reserved: Integer = 0;
      for (const task of this.store.list_tasks()) {
        if (["absent", "cached"].includes(task.state)) continue;
        const resources = resource_mapping(task.resources_json, "resources");
        const reservation = resource_mapping(
          task.resource_reservation_json, "reservation", { allow_empty: true },
        );
        const a = require_int(resources.disk_gib, "disk_gib", 0);
        const r = require_int(reservation.disk_gib ?? 0, "disk_gib", 0);
        allocated = add(allocated, mul(a, GIB));
        reserved = add(reserved, mul(r, GIB));
        allocated_and_reserved = add(allocated_and_reserved, mul(maximum(a, r), GIB));
        durable_reserved = add(durable_reserved, this._durable_reservation_bytes(task.reservation_bytes));
      }
      const total = sum(layers.map((layer: Data) => native_integer(layer.total)));
      const free = sum(layers.map((layer: Data) => native_integer(layer.free)));
      const headroom = layers.map((layer: Data) => sub(native_integer(layer.free), native_integer(layer.low)));
      const free_after_low = headroom.reduce((a: Integer, b: Integer) => minimum(a, b));
      const limit = sum(layers.map((layer: Data) => sub(native_integer(layer.total), native_integer(layer.low))));
      if (compare(total, 0) <= 0 || compare(limit, 0) <= 0 || compare(durable_reserved, 0) < 0)
        throw new TypeError("invalid capacity");
      const used = sub(total, free);
      return {
        verified: true,
        guest_allocation: {
          allocated_bytes: allocated, reserved_bytes: reserved,
          allocated_and_reserved_bytes: allocated_and_reserved,
        },
        physical_admission: {
          allocated_bytes: used, reserved_bytes: durable_reserved,
          allocated_and_reserved_bytes: add(used, durable_reserved),
          remaining_bytes: maximum(0, sub(free_after_low, durable_reserved)),
          limit_bytes: limit, capacity_bytes: total, free_bytes: free,
          durable_reserved_bytes: durable_reserved,
        },
      };
    } catch {
      return unverified("invalid disk admission capacity");
    }
  }

  _instance_count_limits(config: Data): Data {
    return Object.fromEntries(["limits.instances", "limits.virtual-machines"]
      .filter(key => config[key] != null && config[key] !== "" && config[key] !== "-1" &&
        !(typeof config[key] === "number" && config[key] === -1) && config[key] !== -1n)
      .map(key => [key, config[key]]));
  }

  async _instance_diagnostics(): Promise<Data> {
    try {
      const project = await this.incus.query(
        "/1.0/projects/" + quote(this.config.project), { project: "default" },
      );
      if (!is_record(project) || !is_record(project.config))
        throw new ControlError("resource_unverified", "project count configuration cannot be inspected");
      const limits = this._instance_count_limits(project.config);
      const instances = await this.incus.list_instances();
      const running = instances.filter(item => String(item.status ?? "").toLowerCase() === "running").length;
      const stopped = instances.filter(item => String(item.status ?? "").toLowerCase() === "stopped").length;
      const names = new Set(instances.map(item => String(item.name)));
      const tasks = this.store.list_tasks();
      const preparing = tasks.filter(task => task.state === "preparing" && !names.has(task.instance_name)).length;
      const cached_names = new Set(tasks.filter(task => task.state === "cached").map(task => task.instance_name));
      const cached = instances.filter(item =>
        cached_names.has(item.name) && String(item.status ?? "").toLowerCase() === "stopped").length;
      let slot_limit: Integer | null = null;
      for (const value of Object.values(limits)) {
        let candidate: Integer;
        try { candidate = native_integer(value); }
        catch { throw new ControlError("resource_unverified", "instance slot limit is not numeric"); }
        if (compare(candidate, 0) >= 0) {
          if (slot_limit !== null && compare(slot_limit, candidate) !== 0)
            throw new ControlError("resource_unverified", "instance slot limits disagree");
          slot_limit = candidate;
        }
      }
      const allocated_and_reserved = add(instances.length, preparing);
      return {
        verified: true, total: instances.length, running, stopped, cached,
        other: instances.length - running - stopped, limits,
        policy_compliant: Object.keys(limits).length === 0,
        admission: {
          allocated: instances.length, reserved: preparing, allocated_and_reserved,
          remaining: slot_limit === null ? null : maximum(0, sub(slot_limit, allocated_and_reserved)),
          limit: slot_limit, capacity: null,
        },
      };
    } catch (error) {
      if (!(error instanceof ControlError)) throw error;
      return { verified: false, error: error.as_dict() };
    }
  }

  async _default_admission_diagnostics(
    resources: Data, capacity: Data, instances: Data, { global_locked = false }: Data = {},
  ): Promise<Data> {
    const requested = { cpu: 2, memory_mib: 4096, disk_gib: 20 };
    const reasons: Data[] = [], warnings: Data[] = [];
    if (this.store.get_meta("draining") === "1")
      reasons.push({ code: "draining", message: "new leases are refused" });
    if (!resources.verified) reasons.push(resources.error);
    else {
      const exceeded = RESOURCE_DIMENSIONS.filter(key => compare(resources.remaining[key], requested[key]) < 0);
      if (exceeded.length)
        reasons.push({
          code: "resource_insufficient", dimensions: exceeded,
          requested, remaining: resources.remaining,
        });
    }
    if (resources.memory_pressure)
      warnings.push({ code: "memory_pressure", memory: resources.memory });
    let cpu_startup_pressure: Data | null = null;
    if (resources.verified) {
      cpu_startup_pressure = await this._cpu_startup_pressure_diagnostics({
        allocated_cpu: resources.allocated_and_reserved.cpu,
        requested_cpu: requested.cpu, capacity_cpu: resources.ct_capacity.cpu,
        observation: resources.cpu_pressure ?? null,
      });
      if (cpu_startup_pressure.startup_blocked)
        reasons.push({
          code: "resource_pressure", message: "CPU scheduler pressure blocks VM startup",
          detail: cpu_startup_pressure,
        });
      else if (!cpu_startup_pressure.verified)
        reasons.push({
          code: "resource_unverified", message: "CT CPU pressure observation is not verified",
          detail: cpu_startup_pressure,
        });
    }
    if (!instances.verified) reasons.push(instances.error);
    else if (!instances.policy_compliant &&
      !reasons.some(reason => reason.code === "project_instance_limits_conflict"))
      reasons.push({ code: "project_instance_limits_conflict", conflicting_limits: instances.limits });
    let reclaim_required: boolean | null = null;
    if ("error" in capacity) reasons.push(capacity.error);
    else if (resources.verified) {
      const read = () => this.store.transaction(() =>
        !this._space_sufficient(capacity, this.config.create_cost_bytes, "low"));
      reclaim_required = global_locked ? read() : await this._global_lock(read);
      if (reclaim_required)
        reasons.push({ code: "disk_reclaim_required", message: "default creation needs capacity reclamation" });
    }
    return {
      requested, request_cost_bytes: this.config.create_cost_bytes,
      ready: reasons.length === 0, reasons, reclaim_required,
      observations: { memory_pressure: resources.memory_pressure, cpu_startup_pressure },
      warnings,
    };
  }

  async _reserve_resources(task: Data, resources: Data, {
    memory_reduction = false, startup_required = true,
    startup_cpu = null, admission_epoch = null,
  }: Data = {}): Promise<Data> {
    let policy: Data | null;
    if (memory_reduction) {
      const old = this._durable_resources(task, "resources_json", { allow_empty: false });
      const reservation = this._durable_resources(task, "resource_reservation_json", { allow_empty: true });
      if (task.state !== "active" || !equal(resources, old) || Object.keys(reservation).length)
        throw new ControlError("resize_unverified", "memory reduction requires an intact current allocation");
      policy = null;
    } else {
      policy = await this._resource_policy();
      await this._running_inventory_guard();
      const disk = this._disk_admission_diagnostics(await this._storage_headroom(0));
      if (!disk.verified)
        throw new ControlError("resource_unverified", "disk admission state is not verified", disk.error);
    }

    return await this._admission_lock(() => this._global_lock(async () => {
      const validate = (): Data => {
        if (this.store.get_meta("draining") === "1")
          throw new ControlError("draining", "new leases and capacity changes are refused while draining");
        const epoch = this.store.get_meta("admission_epoch") || "initial";
        if (admission_epoch !== null && epoch !== admission_epoch)
          throw new ControlError("draining", "admission state changed while the request was preparing");
        const current = this.store.get_task({ task_id: task.task_id });
        if (task.state === "active") {
          if (!current || compare(current.generation, task.generation) !== 0)
            throw new ControlError("generation_mismatch", "reservation generation changed");
          this._require_active(current);
        } else if (current && (
          !["absent", "cached"].includes(current.state) ||
          compare(add(current.generation, 1), task.generation) !== 0
        )) throw new ControlError("generation_mismatch", "allocation slot changed before reservation");
        const totals = this._allocation_totals(task.task_id);
        const projected = Object.fromEntries(RESOURCE_DIMENSIONS.map(key => [key, add(totals[key], resources[key])]));
        const exceeded = policy
          ? RESOURCE_DIMENSIONS.filter(key => compare(projected[key], policy!.limits[key]) > 0) : [];
        if (exceeded.length)
          throw new ControlError(
            "resource_insufficient", "allocation ceiling exceeded: " + exceeded.join(", "),
            {
              dimensions: exceeded, requested: { ...resources }, other_allocated: totals,
              projected, base: policy!.base, limits: policy!.limits,
            },
          );
        return totals;
      };
      const totals = this.store.transaction(validate);
      if (policy && startup_required)
        await this._enforce_cpu_startup_pressure({
          allocated_cpu: totals.cpu,
          requested_cpu: startup_cpu ?? resources.cpu,
          capacity_cpu: policy.ct_capacity.cpu,
        });
      return this.store.transaction(() => {
        validate();
        const record: Data = { ...task, resource_reservation_json: canonical_json(resources) };
        if (!record.resources_json) record.resources_json = canonical_json(resources);
        this.store.put_task(record);
        return record;
      });
    }));
  }

  async _reserve_resources_with_transition_wait(
    task: Data, resources: Data, { startup_cpu = null, admission_epoch = null }: Data = {},
  ): Promise<Data> {
    const started = monotonic();
    let attempts = 0;
    for (;;) {
      try {
        return await this._reserve_resources(task, resources, { startup_cpu, admission_epoch });
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        if (error.code !== "resource_unverified" || error.message !== TRANSITIONAL_ADMISSION_ERROR_MESSAGE)
          throw error;
        const duration = monotonic() - started;
        const remaining = TRANSITIONAL_ADMISSION_WAIT_SECONDS - duration;
        if (remaining <= 0)
          throw new ControlError(error.code, error.message, {
            ...detail_of(error), admission_wait_seconds: rounded(Math.max(0, duration)),
            admission_retry_count: attempts,
          });
        attempts++;
        await Bun.sleep(Math.min(TRANSITIONAL_ADMISSION_POLL_SECONDS, remaining) * 1000);
      }
    }
  }

  async _reserve_native(task: Data, cost: Integer, {
    inventory = null, ssh_port = null,
  }: { inventory?: Set<Integer> | null; ssh_port?: any } = {}): Promise<Data> {
    const facts = await this._storage_headroom(cost);
    const native_ips = inventory !== null ? await this._native_ips(task.instance_name) : new Set<string>();
    return await this._global_lock(() => this.store.transaction(() => {
      const current = this.store.get_task({ task_id: task.task_id });
      if (!current || compare(current.generation, task.generation) !== 0 ||
        !["active", "preparing"].includes(current.state))
        throw new ControlError("generation_mismatch", "reservation changed before native admission");
      const expiry = parse_time(current.lease_expires_at);
      if (!expiry || expiry <= this.now())
        throw new ControlError("lease_expired", "lease expired before native admission");
      if (this.store.get_meta("draining") === "1")
        throw new ControlError("draining", "native admission is drained");
      if (!this._space_sufficient(facts, cost, "low", task.task_id))
        throw new ControlError("capacity_insufficient", "concurrent disk reservation consumed headroom");
      const record: Data = { ...current, reservation_bytes: cost };
      if (inventory !== null) {
        if (native_ips.has(task.vm_ip))
          throw new ControlError("vm_ip_busy", "cached VM address conflicts with native inventory");
        record.vm_ip = task.vm_ip || this._allocate_vm_ip(native_ips);
        record.ssh_host_port = this._allocate_port("tcp", ssh_port, { inventory });
      }
      this.store.put_task(record);
      if (inventory !== null)
        this.store.put_port(this._new_port(record, "ssh", "tcp", record.ssh_host_port, 22));
      return record;
    }));
  }

  async _cancel_resource_reservation(previous: Data): Promise<void> {
    await this._global_lock(() => this.store.transaction(() => {
      const current = this.store.get_task({ task_id: previous.task_id });
      if (current && compare(current.generation, previous.generation) === 0 && current.state === "active")
        this.store.update_task(previous.task_id, {
          resource_reservation_json: previous.resource_reservation_json,
          reservation_bytes: previous.reservation_bytes,
        });
    }));
  }

  _new_port(task: Data, name: string, protocol: string, host: Integer, guest: Integer): Data {
    return {
      task_id: task.task_id, generation: task.generation, port_name: name,
      protocol, host_port: host, vm_port: guest,
      device_name: `agent-vm-${task.task_id}-${name}-${protocol}`,
      state: "preparing", created_at: iso_time(this.now()),
    };
  }

  _durable_resources(task: Data, field: string, { allow_empty }: { allow_empty: boolean }): Data {
    try { return resource_mapping(task[field], field, { allow_empty }); }
    catch { throw new ControlError("resource_unverified", `task ${field} cannot be verified`); }
  }

  _require_active(task: Data | null, allow_pending_acquire = false): void {
    if (!task || task.state !== "active")
      throw new ControlError("task_not_active", "the VM lease is not active");
    const expiry = parse_time(task.lease_expires_at);
    if (!expiry || expiry <= this.now())
      throw new ControlError("lease_expired", "the VM lease has expired");
    if (!allow_pending_acquire && this._pending_acquire_for_task(task) !== null)
      throw new ControlError("lease_unavailable", "the VM lease has an unresolved acquire operation; release or retry that operation first");
  }

  async _native_resources(task: Data): Promise<Data> {
    const info = await this._ownership(task);
    const config = Object.hasOwn(info, "expanded_config") ? info.expanded_config : info.config ?? {};
    try {
      const cpu = native_integer(config["limits.cpu"]);
      const memory = typeof config["limits.memory"] === "string"
        ? /^([0-9]+)(MiB|GiB)$/.exec(config["limits.memory"]) : null;
      const size = info.expanded_devices.root.size;
      const disk = typeof size === "string" ? /^([0-9]+)GiB$/.exec(size) : null;
      if (compare(cpu, 1) < 0 || !memory || !disk) throw new TypeError("invalid native sizes");
      return {
        cpu,
        memory_mib: mul(narrow(BigInt(memory[1]!)), memory[2] === "GiB" ? 1024 : 1),
        disk_gib: narrow(BigInt(disk[1]!)),
      };
    } catch {
      throw new ControlError("resource_unverified", "native CPU, memory or root size cannot be verified");
    }
  }

  async _verify_native_allocation(task: Data): Promise<void> {
    let expected = this._durable_resources(task, "resources_json", { allow_empty: false });
    const reservation = this._durable_resources(task, "resource_reservation_json", { allow_empty: true });
    if (Object.keys(reservation).length) {
      expected = reservation;
      const native = await this._native_resources(task);
      if (Object.keys(expected).some(key => compare(native[key], expected[key]) > 0))
        throw new ControlError("resource_unverified", "native resources exceed the durable reservation");
    } else if (!equal(await this._native_resources(task), expected))
      throw new ControlError("resource_unverified", "native resources differ from durable allocation");
  }

  async _verify_network_device(task: Data): Promise<void> {
    const info = await this._ownership(task);
    const eth0 = is_record(info.expanded_devices) ? info.expanded_devices.eth0 : null;
    const expected: Data = {
      type: "nic", network: this.config.network, "ipv4.address": task.vm_ip,
      "security.mac_filtering": "true", "security.ipv4_filtering": "true",
      "security.ipv6_filtering": "true", "security.port_isolation": "true",
      "security.acls": this.config.network_acl,
      "security.acls.default.ingress.action": "allow",
      "security.acls.default.egress.action": "allow",
    };
    if (!is_record(eth0) || Object.entries(expected).some(([key, value]) => eth0[key] !== value))
      throw new ControlError(
        "ownership_mismatch", "native network isolation differs from the managed configuration",
      );
  }

  async _host_ports(): Promise<Set<Integer>> {
    const result = await this.runner.run(["ss", "-Hlnptu"], { timeout: 5 });
    const ports = new Set<Integer>();
    for (const line of lines(result.stdout.toString("utf8"))) {
      if (!line && line === lines(result.stdout.toString("utf8")).at(-1)) continue;
      const fields = words(line);
      if (fields.length < 6)
        throw new ControlError("port_inventory_unverified", "unrecognized ss output");
      const match = /:([0-9]+)$/.exec(fields[4]!);
      if (!match) throw new ControlError("port_inventory_unverified", "unrecognized socket endpoint");
      ports.add(narrow(BigInt(match[1]!)));
    }
    return ports;
  }

  async _native_ports(): Promise<Set<Integer>> {
    const ports = new Set<Integer>();
    const code = "port_inventory_unverified";
    const projects = this._inventory_mappings(
      await this.incus.query("/1.0/projects?recursion=1", { project: "default" }), code, "project",
    );
    for (const project of projects) {
      let name: string;
      try { name = require_string(project.name, "project name"); }
      catch { throw new ControlError(code, "project inventory contains an invalid name"); }
      const instances = this._inventory_mappings(
        await this.incus.query("/1.0/instances?recursion=1", { project: name }), code, "instance",
      );
      const networks = this._inventory_mappings(
        await this.incus.query("/1.0/networks?recursion=1", { project: name }), code, "network",
      );
      for (const instance of instances) {
        try { require_string(instance.name, "instance name"); }
        catch { throw new ControlError(code, "instance inventory contains an invalid name"); }
        if (!Object.hasOwn(instance, "expanded_devices"))
          throw new ControlError(code, "instance devices are unavailable");
        const devices = instance.expanded_devices;
        if (!is_record(devices) || Object.values(devices).some(device => !is_record(device)))
          throw new ControlError(code, "instance devices are malformed");
        for (const device of Object.values(devices) as Data[]) {
          if (device.type !== "proxy") continue;
          const listen = device.listen;
          if (typeof listen !== "string" || !listen)
            throw new ControlError(code, "proxy listen address is malformed");
          if (listen.startsWith("tcp:") || listen.startsWith("udp:"))
            for (const port of this._port_numbers(listen.slice(listen.lastIndexOf(":") + 1))) ports.add(port);
          else if (!listen.startsWith("unix:") && !listen.startsWith("vsock:"))
            throw new ControlError(code, "proxy listen address is unrecognized");
        }
      }
      for (const network of networks) {
        const network_name = require_string(network.name, "network name");
        if (!network.managed || network.type !== "bridge") continue;
        const forwards = this._inventory_mappings(await this.incus.query(
          "/1.0/networks/" + quote(network_name) + "/forwards?recursion=1", { project: name },
        ), code, "forward");
        for (const forward of forwards) {
          if (!Object.hasOwn(forward, "config") || !Object.hasOwn(forward, "ports"))
            throw new ControlError(code, "network forward fields are unavailable");
          if (!is_record(forward.config))
            throw new ControlError(code, "network forward config is malformed");
          if (forward.config.target_address) {
            for (let port = Number(this.config.port_start); port <= Number(this.config.port_end); port++)
              ports.add(port);
          }
          if (!Array.isArray(forward.ports) || forward.ports.some((item: any) => !is_record(item)))
            throw new ControlError(code, "network forward ports are malformed");
          for (const item of forward.ports) {
            if (typeof item.listen_port !== "string")
              throw new ControlError(code, "network forward listen port is malformed");
            for (const port of this._port_numbers(item.listen_port)) ports.add(port);
          }
        }
      }
    }
    return ports;
  }

  _port_numbers(value: string): Set<Integer> {
    const result = new Set<Integer>();
    for (const part of value.split(",")) {
      const match = /^([0-9]+)(?:-([0-9]+))?$/.exec(part.trim());
      if (!match) throw new ControlError("port_inventory_unverified", "unrecognized native port range");
      const start = BigInt(match[1]!), end = BigInt(match[2] ?? match[1]!);
      if (start < 1n || start > end || end > 65535n)
        throw new ControlError("port_inventory_unverified", "invalid native port range");
      for (let port = Number(start); port <= Number(end); port++) result.add(port);
    }
    return result;
  }

  async _ephemeral_ports(): Promise<Set<Integer>> {
    try {
      const fields = words(await this._read_text("/proc/sys/net/ipv4/ip_local_port_range"));
      if (fields.length !== 2) throw new TypeError("invalid range");
      const start = integer(native_integer(fields[0])), end = integer(native_integer(fields[1]));
      if (start < 1n || start > end || end > 65535n) throw new TypeError("invalid range");
      const result = new Set<Integer>();
      for (let port = Number(start); port <= Number(end); port++) result.add(port);
      return result;
    } catch {
      throw new ControlError("port_inventory_unverified", "cannot inspect host ephemeral port range");
    }
  }

  async _port_inventory(): Promise<Set<Integer>> {
    const host = await this._host_ports();
    const native = await this._native_ports();
    const ephemeral = await this._ephemeral_ports();
    return new Set([...host, ...native, ...ephemeral]);
  }

  _allocate_port(
    protocol: string, requested: any = null, { inventory }: { inventory: Set<Integer> },
  ): Integer {
    if (!["tcp", "udp"].includes(protocol))
      throw new ControlError("invalid_request", "protocol must be tcp or udp");
    const used = new Set([...this.store.occupied_ports(protocol), ...inventory].map(value => String(integer(value))));
    if (requested !== null && requested !== undefined) {
      const port = require_int(requested, "host_port", this.config.port_start, this.config.port_end);
      if (used.has(String(integer(port))))
        throw new ControlError("port_busy", "requested host port is occupied");
      return port;
    }
    for (let port = Number(this.config.port_start); port <= Number(this.config.port_end); port++)
      if (!used.has(String(port))) return port;
    throw new ControlError("port_exhausted", "no host port is available");
  }

  _allocated_vm_ip(): Set<string> {
    return new Set(this.store.list_tasks()
      .filter(task => task.vm_ip && task.state !== "absent")
      .map(task => String(task.vm_ip)));
  }

  async _native_ips(own_name: string): Promise<Set<string>> {
    const used = new Set<string>();
    let own_mac: string | null = null, own_ip: string | null = null;
    const code = "vm_ip_unverified";
    const validate_ip = (address: string, message: string): void => {
      try { ipv4_to_int(address); }
      catch { throw new ControlError(code, message); }
    };
    const projects = this._inventory_mappings(
      await this.incus.query("/1.0/projects?recursion=1", { project: "default" }), code, "project",
    );
    for (const project of projects) {
      const name = require_string(project.name, "project name");
      const instances = this._inventory_mappings(
        await this.incus.query("/1.0/instances?recursion=1", { project: name }), code, "instance",
      );
      for (const instance of instances) {
        try { require_string(instance.name, "instance name"); }
        catch { throw new ControlError(code, "instance inventory contains an invalid name"); }
        if (!Object.hasOwn(instance, "expanded_devices"))
          throw new ControlError(code, "instance devices are unavailable");
        const devices = instance.expanded_devices;
        if (!is_record(devices) || Object.values(devices).some(device => !is_record(device)))
          throw new ControlError(code, "instance devices are malformed");
        if (name === this.config.project && instance.name === own_name) {
          const nic = devices.eth0 ?? {};
          if (!is_record(nic))
            throw new ControlError(code, "instance network device is malformed");
          const config = instance.config ?? {};
          if (!is_record(config)) throw new ControlError(code, "instance config is malformed");
          own_mac = nic.hwaddr || config["volatile.eth0.hwaddr"] || null;
          own_ip = nic["ipv4.address"] ?? null;
          if (own_mac !== null && typeof own_mac !== "string")
            throw new ControlError(code, "instance MAC address is malformed");
          if (own_ip !== null && typeof own_ip !== "string")
            throw new ControlError(code, "instance IP address is malformed");
          if (own_ip) validate_ip(own_ip, "instance IP address is invalid");
          continue;
        }
        for (const device of Object.values(devices) as Data[]) {
          const network = device.network, parent = device.parent, address = device["ipv4.address"];
          if (network != null && typeof network !== "string" || parent != null && typeof parent !== "string")
            throw new ControlError(code, "instance network identity is malformed");
          if (address != null && typeof address !== "string")
            throw new ControlError(code, "instance IP address is malformed");
          if (address) validate_ip(address, "instance IP address is invalid");
          if ((network === this.config.network || parent === this.config.network) && address)
            used.add(address);
        }
      }
    }
    const leases = this._inventory_mappings(await this.incus.query(
      "/1.0/networks/" + quote(this.config.network) + "/leases", { project: "default" },
    ), code, "network lease");
    for (const lease of leases) {
      const address = lease.address, hwaddr = lease.hwaddr;
      if (typeof address !== "string" || !address)
        throw new ControlError(code, "network lease address is malformed");
      validate_ip(address, "network lease address is invalid");
      if (hwaddr != null && typeof hwaddr !== "string")
        throw new ControlError(code, "network lease MAC address is malformed");
      if (own_mac && hwaddr && hwaddr.toLowerCase() === own_mac.toLowerCase() && address === own_ip)
        continue;
      used.add(address);
    }
    return used;
  }

  _allocate_vm_ip(native_ips: Set<string>): string {
    const start = ipv4_to_int(String(this.config.vm_ip_start));
    const end = ipv4_to_int(String(this.config.vm_ip_end));
    const used = new Set([...this._allocated_vm_ip(), ...native_ips]);
    for (let value = start; value <= end; value++) {
      const candidate = int_to_ipv4(value);
      if (!used.has(candidate)) return candidate;
    }
    throw new ControlError("vm_ip_exhausted", "no VM private IP is available");
  }

  _is_guest_wait_timeout(error: ControlError): boolean {
    if (error instanceof GuestWaitTimeout || error.code === "guest_wait_timeout") return true;
    const detail = is_record(error.detail) ? error.detail : {};
    return error.code === "command_failed" &&
      (detail.returncode === 1 || detail.returncode === 1n) &&
      typeof detail.stderr === "string" && /(?:timed?\s*out|timeout)/i.test(detail.stderr);
  }

  _diagnostic_error(error: any): Data {
    const detail: Data = { type: error instanceof Error ? error.name || error.constructor.name : typeof error };
    if (error instanceof ControlError) {
      detail.code = error.code;
      if (error.code === "command_timeout" && is_record(error.detail)) {
        for (const key of ["reaped", "pipes_drained", "process_group_killed", "process_group_cleanup_error"])
          if (key in error.detail) detail[key] = error.detail[key];
      }
    }
    return detail;
  }

  _diagnostic_timeout(deadline_monotonic: number | null = null): number {
    if (deadline_monotonic === null) return DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS;
    const remaining = deadline_monotonic - monotonic();
    if (remaining <= 0)
      throw new ControlError("diagnostic_deadline", "shared readiness budget is exhausted");
    return Math.min(DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS, remaining);
  }

  _parse_cpu_psi(raw: string): Data { return parse_cpu_psi(raw); }

  _redact_startup_log(raw: string): Data {
    const collector_truncated = raw.endsWith(DIAGNOSTIC_LOG_TRUNCATION_MARKER);
    if (collector_truncated) raw = raw.slice(0, -DIAGNOSTIC_LOG_TRUNCATION_MARKER.length);
    let redacted = raw.replace(
      /-----BEGIN [A-Z0-9 _-]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 _-]*PRIVATE KEY-----|$)/gis,
      "[REDACTED PRIVATE KEY]",
    );
    const label = String.raw`(?:[A-Za-z0-9]+[_-])*(?:password|passwd|passphrase|secret|token|authorization|private[_ -]?key|api[_ -]?key|access[_ -]?key|client[_ -]?secret|credential[s]?)(?:[_-][A-Za-z0-9]+)*`;
    const quoted = new RegExp(
      String.raw`(["']?${label}["']?)(\s*[:=]\s*)(["'])(.*?)\3`, "gi",
    );
    const unquoted = new RegExp(
      String.raw`(["']?${label}["']?)(\s*[:=]\s*)([^\s,;}\]]+)`, "gi",
    );
    redacted = redacted.replace(quoted, (_match, name, separator, delimiter) =>
      `${name}${separator}${delimiter}[REDACTED]${delimiter}`);
    redacted = redacted.replace(unquoted, (_match, name, separator) =>
      `${name}${separator}[REDACTED]`);
    redacted = redacted.replace(/(\bsshpass\b[ \t]+-p)[ \t]*[\s\S]*/gi, "$1 [REDACTED]");
    redacted = redacted.replace(
      /(\b[A-Za-z][A-Za-z0-9+.-]*:\/\/[^/\s:@]+:)([^@\s/]+)(@)/gi, "$1[REDACTED]$3",
    );
    redacted = redacted.replace(/(bearer\s+)[A-Za-z0-9._~+/=-]+/gi, "$1[REDACTED]");
    const characters = Array.from(redacted);
    const truncated = collector_truncated || characters.length > DIAGNOSTIC_LOG_LIMIT;
    if (truncated) redacted = characters.slice(0, DIAGNOSTIC_LOG_LIMIT).join("");
    return { text: redacted, truncated };
  }

  async _qemu_schedstat(pid: any): Promise<Data> {
    if (!(typeof pid === "bigint" || typeof pid === "number" && Number.isSafeInteger(pid)) ||
      compare(pid, 2) < 0)
      throw new TypeError("Incus did not report a valid QEMU pid");
    const cmdline = (await this._read_text(`/proc/${pid}/cmdline`)).toLowerCase();
    if (!cmdline.includes("qemu")) throw new TypeError("reported process is not QEMU");
    const fields = words(await this._read_text(`/proc/${pid}/schedstat`));
    if (fields.length < 2 || fields.slice(0, 2).some(field => !/^[0-9]+$/.test(field)))
      throw new TypeError("QEMU schedstat is invalid");
    return {
    pid,
      runtime_seconds: Number(BigInt(fields[0]!)) / 1_000_000_000,
      runqueue_wait_seconds: Number(BigInt(fields[1]!)) / 1_000_000_000,
    };
  }

  async _emit_slow_start_diagnostic(task: Data, {
    phase, started_monotonic, readiness, deadline_monotonic = null,
  }: Data): Promise<Data> {
    const errors: Data[] = [];
    const payload: Data = {
      event: "agent-vm-slow-start",
      task_id: task.task_id, session_id: task.session_id, generation: task.generation,
      phase, elapsed_seconds: elapsed(started_monotonic),
      budget_seconds: GUEST_READINESS_BUDGET_SECONDS,
      stage_timings: { ...(readiness.stage_timings ?? {}) }, readiness: { ...readiness },
      native_status: {}, cpu_psi: {}, qemu_schedstat: {}, startup_log: {},
    };
    try {
      const info = await this.incus.instance_info(task.instance_name, {
        timeout: this._diagnostic_timeout(deadline_monotonic), bounded: true,
      });
      payload.native_status = Object.fromEntries(
        ["status", "pid", "processes"].filter(key => key in info).map(key => [key, info[key]]),
      );
      try { payload.qemu_schedstat = await this._qemu_schedstat(info.pid); }
      catch (error) { errors.push({ source: "qemu_schedstat", ...this._diagnostic_error(error) }); }
    } catch (error) {
      errors.push({ source: "native_status", ...this._diagnostic_error(error) });
    }
    try {
      const observation = await this._cpu_pressure_observation();
      if (observation.verified) payload.cpu_psi = observation;
      else if (is_record(observation.error))
        errors.push({
          source: "cpu_psi", type: "ControlError",
          code: observation.error.code ?? "resource_unverified",
        });
      else errors.push({ source: "cpu_psi", type: "ResourceObservationError" });
    } catch (error) {
      errors.push({ source: "cpu_psi", ...this._diagnostic_error(error) });
    }
    try {
      payload.startup_log = this._redact_startup_log(await this.incus.instance_log(
        task.instance_name, { timeout: this._diagnostic_timeout(deadline_monotonic) },
      ));
    } catch (error) {
      errors.push({ source: "startup_log", ...this._diagnostic_error(error) });
    }
    if (errors.length) payload.errors = errors;
    try {
      this.store.audit("boot-diagnostic", errors.length ? "partial" : "ok", {
        task_id: task.task_id, session_id: task.session_id,
        generation: task.generation, detail: payload,
      });
    } catch (error) {
      (payload.errors ??= []).push({ source: "audit", ...this._diagnostic_error(error) });
    }
    try {
      await new Promise<void>((resolve, reject) => {
        process.stderr.write(canonical_json(payload) + "\n", error => error ? reject(error) : resolve());
      });
    } catch (error) {
      (payload.errors ??= []).push({ source: "stderr", ...this._diagnostic_error(error) });
    }
    return payload;
  }

  _readiness_timeout(
    condition: string, started_monotonic: number, readiness: Data, diagnostic: Data | null,
  ): never {
    const duration = elapsed(started_monotonic);
    readiness.elapsed_seconds = duration;
    readiness.diagnostic = {
      emitted: diagnostic !== null, phase: diagnostic?.phase ?? null,
      elapsed_seconds: diagnostic?.elapsed_seconds ?? null,
    };
    throw new ControlError(
      "guest_readiness_timeout", "guest readiness did not complete within the shared startup budget",
      {
        phase: condition, condition, elapsed_seconds: duration,
        budget_seconds: GUEST_READINESS_BUDGET_SECONDS, readiness,
      },
    );
  }

  async _wait_guest_ready(task: Data, { stage_timings = null }: Data = {}): Promise<Data> {
    const started = monotonic();
    const readiness: Data = {
      budget_seconds: GUEST_READINESS_BUDGET_SECONDS, elapsed_seconds: 0,
      stage_timings: { ...(stage_timings ?? {}) }, phases: {},
    };
    let diagnostic: Data | null = null;
    const remaining = (condition: string): number => {
      const current = this.store.get_task({ task_id: task.task_id });
      const expiry = current ? parse_time(current.lease_expires_at) : null;
      if (!current || compare(current.generation, task.generation) !== 0 ||
        !["preparing", "active"].includes(current.state) || !expiry)
        throw new ControlError("lease_expired", "guest preparation was revoked");
      if (expiry <= this.now())
        throw new ControlError("lease_expired", "lease expired while waiting for guest readiness");
      const seconds = Math.trunc(GUEST_READINESS_BUDGET_SECONDS - Math.max(0, monotonic() - started));
      if (seconds < 1) this._readiness_timeout(condition, started, readiness, diagnostic);
      const lease_seconds = (expiry.getTime() - this.now().getTime()) / 1000;
      if (lease_seconds < 1)
        throw new ControlError("lease_expired", "lease expired while waiting for guest readiness");
      return Math.min(seconds, Math.trunc(lease_seconds));
    };
    const wait_once = async (condition: string, seconds: number): Promise<void> => {
      await this.incus.wait(task.instance_name, condition, seconds);
      remaining(condition);
    };
    const fail_fast = (condition: string, error: ControlError): never => {
      const detail = is_record(error.detail) ? { ...error.detail } : { cause: error.as_dict() };
      throw new ControlError(error.code, error.message, { ...detail, phase: condition, readiness });
    };
    const record_phase = (condition: string, phase_started: number): void => {
      readiness.phases[condition] = {
        elapsed_seconds: elapsed(phase_started), total_elapsed_seconds: elapsed(started),
      };
    };
    for (const condition of ["agent", "ipv4"]) {
      const phase_started = monotonic();
      const first_seconds = diagnostic === null
        ? Math.min(
          remaining(condition),
          Math.max(1, Math.trunc(GUEST_READINESS_DIAGNOSTIC_SECONDS - Math.max(0, monotonic() - started))),
        ) : remaining(condition);
      try {
        await wait_once(condition, first_seconds);
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        if (!this._is_guest_wait_timeout(error)) fail_fast(condition, error);
        if (diagnostic === null && monotonic() - started < GUEST_READINESS_DIAGNOSTIC_SECONDS) {
          const boundary_seconds = Math.min(
            remaining(condition),
            Math.max(1, Math.trunc(GUEST_READINESS_DIAGNOSTIC_SECONDS - (monotonic() - started))),
          );
          let boundary_succeeded = false;
          try {
            await wait_once(condition, boundary_seconds);
            boundary_succeeded = true;
          } catch (boundary_error) {
            if (!(boundary_error instanceof ControlError)) throw boundary_error;
            if (!this._is_guest_wait_timeout(boundary_error)) fail_fast(condition, boundary_error);
          }
          if (boundary_succeeded) {
            record_phase(condition, phase_started);
            continue;
          }
        }
        if (diagnostic === null)
          diagnostic = await this._emit_slow_start_diagnostic(task, {
            phase: condition, started_monotonic: started, readiness,
            deadline_monotonic: started + GUEST_READINESS_BUDGET_SECONDS,
          });
        try {
          await wait_once(condition, remaining(condition));
        } catch (continuation_error) {
          if (!(continuation_error instanceof ControlError)) throw continuation_error;
          if (this._is_guest_wait_timeout(continuation_error))
            this._readiness_timeout(condition, started, readiness, diagnostic);
          fail_fast(condition, continuation_error);
        }
      }
      record_phase(condition, phase_started);
    }
    readiness.elapsed_seconds = elapsed(started);
    readiness.diagnostic = {
      emitted: diagnostic !== null, phase: diagnostic?.phase ?? null,
      elapsed_seconds: diagnostic?.elapsed_seconds ?? null,
    };
    return readiness;
  }

  async _verify_guest_tools(instance_name: string): Promise<void> {
    const tools = [...new Set(["bun", ...this.config.required_guest_tools.map(String)])];
    const script = "set -eu; for tool in " + tools.map(shell_quote).join(" ") +
      '; do command -v "$tool" >/dev/null; done; ' +
      "if docker compose version >/dev/null 2>&1; then :; " +
      "elif command -v docker-compose >/dev/null 2>&1; then :; else exit 1; fi";
    await this.incus.exec(instance_name, ["sh", "-lc", script]);
  }

  async _guest_ip(instance_name: string): Promise<string> {
    const info = await this.incus.instance_info(instance_name);
    const state = Object.hasOwn(info, "state") ? info.state : info;
    const network = is_record(state) ? state.network ?? {} : {};
    const candidates: string[] = [];
    if (is_record(network)) {
      for (const [name, data] of Object.entries(network)) {
        if (name === "lo" || !is_record(data) || !Array.isArray(data.addresses)) continue;
        for (const address of data.addresses)
          if (is_record(address) && address.family === "inet" && typeof address.address === "string")
            candidates.push(address.address);
      }
    }
    if (!candidates.length)
      throw new ControlError("guest_ip_missing", "Incus did not report a guest IPv4 address");
    const expected = this._expected_vm_ip;
    if (!candidates.includes(expected))
      throw new ControlError(
        "guest_ip_mismatch", "guest IPv4 address does not match the allocated fixed address",
        { expected, reported: candidates },
      );
    return expected;
  }

  async _install_public_key(instance_name: string, public_key: string): Promise<void> {
    public_key = require_string(public_key, "public_key");
    if (public_key.includes("\n") || public_key.includes("\r"))
      throw new ControlError("invalid_request", "public_key must contain one line");
    const fields = words(public_key);
    if (fields.length < 2 || !["ssh-ed25519", "ecdsa-sha2-nistp256", "rsa-sha2-512", "ssh-rsa"].includes(fields[0]!))
      throw new ControlError("invalid_request", "public_key is not a supported SSH public key");
    const script = "set -eu; umask 077; mkdir -p /root/.ssh; " +
      "cat > /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys";
    await this.incus.exec(instance_name, ["sh", "-lc", script], { input_data: public_key + "\n" });
  }

  async _guest_host_key(instance_name: string): Promise<string> {
    const result = await this.incus.exec(
      instance_name, ["sh", "-lc", "cat /etc/ssh/ssh_host_ed25519_key.pub"],
    );
    const fields = words(result.stdout.toString("utf8"));
    if (fields.length < 2 || fields[0] !== "ssh-ed25519")
      throw new ControlError("guest_host_key_missing", "guest did not provide an ed25519 host key");
    return fields.slice(0, 2).join(" ");
  }

  async _configure_network(instance_name: string, vm_ip: string): Promise<void> {
    const properties: Data = {
      "ipv4.address": vm_ip,
      "security.mac_filtering": "true", "security.ipv4_filtering": "true",
      "security.ipv6_filtering": "true", "security.port_isolation": "true",
      "security.acls": this.config.network_acl,
      "security.acls.default.ingress.action": "allow",
      "security.acls.default.egress.action": "allow",
    };
    for (const [key, value] of Object.entries(properties))
      await this.incus.device_set(instance_name, "eth0", key, value);
  }

  async _network_guard(): Promise<void> {
    const name = require_string(this.config.network_acl, "network_acl", PORT_NAME_RE);
    const project = await this.incus.query(
      "/1.0/projects/" + quote(this.config.project), { project: "default" },
    );
    if (!is_record(project) || !is_record(project.config))
      throw new ControlError("network_unverified", "Incus project response is malformed");
    if (project.config["restricted.devices.proxy"] !== "allow")
      throw new ControlError("network_unverified", "Incus project does not allow managed proxy NAT devices");
    const acl = await this.incus.query("/1.0/network-acls/" + quote(name), { project: "default" });
    if (!is_record(acl) || !is_record(acl.config))
      throw new ControlError("network_unverified", "isolation ACL response is not an object");
    if (acl.config["user.agent-vm.managed"] !== "native-v1")
      throw new ControlError("network_unverified", "isolation ACL ownership is not verified");
    const fields = [
      "action", "state", "source", "destination", "protocol",
      "source_port", "destination_port", "icmp_type", "icmp_code",
    ];
    const normalize = (value: Data): Data => Object.fromEntries(
      fields.map(key => [key, Object.hasOwn(value, key) ? value[key] : key === "state" ? "enabled" : ""]),
    );
    for (const [direction, rules] of Object.entries(this._isolation_rules()) as [string, Data[]][]) {
      const actual = acl[direction];
      if (!Array.isArray(actual) || actual.some(value => !is_record(value)))
        throw new ControlError("network_unverified", "isolation ACL rules are malformed");
      const normalized = actual.map(normalize);
      for (const rule of rules)
        if (!normalized.some(value => equal(value, normalize(rule))))
          throw new ControlError(
            "network_unverified", "isolation ACL lacks required peer/host withdrawal rules",
          );
    }
  }

  _isolation_rules(): Data {
    const range = `${this.config.vm_ip_start}-${this.config.vm_ip_end}`;
    return {
      ingress: [{ action: "drop", state: "enabled", source: range }],
      egress: [
        { action: "drop", state: "enabled", destination: range },
        { action: "drop", state: "enabled", destination: this.config.host_address },
        {
          action: "drop", state: "enabled", destination: this.config.gateway_ip,
          protocol: "tcp", destination_port: "22",
        },
      ],
    };
  }

  async _add_proxy(task: Data, port: Data): Promise<void> {
    await this._native_lock(task, async () => {
      const current = this._native_operation_current(task);
      let native = (await this._ownership(current)).devices[port.device_name];
      const expected = this._proxy_properties(current, port);
      if (native != null) {
        if (Object.entries(expected).some(([key, value]) => native[key] !== value))
          throw new ControlError("ownership_mismatch", "native device does not match reservation");
        return;
      }
      await this.incus.device_add(
        current.instance_name, port.device_name, "proxy",
        Object.fromEntries(Object.entries(expected).filter(([key]) => key !== "type")),
      );
      native = (await this._ownership(current)).devices[port.device_name] ?? {};
      if (Object.entries(expected).some(([key, value]) => native[key] !== value))
        throw new ControlError("mapping_unverified", "native proxy addition was not confirmed");
    });
  }

  async _conntrack_delete(port: Data, task: Data): Promise<void> {
    if (await this._which("conntrack") == null)
      throw new ControlError("conntrack_unavailable", "conntrack-tools is required for exact flow withdrawal");
    const match = [
      "-p", String(port.protocol), "--orig-dst", String(this.config.host_address),
      "--dport", String(port.host_port), "--reply-src", String(task.vm_ip),
      "--reply-port-src", String(port.vm_port),
    ];
    const result = await this.runner.run(["conntrack", "-D", ...match], { check: false, timeout: 2 });
    if (result.returncode !== 0 && !(result.returncode === 1 && result.stderr.includes("0 flow entries")))
      throw new ControlError("conntrack_delete_failed", "exact conntrack deletion failed", {
        returncode: result.returncode, stderr: result.stderr.toString("utf8"),
      });
    const remaining = await this.runner.run(["conntrack", "-L", ...match], { check: false, timeout: 2 });
    if (remaining.returncode || remaining.stdout.toString("utf8").trim())
      throw new ControlError("conntrack_unverified", "matching flows remain or cannot be inspected");
  }

  _cleanup_current(task: Data): Data | null {
    // Synchronous snapshot: callers serialize effects with the native lock.
    return this.store.transaction(() => {
      const current = this.store.get_task({ task_id: task.task_id });
      if (!current || compare(current.generation, task.generation) !== 0 ||
        current.state !== "closing" || current.instance_name !== task.instance_name)
        return null;
      return current;
    });
  }

  async _cleanup_task(task: Data): Promise<[string[], string[]]> {
    return await this._native_lock(task, () => this._cleanup_task_locked(task));
  }

  async _cleanup_task_locked(task: Data): Promise<[string[], string[]]> {
    const errors: string[] = [], completed: string[] = [];
    const pending = new Map<string, [Data, string, boolean]>();
    let current = this._cleanup_current(task);
    if (!current) return [["stale-cleanup:generation"], completed];
    task = current;
    let exists: boolean;
    try {
      exists = (await this.incus.list_instances()).some(item => item.name === task.instance_name);
    } catch (error) {
      if (!(error instanceof ControlError)) throw error;
      errors.push("inventory:" + error.code);
      exists = true;
    }
    if (exists) {
      try { await this._ownership(task); }
      catch (error) {
        if (!(error instanceof ControlError)) throw error;
        return [["ownership:" + error.code], []];
      }
    }
    for (const port of this.store.get_ports(task.task_id)) {
      current = this._cleanup_current(task);
      if (!current) { errors.push("stale-cleanup:generation"); break; }
      task = current;
      let proxy_error: string | null = null;
      try { if (exists) await this._remove_proxy(task, port); }
      catch (error) {
        if (!(error instanceof ControlError)) throw error;
        proxy_error = "remove-proxy:" + port.device_name + ":" + error.code;
      }
      current = this._cleanup_current(task);
      if (!current) { errors.push("stale-cleanup:generation"); break; }
      task = current;
      let conntrack_ok = false;
      try {
        await this._conntrack_delete(port, task);
        conntrack_ok = true;
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        errors.push("withdraw:" + port.device_name + ":" + error.code);
      }
      if (proxy_error === null && conntrack_ok) completed.push("withdraw:" + port.device_name);
      else if (proxy_error !== null) pending.set(port.device_name, [port, proxy_error, conntrack_ok]);
    }

    // Withdrawal failures never suppress the stop attempt.
    if (exists) {
      current = this._cleanup_current(task);
      if (!current) errors.push("stop:stale-cleanup");
      else {
        task = current;
        try {
          await this._ownership(task);
          if ((await this.incus.instance_info(task.instance_name)).status.toLowerCase() !== "stopped")
            await this.incus.stop(task.instance_name, { force: true });
          if ((await this.incus.instance_info(task.instance_name)).status.toLowerCase() !== "stopped")
            throw new ControlError("stop_unverified", "native instance is not stopped");
          completed.push("verify-stop");
        } catch (error) {
          if (!(error instanceof ControlError)) throw error;
          errors.push("stop:" + error.code);
        }
      }
    } else completed.push("instance-absent");

    for (const [device_name, [port, proxy_error, conntrack_ok]] of pending) {
      current = this._cleanup_current(task);
      if (!current) {
        errors.push(proxy_error, "remove-proxy-retry:" + device_name + ":stale-cleanup");
        continue;
      }
      task = current;
      try { await this._remove_proxy(task, port); }
      catch (error) {
        if (!(error instanceof ControlError)) throw error;
        errors.push(proxy_error, "remove-proxy-retry:" + device_name + ":" + error.code);
        continue;
      }
      if (conntrack_ok) completed.push("withdraw:" + device_name);
    }
    return [errors, completed];
  }

  _proxy_properties(task: Data, port: Data): Data {
    return {
      type: "proxy",
      listen: `${port.protocol}:${this.config.host_address}:${port.host_port}`,
      connect: `${port.protocol}:${task.vm_ip}:${port.vm_port}`,
      nat: "true",
    };
  }

  async _remove_proxy(task: Data, port: Data): Promise<void> {
    const info = await this._ownership(task);
    const native = info.devices[port.device_name];
    if (native != null) {
      const expected = this._proxy_properties(task, port);
      if (Object.entries(expected).some(([key, value]) => native[key] !== value))
        throw new ControlError("ownership_mismatch", "native mapping differs from its reservation");
      await this.incus.device_remove(task.instance_name, port.device_name);
    }
    const after = await this._ownership(task);
    if (Object.hasOwn(after.expanded_devices ?? after.devices ?? {}, port.device_name))
      throw new ControlError("mapping_unverified", "native mapping is still present");
  }

  async _schedule_expiry(task: Data): Promise<void> {
    if (!task.lease_expires_at)
      throw new ControlError("lease_missing", "active task has no lease expiry");
    await this.systemd.schedule_expiry(
      String(task.task_id), narrow(integer(task.generation)), String(task.lease_expires_at),
    );
  }

  async _image_guard(): Promise<void> {
    const fingerprint = String(this.config.image_fingerprint);
    let image: any;
    try { image = await this.incus.query("/1.0/images/" + quote(fingerprint)); }
    catch (error) {
      if (!(error instanceof ControlError)) throw error;
      throw new ControlError("image_unavailable", "pinned VM image is unavailable", error.as_dict());
    }
    if (!is_record(image) || image.fingerprint !== fingerprint || image.type !== "virtual-machine")
      throw new ControlError("image_unverified", "pinned image identity or VM type does not match");
  }

  async doctor(_request: Data, _session_id: string | null): Promise<Data> {
    const issues: string[] = [];
    let version: string | null;
    try {
      version = (await this.runner.run(["incus", "version"], { timeout: 15 })).stdout.toString("utf8").trim();
      await this.incus.list_instances();
    } catch (error) {
      if (!(error instanceof ControlError)) throw error;
      issues.push(error.code);
      version = null;
    }
    try { await this._image_guard(); }
    catch (error) {
      if (!(error instanceof ControlError)) throw error;
      issues.push(error.code);
    }
    let capacity: Data;
    try { capacity = await this._ensure_capacity_verified(); }
    catch (error) {
      if (!(error instanceof ControlError)) throw error;
      issues.push(error.code);
      capacity = { error: error.as_dict() };
    }
    if (await this._which("conntrack") == null) issues.push("conntrack_missing");
    try { await this._network_guard(); }
    catch (error) {
      if (!(error instanceof ControlError)) throw error;
      issues.push(error.code);
    }
    if (this.systemd.enabled) {
      for (const unit of ["incus.service", "agent-vm-native-age-gc.timer"]) {
        try {
          const result = await this.runner.run(
            ["systemctl", "is-active", "--quiet", unit], { timeout: 5, check: false },
          );
          if (result.returncode) issues.push("unit_inactive:" + unit);
        } catch (error) {
          if (!(error instanceof ControlError)) throw error;
          issues.push("unit_unverified:" + unit);
        }
      }
      if (!await this.systemd.enabled_unit_active("agent-vm-native-age-gc.timer"))
        issues.push("unit_disabled:agent-vm-native-age-gc.timer");
      const executable = "ExecStart=/usr/local/bin/bun /opt/agent-vm-native/agent_vm_control.ts";
      for (const [unit, argument] of [
        ["agent-vm-native-expire@.service", "--timer-expire %i"],
        ["agent-vm-native-reconcile.service", "--timer-reconcile"],
      ] as const) {
        if (!await this.systemd.service_asset_active(unit, [`${executable} ${argument}`]))
          issues.push("unit_unverified:" + unit);
        if (unit === "agent-vm-native-reconcile.service" &&
          !await this.systemd.enabled_service_ready(unit))
          issues.push("unit_disabled:" + unit);
      }
      if (!await this.systemd.service_asset_active(
        "agent-vm-native-age-gc.service", [`${executable} --timer-age-gc`],
      )) issues.push("unit_unverified:agent-vm-native-age-gc.service");
      for (const task of this.store.list_tasks()) {
        if (task.state !== "active") continue;
        let valid_timer = false;
        try {
          valid_timer = await this.systemd.expiry_active(
            String(task.task_id), narrow(integer(task.generation)), String(task.lease_expires_at),
          );
        } catch (error) {
          if (!(error instanceof ControlError || error instanceof TypeError ||
            error instanceof RangeError || error instanceof SyntaxError)) throw error;
        }
        if (!valid_timer) issues.push("timer_unverified:" + String(task.task_id));
      }
    }
    const resources = await this._resource_diagnostics({
      capacity_observation: "error" in capacity ? null : capacity,
    });
    const instances = await this._instance_diagnostics();
    const admission = await this._default_admission_diagnostics(resources, capacity, instances);
    return {
      ready: issues.length === 0, issues, incus_version: version, capacity,
      resources, instances, default_admission: admission,
      admission_ready: issues.length === 0 && admission.ready,
      release: this._release_metadata(),
      release_warnings: this._merge_warnings(this.release_warnings),
      config: Object.fromEntries(
        ["project", "profile", "image_fingerprint", "network", "pool", "host_address"]
          .map(key => [key, this.config[key]]),
      ),
    };
  }
}
