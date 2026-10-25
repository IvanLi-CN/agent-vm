#!/usr/bin/env bun

import { randomUUID } from "node:crypto";
import { TextDecoder } from "node:util";

import { LifecycleControl } from "./control-lifecycle.ts";
import {
  ControlError,
  DEFAULT_CONFIG_PATH,
  PROTOCOL_VERSION,
  TASK_ID_RE,
  TASK_STATES,
  add,
  canonical_json,
  compare,
  integer,
  is_record,
  iso_time,
  load_config,
  mul,
  narrow,
  parse_json,
  parse_time,
  require_int,
  require_string,
  resource_mapping,
  sub,
  type Data,
  type Integer,
} from "./control-types.ts";

export { LifecycleControl };
export * from "./control-types.ts";
export * from "./control-core.ts";
export * from "./control-native.ts";
export * from "./control-store.ts";

const MAX_REQUEST_BYTES = 2 * 1024 * 1024;

function equal(left: unknown, right: unknown): boolean {
  return canonical_json(left ?? null) === canonical_json(right ?? null);
}

function is_integer(value: unknown): value is Integer {
  return (
    typeof value === "bigint" ||
    (typeof value === "number" && Number.isSafeInteger(value))
  );
}

function has_native_proxy(owned: Data): boolean {
  return Object.values(owned.expanded_devices ?? {}).some(
    (device: any) => device.type === "proxy",
  );
}

export class Control extends LifecycleControl {
  async admin_status(
    request: Data,
    session_id: string | null,
  ): Promise<Data> {
    const { draining, task_snapshots } = await this._global_lock(
      async () => ({
        draining: this.store.get_meta("draining") === "1",
        task_snapshots: this.store.list_tasks(),
      }),
    );

    const tasks: Data[] = [];
    for (const task of task_snapshots) {
      tasks.push(await this._runtime_summary(task));
    }

    return {
      draining,
      tasks,
      doctor: await this.doctor(request, session_id),
    };
  }

  async admin_metrics(
    request: Data,
    session_id: string | null,
  ): Promise<Data> {
    const doctor = await this.doctor(request, session_id);

    return this._global_lock(async () => {
      const tasks = this.store.list_tasks();
      const task_states: Record<string, Integer> = Object.create(null);

      for (const task of tasks) {
        const state = String(task.state);
        task_states[state] = add(task_states[state] ?? 0, 1);
      }

      const resources = Object.hasOwn(doctor, "resources")
        ? doctor.resources
        : {};
      const resource_fields = is_record(resources) ? resources : {};
      const memory = is_record(resource_fields.memory)
        ? resource_fields.memory
        : {};
      const ct_capacity = is_record(resource_fields.ct_capacity)
        ? resource_fields.ct_capacity
        : {};
      const admission = doctor.default_admission ?? {};
      const observations = admission.observations ?? {};
      const warnings = [...(admission.warnings ?? [])];
      const audit = this.store.audit_summary();

      let disk_admission = resource_fields.disk_admission;
      if (!is_record(disk_admission)) {
        disk_admission = await this._disk_admission_diagnostics(
          doctor.capacity ?? null,
        );
      }

      return {
        observed_at: iso_time(this.now()),
        up: true,
        draining: this.store.get_meta("draining") === "1",
        admission_ready: Boolean(doctor.admission_ready),
        reclaim_required: admission.reclaim_required ?? null,
        resources,
        capacity: doctor.capacity ?? null,
        disk_admission,
        ct_cpu: {
          capacity_vcpu: ct_capacity.cpu ?? null,
          pressure: resource_fields.cpu_pressure ?? null,
          startup_pressure: observations.cpu_startup_pressure ?? null,
        },
        ct_memory: {
          capacity_bytes: memory.limit_bytes ?? null,
          current_bytes: memory.current_bytes ?? null,
          available_bytes: memory.available_bytes ?? null,
          pressure_threshold_bytes: memory.threshold_bytes ?? null,
          pressure: resource_fields.memory_pressure ?? null,
          oom_events: memory.oom_events ?? null,
        },
        instances: doctor.instances ?? null,
        task_states,
        operations: audit.operations,
        pending_operations: this.store.pending_operations(),
        image_unpack_failures: audit.image_unpack_failures,
        warnings,
        warning_counts: audit.warnings,
        doctor: {
          ready: doctor.ready ?? null,
          issues: doctor.issues ?? [],
        },
        release: this._release_metadata(),
        release_warnings: this._merge_warnings(this.release_warnings),
      };
    });
  }

  async drain_status(
    _request: Data,
    _session_id: string | null,
  ): Promise<Data> {
    return { draining: this.store.get_meta("draining") === "1" };
  }

  async drain_enter(
    _request: Data,
    _session_id: string | null,
  ): Promise<Data> {
    await this._admission_lock(async () => {
      await this._global_lock(async () => {
        this.store.set_meta("draining", "1");
      });
    });

    return { draining: true };
  }

  async drain_exit(
    request: Data,
    session_id: string | null,
  ): Promise<Data> {
    if (this.store.get_meta("draining") !== "1") {
      return { draining: false };
    }

    const operation_id = request.operation_id ?? null;
    const pending = this.store.pending_operations().filter(
      item => (item.operation_id ?? null) !== operation_id,
    );

    if (pending.length) {
      throw new ControlError(
        "reconciliation_required",
        "drain exit requires all pending operations to be reconciled",
        { pending_operations: pending },
      );
    }

    const snapshots = this.store.list_tasks();

    // Match Python's Unicode code-point ordering, independently of locale.
    const owner_key = (task: Data): string =>
      canonical_json(String(task.session_id ?? ""));
    const owner_order = new Map(
      Object.keys(
        Object.fromEntries(
          snapshots.map(task => [String(task.session_id ?? ""), null]),
        ),
      ).map(owner => [owner, 0]),
    );
    const sorted_owners = parse_json(canonical_json(
      Object.fromEntries([...owner_order.keys()].map(owner => [owner, null])),
    ));
    const ranks = new Map(
      Object.keys(sorted_owners).map((owner, index) => [owner_key({ session_id: owner }), index]),
    );
    snapshots.sort(
      (left, right) =>
        ranks.get(owner_key(left))! - ranks.get(owner_key(right))!,
    );

    const inspect_and_exit = async (): Promise<void> => {
      for (const task of snapshots) {
        if (["preparing", "closing", "deleting", "failed"].includes(task.state)) {
          throw new ControlError(
            "reconciliation_required",
            "drain exit requires all task transitions to be reconciled",
            {
              task_id: task.task_id ?? null,
              state: task.state ?? null,
            },
          );
        }
      }

      const health = await this.doctor(request, session_id);
      const resources = health.resources;
      const instances = health.instances;
      const capacity = health.capacity;

      if (
        !health.ready ||
        !is_record(resources) ||
        !resources.verified ||
        !is_record(instances) ||
        !instances.verified ||
        !is_record(capacity) ||
        "error" in capacity
      ) {
        throw new ControlError(
          "recovery_unverified",
          "drain exit requires verified control health and capacity state",
          {
            issues: health.issues ?? [],
            admission: health.default_admission ?? null,
          },
        );
      }

      for (const task of this.store.list_tasks()) {
        const state = task.state;

        if (state === "absent") continue;

        if (["preparing", "closing", "deleting", "failed"].includes(state)) {
          throw new ControlError(
            "reconciliation_required",
            "drain exit requires all task transitions to be reconciled",
            { task_id: task.task_id ?? null, state },
          );
        }

        if (state === "active") {
          const summary = this._state_summary(task);

          if (!summary.effective_lease) {
            throw new ControlError(
              "reconciliation_required",
              "active task has no effective lease",
              { task_id: task.task_id ?? null },
            );
          }

          await this._ownership(task);
          await this._network_guard();
          await this._verify_network_device(task);

          const info = await this.incus.instance_info(task.instance_name);
          if (info.status.toLowerCase() !== "running") {
            throw new ControlError(
              "state_mismatch",
              "active task guest is not running",
              { task_id: task.task_id ?? null },
            );
          }

          if (
            !await this.systemd.expiry_active(
              task.task_id,
              task.generation,
              task.lease_expires_at,
            )
          ) {
            throw new ControlError(
              "timer_unverified",
              "active task expiry timer is not verified",
              { task_id: task.task_id ?? null },
            );
          }

          continue;
        }

        if (state === "cached") {
          await this._ownership(task);

          const info = await this.incus.instance_info(task.instance_name);
          if (info.status.toLowerCase() !== "stopped") {
            throw new ControlError(
              "state_mismatch",
              "cached task guest is not stopped",
              { task_id: task.task_id ?? null },
            );
          }

          if (this.store.get_ports(task.task_id).length) {
            throw new ControlError(
              "cleanup_incomplete",
              "cached task still has port mappings",
              { task_id: task.task_id ?? null },
            );
          }

          const owned = await this._ownership(task);
          if (has_native_proxy(owned)) {
            throw new ControlError(
              "cleanup_incomplete",
              "cached task still has native proxy mappings",
              { task_id: task.task_id ?? null },
            );
          }

          continue;
        }

        throw new ControlError(
          "resource_unverified",
          "drain state contains an unknown task state",
          {
            task_id: task.task_id ?? null,
            state: state ?? null,
          },
        );
      }

      // Reservations remain fenced while session locks are held.
      await this._admission_lock(async () => {
        if (this._release_activation_fenced()) {
          throw new ControlError(
            "release_activation_in_progress",
            "drain exit is blocked while release activation is fenced",
          );
        }

        if (this.store.get_meta("draining") === "1") {
          await this._global_lock(async () => {
            this.store.set_meta(
              "admission_epoch",
              randomUUID().replaceAll("-", ""),
            );
            this.store.set_meta("draining", "0");
          });
        }
      });
    };

    // Nest callbacks to retain every session lock until inspection and commit
    // finish. Session locks precede admission.lock, matching session operations.
    const with_sessions = async (index: number): Promise<void> => {
      if (index === snapshots.length) {
        await inspect_and_exit();
        return;
      }

      const owner = snapshots[index].session_id;
      if (typeof owner !== "string" || !owner) {
        throw new ControlError(
          "resource_unverified",
          "drain state contains an invalid session owner",
        );
      }

      await this._object_lock(
        "session:" + owner,
        async () => {
          await with_sessions(index + 1);
        },
        false,
      );
    };

    await with_sessions(0);
    return { draining: false };
  }

  async admin_session_list(
    _request: Data,
    _session_id: string | null,
  ): Promise<Data> {
    const tasks: Data[] = [];
    for (const task of this.store.list_tasks()) {
      tasks.push(await this._runtime_summary(task));
    }
    return { tasks };
  }

  async admin_reconcile(
    request: Data,
    _session_id: string | null,
  ): Promise<Data> {
    const apply = Object.hasOwn(request, "apply") ? request.apply : false;
    if (typeof apply !== "boolean") {
      throw new ControlError(
        "invalid_request",
        "reconcile apply must be a JSON boolean",
      );
    }

    const actions: Data[] = [];
    let native_instance_names: Set<string> | null = null;

    const absent_native_instance_names = async (): Promise<Set<string>> => {
      if (native_instance_names === null) {
        native_instance_names = new Set(
          (await this.incus.list_instances()).map(native => native.name),
        );
      }
      return native_instance_names;
    };

    for (const snapshot of this.store.list_tasks()) {
      let item: Data = {
        task_id: snapshot.task_id ?? null,
        generation: is_integer(snapshot.generation)
          ? snapshot.generation
          : null,
        action: "inspect",
      };

      try {
        await this._object_lock(
          "session:" + snapshot.session_id,
          async () => {
            const task = this.store.get_task({ task_id: snapshot.task_id })!;

            if (!is_integer(task.generation)) {
              throw new ControlError(
                "resource_unverified",
                "task generation cannot be verified",
              );
            }

            item = {
              task_id: task.task_id,
              generation: task.generation,
              action: "inspect",
            };

            if (!TASK_STATES.has(task.state)) {
              throw new ControlError(
                "resource_unverified",
                "task state cannot be verified; reconciliation is required",
                {
                  task_id: task.task_id,
                  state: task.state ?? null,
                },
              );
            }

            if (task.state === "absent") {
              item.action = "verify-absent-resources";

              if (
                (await absent_native_instance_names()).has(task.instance_name)
              ) {
                throw new ControlError(
                  "resource_unverified",
                  "absent task still has a native instance",
                );
              }

              if (this.store.get_ports(task.task_id).length) {
                throw new ControlError(
                  "cleanup_incomplete",
                  "absent task still has port mappings",
                );
              }

              let repair_required = false;

              try {
                const resources = resource_mapping(
                  task.resources_json,
                  "resources",
                );
                const reservation = resource_mapping(
                  task.resource_reservation_json,
                  "reservation",
                  { allow_empty: true },
                );

                if (
                  !equal(resources, {
                    cpu: 0,
                    memory_mib: 0,
                    disk_gib: 0,
                  }) ||
                  Object.keys(reservation).length !== 0
                ) {
                  repair_required = true;
                }
              } catch {
                repair_required = true;
              }

              if (
                !is_integer(task.reservation_bytes) ||
                compare(task.reservation_bytes, 0) !== 0
              ) {
                repair_required = true;
              }

              if (apply && repair_required) {
                if (
                  (await this.incus.list_instances()).some(
                    native => native.name === task.instance_name,
                  )
                ) {
                  throw new ControlError(
                    "resource_unverified",
                    "absent task still has a native instance",
                  );
                }

                if (this.store.get_ports(task.task_id).length) {
                  throw new ControlError(
                    "cleanup_incomplete",
                    "absent task still has port mappings",
                  );
                }

                await this._global_lock(async () => {
                  this.store.transaction(() => {
                    const current = this.store.get_task({
                      task_id: task.task_id,
                    });

                    if (current === null || current.state !== "absent") {
                      throw new ControlError(
                        "generation_mismatch",
                        "absent task changed during reconciliation",
                      );
                    }

                    this.store.update_task(task.task_id, {
                      resources_json: canonical_json({
                        cpu: 0,
                        memory_mib: 0,
                        disk_gib: 0,
                      }),
                      resource_reservation_json: "{}",
                      reservation_bytes: 0,
                      last_error: null,
                    });
                  });
                });
              }

              actions.push(item);
              return;
            }

            if (task.state === "deleting") {
              item.action = "finish-delete";

              if (apply) {
                const expiry = parse_time(task.lease_expires_at);
                if (expiry && expiry > this.now()) {
                  throw new ControlError(
                    "cache_ineligible",
                    "deletion retry has an effective deadline",
                  );
                }

                const exists = (await this.incus.list_instances()).some(
                  native => native.name === task.instance_name,
                );

                if (exists) {
                  const owned = await this._ownership(task);

                  if (has_native_proxy(owned)) {
                    throw new ControlError(
                      "cache_ineligible",
                      "deletion retry still has native proxy mappings",
                    );
                  }

                  const info = await this.incus.instance_info(
                    task.instance_name,
                  );

                  if (
                    info.status.toLowerCase() !== "stopped" ||
                    this.store.get_ports(task.task_id).length
                  ) {
                    throw new ControlError(
                      "cache_ineligible",
                      "deletion retry is not safely stopped",
                    );
                  }

                  await this.incus.delete(task.instance_name);
                }

                if (
                  (await this.incus.list_instances()).some(
                    native => native.name === task.instance_name,
                  )
                ) {
                  throw new ControlError(
                    "cleanup_incomplete",
                    "deletion is not confirmed",
                  );
                }

                await this._global_lock(async () => {
                  this.store.update_task(task.task_id, {
                    state: "absent",
                    vm_ip: null,
                    ssh_host_port: null,
                    lease_expires_at: null,
                    inactive_since: null,
                    cleanup_after: null,
                    reservation_bytes: 0,
                    resource_reservation_json: "{}",
                    last_error: null,
                  });
                });
              }
            } else if (
              ["closing", "preparing", "failed"].includes(task.state) ||
              (
                task.state === "active" &&
                !this._state_summary(task).effective_lease
              )
            ) {
              item.action = "close-incomplete-or-expired";
              if (apply) await this._release_task(task);
            } else {
              await this._ownership(task);

              if (task.state === "active") {
                await this._network_guard();
                await this._verify_network_device(task);
              }

              const info = await this.incus.instance_info(task.instance_name);
              const status = info.status.toLowerCase();

              if (task.state === "cached" && status !== "stopped") {
                item.action = "stop-unexpected-running-cache";

                if (apply) {
                  await this._release_task({ ...task, state: "closing" });
                }
              } else if (task.state === "active") {
                item.action = "rearm-current-expiry";

                if (status !== "running") {
                  throw new ControlError(
                    "state_mismatch",
                    "active guest is not running",
                  );
                }

                if (apply) {
                  await this._schedule_expiry(task);

                  for (const port of this.store.get_ports(task.task_id)) {
                    if (
                      port.state !== "closing" &&
                      port.state !== "preparing"
                    ) {
                      continue;
                    }

                    // An unacknowledged port-add is closed, never reopened.
                    await this._native_lock(task, async () => {
                      await this._remove_proxy(task, port);
                      await this._conntrack_delete(port, task);
                      this.store.remove_port(
                        task.task_id,
                        port.port_name,
                        port.protocol,
                      );
                    });
                  }
                }
              }
            }

            actions.push(item);
          },
          false,
        );
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        item.error = error.as_dict();
        actions.push(item);
      }
    }

    return { dry_run: !apply, actions };
  }

  async admin_gc(
    request: Data,
    _session_id: string | null,
    { age_filtered = false }: { age_filtered?: boolean } = {},
  ): Promise<Data> {
    const apply = Object.hasOwn(request, "apply") ? request.apply : false;
    if (typeof apply !== "boolean") {
      throw new ControlError(
        "invalid_request",
        "GC apply must be a JSON boolean",
      );
    }

    const candidates: Data[] = [];

    for (const snapshot of this.store.list_tasks()) {
      try {
        await this._object_lock(
          "session:" + snapshot.session_id,
          async () => {
            let task = this.store.get_task({ task_id: snapshot.task_id });

            if (
              task === null ||
              task.state !== "cached" ||
              !task.inactive_since
            ) {
              return;
            }

            if (!equal(task.generation, snapshot.generation)) return;

            if (age_filtered) {
              const inactive = parse_time(task.inactive_since)!;
              const elapsed_ms = sub(
                this.now().getTime(),
                inactive.getTime(),
              );
              const minimum_idle_ms = mul(this.config.cache_seconds, 1000);

              if (compare(elapsed_ms, minimum_idle_ms) < 0) return;
            }

            task = await this._eligible_cache(task);

            const candidate = {
              task_id: task.task_id,
              instance_name: task.instance_name,
              generation: task.generation,
              inactive_since: task.inactive_since,
            };

            if (apply) await this._delete_cache(task);
            candidates.push(candidate);
          },
          false,
        );
      } catch (error) {
        if (!(error instanceof ControlError)) throw error;
        if (["cache_ineligible", "object_busy"].includes(error.code)) continue;

        candidates.push({
          task_id: snapshot.task_id,
          error: error.as_dict(),
        });
      }
    }

    return {
      dry_run: !apply,
      selection: age_filtered ? "age" : "eligible",
      minimum_idle_seconds: age_filtered ? this.config.cache_seconds : 0,
      candidates,
    };
  }

  async timer_expire(
    request: Data,
    _session_id: string | null,
  ): Promise<Data> {
    const task_id = require_string(request.task_id, "task_id", TASK_ID_RE);
    const generation = require_int(request.generation, "generation", 1);
    const task = this.store.get_task({ task_id });

    if (task === null || integer(task.generation) !== integer(generation)) {
      return {
        task_id,
        reason: task === null ? "missing" : "stale-generation",
        action: "noop",
      };
    }

    if (task.state === "closing") return this._release_task(task);

    if (!["active", "preparing"].includes(task.state)) {
      return { action: "noop", reason: "inactive" };
    }

    const expiry = parse_time(task.lease_expires_at);
    if (expiry && expiry > this.now()) {
      await this._schedule_expiry(task);
      return { action: "rearmed", reason: "not-expired" };
    }

    return this._release_task(task);
  }

  async timer_age_gc(
    _request: Data,
    session_id: string | null,
  ): Promise<Data> {
    return this.admin_gc(
      { apply: true },
      session_id,
      { age_filtered: true },
    );
  }

  async timer_reconcile(
    _request: Data,
    session_id: string | null,
  ): Promise<Data> {
    return this.admin_reconcile({ apply: true }, session_id);
  }
}

export async function request_from_stdin(
  input: AsyncIterable<Uint8Array | string> = process.stdin,
): Promise<Data> {
  const chunks: Buffer[] = [];
  let length = 0;

  for await (const chunk of input) {
    const bytes = typeof chunk === "string"
      ? Buffer.from(chunk, "utf8")
      : Buffer.from(chunk);

    if (bytes.length > MAX_REQUEST_BYTES - length) {
      throw new ControlError(
        "request_too_large",
        "request exceeds 2 MiB",
      );
    }

    chunks.push(bytes);
    length += bytes.length;
  }

  let value: unknown;

  try {
    // Preserve a BOM as a character so the JSON parser rejects it, matching
    // Python's explicit UTF-8 decoding followed by json.loads(string).
    const decoder = new TextDecoder("utf-8", {
      fatal: true,
      ignoreBOM: true,
    });
    value = parse_json(decoder.decode(Buffer.concat(chunks, length)));
  } catch {
    throw new ControlError(
      "invalid_request",
      "stdin must contain one UTF-8 JSON object",
    );
  }

  if (!is_record(value)) {
    throw new ControlError(
      "invalid_request",
      "stdin JSON root must be an object",
    );
  }

  return value;
}

export function parse_timer_args(argv: readonly string[]): Data | null {
  if (argv.length === 2 && argv[0] === "--timer-expire") {
    const value = argv[1];
    const match = /^([a-z0-9][a-z0-9-]{0,47})-g([0-9]+)$/.exec(value);

    // JavaScript's $ also permits a final newline; Python fullmatch does not.
    if (match === null || match[0] !== value) {
      throw new ControlError(
        "invalid_timer",
        "invalid expiry timer instance",
      );
    }

    return {
      protocol: PROTOCOL_VERSION,
      action: "timer-expire",
      task_id: match[1],
      generation: narrow(BigInt(match[2])),
    };
  }

  if (argv.length === 1 && argv[0] === "--timer-age-gc") {
    return {
      protocol: PROTOCOL_VERSION,
      action: "timer-age-gc",
    };
  }

  if (argv.length === 1 && argv[0] === "--timer-reconcile") {
    return {
      protocol: PROTOCOL_VERSION,
      action: "timer-reconcile",
    };
  }

  return null;
}

export function error_envelope(request: Data | null, error: Data): Data {
  const envelope: Data = {
    ok: false,
    protocol: PROTOCOL_VERSION,
  };

  if (request !== null) {
    for (const key of [
      "action",
      "session_id",
      "generation",
      "operation_id",
    ]) {
      if (Object.hasOwn(request, key)) envelope[key] = request[key];
    }
  }

  envelope.error = error;
  return envelope;
}

export async function main(
  argv: readonly string[] = process.argv.slice(2),
): Promise<number> {
  let request: Data | null = null;

  try {
    request = parse_timer_args(argv);

    if (request === null) {
      if (argv.length) {
        throw new ControlError(
          "invalid_request",
          "unknown command-line arguments",
        );
      }
      request = await request_from_stdin();
    }

    const config = load_config();
    const control = new Control(config);
    const response = await control.dispatch(request);

    process.stdout.write(canonical_json(response) + "\n");
    return 0;
  } catch (error) {
    if (error instanceof ControlError) {
      process.stderr.write(
        canonical_json(error_envelope(request, error.as_dict())) + "\n",
      );
      return 2;
    }

    const message = error instanceof Error
      ? error.constructor.name
      : error === null
        ? "null"
        : typeof error;

    process.stderr.write(
      canonical_json(error_envelope(request, {
        code: "internal_error",
        message,
      })) + "\n",
    );
    return 3;
  }
}

if (import.meta.main) {
  process.exitCode = await main();
}
