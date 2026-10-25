import { dirname, join, basename } from "node:path";
import { lstatSync } from "node:fs";
import { ControlError, Data, PROTOCOL_VERSION, DEFAULT_TIMER_DIR, DEFAULT_RELEASE_FENCE_PATH, SESSION_ID_RE, TASK_ID_RE, require_string, require_int, parse_json, canonical_json, digest, iso_time, parse_time, is_record, resource_mapping, sub, compare } from "./control-types.ts";
import { CommandRunner, IncusClient, with_lock, object_lock_path } from "./control-native.ts";
import { StateStore } from "./control-store.ts";
import { SystemdManager } from "./control-systemd.ts";
import { CTResources } from "./control-observation.ts";
import { ACTION_NAMES, ManifestError, check_compatibility, legacy_manifest, load_runtime_manifest, manifest_metadata, verify_runtime_bundle } from "./release-manifest.ts";
import { JsonFloat } from "./control-types.ts";

const equal = (a: any, b: any): boolean => canonical_json(a ?? null) === canonical_json(b ?? null);
const is_integer = (value: any): value is number | bigint =>
  typeof value === "bigint" || typeof value === "number" && Number.isSafeInteger(value);
export function runtime_artifact_paths(_config: Data, manifest_path: string, manifest: Data): Data {
  return Object.fromEntries(manifest.artifacts.map((item: Data) => [item.path, item.role === "unit" ? join(DEFAULT_TIMER_DIR, basename(item.path)) : join(dirname(manifest_path), item.path)]));
}
export class ControlCore {
  [method: string]: any;
  static WRITE_ACTIONS = new Set(["acquire", "renew", "release", "destroy", "restart", "resize", "port-add", "port-remove", "drain-enter", "drain-exit", "admin-reconcile", "admin-gc"]);
  static SESSION_ACTIONS = new Set(["status", "acquire", "renew", "release", "destroy", "restart", "resize", "ssh-info", "port-add", "port-list", "port-remove"]);
  static ACTIVE_STATES = new Set(["preparing", "active"]);
  static TRANSITION_STATES = new Set(["preparing", "closing", "deleting"]);
  config: Data; runner: CommandRunner; store: StateStore; incus: IncusClient; systemd: SystemdManager; resources: Data; clock: () => Date;
  release_manifest_path: string; release_fence_path: string;
  release_manifest: Data | null = null; release_manifest_present = false; release_artifacts_verified = true;
  release_manifest_error: ControlError | null = null; release_warnings: Data[] = [];
  lock_path: string; admission_lock_path: string;
  constructor(config: Data, { runner, store, systemd, clock = () => new Date(), resources }: { runner?: CommandRunner; store?: StateStore; systemd?: SystemdManager; clock?: () => Date; resources?: Data } = {}) {
    this.config = { ...config }; this.runner = runner ?? new CommandRunner(); this.clock = clock;
    this.release_manifest_path = config.release_manifest_path ?? join(import.meta.dir, "release-manifest.json");
    this.release_fence_path = config.release_fence_path ?? DEFAULT_RELEASE_FENCE_PATH;
    try {
      [this.release_manifest, this.release_manifest_present, this.release_artifacts_verified, this.release_warnings] = load_runtime_manifest(this.release_manifest_path);
      if (this.release_manifest) verify_runtime_bundle(this.release_manifest, runtime_artifact_paths(config, this.release_manifest_path, this.release_manifest));
    } catch (error) {
      if (!(error instanceof ControlError)) throw error;
      this.release_manifest_error = error; this.release_artifacts_verified = false;
      if (!store) throw error;
    }
    this.store = store ?? new StateStore(config.state_db); this.incus = new IncusClient(this.runner, config);
    this.systemd = systemd ?? new SystemdManager(this.runner, config); this.resources = resources ?? new CTResources();
    this.lock_path = join(dirname(config.state_db), "control.lock"); this.admission_lock_path = join(dirname(config.state_db), "admission.lock");
  }
  static _is_write_action(action: string, request?: Data): boolean {
    if (action === "admin-reconcile" && request && (request.apply ?? false) === false && !("operation_id" in request) && !("generation" in request)) return false;
    return this.WRITE_ACTIONS.has(action);
  }
  _is_write_action(action: string, request?: Data): boolean { return ControlCore._is_write_action(action, request); }
  _global_lock<T>(fn: () => Promise<T> | T): Promise<T> { return with_lock(this.lock_path, fn); }
  _admission_lock<T>(fn: () => Promise<T> | T): Promise<T> { return with_lock(this.admission_lock_path, fn); }
  _object_lock<T>(identity: string, fn: () => Promise<T> | T, blocking: boolean | Data = true): Promise<T> { return with_lock(object_lock_path(dirname(this.store.path), identity), fn, typeof blocking === "object" ? blocking.blocking !== false : blocking); }
  _native_lock<T>(task: Data, fn: () => Promise<T> | T): Promise<T> { return with_lock(object_lock_path(dirname(this.store.path), "native:" + task.session_id), fn, true, true); }
  now(): Date { return this.clock(); }
  _release_metadata(): Data { return manifest_metadata(this.release_manifest, { present: this.release_manifest_present, verified: this.release_artifacts_verified, legacy: this.release_manifest === null }); }
  _response_compatibility(): Data { return { schema: 1, semantic_revision: 1 }; }
  _release_activation_fenced(): boolean {
    let info;
    try { info = lstatSync(this.release_fence_path); } catch (error: any) { if (error.code === "ENOENT") return false; throw new ControlError("release_fence_unverified", "release activation fence cannot be inspected"); }
    if (!info.isFile() || info.isSymbolicLink() || this.release_fence_path === DEFAULT_RELEASE_FENCE_PATH && (info.uid !== 0 || (info.mode & 0o777) !== 0o600)) throw new ControlError("release_fence_unverified", "release activation fence is not a regular file");
    return true;
  }
  _merge_warnings(...groups: Data[][]): Data[] {
    const seen = new Set<string>(); return groups.flat().filter(item => { const identity = canonical_json([item.code ?? null, item.action ?? null, item.message ?? null]); if (seen.has(identity)) return false; seen.add(identity); return true; }).map(item => ({ ...item }));
  }
  _preflight_release(request: Data): Data[] {
    if (this.release_manifest_error) throw this.release_manifest_error;
    if (!(typeof request.protocol === "number" || typeof request.protocol === "bigint" || request.protocol instanceof JsonFloat) || Number(request.protocol) !== PROTOCOL_VERSION) throw new ControlError("protocol_mismatch", `protocol must be ${PROTOCOL_VERSION}`);
    const action = require_string(request.action, "action");
    if (!ACTION_NAMES.includes(action)) throw new ControlError("unknown_action", `unknown action: ${action}`);
    if (this.release_manifest) verify_runtime_bundle(this.release_manifest, runtime_artifact_paths(this.config, this.release_manifest_path, this.release_manifest));
    const warnings = check_compatibility(this.release_manifest ?? legacy_manifest(), action, request.compatibility ?? null);
    if (!this.release_manifest) warnings.push({ code: "legacy_control_release", action, message: "control release manifest is absent; using legacy protocol-4 compatibility" });
    return this._merge_warnings(this.release_warnings, warnings);
  }
  async dispatch(request: any): Promise<Data> {
    if (!is_record(request)) throw new ControlError("invalid_request", "request must be an object");
    const warnings = this._preflight_release(request), action = request.action;
    let session = request.session_id ?? null;
    if (action === "timer-expire") session = this.store.get_task({ task_id: String(request.task_id) })?.session_id ?? "missing-timer";
    const identity = ControlCore.SESSION_ACTIONS.has(action) || action === "timer-expire" ? "session:" + String(session) : "admin";
    return this._object_lock(identity, async () => {
      if (action === "timer-expire") await this._expire_preempt(request);
      if (this._is_write_action(action, request)) {
        const operation = require_string(request.operation_id, "operation_id", SESSION_ID_RE);
        return this._object_lock("operation:" + operation, () => this._dispatch_locked(request, warnings));
      }
      return this._dispatch_locked(request, warnings);
    });
  }
  _annotate_state_error(error: ControlError, { action, operation_id }: Data): ControlError {
    if (error.code !== "sqlite_unavailable" || !operation_id || !ControlCore.WRITE_ACTIONS.has(action)) return error;
    const detail = is_record(error.detail) ? { ...error.detail } : {};
    try { const journal = this.store.operation(operation_id); detail.state_committed = journal ? ["success", "error"].includes(journal.outcome) ? true : null : detail.state_committed ?? null; } catch { detail.state_committed = null; }
    return new ControlError(error.code, error.message, detail);
  }
  _native_init_absence_retry_record(task: Data): Data | null {
    try { const value = parse_json(task.last_error); return is_record(value) && value.native_init_absence_retry === true ? value : null; } catch { return null; }
  }
  _native_init_absence_retry_pending(task: Data): boolean { return this._native_init_absence_retry_record(task) !== null; }
  _native_init_absence_retry_ready(task: Data): boolean { try { const deadline = parse_time(this._native_init_absence_retry_record(task)?.native_init_absence_retry_after); return !!deadline && deadline <= this.now(); } catch { return false; } }
  _native_init_retry_after(task: Data): string { let deadline: Date | null = null; try { deadline = parse_time(this._native_init_absence_retry_record(task)?.native_init_absence_retry_after); } catch {} return iso_time(deadline && deadline > this.now() ? deadline : new Date(this.now().getTime() + 2000)); }
  _native_init_absence_checks(task: Data): any { const value = this._native_init_absence_retry_record(task)?.native_init_absence_checks ?? 0; try { return require_int(value, "checks", 0); } catch { return 0; } }
  _native_init_cleanup_verified(task: Data): boolean | null { const value = this._native_init_absence_retry_record(task)?.native_init_cleanup_verified; return typeof value === "boolean" ? value : null; }
  _pending_acquire_operations_for_task(task: Data): Data[] {
    let pending: Data[];
    try { pending = this.store.pending_operations(); }
    catch (error) {
      throw new ControlError("operation_unverified", "pending acquire operations cannot be verified", {
        task_id: task.task_id,
        cause: error instanceof ControlError ? error.as_dict() : null,
      });
    }
    const previous = is_integer(task.generation) ? sub(task.generation, 1) : null;
    return pending.filter(item =>
      item.action === "acquire" && item.session_id === task.session_id && (
        item.task_id === task.task_id && equal(item.task_generation, task.generation) ||
        (item.task_id == null && (
          equal(item.generation, task.generation) || previous !== null && equal(item.generation, previous)
        ))
      )
    );
  }
  _pending_acquire_retry_item(operation_id: string, session_id: string, request: Data, task: Data): Data | null {
    let pending: Data[];
    try { pending = this.store.pending_operations().filter(item => item.operation_id === operation_id); }
    catch { return null; }
    if (pending.length !== 1) return null;
    const item = pending[0];
    const request_generation = request.generation;
    const current_generation = task.generation;
    const previous = is_integer(current_generation) ? sub(current_generation, 1) : null;
    if (item.action !== "acquire" || item.session_id !== session_id || !equal(item.generation, request_generation)) return null;
    if (!equal(request_generation, current_generation) && (previous === null || !equal(request_generation, previous))) return null;
    if (equal(request_generation, current_generation)) {
      if (item.task_id != null && item.task_id !== task.task_id) return null;
      if (item.task_generation != null && !equal(item.task_generation, current_generation)) return null;
    } else if (item.task_id !== task.task_id || !equal(item.task_generation, current_generation)) return null;
    return item;
  }
  _bind_pending_acquire_operation(request: Data, task: Data, requested_resources: Data): void {
    const operation_id = require_string(request.operation_id, "operation_id", SESSION_ID_RE);
    const operation = this.store.operation(operation_id);
    if (!operation || operation.outcome !== "pending") return;
    if (operation.task_id != null && operation.task_id !== task.task_id)
      throw new ControlError("operation_unverified", "pending acquire operation is bound to a different task", { operation_id });
    if (operation.task_generation != null && !equal(operation.task_generation, task.generation))
      throw new ControlError("operation_conflict", "pending acquire operation belongs to a different generation", { operation_id });
    if (operation.acquire_resources != null) {
      let recorded: Data;
      try { recorded = resource_mapping(canonical_json(operation.acquire_resources), "pending acquire resources", { allow_empty: false }); }
      catch { throw new ControlError("operation_conflict", "pending acquire request lacks verified resource evidence", { operation_id }); }
      if (!equal(recorded, requested_resources))
        throw new ControlError("operation_conflict", "pending acquire request differs from the active lease", { operation_id, requested: recorded, active: requested_resources });
    } else operation.acquire_resources = { ...requested_resources };
    operation.task_id = task.task_id;
    operation.task_generation = task.generation;
    this.store.record_operation(operation_id, "acquire", task.session_id, request.generation, operation);
  }
  _verify_pending_acquire_resources(pending: Data[], expected: Data): void {
    for (const item of pending) {
      const operation_id = require_string(item.operation_id, "operation_id", SESSION_ID_RE);
      const operation = this.store.operation(operation_id);
      let recorded: Data;
      try { recorded = resource_mapping(canonical_json(operation?.acquire_resources), "pending acquire resources", { allow_empty: false }); }
      catch { throw new ControlError("operation_conflict", "pending acquire request lacks verified resource evidence", { operation_id }); }
      if (!equal(recorded, expected))
        throw new ControlError("operation_conflict", "pending acquire request differs from the active lease", { operation_id, requested: recorded, active: expected });
    }
  }
  _complete_pending_acquire_operation(pending: Data, task: Data, result: Data): void {
    const operation_id = require_string(pending.operation_id, "operation_id", SESSION_ID_RE);
    if (!is_integer(pending.generation) || compare(pending.generation, 0) < 0)
      throw new ControlError("operation_unverified", "pending acquire generation is invalid", { operation_id });
    const previous = is_integer(task.generation) ? sub(task.generation, 1) : null;
    if (!equal(pending.generation, task.generation) && (previous === null || !equal(pending.generation, previous)))
      throw new ControlError("operation_conflict", "pending acquire generation is no longer current", { operation_id });
    const operation = this.store.operation(operation_id);
    if (!operation || operation.outcome !== "pending" || typeof operation._request_digest !== "string" || !operation._request_digest)
      throw new ControlError("operation_unverified", "pending acquire operation cannot be completed", { operation_id });
    if (!is_record(result.resources))
      throw new ControlError("operation_unverified", "active acquire result lacks resource evidence", { operation_id });
    this._verify_pending_acquire_resources([pending], result.resources);
    const warnings = Array.isArray(operation.warnings) ? operation.warnings : [];
    this.store.record_operation(operation_id, "acquire", task.session_id, pending.generation, {
      ok: true, protocol: PROTOCOL_VERSION, server_time: iso_time(this.now()), action: "acquire",
      session_id: task.session_id, generation: pending.generation, operation_id,
      outcome: "success", result: { ...result }, response: this._response_compatibility(),
      release: this._release_metadata(), warnings, _request_digest: operation._request_digest,
    });
  }
  _pending_acquire_cancellation_marker(task: Data, strict = false): Data | null {
    let value: any;
    try { value = parse_json(task.last_error); }
    catch (error) {
      if (strict) throw new ControlError("operation_unverified", "pending acquire cancellation marker is invalid");
      return null;
    }
    if (!is_record(value) || value.pending_acquire_cancellation !== true) return null;
    if (value.task_id !== task.task_id || !equal(value.task_generation, task.generation)) {
      if (strict) throw new ControlError("operation_unverified", "pending acquire cancellation marker does not match the task", { task_id: task.task_id });
      return null;
    }
    const ids = value.pending_acquire_operation_ids;
    if (!Array.isArray(ids) || ids.length === 0 || new Set(ids).size !== ids.length || ids.some(id => typeof id !== "string" || !SESSION_ID_RE.test(id))) {
      if (strict) throw new ControlError("operation_unverified", "pending acquire cancellation operation IDs are invalid", { task_id: task.task_id });
      return null;
    }
    return { pending_acquire_operation_ids: [...ids] };
  }
  _pending_acquire_cancellation_error(task: Data, pending: Data[]): string | null {
    if (!pending.length) return typeof task.last_error === "string" ? task.last_error : null;
    const previous = (() => { try { const value = parse_json(task.last_error); return is_record(value) ? value : {}; } catch { return {}; } })();
    const ids = pending.map(item => require_string(item.operation_id, "operation_id", SESSION_ID_RE));
    return canonical_json({ ...previous, pending_acquire_cancellation: true, pending_acquire_operation_ids: ids, task_id: task.task_id, task_generation: task.generation });
  }
  _pending_acquire_cancellation_operations(task: Data): Data[] {
    const marker = this._pending_acquire_cancellation_marker(task, true);
    if (!marker) return [];
    const pending = this._pending_acquire_operations_for_task(task);
    const expected = new Set(marker.pending_acquire_operation_ids);
    const unexpected = pending.filter(item => !expected.has(item.operation_id)).map(item => item.operation_id);
    if (unexpected.length) throw new ControlError("operation_unverified", "unexpected pending acquire operation belongs to cancellation marker", { task_id: task.task_id, operation_ids: unexpected });
    return pending;
  }
  _cancel_pending_acquire_operations(pending: Data[], task: Data): void {
    for (const item of pending) {
      const operation_id = require_string(item.operation_id, "operation_id", SESSION_ID_RE);
      const operation = this.store.operation(operation_id);
      if (!operation || operation.outcome !== "pending" || typeof operation._request_digest !== "string" || !operation._request_digest)
        throw new ControlError("operation_unverified", "pending acquire operation cannot be finalized", { operation_id });
      this.store.record_operation(operation_id, "acquire", task.session_id, item.generation, {
        outcome: "error",
        error: { code: "lease_released", message: "the lease was released before the acquire operation was acknowledged", detail: { task_id: task.task_id, task_generation: task.generation, outcome_verified: true } },
        _request_digest: operation._request_digest,
      });
    }
  }
  _pending_native_init_for_task(task: Data): Data | null {
    const matches = this._pending_acquire_operations_for_task(task);
    if (matches.length > 1) throw new ControlError("operation_unverified", "multiple pending native initialization operations match the task", { task_id: task.task_id });
    return matches[0] ?? null;
  }
  _pending_acquire_for_task(task: Data): Data | null {
    const matches = this._pending_acquire_operations_for_task(task);
    if (matches.length > 1)
      throw new ControlError("operation_unverified", "multiple pending acquire operations match the task", { task_id: task.task_id });
    return matches[0] ?? null;
  }
  _recovered_native_init_operation(task: Data, pending: Data): [string, Data] {
    const operation_id = pending.operation_id;
    if (typeof operation_id !== "string" || !operation_id) throw new ControlError("operation_unverified", "pending acquire operation ID is invalid");
    const record = this.store.operation(operation_id), request_digest = record?._request_digest;
    if (typeof request_digest !== "string" || !request_digest) throw new ControlError("operation_unverified", "pending acquire request digest is invalid", { operation_id });
    const original = this._native_init_absence_retry_record(task)?.error;
    const valid = is_record(original) && typeof original.code === "string" && original.code;
    const error = { code: valid ? original.code : "acquire_partial", message: valid && typeof original.message === "string" && original.message ? original.message : "native initialization did not complete", detail: { ...(valid && is_record(original.detail) ? original.detail : {}), phase: "native-init", operation_phase: "native-init", native_init_recovered_absent: true, outcome_verified: true } };
    return [operation_id, { outcome: "error", error, _request_digest: request_digest }];
  }
  _native_init_timeout_cleanup_verified(error: ControlError): boolean {
    const detail = is_record(error.detail) ? error.detail : {};
    if (!["process_group_killed", "process_group_cleanup_error", "reaped", "pipes_drained"].some(key => key in detail)) return true;
    return ["process_group_killed", ...(error.code === "command_timeout" ? ["reaped", "pipes_drained"] : [])].every(key => detail[key] === true) && !detail.process_group_cleanup_error;
  }
  _operation_outcome_uncertain(_action: string, error: ControlError): boolean {
    const detail = is_record(error.detail) ? error.detail : {};
    if (error.code === "sqlite_unavailable" && detail.state_committed !== false || detail.native_init_absence_retry === true || is_record(detail.cleanup) && detail.cleanup.ok === false) return true;
    if (detail.native_init_absence_retry === false) return false;
    if (typeof detail.outcome_verified === "boolean") return !detail.outcome_verified;
    if (detail.cleanup?.ok && ["cached", "absent"].includes(detail.cleanup.result?.state)) return false;
    if (["acquire_partial", "resize_partial", "restart_partial", "cleanup_incomplete", "operation_unverified"].includes(error.code)) return true;
    return ["phase", "operation_phase"].some(key => ["native-generation", "native-init", "native-config", "native-stop", "native-start", "guest-readiness", "guest-post-check", "access-exposure", "port-add", "port-remove", "cache-delete"].includes(detail[key]));
  }
  async _expire_preempt(request: Data): Promise<void> {
    const task_id = require_string(request.task_id, "task_id", TASK_ID_RE), generation = require_int(request.generation, "generation", 1);
    const task = await this._global_lock(() => this.store.transaction(() => {
      const current = this.store.get_task({ task_id }); if (!current || !equal(current.generation, generation)) return null;
      const expiry = parse_time(current.lease_expires_at);
      if (!["active", "preparing"].includes(current.state) || expiry && expiry > this.now()) return null;
      const inactive = parse_time(current.inactive_since) ?? new Date(Math.min(this.now().getTime(), expiry?.getTime() ?? this.now().getTime()));
      const pending = current.state === "active" ? this._pending_acquire_operations_for_task(current) : [];
      return this.store.update_task(task_id, {
        state: "closing", lease_expires_at: null, inactive_since: iso_time(inactive),
        cleanup_after: iso_time(new Date(inactive.getTime() + Number(this.config.cache_seconds) * 1000)),
        ...(pending.length ? { last_error: this._pending_acquire_cancellation_error(current, pending) } : {}),
      });
    }));
    if (!task) return;
    const record_error = async (last_error: string) => this._global_lock(() => this.store.transaction(() => {
      const latest = this.store.get_task({ task_id });
      if (!latest || !equal(latest.generation, generation) || latest.state !== "closing") return;
      let detail: Data = {};
      try { const value = parse_json(last_error); if (is_record(value)) detail = value; } catch {}
      const marker = this._pending_acquire_cancellation_marker(latest);
      if (marker) {
        detail = {
          ...detail, pending_acquire_cancellation: true,
          pending_acquire_operation_ids: marker.pending_acquire_operation_ids,
          task_id: latest.task_id, task_generation: latest.generation,
        };
      }
      this.store.update_task(task_id, { last_error: canonical_json(detail) });
    }));
    try {
      await this._object_lock("cleanup:" + task.session_id, async () => { const current = this._cleanup_current(task); if (!current) return; const [errors, completed] = await this._cleanup_task(current); if (errors.length) await record_error(canonical_json({ errors, completed })); });
    } catch (error) { if (!(error instanceof ControlError)) throw error; await record_error(canonical_json(error.as_dict())); }
  }
  async _dispatch_locked(request: Data, warnings: Data[] = []): Promise<Data> {
    const action = require_string(request.action, "action");
    let session_id = request.session_id ?? null;
    if (ControlCore.SESSION_ACTIONS.has(action) || session_id !== null) session_id = require_string(session_id, "session_id", SESSION_ID_RE);
    if (action === "admin-metrics" && ("session_id" in request || "operation_id" in request)) throw new ControlError("invalid_request", "admin-metrics is sessionless and read-only");
    const is_write = this._is_write_action(action, request);
    const operation_id = is_write ? require_string(request.operation_id, "operation_id", SESSION_ID_RE) : request.operation_id ?? null;
    const request_digest = is_write ? digest(request) : null;
    let pending_retry = false;
    if (is_write) {
      const saved = this.store.operation(operation_id);
      if (saved) {
        if (saved._request_digest !== request_digest) throw new ControlError("operation_conflict", "operation ID belongs to a different request");
        if (saved.outcome === "pending") {
          const task = session_id === null ? null : this.store.get_task({ session_id });
          const allowed = (action === "acquire" && task !== null && task.state === "active" &&
            this._pending_acquire_retry_item(operation_id, session_id!, request, task) !== null) ||
            (["release", "destroy"].includes(action) && task !== null && ["closing", "cached", "deleting", "absent"].includes(task.state));
          if (!allowed) throw new ControlError("operation_incomplete", "previous execution has an uncertain outcome; inspect status and reconcile before another mutation", { operation_id });
          pending_retry = true;
        } else {
          if (saved.outcome === "error") throw new ControlError(saved.error.code, saved.error.message, saved.error.detail);
          await this._validate_cached_success(action, operation_id, session_id, request, saved);
          const response = { ...saved }; delete response._request_digest;
          response.server_time = iso_time(this.now()); response.outcome = "replayed"; response.warnings = this._merge_warnings(Array.isArray(response.warnings) ? response.warnings : [], warnings);
          response.response ??= this._response_compatibility(); response.release = this._release_metadata();
          if (session_id) { const task = this.store.get_task({ session_id }); response.current_state = task ? this._state_summary(task) : null; if (action === "renew" && task && this._state_summary(task).effective_lease) await this._schedule_expiry(task); }
          return response;
        }
      }
      if (!("generation" in request)) throw new ControlError("invalid_request", "write request requires generation");
      require_int(request.generation, "generation", 0);
      if (action !== "renew" && !pending_retry) this.store.record_operation(operation_id, action, session_id, request.generation, { outcome: "pending", _request_digest: request_digest });
    }
    let result;
    try { result = await this._dispatch_action(action, { ...request }, session_id, warnings); }
    catch (error) {
      if (!(error instanceof ControlError)) throw error;
      let annotated = error;
      try {
        let uncertain = this._operation_outcome_uncertain(action, error);
        if (!uncertain && ["release", "destroy"].includes(action) && session_id !== null) { try { uncertain = ["closing", "deleting"].includes(this.store.get_task({ session_id })?.state ?? ""); } catch { uncertain = true; } }
        const detail = is_record(error.detail) ? { ...error.detail } : {}; detail.outcome_verified ??= !uncertain; annotated = new ControlError(error.code, error.message, detail);
        if (operation_id && is_write) {
          const saved = this.store.operation(operation_id);
          const preserve_pending_retry = pending_retry && action === "acquire";
          if (!preserve_pending_retry && (!saved || saved.outcome === "pending" && !this._operation_outcome_uncertain(action, annotated)))
            this.store.record_operation(operation_id, action, session_id, request.generation ?? null, { outcome: "error", error: annotated.as_dict(), _request_digest: request_digest });
        }
        if (session_id !== null || is_write) this.store.audit(action, "error", { session_id, generation: request.generation ?? null, detail: annotated.as_dict() });
      } catch (state_error) { if (!(state_error instanceof ControlError)) throw state_error; throw this._annotate_state_error(state_error, { action, operation_id }); }
      throw annotated;
    }
    const response = { ok: true, protocol: PROTOCOL_VERSION, server_time: iso_time(this.now()), action, session_id, generation: request.generation ?? null, operation_id, outcome: "success", result, response: this._response_compatibility(), release: this._release_metadata(), warnings: this._merge_warnings(warnings) };
    try {
      if (operation_id !== null && is_write && action !== "renew") this.store.record_operation(operation_id, action, session_id, request.generation ?? null, { ...response, _request_digest: request_digest });
      if (session_id !== null || is_write) this.store.audit(action, "ok", { session_id, generation: request.generation ?? null, detail: result });
    } catch (error) { if (!(error instanceof ControlError)) throw error; throw this._annotate_state_error(error, { action, operation_id }); }
    return response;
  }
  async _dispatch_action(action: string, request: Data, session_id: string | null, warnings: Data[] = []): Promise<Data> {
    if (action === "renew") return this.renew(request, session_id, warnings);
    const method = action.replaceAll("-", "_");
    if (!ACTION_NAMES.includes(action) || typeof this[method] !== "function") throw new ControlError("unknown_action", `unknown action: ${action}`);
    return this[method](request, session_id);
  }
  async _validate_cached_success(action: string, operation_id: string, session_id: string | null, request: Data, cached: Data): Promise<void> {
    const fail = (message: string): never => { throw new ControlError("operation_unverified", message, { operation_id }); };
    if (!equal([cached.action, cached.operation_id, cached.session_id, cached.generation], [action, operation_id, session_id, request.generation])) fail("operation journal identity does not match the request");
    const result = cached.result; if (!is_record(result)) fail("operation journal result is invalid");
    if (["drain-enter", "drain-exit"].includes(action)) { const expected = action === "drain-enter"; if (result.draining !== expected || (this.store.get_meta("draining") === "1") !== expected) fail("drain state does not match the journal result"); return; }
    if (["acquire", "renew", "release", "destroy", "restart", "resize"].includes(action)) {
      if (!session_id) fail("session-scoped operation has no session");
      const task = this.store.get_task({ session_id }); if (!task) fail("task state is unavailable for journal replay");
      let current: Data = {}; try { current = this._state_summary(task); } catch { fail("task state cannot be verified for journal replay"); }
      const fields = ["task_id", "session_id", "generation", "state", "instance_name", "vm_ip", "ssh_host_port", "lease_expires_at", "inactive_since", "cleanup_after", "ports", "resources", "resource_reservation", "last_error"];
      if (fields.some(key => !equal(result[key], current[key]))) fail("task state does not match the journal result");
      try {
        if (["acquire", "restart", "resize"].includes(action)) {
          if (task!.state !== "active" || !current.effective_lease) fail("replayed operation lease is no longer effective");
          await this._ownership(task);
          if ((await this.incus.instance_info(task!.instance_name)).status.toLowerCase() !== "running") fail("replayed guest is not running");
          if (!equal(await this._native_resources(task), result.resources)) fail("replayed native resources differ from the journal");
        } else if (["release", "destroy"].includes(action)) await this._validate_cleanup_replay(task!, fail);
      } catch (error) { if (error instanceof ControlError && error.code === "operation_unverified") throw error; fail("replayed native state cannot be verified"); }
      return;
    }
    if (action === "port-add" || action === "port-remove") {
      if (!session_id) fail("port operation has no session");
      const task = this.store.get_task({ session_id }); if (!task || task.state !== "active" || !this._state_summary(task).effective_lease) fail("port replay lease is no longer effective");
      if (action === "port-remove") {
        const name = request.name, protocol = request.port_protocol ?? "tcp";
        if (!equal(task!.generation, request.generation) || result.task_id !== task!.task_id || result.name !== name || result.protocol !== protocol || result.removed !== true || typeof name !== "string" || !["tcp", "udp"].includes(protocol) || this.store.get_port(task!.task_id, name, protocol)) fail("removed port still exists after journal success");
      } else {
        const port = typeof result.port_name === "string" && ["tcp", "udp"].includes(result.protocol) ? this.store.get_port(task!.task_id, result.port_name, result.protocol) : null;
        if (!port || port.state !== "active" || !equal(port.generation, task!.generation) || ["task_id", "port_name", "protocol", "host_port", "vm_port", "generation"].some(key => !equal(result[key], port[key]))) fail("port state does not match the journal result");
        try { const native = (await this._ownership(task)).devices?.[port!.device_name], expected = this._proxy_properties(task, port); if (!is_record(native) || Object.keys(expected).some(key => native[key] !== expected[key])) fail("port mapping does not match the journal result"); }
        catch (error) { if (error instanceof ControlError && error.code === "operation_unverified") throw error; fail("replayed port mapping cannot be verified"); }
      }
      return;
    }
    if (["admin-gc", "timer-age-gc"].includes(action)) await this._validate_cached_gc_result(operation_id, result);
    if (["admin-reconcile", "timer-reconcile"].includes(action)) await this._validate_cached_reconcile_result(operation_id, result);
  }
  async _validate_cleanup_replay(task: Data, fail: (message: string) => never): Promise<void> {
    if (task.state === "cached") { const owned = await this._ownership(task); if ((await this.incus.instance_info(task.instance_name)).status.toLowerCase() !== "stopped" || this.store.get_ports(task.task_id).length || Object.values(owned.expanded_devices ?? {}).some((item: any) => item.type === "proxy")) fail("replayed cache still has native resources"); }
    else if (task.state === "absent") { if ((await this.incus.list_instances()).some(item => item.name === task.instance_name)) fail("replayed absent instance still exists"); }
    else fail("replayed cleanup state is invalid");
  }
  async _validate_cached_gc_result(operation_id: string, result: Data): Promise<void> {
    const fail = (message: string): never => { throw new ControlError("operation_unverified", message, { operation_id }); };
    if (typeof result.dry_run !== "boolean" || !["eligible", "age"].includes(result.selection) || !Array.isArray(result.candidates)) fail("GC journal result is invalid");
    try { require_int(result.minimum_idle_seconds, "minimum_idle_seconds"); } catch { fail("GC journal result is invalid"); }
    for (const candidate of result.candidates) {
      if (!is_record(candidate) || typeof candidate.task_id !== "string") fail("GC journal candidate is invalid");
      if ("error" in candidate) { if (!is_record(candidate.error) || typeof candidate.error.code !== "string" || typeof candidate.error.message !== "string") fail("GC journal error is invalid"); continue; }
      if (typeof candidate.instance_name !== "string" || typeof candidate.inactive_since !== "string") fail("GC journal candidate is incomplete");
      try { require_int(candidate.generation, "generation"); } catch { fail("GC journal candidate is incomplete"); }
      const task = this.store.get_task({ task_id: candidate.task_id });
      if (!task) fail("GC task state is unavailable for replay");
      if (result.dry_run) { if (task!.state !== "cached" || ["task_id", "instance_name", "generation", "inactive_since"].some(key => !equal(candidate[key], task![key]))) fail("GC preview no longer matches durable state"); }
      else { if (task!.state !== "absent" || !equal(task!.generation, candidate.generation)) fail("GC deletion is not reflected in durable state"); if ((await this.incus.list_instances()).some(item => item.name === candidate.instance_name)) fail("GC-deleted native instance still exists"); }
    }
  }
  _inventory_mappings(value: any, code: string, label: string): Data[] { if (!Array.isArray(value) || value.some(item => !is_record(item))) throw new ControlError(code, `${label} inventory is malformed`); return value; }
  async _validate_cached_absent_reconcile(task: Data, generation: any, { dry_run, operation_id }: Data): Promise<void> {
    const fail = (message: string): never => { throw new ControlError("operation_unverified", message, { operation_id }); };
    const owner = task.session_id; if (typeof owner !== "string" || !owner) fail("absent resource owner cannot be verified");
    const current = () => { const value = this.store.get_task({ task_id: task.task_id }); if (!value || value.state !== "absent" || !equal(value.generation, generation)) fail("absent resource task changed during replay"); return value!; };
    const name = await this._object_lock("session:" + owner, () => current().instance_name, false);
    const exists = (await this.incus.list_instances()).some(item => item.name === name);
    await this._object_lock("session:" + owner, () => {
      const value = current(); if (exists || this.store.get_ports(value.task_id).length) fail("absent resource cleanup is no longer verified");
      let resources = null, reservation = null;
      try { resources = resource_mapping(value.resources_json, "resources"); } catch {}
      try { reservation = resource_mapping(value.resource_reservation_json, "reservation", { allow_empty: true }); } catch {}
      if (!dry_run && (!equal(resources, { cpu: 0, memory_mib: 0, disk_gib: 0 }) || !equal(reservation, {}) || !equal(value.reservation_bytes, 0))) fail("absent resource repair is not reflected in durable state");
    }, false);
  }
  async _validate_cached_reconcile_result(operation_id: string, result: Data): Promise<void> {
    const fail = (message: string): never => { throw new ControlError("operation_unverified", message, { operation_id }); };
    if (typeof result.dry_run !== "boolean" || !Array.isArray(result.actions)) fail("reconcile journal result is invalid");
    const dry_states: Data = { inspect: ["active", "cached", "closing", "preparing", "failed", "deleting"], "finish-delete": ["deleting"], "close-incomplete-or-expired": ["active", "closing", "preparing", "failed"], "stop-unexpected-running-cache": ["cached"], "rearm-current-expiry": ["active"], "verify-absent-resources": ["absent"] };
    const applied_states: Data = { ...dry_states, "finish-delete": ["absent"], "close-incomplete-or-expired": ["cached", "absent"], "stop-unexpected-running-cache": ["cached", "absent"] };
    for (const item of result.actions) {
      if (!is_record(item) || typeof item.task_id !== "string" || !(item.action in dry_states)) fail("reconcile journal action is invalid");
      try { require_int(item.generation, "generation"); } catch { fail("reconcile journal action is invalid"); }
      const task = this.store.get_task({ task_id: item.task_id }); if (!task || !equal(task.generation, item.generation)) fail("reconcile task generation no longer matches");
      if ("error" in item) { if (!is_record(item.error) || typeof item.error.code !== "string" || typeof item.error.message !== "string") fail("reconcile journal error is invalid"); continue; }
      if (!(result.dry_run ? dry_states : applied_states)[item.action].includes(task!.state)) fail("reconcile result is not reflected in durable state");
      if (item.action === "verify-absent-resources") await this._validate_cached_absent_reconcile(task!, item.generation, { dry_run: result.dry_run, operation_id });
      if (!result.dry_run) {
        if (item.action === "rearm-current-expiry") { if (!this._state_summary(task).effective_lease) fail("reconciled lease is no longer effective"); await this._ownership(task); if ((await this.incus.instance_info(task!.instance_name)).status.toLowerCase() !== "running" || !await this.systemd.expiry_active(task!.task_id, item.generation, task!.lease_expires_at)) fail("reconciled expiry timer is not active"); }
        else if (["close-incomplete-or-expired", "stop-unexpected-running-cache", "finish-delete"].includes(item.action) || task!.state === "absent") await this._validate_cleanup_replay(task!, fail);
      }
    }
  }
}
