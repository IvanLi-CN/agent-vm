import { ResourceControl } from "./control-resources.ts";
import { guest_source } from "./guest-root-growth.ts";
import {
  ControlError, PROTOCOL_VERSION, GUEST_READINESS_BUDGET_SECONDS, MEMORY_SETTLE_SECONDS,
  PORT_NAME_RE, RESOURCE_KEYS, SESSION_ID_RE, type Data, type Integer, require_nonempty_string, require_string, require_int,
  canonical_json, parse_json, digest, iso_time, parse_time, is_record, resource_mapping,
  add, sub, mul, floor_div, compare, integer, narrow,
} from "./control-types.ts";

const equal = (a: any, b: any): boolean => canonical_json(a ?? null) === canonical_json(b ?? null);
const is_integer = (value: any): value is Integer =>
  typeof value === "bigint" || typeof value === "number" && Number.isSafeInteger(value);
const maximum = (...values: Integer[]): Integer => values.reduce((a, b) => compare(a, b) >= 0 ? a : b);
const reservation = (...values: Data[]): Data =>
  Object.fromEntries(RESOURCE_KEYS.map(key => [key, maximum(...values.map(value => value[key] ?? 0))]));
const elapsed = (started: number): number => Math.round(Math.max(0, performance.now() - started)) / 1000;
const after = (date: Date, seconds: Integer): Date => {
  const milliseconds = integer(add(date.getTime(), mul(seconds, 1000)));
  if (milliseconds < -8640000000000000n || milliseconds > 8640000000000000n)
    throw new ControlError("invalid_time", "timestamp exceeds the supported date range");
  return new Date(Number(milliseconds));
};
const detail_of = (error: ControlError, cause = true): Data =>
  is_record(error.detail) ? { ...error.detail } : cause ? { cause: error.as_dict() } : {};
const fields_equal = (a: Data, b: Data, keys: string[]): boolean =>
  keys.every(key => equal(a[key], b[key]));
const FAILURE_REQUIRED = [
  "phase", "command", "returncode", "stdout", "stderr", "capacity", "transient",
  "stdout_truncated", "stderr_truncated",
];
const CAPACITY_NUMBERS = [
  "requested_bytes", "disk_bytes", "partition_bytes", "filesystem_bytes",
  "filesystem_before", "filesystem_threshold_bytes",
];
const CAPACITY_STRINGS = ["filesystem", "source", "parent", "partition"];
const CAPACITY_REQUIRED = [...CAPACITY_NUMBERS, ...CAPACITY_STRINGS, "tools", "verified"];
const FAILURE_MATCH = [
  "phase", "command", "returncode", "stdout", "stderr", "capacity", "transient", "cause",
  "stdout_truncated", "stderr_truncated", "reaped", "pipes_drained",
  "process_group_killed", "process_group_cleanup_error",
];
const FAILURE_SAME = [
  "phase", "command", "returncode", "stdout", "stderr", "transient", "cause", "retry_of",
  "stdout_truncated", "stderr_truncated", "reaped", "pipes_drained",
  "process_group_killed", "process_group_cleanup_error",
];
const SUCCESS_FAILURE_MATCH = [
  "phase", "command", "returncode", "stdout", "stderr", "capacity", "transient", "cause",
  "stdout_truncated", "stderr_truncated", "reaped", "pipes_drained",
];
const CAPACITY_MATCH = [
  "requested_bytes", "disk_bytes", "filesystem_before", "filesystem_threshold_bytes",
  "filesystem", "source", "parent", "partition", "tools", "verified",
];

function valid_failure(error: any, complete: boolean, report?: Data): boolean {
  const pending: [any, number][] = [[error, 0]];
  while (pending.length) {
    const [current, depth] = pending.pop()!;
    if (!is_record(current) || !FAILURE_REQUIRED.every(key => key in current)) return false;
    const capacity = current.capacity;
    if (
      typeof current.phase !== "string" ||
      !Array.isArray(current.command) || !current.command.length ||
      current.command.some((item: any) => typeof item !== "string") ||
      current.returncode !== null && !is_integer(current.returncode) ||
      typeof current.stdout !== "string" || typeof current.stderr !== "string" ||
      Buffer.byteLength(current.stdout, "utf8") > 8192 || Buffer.byteLength(current.stderr, "utf8") > 4000 ||
      !is_record(capacity) || typeof capacity.verified !== "boolean" || !is_integer(capacity.requested_bytes) ||
      typeof current.transient !== "boolean" ||
      typeof current.stdout_truncated !== "boolean" || typeof current.stderr_truncated !== "boolean" ||
      ["reaped", "pipes_drained", "process_group_killed"].some(key => key in current && typeof current[key] !== "boolean") ||
      "process_group_cleanup_error" in current &&
        current.process_group_cleanup_error !== null && typeof current.process_group_cleanup_error !== "string" ||
      "cause" in current && typeof current.cause !== "string" ||
      current.cause === "command_timeout" && (
        ["reaped", "pipes_drained", "process_group_killed"].some(key => typeof current[key] !== "boolean") ||
        !("process_group_cleanup_error" in current)
      )
    ) return false;
    if (complete && (
      !CAPACITY_REQUIRED.every(key => key in capacity) ||
      CAPACITY_NUMBERS.some(key => !is_integer(capacity[key])) ||
      CAPACITY_STRINGS.some(key => typeof capacity[key] !== "string") ||
      !is_record(capacity.tools)
    )) return false;
    if (report && (capacity.verified !== true || !fields_equal(capacity, report, CAPACITY_MATCH))) return false;
    if ("retry_of" in current) {
      if (depth >= 2 || !is_record(current.retry_of)) return false;
      pending.push([current.retry_of, depth + 1]);
    }
  }
  return true;
}

function valid_failure_attempts(attempts: any): boolean {
  if (!Array.isArray(attempts) || !attempts.length || attempts.length > 2) return false;
  for (const [index, record] of attempts.entries()) {
    if (
      !is_record(record) || !is_integer(record.attempt) || compare(record.attempt, index + 1) !== 0 ||
      "error" in record && !valid_failure(record.error, true) ||
      "refresh_error" in record && !valid_failure(record.refresh_error, true)
    ) return false;
    if ("refresh_error" in record) {
      const refresh = record.refresh_error, previous = record.error;
      if (
        !is_record(refresh.retry_of) || !is_record(previous) ||
        !fields_equal(previous, refresh.retry_of, FAILURE_MATCH) ||
        refresh.transient === false && index !== attempts.length - 1 ||
        refresh.transient === true && index === attempts.length - 1
      ) return false;
    }
  }
  return true;
}

function valid_failure_report(error: any): boolean {
  if (!is_record(error) || !valid_failure(error, false)) return false;
  const attempts = error.capacity.attempts;
  if (!Array.isArray(attempts) || attempts.length > 2) return false;
  if (!attempts.length) return error.capacity.verified === false && !("retry_of" in error);
  if (!valid_failure(error, true) || !valid_failure_attempts(attempts)) return false;
  const final = attempts[attempts.length - 1], final_error = final.refresh_error || final.error;
  if (!is_record(final_error) || !fields_equal(error, final_error, FAILURE_SAME)) return false;
  if ("retry_of" in error) {
    const previous = "refresh_error" in final ? final.error : attempts.length > 1 ? attempts[attempts.length - 2].error : null;
    if (!is_record(previous) || !fields_equal(previous, error.retry_of, FAILURE_MATCH)) return false;
  }
  return true;
}

function valid_capacity_report(value: any, disk_gib: Integer): value is Data {
  if (!is_record(value) || !["grown", "grown_after_retry", "already_grown"].includes(value.status)) return false;
  if (["virtual_bytes", "partition_bytes", "filesystem_before", "filesystem_bytes", "filesystem_threshold_bytes", "attempts"]
    .some(key => !is_integer(value[key]))) return false;
  const capacity = value.capacity;
  if (
    !is_record(capacity) || capacity.verified !== true ||
    !["status", ...CAPACITY_NUMBERS, ...CAPACITY_STRINGS, "tools", "attempts"].every(key => key in capacity) ||
    CAPACITY_NUMBERS.some(key => !is_integer(capacity[key])) ||
    !Array.isArray(capacity.attempts) || compare(value.attempts, 2) > 0 ||
    capacity.attempts.length > 2 || compare(value.attempts, capacity.attempts.length) !== 0
  ) return false;
  const attempts = capacity.attempts;
  for (const [index, record] of attempts.entries()) {
    if (
      !is_record(record) || !is_integer(record.attempt) || compare(record.attempt, index + 1) !== 0 ||
      "error" in record && !valid_failure(record.error, true, capacity) ||
      "refresh_error" in record && !valid_failure(record.refresh_error, true, capacity)
    ) return false;
    if ("refresh_error" in record) {
      const refresh = record.refresh_error, previous = record.error;
      if (
        refresh.transient !== true || !is_record(refresh.retry_of) || !is_record(previous) ||
        !fields_equal(previous, refresh.retry_of, SUCCESS_FAILURE_MATCH)
      ) return false;
    }
  }
  if (["grown_after_retry", "already_grown"].includes(value.status) && attempts.length === 2) {
    const first = attempts[0].error;
    if (
      !valid_failure(first, true, capacity) || first.transient !== true ||
      "error" in attempts[1] || "refresh_error" in attempts[1]
    ) return false;
  } else if (attempts.some((record: Data) => "error" in record)) return false;
  const target = mul(disk_gib, 1024 ** 3), threshold = floor_div(mul(target, 95), 100);
  const { status, filesystem, source, parent, partition } = value;
  const direct_device = (item: any): boolean =>
    typeof item === "string" && item.startsWith("/dev/") && item.split("/").length === 3;
  const parent_name = typeof parent === "string" ? parent.split("/").pop()! : "";
  const expected_source = typeof parent === "string" && typeof partition === "string"
    ? parent + (/[0-9]$/.test(parent_name) ? "p" : "") + partition : "";
  const tools = capacity.tools;
  const required_tools = ["findmnt", "lsblk", "blockdev", "growpart", filesystem === "ext4" ? "resize2fs" : "xfs_growfs"];
  return (
    equal(value.partition, capacity.partition) && equal(capacity.status, status) &&
    compare(capacity.requested_bytes, target) === 0 &&
    compare(value.virtual_bytes, capacity.disk_bytes) === 0 &&
    compare(value.partition_bytes, capacity.partition_bytes) === 0 &&
    compare(value.filesystem_bytes, capacity.filesystem_bytes) === 0 &&
    compare(value.filesystem_threshold_bytes, capacity.filesystem_threshold_bytes) === 0 &&
    equal(filesystem, capacity.filesystem) && equal(source, capacity.source) && equal(parent, capacity.parent) &&
    compare(capacity.filesystem_before, value.filesystem_before) === 0 &&
    compare(value.virtual_bytes, target) >= 0 && compare(value.partition_bytes, threshold) >= 0 &&
    compare(value.filesystem_before, 0) >= 0 && compare(value.filesystem_before, value.filesystem_bytes) <= 0 &&
    compare(value.filesystem_bytes, value.filesystem_threshold_bytes) >= 0 &&
    !(["grown", "grown_after_retry"].includes(status) &&
      (compare(value.filesystem_before, threshold) >= 0 || compare(value.filesystem_bytes, value.filesystem_before) <= 0)) &&
    !(status === "already_grown" && compare(value.attempts, 0) === 0 && compare(value.filesystem_before, threshold) < 0) &&
    compare(capacity.disk_bytes, target) >= 0 &&
    compare(capacity.partition_bytes, capacity.disk_bytes) <= 0 && compare(capacity.partition_bytes, 0) >= 0 &&
    compare(capacity.filesystem_before, 0) >= 0 && compare(capacity.filesystem_bytes, capacity.partition_bytes) <= 0 &&
    compare(capacity.filesystem_threshold_bytes, threshold) === 0 &&
    ["ext4", "xfs"].includes(filesystem) && direct_device(source) && direct_device(parent) &&
    source !== parent && source === expected_source && typeof partition === "string" && /^[0-9]+$/.test(partition) &&
    is_record(tools) && required_tools.every(tool => typeof tools[tool] === "string" && tools[tool].length > 0) &&
    compare(value.attempts, 0) >= 0 && compare(value.attempts, 2) <= 0 &&
    (status !== "grown" || compare(value.attempts, 1) === 0) &&
    (status !== "grown_after_retry" || compare(value.attempts, 2) === 0)
  );
}

export class LifecycleControl extends ResourceControl {
  async status(request: Data, session_id: string | null): Promise<Data> {
    const task = await this._global_lock(() => this.store.get_task({ session_id: session_id! }));
    const result = task ? await this._runtime_summary(task) :
      { state: "absent", generation: 0, effective_lease: false, native_status: "absent", guest_available: false };
    const operation_id = request.operation_id === undefined
      ? null : require_string(request.operation_id, "operation_id", SESSION_ID_RE);
    if (operation_id !== null && task !== null) {
      let operation: Data | null = null;
      try { operation = this.store.operation(operation_id); } catch { operation = null; }
      const operation_result = is_record(operation?.result) ? operation.result : null;
      const request_generation = operation?.generation;
      const operation_generation = operation_result?.generation;
      const current_generation = task.generation;
      const previous_generation = typeof current_generation === "number" || typeof current_generation === "bigint"
        ? sub(current_generation, 1) : null;
      const generation_matches = (value: any): boolean =>
        equal(value, current_generation) || previous_generation !== null && equal(value, previous_generation);
      if (
        operation?.outcome === "success" && operation.action === "acquire" &&
        operation.operation_id === operation_id && operation.session_id === session_id &&
        operation_result?.session_id === session_id && operation_result.task_id === task.task_id &&
        generation_matches(request_generation) && equal(operation_generation, current_generation)
      ) result.recovery_operation_id = operation_id;
      else if (operation?.outcome === "pending") {
        const pending = this._pending_acquire_operations_for_task(task).filter(item => item.operation_id === operation_id);
        if (pending.length === 1 && generation_matches(pending[0].generation) &&
          pending[0].task_id === task.task_id && equal(pending[0].task_generation, current_generation))
          result.partial_operation_id = operation_id;
      } else {
        let last_error: Data | null = null;
        try { const value = parse_json(task.last_error); if (is_record(value)) last_error = value; } catch {}
        if (last_error !== null && (
          last_error.operation_id === operation_id && last_error.task_id === task.task_id &&
          equal(last_error.task_generation, current_generation) ||
          Array.isArray(last_error.pending_acquire_operation_ids) &&
          last_error.pending_acquire_operation_ids.includes(operation_id) &&
          last_error.task_id === task.task_id && equal(last_error.task_generation, current_generation)
        )) result.partial_operation_id = operation_id;
      }
    }
    const admission = await this._resource_diagnostics();
    admission.instances = await this._instance_diagnostics();
    return { ...result, admission };
  }

  async acquire(request: Data, session_id: string | null): Promise<Data> {
    const existing = this.store.get_task({ session_id: session_id! });
    if (existing?.state === "active") {
      let pending_acquires = this._pending_acquire_operations_for_task(existing);
      try { this._check_generation(existing, request); }
      catch (error) {
        if (!(error instanceof ControlError) || error.code !== "generation_mismatch" ||
          this._pending_acquire_retry_item(String(request.operation_id), session_id!, request, existing) === null) throw error;
      }
      this._require_active(existing, true);
      await this._ownership(existing);
      const resources = this._durable_resources(existing, "resources_json", { allow_empty: false });
      const target = this._requested_resources(request, resources, { active_reuse: true });
      if (!equal(target, resources))
        throw new ControlError("resource_config_mismatch", "active lease configuration differs; use resize explicitly", {
          current: resources, requested: target, differences: Object.keys(resources).filter(key => !equal(resources[key], target[key])),
        });
      const current_operation_id = String(request.operation_id);
      this._verify_pending_acquire_resources(
        pending_acquires.filter(item => item.operation_id !== current_operation_id),
        resources,
      );
      this._bind_pending_acquire_operation(request, existing, target);
      pending_acquires = this._pending_acquire_operations_for_task(existing);
      this._verify_pending_acquire_resources(pending_acquires, resources);
      const result = await this._runtime_summary(existing);
      if (pending_acquires.length) {
        await this._global_lock(() => this.store.transaction(() => {
          for (const pending of pending_acquires) this._complete_pending_acquire_operation(pending, existing, result);
        }));
      }
      return result;
    }
    if (existing && !["absent", "cached"].includes(existing.state))
      throw new ControlError("task_transition", "session slot is not available until reconciliation completes");
    const expected = existing?.generation ?? 0;
    if (compare(request.generation, expected) !== 0)
      throw new ControlError("generation_mismatch", "acquire requires the latest observed generation", { current_generation: expected });
    const admission_epoch = await this._admission_lock(() => {
      if (this.store.get_meta("draining") === "1") throw new ControlError("draining", "new leases are refused");
      return this.store.get_meta("admission_epoch") || "initial";
    });
    await this._network_guard();
    const public_key = require_nonempty_string(request.public_key, "public_key");
    const key_match = /^ssh-ed25519 [A-Za-z0-9+/=]+(?: [^\r\n]*)?$/.exec(public_key);
    if (!key_match || key_match[0] !== public_key)
      throw new ControlError("invalid_request", "acquire requires a single ed25519 public key");
    const cached = existing?.state === "cached";
    if (!cached) await this._image_guard();
    if (cached) {
      await this._verify_network_device(existing);
      if (String((await this.incus.instance_info(existing.instance_name)).status).toLowerCase() !== "stopped" ||
          this.store.get_ports(existing.task_id).length)
        throw new ControlError("cache_ineligible", "cached instance is not safely stopped");
    }
    const old = cached ? this._durable_resources(existing, "resources_json", { allow_empty: false }) :
      { cpu: 2, memory_mib: 4096, disk_gib: 20 };
    if (cached && !equal(await this._native_resources(existing), old))
      throw new ControlError("resource_unverified", "cached native sizes differ from durable resource sizes");
    const resources = this._requested_resources(request, old);
    let cost: Integer = this.config[cached ? "resume_cost_bytes" : "create_cost_bytes"];
    if (cached && compare(resources.disk_gib, old.disk_gib) > 0) cost = add(cost, this.config.resize_cost_bytes);
    let task = this._base_task(session_id!, add(expected, 1));
    if (existing) Object.assign(task, {
      task_id: existing.task_id, instance_name: existing.instance_name, created_at: existing.created_at,
      vm_ip: cached ? existing.vm_ip : null,
    });
    if (cached) Object.assign(task, {
      previous_generation: expected, inactive_since: existing.inactive_since, cleanup_after: existing.cleanup_after,
    });
    task.resources_json = canonical_json(cached ? old : resources);
    task.lease_expires_at = iso_time(after(this.now(), this.config.initial_lease_seconds));
    task = await this._reserve_resources_with_transition_wait(task, cached ? reservation(old, resources) : resources, {
      startup_cpu: resources.cpu, admission_epoch,
    });
    const completed: string[] = [], stage_timings: Data = {};
    let phase = "reservation", readiness: Data | null = null;
    try {
      this._bind_pending_acquire_operation(request, task, resources);
      phase = "expiry-timer";
      await this._schedule_expiry(task);
      const reclaimed = await this._reclaim_capacity(cost, task.task_id);
      task = await this._reserve_native(task, cost, { inventory: await this._port_inventory(), ssh_port: request.ssh_host_port });
      if (cached) {
        phase = "native-generation";
        await this.incus.config_set(task.instance_name, "user.agent-vm.generation", String(task.generation));
      } else {
        phase = "native-init";
        await this.incus.init(this.config.image_fingerprint, task.instance_name, {
          "user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.task_id,
          "user.agent-vm.session": session_id, "user.agent-vm.generation": String(task.generation),
        });
        await this._configure_network(task.instance_name, task.vm_ip);
      }
      phase = "native-config";
      for (const [key, native_key, suffix] of [["cpu", "limits.cpu", ""], ["memory_mib", "limits.memory", "MiB"]]) {
        if (!cached || !equal(resources[key], old[key])) {
          await this.incus.config_set(task.instance_name, native_key, String(resources[key]) + suffix);
          completed.push(key);
        }
      }
      if (!cached || !equal(resources.disk_gib, old.disk_gib)) {
        await this.incus.device_set(task.instance_name, "root", "size", `${resources.disk_gib}GiB`);
        completed.push("virtual_disk");
      }
      await this._ownership(task);
      if (!equal(await this._native_resources(task), resources))
        throw new ControlError("resource_unverified", "native sizes differ from the session reservation");
      phase = "native-start";
      const native_started = performance.now();
      await this._start_instance(task);
      stage_timings.native_start_seconds = elapsed(native_started);
      phase = "guest-readiness";
      readiness = await this._wait_guest_ready(task, { stage_timings });
      phase = "guest-post-check";
      const post_check_started = performance.now();
      this._expected_vm_ip = task.vm_ip;
      await this._verify_guest_tools(task.instance_name);
      await this._guest_ip(task.instance_name);
      const guest = await this._grow_guest_root(task, resources.disk_gib);
      completed.push("guest_filesystem");
      await this._install_public_key(task.instance_name, public_key);
      stage_timings.post_check_seconds = elapsed(post_check_started);
      phase = "access-exposure";
      if (parse_time(task.lease_expires_at)! <= this.now())
        throw new ControlError("lease_expired", "lease expired during preparation");
      const port = this.store.get_port(task.task_id, "ssh", "tcp");
      if (!port) throw new ControlError("mapping_unverified", "managed SSH reservation is missing");
      await this._add_proxy(task, port);
      if (parse_time(task.lease_expires_at)! <= this.now())
        throw new ControlError("lease_expired", "lease expired while exposing SSH");
      task = await this._global_lock(() => this.store.transaction(() => {
        const current = this.store.get_task({ task_id: task.task_id })!;
        if (this.store.get_meta("draining") === "1")
          throw new ControlError("draining", "new lease entered drain before commit");
        if (current.state !== "preparing" || parse_time(current.lease_expires_at)! <= this.now())
          throw new ControlError("lease_expired", "preparation was revoked before commit");
        this.store.update_port(task.task_id, "ssh", "tcp", { state: "active" });
        return this.store.update_task(task.task_id, {
          state: "active", reservation_bytes: 0, previous_generation: null, resource_reservation_json: "{}",
          resources_json: canonical_json(resources), inactive_since: null, cleanup_after: null, last_error: null,
        });
      }));
      return { ...await this._runtime_summary(task), reclaimed, guest_filesystem: guest, readiness, stage_timings };
    } catch (exc) {
      if (!(exc instanceof ControlError)) throw exc;
      let cleanup: Data = { ok: false, error: { code: "not_attempted", message: "cleanup was not attempted" } };
      const verified = phase === "native-init" ? this._native_init_timeout_cleanup_verified(exc) : null;
      const uncertain = phase === "native-init" && (exc.code === "command_timeout" || verified === false);
      try {
        cleanup.result = await this._release_task(this.store.get_task({ task_id: task.task_id })!, {
          require_native_init_absence_retry: uncertain, native_init_cleanup_verified: verified,
          protect_native_init_residue: phase === "native-init", recover_pending_acquire: true,
        });
        cleanup.ok = true;
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        cleanup = { ok: false, error: error.as_dict() };
      }
      const detail = detail_of(exc);
      Object.assign(detail, {
        phase: detail.phase ?? phase, operation_phase: phase, completed, requested: resources, cleanup,
        operation_id: request.operation_id, task_id: task.task_id, task_generation: task.generation,
      });
      if (uncertain) {
        const cleanup_detail = is_record(cleanup.error?.detail) ? cleanup.error.detail : null;
        Object.assign(detail, {
          native_init_absence_retry: true, native_init_cleanup_verified: verified,
          native_init_absence_retry_after: cleanup_detail?.native_init_absence_retry_after || iso_time(after(this.now(), 2)),
        });
        if (cleanup_detail) {
          detail.native_init_absence_checks = cleanup_detail.native_init_absence_checks ?? 1;
          detail.native_init_cleanup_verified = cleanup_detail.native_init_cleanup_verified === undefined
            ? verified : cleanup_detail.native_init_cleanup_verified;
        }
      } else if (phase === "native-init") detail.native_init_absence_retry = false;
      if (readiness !== null) detail.readiness = readiness;
      this.store.update_task(task.task_id, {
        last_error: canonical_json({ code: "acquire_partial", ...detail, error: exc.as_dict() }),
      });
      throw new ControlError(exc.code, exc.message, detail);
    }
  }

  async renew(request: Data, session_id: string | null, compatibility_warnings: Data[] = []): Promise<Data> {
    const [task, result] = await this._global_lock(() => this.store.transaction(() => {
      let task = this._task_for_session(session_id!);
      this._check_generation(task, request); this._require_active(task);
      if (this.store.get_meta("draining") === "1")
        throw new ControlError("draining", "extensions are refused while draining");
      const extension = after(parse_time(task.lease_expires_at)!, this.config.renew_seconds);
      const ceiling = after(this.now(), this.config.max_lease_seconds);
      task = this.store.update_task(task.task_id, { lease_expires_at: iso_time(extension < ceiling ? extension : ceiling) });
      const result = this._state_summary(task);
      this.store.record_operation(request.operation_id, "renew", session_id, request.generation, {
        ok: true, protocol: PROTOCOL_VERSION, server_time: iso_time(this.now()), action: "renew", session_id,
        generation: request.generation, operation_id: request.operation_id, outcome: "success", result,
        response: this._response_compatibility(), release: this._release_metadata(),
        warnings: this._merge_warnings(compatibility_warnings), _request_digest: digest(request),
      });
      return [task, result];
    }));
    await this._schedule_expiry(task);
    return result;
  }

  async release(request: Data, session_id: string | null): Promise<Data> {
    const task = this.store.get_task({ session_id: session_id! });
    if (!task) throw new ControlError("task_missing", "no VM lease exists for this session");
    this._validate_task_state(task);
    this._check_generation(task, request);
    return this._release_task(task);
  }

  async _release_task(task: Data, {
    require_native_init_absence_retry = false, native_init_cleanup_verified = null,
    protect_native_init_residue = false, recover_pending_acquire = true,
  }: {
    require_native_init_absence_retry?: boolean; native_init_cleanup_verified?: boolean | null;
    protect_native_init_residue?: boolean; recover_pending_acquire?: boolean;
  } = {}): Promise<Data> {
    const requested_state = task.state;
    return this._object_lock("cleanup:" + task.session_id, async () => {
      const current = this.store.get_task({ task_id: task.task_id });
      if (!current || compare(current.generation, task.generation) !== 0)
        throw new ControlError("generation_mismatch", "cleanup generation changed");
      task = current;
      let pending_acquires_to_cancel: Data[] = [];
      const cancellation_marker = this._pending_acquire_cancellation_marker(task, true);
      if (recover_pending_acquire && cancellation_marker !== null) {
        pending_acquires_to_cancel = this._pending_acquire_cancellation_operations(task);
      } else if (recover_pending_acquire &&
        (requested_state === "active" || requested_state === "preparing" ||
          ["cached", "absent"].includes(task.state))) {
        pending_acquires_to_cancel = this._pending_acquire_operations_for_task(task);
      }
      if (["cached", "absent"].includes(task.state) && requested_state !== "closing") {
        if (pending_acquires_to_cancel.length) {
          task = await this._global_lock(() => this.store.transaction(() => {
            this._cancel_pending_acquire_operations(pending_acquires_to_cancel, task);
            return this.store.update_task(task.task_id, {
              last_error: this._pending_acquire_cancellation_error(task, pending_acquires_to_cancel),
            });
          }));
        }
        return this._state_summary(task);
      }
      const expiry = parse_time(task.lease_expires_at);
      const inactive = parse_time(task.inactive_since) ??
        new Date(Math.min(this.now().getTime(), (expiry ?? this.now()).getTime()));
      task = await this._global_lock(() => this.store.transaction(() => this.store.update_task(task.task_id, {
        state: "closing", lease_expires_at: null, inactive_since: iso_time(inactive),
        cleanup_after: iso_time(after(inactive, this.config.cache_seconds)),
        ...(pending_acquires_to_cancel.length ? { last_error: this._pending_acquire_cancellation_error(task, pending_acquires_to_cancel) } : {}),
      })));
      const [errors, completed] = await this._cleanup_task(task);
      if (errors.length) {
        const detail: Data = { errors, completed };
        if (pending_acquires_to_cancel.length) {
          detail.pending_acquire_cancellation = true;
          detail.pending_acquire_operation_ids = pending_acquires_to_cancel.map(item => item.operation_id);
          detail.task_id = task.task_id; detail.task_generation = task.generation;
        }
        this.store.update_task(task.task_id, { last_error: canonical_json(detail) });
        await this.systemd.retry_cleanup(task.task_id, task.generation);
        throw new ControlError("cleanup_incomplete", "entry/port reservations remain closed until reconciliation succeeds", errors);
      }
      const defer = async (message: string, detail: Data): Promise<never> => {
        if (pending_acquires_to_cancel.length) {
          detail = {
            ...detail, pending_acquire_cancellation: true,
            pending_acquire_operation_ids: pending_acquires_to_cancel.map(item => item.operation_id),
            task_id: task.task_id, task_generation: task.generation,
          };
        }
        this.store.update_task(task.task_id, { last_error: canonical_json(detail) });
        await this.systemd.retry_cleanup(task.task_id, task.generation);
        throw new ControlError("cleanup_incomplete", message, detail);
      };
      let instances: Data[];
      try { instances = await this.incus.list_instances(); }
      catch (exc) {
        if (!(exc instanceof ControlError)) throw exc;
        return defer("native absence cannot be verified; delayed reconciliation is required",
          { native_absence: "unverified", cause: exc.as_dict() });
      }
      let absent = !instances.some(item => item.name === task.instance_name);
      let pending_acquire: Data | null;
      try {
        pending_acquire = recover_pending_acquire && cancellation_marker === null && pending_acquires_to_cancel.length === 0
          ? this._pending_native_init_for_task(task) : null;
      }
      catch (exc) {
        if (!(exc instanceof ControlError)) throw exc;
        return defer("pending native initialization cannot be verified; delayed reconciliation is required",
          { native_absence: "unverified", cause: exc.as_dict() });
      }
      const delayed_absence = this._native_init_absence_retry_pending(task);
      const retry_required = require_native_init_absence_retry || delayed_absence || pending_acquire !== null;
      const retry_ready = this._native_init_absence_retry_ready(task);
      const cleanup_verified = native_init_cleanup_verified !== null
        ? native_init_cleanup_verified : this._native_init_cleanup_verified(task);
      const native_init_retry_required = require_native_init_absence_retry || delayed_absence ||
        protect_native_init_residue && !absent;
      const prior_error = (detail: Data): Data => {
        const prior = this._native_init_absence_retry_record(task) ?? {};
        if (is_record(prior.error)) detail.error = prior.error;
        return detail;
      };
      if (native_init_retry_required && !absent) {
        if (cleanup_verified === true) {
          try {
            await this._native_lock(task, async () => {
              const current = this._cleanup_current(task);
              if (!current) throw new ControlError("generation_mismatch", "cleanup generation changed");
              await this._ownership(current);
              await this.incus.delete(current.instance_name);
              if ((await this.incus.list_instances()).some(item => item.name === current.instance_name))
                throw new ControlError("cleanup_incomplete", "uncertain native init instance deletion is not confirmed");
            });
            absent = true;
          } catch (exc) {
            if (!(exc instanceof ControlError)) throw exc;
            return defer("uncertain native init instance cannot be removed; delayed reconciliation is required", prior_error({
              native_init_absence_retry: true, outcome_verified: false, native_absence: "present",
              native_init_cleanup_verified: cleanup_verified, cause: exc.as_dict(),
            }));
          }
        } else return defer("native init cleanup is unverified; delayed reconciliation is required", prior_error({
          native_init_absence_retry: true, outcome_verified: false, native_absence: "present",
          native_init_cleanup_verified: cleanup_verified,
        }));
      }
      let absence_checks: Integer = this._native_init_absence_checks(task);
      if (absent && retry_required) absence_checks = add(absence_checks, 1);
      const stable_absence = retry_ready && compare(absence_checks, 2) >= 0;
      const absence_verified = cleanup_verified === true ||
        cleanup_verified === null && pending_acquire !== null && stable_absence;
      if (absent && retry_required && !(stable_absence && absence_verified))
        return defer("native init result remains unresolved; delayed reconciliation is required", prior_error({
          native_init_absence_retry: true, outcome_verified: false, native_absence: "pending",
          native_init_absence_retry_after: this._native_init_retry_after(task),
          native_init_absence_checks: absence_checks, native_init_cleanup_verified: cleanup_verified,
        }));
      let recovered_operation: [string, Data] | null = null;
      const recovery_operation_id = pending_acquire?.operation_id ?? null;
      if (absent && pending_acquire !== null && stable_absence && absence_verified) {
        try { recovered_operation = this._recovered_native_init_operation(task, pending_acquire); }
        catch (exc) {
          if (!(exc instanceof ControlError)) throw exc;
          return defer("native init operation cannot be finalized; delayed reconciliation is required",
            { native_absence: "verified", outcome_verified: false, cause: exc.as_dict() });
        }
      }
      await this.systemd.remove_expiry(task.task_id, task.generation);
      if (!absent && task.previous_generation != null) {
        await this._ownership(task);
        await this.incus.config_set(task.instance_name, "user.agent-vm.generation", String(task.generation));
      }
      let resources: Data;
      if (absent) {
        try { resources = resource_mapping(task.resources_json, "resources", { allow_empty: false }); }
        catch { throw new ControlError("resource_unverified", "absent task resources cannot be verified"); }
      } else resources = await this._native_resources(task);
      let retained_error: string | null = null;
      if (pending_acquires_to_cancel.length) {
        retained_error = this._pending_acquire_cancellation_error(task, pending_acquires_to_cancel);
      } else if (recovery_operation_id !== null) {
        let previous: Data = {};
        try { const value = parse_json(task.last_error); if (is_record(value)) previous = value; } catch {}
        retained_error = canonical_json({
          ...previous,
          code: previous.code ?? "acquire_partial",
          phase: previous.phase ?? "cleanup",
          operation_id: recovery_operation_id,
          task_id: task.task_id,
          task_generation: task.generation,
          outcome_verified: true,
        });
      }
      task = await this._global_lock(() => this.store.transaction(() => {
        for (const port of this.store.get_ports(task.task_id))
          this.store.remove_port(task.task_id, port.port_name, port.protocol);
        if (recovered_operation !== null)
          this.store.record_operation(recovered_operation[0], "acquire", task.session_id, task.generation, recovered_operation[1]);
        if (pending_acquires_to_cancel.length)
          this._cancel_pending_acquire_operations(pending_acquires_to_cancel, task);
        return this.store.update_task(task.task_id, {
          state: absent ? "absent" : "cached", ssh_host_port: null, reservation_bytes: 0, last_error: retained_error,
          previous_generation: null, resource_reservation_json: "{}", resources_json: canonical_json(resources),
          vm_ip: absent ? null : task.vm_ip,
        });
      }));
      return this._state_summary(task);
    });
  }

  async destroy(request: Data, session_id: string | null): Promise<Data> {
    let task = this.store.get_task({ session_id: session_id! });
    if (!task) throw new ControlError("task_missing", "no owned instance exists");
    this._validate_task_state(task); this._check_generation(task, request);
    if (task.state === "absent") return this._state_summary(task);
    if (!["cached", "deleting"].includes(task.state)) {
      await this._release_task(task);
      task = this.store.get_task({ task_id: task.task_id })!;
    }
    if (["cached", "deleting"].includes(task.state)) await this._delete_cache(task);
    return this._state_summary(this.store.get_task({ task_id: task.task_id })!);
  }

  async restart(request: Data, session_id: string | null): Promise<Data> {
    let task = this._task_for_session(session_id!);
    this._check_generation(task, request); this._require_active(task);
    let phase = "native-stop", readiness: Data | null = null, native_admitted = false;
    const stage_timings: Data = {};
    try {
      await this._global_lock(async () => {
        if (this.store.get_meta("draining") === "1")
          throw new ControlError("draining", "restart work is refused while draining");
        await this._ownership(task);
        await this._verify_native_allocation(task);
        await this._enforce_cpu_startup_pressure({
          allocated_cpu: (await this._allocation_totals()).cpu, requested_cpu: 0, capacity_cpu: await this.resources.cpu(),
        });
        await this._network_guard();
        await this._verify_network_device(task);
        this._require_window(task);
        native_admitted = true;
      });
      await this._stop_instance(task);
      phase = "native-start";
      const native_started = performance.now();
      await this._start_instance(task);
      stage_timings.native_start_seconds = elapsed(native_started);
      phase = "guest-readiness";
      readiness = await this._wait_guest_ready(task, { stage_timings });
      phase = "guest-post-check";
      const post_check_started = performance.now();
      this._expected_vm_ip = task.vm_ip;
      await this._guest_ip(task.instance_name);
      await this._verify_guest_tools(task.instance_name);
      stage_timings.post_check_seconds = elapsed(post_check_started);
      task = await this._global_lock(() => this.store.transaction(() => {
        const current = this.store.get_task({ task_id: task.task_id })!;
        this._check_generation(current, request); this._require_active(current);
        return current;
      }));
      return { ...this._state_summary(task), readiness, stage_timings };
    } catch (exc) {
      if (!(exc instanceof ControlError) || !native_admitted) throw exc;
      let cleanup: Data = { ok: false, error: { code: "not_attempted", message: "cleanup was not attempted" } };
      try {
        cleanup.result = await this._release_task(this.store.get_task({ task_id: task.task_id })!);
        cleanup.ok = true;
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        cleanup = { ok: false, error: error.as_dict() };
      }
      const detail = detail_of(exc);
      Object.assign(detail, { phase: detail.phase ?? phase, operation_phase: phase, cleanup });
      if (readiness !== null) detail.readiness = readiness;
      this.store.update_task(task.task_id, {
        last_error: canonical_json({ code: "restart_partial", ...detail, error: exc.as_dict() }),
      });
      throw new ControlError(exc.code, exc.message, detail);
    }
  }

  _require_window(task: Data, { minimum_seconds = GUEST_READINESS_BUDGET_SECONDS }: { minimum_seconds?: Integer } = {}): void {
    const expiry = parse_time(task.lease_expires_at)!;
    if (integer(expiry.getTime()) - integer(this.now().getTime()) < integer(mul(minimum_seconds, 1000)))
      throw new ControlError("lease_window_short", "explicitly renew before starting a potentially slow operation");
  }

  async resize(request: Data, session_id: string | null): Promise<Data> {
    let task = this._task_for_session(session_id!);
    this._check_generation(task, request); this._require_active(task);
    await this._ownership(task);
    const old = this._durable_resources(task, "resources_json", { allow_empty: false });
    const values = { cpu: request.cpu ?? null, memory_mib: request.memory_mib ?? null, disk_gib: request.disk_gib ?? null };
    if (Object.values(values).every(value => value === null))
      throw new ControlError("invalid_request", "resize requires at least one resource");
    const target = this._requested_resources(request, old);
    const live_memory = request.live_memory === undefined ? false : request.live_memory;
    const allow_restart = request.allow_restart === undefined ? false : request.allow_restart;
    if (typeof live_memory !== "boolean" || typeof allow_restart !== "boolean")
      throw new ControlError("invalid_request", "resize mode flags must be booleans");
    if (live_memory) {
      if (values.memory_mib === null || values.cpu !== null || values.disk_gib !== null || allow_restart)
        throw new ControlError("invalid_request", "live resize requires memory only and forbids restart allowance");
      return this._resize_live_memory(task, request, old, target);
    }
    const previous = this._durable_resources(task, "resource_reservation_json", { allow_empty: true });
    const recovering = Object.keys(previous).length > 0, needs_restart = !equal(target, old) || recovering;
    if (needs_restart && !allow_restart)
      return { applied: false, plan: "restart-required-for-resource-change", requested: target, ...this._state_summary(task) };
    if (needs_restart && this.store.get_meta("draining") === "1")
      throw new ControlError("draining", "restart work is refused while draining");
    this._require_window(task);
    const cost: Integer = compare(target.disk_gib, old.disk_gib) > 0 ? this.config.resize_cost_bytes : 0;
    let reclaimed: string[] = [];
    if (needs_restart) {
      await this._reserve_resources(task, reservation(old, target, previous), { startup_cpu: target.cpu });
      try {
        reclaimed = await this._reclaim_capacity(cost, task.task_id);
        await this._reserve_native(task, cost);
      } catch (exc) {
        if (!(exc instanceof ControlError)) throw exc;
        await this._cancel_resource_reservation(task);
        throw exc;
      }
    }
    const completed: string[] = [], stage_timings: Data = {};
    let guest: Data | null = null, guest_growth_attempted = false, phase = "native-stop", readiness: Data | null = null;
    try {
      if (needs_restart) await this._stop_instance(task);
      phase = "native-config";
      for (const [key, native_key, suffix] of [["cpu", "limits.cpu", ""], ["memory_mib", "limits.memory", "MiB"]]) {
        if (!equal(target[key], old[key]) || recovering) {
          await this.incus.config_set(task.instance_name, native_key, String(target[key]) + suffix);
          completed.push(key);
        }
      }
      if (compare(target.disk_gib, old.disk_gib) > 0) {
        await this.incus.device_set(task.instance_name, "root", "size", `${target.disk_gib}GiB`);
        const info = await this._ownership(task);
        if (info.expanded_devices?.root?.size !== `${target.disk_gib}GiB`)
          throw new ControlError("resize_unverified", "native root size does not match request");
        completed.push("virtual_disk");
      }
      if (needs_restart) {
        phase = "native-start";
        const native_started = performance.now();
        await this._start_instance(task);
        stage_timings.native_start_seconds = elapsed(native_started);
        phase = "guest-readiness";
        readiness = await this._wait_guest_ready(task, { stage_timings });
      }
      phase = "guest-post-check";
      const post_check_started = performance.now();
      if (!equal(await this._native_resources(task), target))
        throw new ControlError("resize_unverified", "native resource sizes differ from the requested sizes");
      this._require_active(task);
      this._expected_vm_ip = task.vm_ip;
      await this._guest_ip(task.instance_name);
      if (values.disk_gib !== null) {
        guest_growth_attempted = true;
        guest = await this._grow_guest_root(task, target.disk_gib);
        completed.push("guest_filesystem");
      }
      stage_timings.post_check_seconds = elapsed(post_check_started);
      task = await this._global_lock(() => this.store.transaction(() => {
        const current = this.store.get_task({ task_id: task.task_id })!;
        this._check_generation(current, request); this._require_active(current);
        return this.store.update_task(task.task_id, {
          resources_json: canonical_json(target), reservation_bytes: 0, resource_reservation_json: "{}", last_error: null,
        });
      }));
      return {
        applied: true, completed, guest_filesystem: guest, reclaimed, readiness, stage_timings, ...this._state_summary(task),
      };
    } catch (exc) {
      if (!(exc instanceof ControlError)) throw exc;
      const performed = needs_restart || guest_growth_attempted;
      let cleanup: Data = { performed, ok: true };
      if (performed) {
        try { cleanup.result = await this._release_task(this.store.get_task({ task_id: task.task_id })!); }
        catch (error) {
          if (!(error instanceof ControlError)) throw error;
          cleanup = { performed: true, ok: false, error: error.as_dict() };
        }
      }
      const detail = { ...detail_of(exc), phase, completed, requested: target, cleanup };
      if (readiness !== null) Object.assign(detail, { readiness });
      this.store.update_task(task.task_id, {
        last_error: canonical_json({ code: "resize_partial", ...detail, error: exc.as_dict() }),
      });
      throw new ControlError("resize_partial", "resize did not complete; inspect native and guest sizes before retrying", detail);
    }
  }

  async _guest_boot_id(task: Data): Promise<string> {
    const name = encodeURIComponent(task.instance_name).replace(/[!'()*]/g, ch => `%${ch.charCodeAt(0).toString(16).toUpperCase()}`);
    const state = await this.incus.query(`/1.0/instances/${name}/state`);
    if (!is_record(state) || String(state.status ?? "").toLowerCase() !== "running")
      throw new ControlError("guest_unavailable", "live memory adjustment requires a running guest");
    const result = await this.incus.exec(task.instance_name, ["cat", "/proc/sys/kernel/random/boot_id"], { timeout: 15 });
    try {
      const raw = new TextDecoder("utf-8", { fatal: true }).decode(result.stdout).trim();
      const hex = raw.replaceAll("urn:", "").replaceAll("uuid:", "").replace(/^[{}]+|[{}]+$/g, "").replaceAll("-", "");
      if (!/^[0-9a-fA-F]{32}$/.test(hex)) throw new Error("invalid UUID");
      const value = hex.toLowerCase();
      return `${value.slice(0, 8)}-${value.slice(8, 12)}-${value.slice(12, 16)}-${value.slice(16, 20)}-${value.slice(20)}`;
    } catch {
      throw new ControlError("resize_unverified", "guest boot identity is unavailable");
    }
  }

  async _resize_live_memory(task: Data, request: Data, old: Data, target: Data): Promise<Data> {
    this._require_window(task, { minimum_seconds: 180 });
    if (Object.keys(this._durable_resources(task, "resource_reservation_json", { allow_empty: true })).length)
      throw new ControlError("resize_incomplete", "a previous partial resize requires inspection and explicit restart recovery or release");
    if (!equal(await this._native_resources(task), old))
      throw new ControlError("resize_unverified", "native configuration differs from the recorded allocation");
    const before = await this._guest_boot_id(task);
    if (equal(target, old))
      return { applied: false, mode: "live-memory", plan: "unchanged", ...this._state_summary(task) };
    const reserved = reservation(old, target);
    await this._reserve_resources(task, reserved, {
      memory_reduction: compare(target.memory_mib, old.memory_mib) < 0, startup_required: false,
    });
    const completed: string[] = [], attempts: Data[] = [];
    try {
      const current = this.store.get_task({ task_id: task.task_id })!;
      this._check_generation(current, request); this._require_active(current);
    } catch (exc) {
      if (!(exc instanceof ControlError)) throw exc;
      await this._cancel_resource_reservation(task);
      throw exc;
    }
    const apply_memory = async (): Promise<void> => {
      try { await this.incus.config_set(task.instance_name, "limits.memory", `${target.memory_mib}MiB`); }
      catch (error) {
        if (!(error instanceof ControlError)) throw error;
        attempts.push({ ok: false, error: error.as_dict() });
        throw error;
      }
      attempts.push({ ok: true });
    };
    try {
      try { await apply_memory(); }
      catch (first_error) {
        if (!(first_error instanceof ControlError)) throw first_error;
        const detail = is_record(first_error.detail) ? first_error.detail : {};
        const stderr = detail.stderr;
        const expected = new RegExp(
          `^Error: Failed updating memory limit: Failed setting memory to ${target.memory_mib}MiB \\(currently (\\d+)MiB\\) as it was taking too long$`,
        );
        const text = typeof stderr === "string" ? stderr.trim() : "";
        const match = expected.exec(text);
        if (
          first_error.code !== "command_failed" || !equal(detail.returncode, 1) ||
          compare(target.memory_mib, old.memory_mib) >= 0 || !match || match[0] !== text ||
          compare(narrow(BigInt(match[1])), target.memory_mib) <= 0
        ) throw first_error;
        let current = this.store.get_task({ task_id: task.task_id })!;
        this._check_generation(current, request); this._require_active(current);
        this._require_window(current, { minimum_seconds: MEMORY_SETTLE_SECONDS + 60 });
        if (
          !equal(this._durable_resources(current, "resource_reservation_json", { allow_empty: true }), reserved) ||
          !equal(await this._native_resources(task), old) || await this._guest_boot_id(task) !== before
        ) throw new ControlError("resize_unverified", "native rollback or guest continuity is unverified after balloon timeout");
        await this._reserve_resources(task, reserved, {
          memory_reduction: compare(target.memory_mib, old.memory_mib) < 0, startup_required: false,
        });
        await Bun.sleep(MEMORY_SETTLE_SECONDS * 1000);
        current = this.store.get_task({ task_id: task.task_id })!;
        this._check_generation(current, request); this._require_active(current);
        this._require_window(current, { minimum_seconds: 60 });
        if (
          !equal(this._durable_resources(current, "resource_reservation_json", { allow_empty: true }), reserved) ||
          !equal(await this._native_resources(task), old) || await this._guest_boot_id(task) !== before
        ) throw new ControlError("resize_unverified", "native rollback, reservation or guest continuity changed during balloon settlement");
        await this._reserve_resources(task, reserved, { memory_reduction: true, startup_required: false });
        await apply_memory();
      }
      completed.push("native_memory_update");
      if (!equal(await this._native_resources(task), target))
        throw new ControlError("resize_unverified", "native memory configuration does not match the request");
      const boot_id = await this._guest_boot_id(task);
      if (boot_id !== before) throw new ControlError("resize_unverified", "guest boot identity changed during live adjustment");
      completed.push("guest_continuity");
      task = await this._global_lock(() => this.store.transaction(() => {
        const current = this.store.get_task({ task_id: task.task_id })!;
        this._check_generation(current, request); this._require_active(current);
        return this.store.update_task(task.task_id, {
          resources_json: canonical_json(target), resource_reservation_json: "{}", last_error: null,
        });
      }));
      return {
        applied: true, mode: "live-memory", completed, native_attempts: attempts,
        verification: { source: "incus-live-update", configured_memory_mib: target.memory_mib, guest_boot_id: boot_id },
        ...this._state_summary(task),
      };
    } catch (exc) {
      if (!(exc instanceof ControlError)) throw exc;
      const detail = {
        code: "resize_partial", mode: "live-memory", completed, requested: { ...target },
        native_attempts: attempts, error: exc.as_dict(),
      };
      await this._global_lock(() => this.store.transaction(() => {
        const current = this.store.get_task({ task_id: task.task_id });
        if (current && compare(current.generation, task.generation) === 0)
          this.store.update_task(task.task_id, { last_error: canonical_json(detail) });
      }));
      throw new ControlError("resize_partial",
        "live memory adjustment is unverified; conservative allocation retained, no restart fallback", detail);
    }
  }

  async _grow_guest_root(task: Data, disk_gib: Integer): Promise<Data> {
    const command = ["incus", "exec", task.instance_name, "--", "bun", "run", "-", String(disk_gib)];
    let result;
    try {
      result = await this.incus.exec(task.instance_name, ["bun", "run", "-", String(disk_gib)], {
        input_data: await guest_source(), timeout: 120, check: false, bounded: true,
      });
    } catch (exc) {
      if (!(exc instanceof ControlError)) throw exc;
      const underlying = exc.as_dict(), transport = is_record(underlying.detail) ? underlying.detail : {};
      throw new ControlError("guest_resize_unverified", "guest root filesystem growth command was not completed", {
        phase: "guest-exec", command, returncode: null,
        stdout: String(transport.stdout || ""), stderr: String(transport.stderr || ""),
        stdout_truncated: transport.stdout_truncated === true, stderr_truncated: transport.stderr_truncated === true,
        guest_capacity: { verified: false, error: underlying },
      });
    }
    const stdout = result.stdout.toString("utf8"), stderr = result.stderr.toString("utf8");
    const stdout_truncated = result.stdout_truncated === true, stderr_truncated = result.stderr_truncated === true;
    if (stdout_truncated || stderr_truncated)
      throw new ControlError("guest_resize_unverified", "guest root filesystem growth report was truncated", {
        phase: "guest-exec", command, returncode: result.returncode, stdout, stderr,
        stdout_truncated, stderr_truncated, guest_capacity: { verified: false },
      });
    let guest_error: Data | null = null;
    for (const line of (stderr + "\n" + stdout).split(/\r\n|[\n\r\v\f\u001c-\u001e\u0085\u2028\u2029]/).reverse()) {
      try {
        const parsed = parse_json(line);
        if (is_record(parsed) && is_record(parsed.error)) { guest_error = { ...parsed.error }; break; }
      } catch {}
    }
    if (result.returncode !== 0) {
      guest_error ??= {};
      if (!valid_failure_report(guest_error))
        throw new ControlError("guest_resize_unverified", "guest failure report is invalid", {
          phase: "guest-report", command, returncode: result.returncode, stderr, stdout,
          cause: "invalid_failure_report", guest_capacity: { verified: false },
        });
      const detail: Data = {
        phase: guest_error.phase, command: guest_error.command, returncode: guest_error.returncode,
        stderr: guest_error.stderr, stdout: guest_error.stdout, control_stderr: stderr, guest_capacity: guest_error.capacity,
      };
      for (const key of [
        "transient", "retry_of", "cause", "stdout_truncated", "stderr_truncated",
        "reaped", "pipes_drained", "process_group_killed", "process_group_cleanup_error",
      ]) if (key in guest_error) detail[key] = guest_error[key];
      throw new ControlError("guest_resize_unverified", "guest root filesystem growth is unverified", detail);
    }
    try {
      const value = parse_json(stdout);
      if (!valid_capacity_report(value, disk_gib)) throw new Error("invalid capacity report");
      return value;
    } catch {
      throw new ControlError("guest_resize_unverified", "guest capacity report is invalid", {
        phase: "guest-report", command, returncode: result.returncode, stderr, stdout, guest_capacity: { verified: false },
      });
    }
  }

  async ssh_info(_request: Data, session_id: string | null): Promise<Data> {
    const task = this._task_for_session(session_id!);
    this._require_active(task);
    await this._ownership(task);
    const port = this.store.get_port(task.task_id, "ssh", "tcp");
    if (!port || port.state !== "active" || compare(port.generation, task.generation) !== 0)
      throw new ControlError("mapping_unverified", "managed SSH reservation is not active");
    const host_key = await this._guest_host_key(String(task.instance_name));
    this._require_active(this.store.get_task({ task_id: task.task_id }));
    return {
      task_id: task.task_id, generation: narrow(integer(task.generation)), host: this.config.host_address,
      port: narrow(integer(task.ssh_host_port)), user: "root",
      host_key_alias: `agent-vm-${task.task_id}-g${task.generation}`, host_key,
    };
  }

  async port_add(request: Data, session_id: string | null): Promise<Data> {
    let task = this._task_for_session(session_id!);
    this._check_generation(task, request); this._require_active(task);
    await this._ownership(task);
    const name = require_nonempty_string(request.name, "name", PORT_NAME_RE);
    if (name === "ssh") throw new ControlError("invalid_request", "ssh is reserved");
    const protocol = request.port_protocol === undefined ? "tcp" : request.port_protocol;
    if (!["tcp", "udp"].includes(protocol)) throw new ControlError("invalid_request", "unsupported port protocol");
    const guest = require_int(request.vm_port, "vm_port", 1, 65535);
    const existing = this.store.get_port(task.task_id, name, protocol);
    if (existing) {
      if (existing.state !== "active") throw new ControlError("cleanup_incomplete", "port reservation is transitional");
      if (!equal(existing.vm_port, guest) ||
          !equal(request.host_port === undefined ? existing.host_port : request.host_port, existing.host_port))
        throw new ControlError("port_exists", "existing mapping has different settings");
      task = await this._global_lock(() => this.store.transaction(() => {
        const current = this.store.get_task({ task_id: task.task_id })!;
        this._check_generation(current, request); this._require_active(current);
        return current;
      }));
      try {
        await this._add_proxy(task, existing);
        await this._global_lock(() => this.store.transaction(() => {
          const current = this.store.get_task({ task_id: task.task_id })!;
          this._check_generation(current, request); this._require_active(current);
        }));
        return this.store.get_port(task.task_id, name, protocol)!;
      } catch (exc) {
        if (!(exc instanceof ControlError)) throw exc;
        let cleanup_verified = false;
        const current = this.store.get_task({ task_id: task.task_id });
        const same_generation = !!current && compare(current.generation, existing.generation) === 0 &&
          compare(existing.generation, task.generation) === 0;
        let stale = !same_generation || !current || current.state !== "active";
        if (same_generation && current?.state === "active") {
          const expiry = parse_time(current.lease_expires_at);
          stale = expiry === null || expiry <= this.now();
        }
        if (stale && same_generation) {
          try {
            await this._native_lock(task, async () => {
              const latest = this.store.get_task({ task_id: task.task_id });
              if (latest && compare(latest.generation, existing.generation) === 0 && latest.state !== "absent") {
                const current_port = this.store.get_port(task.task_id, name, protocol);
                if (!current_port || compare(current_port.generation, existing.generation) === 0) {
                  const cleanup_port = current_port ?? existing;
                  await this._remove_proxy(task, cleanup_port);
                  await this._conntrack_delete(cleanup_port, task);
                  if (current_port) this.store.remove_port(task.task_id, name, protocol);
                  cleanup_verified = true;
                }
              }
            });
          } catch (error) {
            if (!(error instanceof ControlError)) throw error;
            const current_port = this.store.get_port(task.task_id, name, protocol);
            if (current_port && compare(current_port.generation, existing.generation) === 0)
              this.store.update_port(task.task_id, name, protocol, { state: "closing" });
          }
        }
        const detail = detail_of(exc, false);
        if (!("operation_phase" in detail)) detail.operation_phase = "port-add";
        detail.outcome_verified = cleanup_verified;
        throw new ControlError(exc.code, exc.message, detail);
      }
    }
    const inventory = await this._port_inventory();
    const port = await this._global_lock(() => this.store.transaction(() => {
      this._require_active(this.store.get_task({ task_id: task.task_id }));
      const host = this._allocate_port(protocol, request.host_port, { inventory });
      const port = this._new_port(task, name, protocol, host, guest);
      this.store.put_port(port);
      return port;
    }));
    try {
      await this._add_proxy(task, port);
      await this._global_lock(() => this.store.transaction(() => {
        const current = this.store.get_task({ task_id: task.task_id })!;
        this._check_generation(current, request); this._require_active(current);
        this.store.update_port(task.task_id, name, protocol, { state: "active" });
      }));
      return this.store.get_port(task.task_id, name, protocol)!;
    } catch (exc) {
      if (!(exc instanceof ControlError)) throw exc;
      let cleanup_verified = false;
      try {
        await this._native_lock(task, async () => {
          const current = this.store.get_task({ task_id: task.task_id });
          if (current && compare(current.generation, port.generation) === 0) {
            await this._remove_proxy(task, port);
            await this._conntrack_delete(port, task);
            const current_port = this.store.get_port(task.task_id, name, protocol);
            if (current_port && compare(current_port.generation, port.generation) === 0)
              this.store.remove_port(task.task_id, name, protocol);
            cleanup_verified = true;
          }
        });
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        const current_port = this.store.get_port(task.task_id, name, protocol);
        if (current_port && compare(current_port.generation, port.generation) === 0)
          this.store.update_port(task.task_id, name, protocol, { state: "closing" });
      }
      const detail = detail_of(exc, false);
      if (!("operation_phase" in detail)) detail.operation_phase = "port-add";
      detail.outcome_verified = cleanup_verified;
      throw new ControlError(exc.code, exc.message, detail);
    }
  }

  async port_list(request: Data, session_id: string | null): Promise<Data> {
    const task = this._task_for_session(session_id!);
    this._check_generation(task, request); this._require_active(task);
    return { task_id: task.task_id, ports: this.store.get_ports(task.task_id) };
  }

  async port_remove(request: Data, session_id: string | null): Promise<Data> {
    const task = this._task_for_session(session_id!);
    this._check_generation(task, request); this._require_active(task);
    const name = require_nonempty_string(request.name, "name", PORT_NAME_RE);
    const protocol = request.port_protocol === undefined ? "tcp" : request.port_protocol;
    if (name === "ssh" || !["tcp", "udp"].includes(protocol))
      throw new ControlError("invalid_request", "invalid project mapping");
    const port = this.store.get_port(task.task_id, name, protocol);
    if (port) {
      this.store.update_port(task.task_id, name, protocol, { state: "closing" });
      try {
        await this._native_lock(task, async () => {
          await this._remove_proxy(task, port);
          await this._conntrack_delete(port, task);
          this.store.remove_port(task.task_id, name, protocol);
        });
      } catch (exc) {
        if (!(exc instanceof ControlError)) throw exc;
        const detail = detail_of(exc, false);
        if (!("operation_phase" in detail)) detail.operation_phase = "port-remove";
        detail.outcome_verified = false;
        throw new ControlError(exc.code, exc.message, detail);
      }
    }
    return { task_id: task.task_id, name, protocol, removed: true };
  }
}
