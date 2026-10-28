import { emptyReferences, mockState } from "./mockData";
import type { ActionPreview, AuditEvent, ImageImportRequest, ManagedImage, ManagedVM, Operation, Overview, SystemStatus } from "./types";
import { demoForcedOutcome, demoMutationError, emitDemoChange, getMockState, isDemoRuntime } from "../demo/runtime";

const useRealApi = import.meta.env.VITE_MANAGEMENT_API === "real" && !isDemoRuntime();

type RealRequestOptions = { contentType?: string | null };
type ManagementSession = { csrf_token: string; expires_at?: string };

export class ManagementRequestError extends Error {
  readonly code: string;
  readonly phase: string | null;
  readonly operationId: string | null;
  readonly safeFields: Record<string, unknown> | null;

  constructor(code: string, phase: string | null = null, operationId: string | null = null, safeFields: Record<string, unknown> | null = null) {
    super(code);
    this.name = "ManagementRequestError";
    this.code = code;
    this.phase = phase;
    this.operationId = operationId;
    this.safeFields = safeFields;
  }
}

let csrfToken: string | null = null;
let csrfExpiresAt = 0;
let sessionPromise: Promise<string> | null = null;

export function createOperationId(): string {
  const cryptoApi = typeof globalThis !== "undefined" ? globalThis.crypto : undefined;
  if (cryptoApi && typeof cryptoApi.randomUUID === "function") return cryptoApi.randomUUID();
  const bytes = new Uint8Array(16);
  if (cryptoApi && typeof cryptoApi.getRandomValues === "function") cryptoApi.getRandomValues(bytes);
  else for (let index = 0; index < bytes.length; index += 1) bytes[index] = Math.floor(Math.random() * 256);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

async function requestJSON<T>(path: string, init: RequestInit | undefined, options: RealRequestOptions = {}): Promise<T> {
  const headers = new Headers(init?.headers);
  headers.set("Accept", "application/json");
  if (options.contentType !== null && !headers.has("Content-Type")) headers.set("Content-Type", options.contentType ?? "application/json");
  const response = await fetch(`/api/v1${path}`, { credentials: "same-origin", ...init, headers });
  if (response.ok) return JSON.parse(await response.text()) as T;
  const raw = await response.text();
  let body: (T & { error?: { code?: string; phase?: string; operation_id?: string; safe_fields?: Record<string, unknown> } }) | null = null;
  if (raw) {
    try {
      body = JSON.parse(raw) as T & { error?: { code?: string } };
    } catch {
      body = null;
    }
  }
  throw new ManagementRequestError(body?.error?.code ?? "internal_error", body?.error?.phase ?? null, body?.error?.operation_id ?? null, body?.error?.safe_fields ?? null);
}

async function ensureSession(): Promise<string> {
  if (csrfToken && Date.now() < csrfExpiresAt - 5_000) return csrfToken;
  csrfToken = null;
  if (!sessionPromise) {
    sessionPromise = requestJSON<ManagementSession>("/session", undefined)
      .then((session) => {
        if (!session.csrf_token) throw new Error("authentication_required");
        csrfToken = session.csrf_token;
        csrfExpiresAt = session.expires_at ? Date.parse(session.expires_at) : Date.now() + 14 * 60_000;
        return session.csrf_token;
      })
      .finally(() => {
        sessionPromise = null;
      });
  }
  return sessionPromise;
}

async function realRequest<T>(path: string, init?: RequestInit, options: RealRequestOptions = {}): Promise<T> {
  const method = (init?.method ?? "GET").toUpperCase();
  const headers = new Headers(init?.headers);
  if (method !== "GET" && method !== "HEAD") headers.set("X-CSRF-Token", await ensureSession());
  else await ensureSession();
  try {
    return await requestJSON<T>(path, { ...init, headers }, options);
  } catch (error) {
    if (error instanceof ManagementRequestError && (error.code === "authentication_required" || error.code === "csrf_invalid")) {
      csrfToken = null;
      csrfExpiresAt = 0;
    }
    throw error;
  }
}

function delay<T>(value: T): Promise<T> {
  return new Promise((resolve) => globalThis.setTimeout(() => resolve(value), 120));
}

function mockStateForRequest(): typeof mockState {
  if (isDemoRuntime()) {
    const mutationError = demoMutationError();
    if (mutationError === "network_unavailable") throw new ManagementRequestError(mutationError);
  }
  return getMockState();
}

function assertMockMutationAllowed(): void {
  if (!isDemoRuntime()) return;
  const error = demoMutationError();
  if (error) throw new ManagementRequestError(error);
}

export async function getOverview(): Promise<Overview> {
  return useRealApi ? realRequest<Overview>("/overview") : delay(mockStateForRequest().overview);
}

export async function listVMs(filters: { q?: string; state?: string; leaseState?: string; cursor?: string } = {}): Promise<{ items: ManagedVM[]; page: { limit: number; next_cursor: string | null } }> {
  if (useRealApi) {
    const search = new URLSearchParams({ limit: "25" });
    if (filters.q) search.set("q", filters.q);
    if (filters.state && filters.state !== "all") search.set("state", filters.state);
    if (filters.leaseState && filters.leaseState !== "all") search.set("lease_state", filters.leaseState);
    if (filters.cursor) search.set("cursor", filters.cursor);
    return realRequest(`/vms?${search.toString()}`);
  }
  const normalized = filters.q?.toLowerCase() ?? "";
  const state = mockStateForRequest();
  const items = state.vms.filter((vm) => (!normalized || `${vm.task_id} ${vm.session_id}`.toLowerCase().includes(normalized)) && (!filters.state || filters.state === "all" || vm.state === filters.state) && (!filters.leaseState || filters.leaseState === "all" || vm.lease.state === filters.leaseState));
  return delay({ items, page: { limit: 25, next_cursor: null } });
}

export async function getVM(taskId: string): Promise<ManagedVM> {
  const state = mockStateForRequest();
  return useRealApi ? realRequest<ManagedVM>(`/vms/${encodeURIComponent(taskId)}`) : delay(state.vms.find((vm) => vm.task_id === taskId) ?? state.vms[0]);
}

export async function listImages(query = "", cursor?: string): Promise<{ items: ManagedImage[]; page: { limit: number; next_cursor: string | null }; default_fingerprint: string | null; selection_version: string; catalog_version: string }> {
  if (useRealApi) return realRequest(`/images?limit=25${query ? `&q=${encodeURIComponent(query)}` : ""}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
  const normalized = query.toLowerCase();
  const state = mockStateForRequest();
  const items = state.images.filter((image) => !normalized || `${image.version_label} ${image.fingerprint}`.toLowerCase().includes(normalized));
  return delay({ items, page: { limit: 25, next_cursor: null }, default_fingerprint: state.images.find((image) => image.is_default)?.fingerprint ?? null, selection_version: state.image_selection_version, catalog_version: state.image_catalog_version });
}

export async function listOperations(status = "all", cursor?: string): Promise<{ items: Operation[]; page: { limit: number; next_cursor: string | null } }> {
  if (useRealApi) return realRequest(`/operations?limit=25${status !== "all" ? `&status=${status}` : ""}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
  const state = mockStateForRequest();
  return delay({ items: state.operations.filter((operation) => status === "all" || operation.status === status), page: { limit: 25, next_cursor: null } });
}

export async function listAudit(cursor?: string): Promise<{ items: AuditEvent[]; page: { limit: number; next_cursor: string | null } }> {
  return useRealApi ? realRequest(`/audit?limit=25${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`) : delay({ items: mockStateForRequest().audits, page: { limit: 25, next_cursor: null } });
}

export async function getSystem(): Promise<SystemStatus> {
  return useRealApi ? realRequest<SystemStatus>("/system") : delay(mockStateForRequest().system);
}

export async function previewVMAction(taskId: string, action: string, current?: Pick<ManagedVM, "generation" | "resource_version">, parameters: Record<string, unknown> = {}): Promise<ActionPreview> {
  if (useRealApi) {
    const target = current ?? await getVM(taskId);
    return realRequest(`/vms/${encodeURIComponent(taskId)}/actions/preview`, { method: "POST", body: JSON.stringify({ action, expected_generation: target.generation, target_version: target.resource_version, parameters }) });
  }
  mockStateForRequest();
  assertMockMutationAllowed();
  const state = getMockState();
  const vm = state.vms.find((candidate) => candidate.task_id === taskId) ?? state.vms[0];
  if (!vm) throw new ManagementRequestError("not_found");
  return delay(issueMockPreview(state, { kind: "vm", targetId: taskId, action, targetVersion: vm.resource_version, expectedGeneration: vm.generation, parameters }, { summary_code: vm.lease.state === "effective" ? "effective_lease_affected" : "no_active_lease", affected_session_count: vm.lease.state === "effective" ? 1 : 0, affected_image_references: vm.image.fingerprint === mockState.images[0].fingerprint ? mockState.images[0].references : { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, changes: [{ code: action === "destroy" ? "session_access_revoked" : "native_action", count: 1, value: null }] }));
}

export async function submitVMAction(taskId: string, action: string, reason: string, preview: ActionPreview, expectedGeneration: string, parameters: Record<string, unknown> = {}, operationId = createOperationId()): Promise<Operation> {
  if (useRealApi) return realRequest(`/vms/${encodeURIComponent(taskId)}/actions`, { method: "POST", body: JSON.stringify({ operation_id: operationId, reason, target_version: preview.target_version, action, expected_generation: expectedGeneration, parameters, confirmation_token: preview.confirmation_token }) });
  assertMockMutationAllowed();
  const state = getMockState();
  const mockAction = action as Operation["action"];
  const target = { kind: "vm" as const, id: taskId };
  const requestPayload = { action, taskId, reason, expectedGeneration, targetVersion: preview.target_version, confirmationToken: preview.confirmation_token, parameters };
  if (state.operations.some((operation) => operation.operation_id === operationId)) return createMockOperation(mockAction, target, reason, undefined, operationId, requestPayload, parameters);
  const vm = state.vms.find((candidate) => candidate.task_id === taskId);
  if (!vm) throw new ManagementRequestError("not_found", "precondition");
  if (!vm.allowed_actions.includes(action as ManagedVM["allowed_actions"][number])) throw new ManagementRequestError("action_not_allowed", "precondition", operationId);
  if (expectedGeneration !== vm.generation) throw new ManagementRequestError("stale_target", "precondition", operationId);
  assertMockPreviewFresh(state, preview, { kind: "vm", targetId: taskId, action, targetVersion: vm.resource_version, expectedGeneration, parameters }, operationId);
  return createMockOperation(mockAction, target, reason, undefined, operationId, requestPayload, parameters);
}

export async function startImageImport(request: ImageImportRequest): Promise<Operation> {
  if (useRealApi) return realRequest("/images/imports", { method: "POST", body: JSON.stringify(request) });
  assertMockMutationAllowed();
  const initialStatus = request.source.kind === "local_upload" ? "awaiting_upload" : undefined;
  const operation = createMockOperation("image_import", { kind: "image_catalog", id: "image-catalog" }, request.reason, initialStatus, request.operation_id, request, request as unknown as Record<string, unknown>);
  const state = getMockState();
  const imports = mockImageImportRequests.get(state) ?? new Map<string, ImageImportRequest>();
  mockImageImportRequests.set(state, imports);
  imports.set(request.operation_id, request);
  return operation;
}

export async function uploadImageImportContent(operationId: string, content: File | { metadata: File; rootfs: File }): Promise<Operation> {
  if (!useRealApi) {
    assertMockMutationAllowed();
    return continueMockUploadOperation(operationId);
  }
  if ("metadata" in content) {
    const form = new FormData();
    form.append("metadata", content.metadata, content.metadata.name);
    form.append("rootfs", content.rootfs, content.rootfs.name);
    return realRequest(`/images/imports/${encodeURIComponent(operationId)}/content`, { method: "PUT", body: form }, { contentType: null });
  }
  return realRequest(`/images/imports/${encodeURIComponent(operationId)}/content`, { method: "PUT", body: content }, { contentType: "application/octet-stream" });
}

export async function previewImageAction(fingerprint: string, action: "default" | "delete" | "retire", currentResourceVersion?: string): Promise<ActionPreview> {
  if (useRealApi) {
    const targetVersion = currentResourceVersion ?? (await realRequest<ManagedImage>(`/images/${encodeURIComponent(fingerprint)}`)).resource_version;
    const path = action === "default" ? "/images/default/preview" : `/images/${fingerprint}/${action === "delete" ? "delete" : "retire"}/preview`;
    const body = action === "default" ? { fingerprint, target_version: targetVersion } : { target_version: targetVersion };
    return realRequest(path, { method: "POST", body: JSON.stringify(body) });
  }
  mockStateForRequest();
  assertMockMutationAllowed();
  const state = getMockState();
  const image = state.images.find((candidate) => candidate.fingerprint === fingerprint) ?? state.images[0];
  if (!image) throw new ManagementRequestError("not_found");
  if (action === "default" && (image.state === "retired" || image.verification.suitability !== "passed")) throw new ManagementRequestError("image_not_applicable", "precondition");
  if (action === "retire" && image.is_default) throw new ManagementRequestError("default_image_required", "precondition");
  const targetVersion = action === "default" ? state.image_selection_version : image.resource_version;
  return delay(issueMockPreview(state, { kind: "image", targetId: fingerprint, action, targetVersion }, { summary_code: action === "delete" && image.references.active + image.references.cached > 0 ? "image_referenced" : "image_change_preview", affected_session_count: image.references.active, affected_image_references: image.references, changes: [{ code: action === "default" ? "new_sessions_use_image" : action === "retire" ? "new_selection_blocked" : "image_removed", count: null, value: image.version_label }] }));
}

export async function submitImageAction(fingerprint: string, action: "default" | "delete" | "retire", reason: string, preview: ActionPreview, operationId = createOperationId()): Promise<Operation> {
  if (useRealApi) {
    const path = action === "default" ? "/images/default" : `/images/${fingerprint}/${action === "delete" ? "delete" : "retire"}`;
    const body = action === "default"
      ? { operation_id: operationId, reason, target_version: preview.target_version, fingerprint, confirmation_token: preview.confirmation_token }
      : { operation_id: operationId, reason, target_version: preview.target_version, confirmation_token: preview.confirmation_token };
    return realRequest(path, { method: "POST", body: JSON.stringify(body) });
  }
  assertMockMutationAllowed();
  const state = getMockState();
  const mockAction = action === "default" ? "default_image_select" : action === "delete" ? "image_delete" : "image_retire";
  const target = { kind: "image" as const, id: fingerprint };
  const requestPayload = { action, fingerprint, reason, targetVersion: preview.target_version, confirmationToken: preview.confirmation_token };
  if (state.operations.some((operation) => operation.operation_id === operationId)) return createMockOperation(mockAction, target, reason, undefined, operationId, requestPayload);
  const image = state.images.find((candidate) => candidate.fingerprint === fingerprint);
  if (!image) throw new ManagementRequestError("not_found", "precondition");
  if (action === "default" && (image.state === "retired" || image.verification.suitability !== "passed")) throw new ManagementRequestError("image_not_applicable", "precondition");
  if (action === "retire" && image.is_default) throw new ManagementRequestError("default_image_required", "precondition");
  if (action === "delete" && image.is_default) throw new ManagementRequestError("default_image_required", "precondition");
  if (action === "delete" && image.references.active + image.references.cached + image.references.releasing + image.references.pending_creation + image.references.verification > 0) throw new ManagementRequestError("image_referenced", "precondition");
  const targetVersion = action === "default" ? state.image_selection_version : image.resource_version;
  assertMockPreviewFresh(state, preview, { kind: "image", targetId: fingerprint, action, targetVersion }, operationId);
  return createMockOperation(mockAction, target, reason, undefined, operationId, requestPayload);
}

export async function previewMaintenance(action: "drain_enter" | "reconcile" | "gc", currentResourceVersion?: string): Promise<ActionPreview> {
  if (useRealApi) {
    const targetVersion = currentResourceVersion ?? (await getSystem()).resource_version;
    return realRequest("/system/maintenance/preview", { method: "POST", body: JSON.stringify({ action, target_version: targetVersion, parameters: { apply: true } }) });
  }
  mockStateForRequest();
  assertMockMutationAllowed();
  const state = getMockState();
  return delay(issueMockPreview(state, { kind: "system", targetId: "system", action, targetVersion: state.system.resource_version }, { summary_code: "maintenance_preview", affected_session_count: action === "drain_enter" ? state.overview.task_counts.active : 0, affected_image_references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, changes: [{ code: action, count: null, value: null }] }));
}

export async function submitMaintenance(action: "drain_enter" | "reconcile" | "gc", reason: string, preview: ActionPreview, operationId = createOperationId()): Promise<Operation> {
  if (useRealApi) return realRequest("/system/maintenance", { method: "POST", body: JSON.stringify({ operation_id: operationId, reason, target_version: preview.target_version, action, confirmation_token: preview.confirmation_token, parameters: { apply: true } }) });
  assertMockMutationAllowed();
  const state = getMockState();
  const target = { kind: "system" as const, id: "system" };
  const requestPayload = { action, reason, targetVersion: preview.target_version, confirmationToken: preview.confirmation_token };
  if (state.operations.some((operation) => operation.operation_id === operationId)) return createMockOperation(action, target, reason, undefined, operationId, requestPayload);
  assertMockPreviewFresh(state, preview, { kind: "system", targetId: "system", action, targetVersion: state.system.resource_version }, operationId);
  return createMockOperation(action, target, reason, undefined, operationId, requestPayload);
}

const mockOperationFingerprints = new WeakMap<object, Map<string, string>>();
const mockImageImportRequests = new WeakMap<object, Map<string, ImageImportRequest>>();
type MockPreviewRecord = { kind: "vm" | "image" | "system"; targetId: string; action: string; targetVersion: string; expectedGeneration?: string; parameters?: unknown; expiresAt?: number };
const mockPreviewRecords = new WeakMap<object, Map<string, MockPreviewRecord>>();

function issueMockPreview(state: object, record: MockPreviewRecord, impact: ActionPreview["impact"]): ActionPreview {
  const confirmationToken = `synthetic-confirmation-${createOperationId()}`;
  const expiresAt = Date.now() + 5 * 60_000;
  const previews = mockPreviewRecords.get(state) ?? new Map<string, MockPreviewRecord>();
  mockPreviewRecords.set(state, previews);
  previews.set(confirmationToken, { ...record, parameters: canonicalize(record.parameters), expiresAt });
  return { target_version: record.targetVersion, confirmation_token: confirmationToken, expires_at: new Date(expiresAt).toISOString(), impact };
}

function assertMockPreviewFresh(state: object, preview: ActionPreview, expected: MockPreviewRecord, operationId?: string): void {
  const record = mockPreviewRecords.get(state)?.get(preview.confirmation_token);
  if (!record || (record.expiresAt !== undefined && Date.now() >= record.expiresAt) || record.kind !== expected.kind || record.targetId !== expected.targetId || record.action !== expected.action || record.targetVersion !== expected.targetVersion || record.targetVersion !== preview.target_version || record.expectedGeneration !== expected.expectedGeneration || JSON.stringify(canonicalize(record.parameters)) !== JSON.stringify(canonicalize(expected.parameters))) {
    throw new ManagementRequestError("stale_target", "precondition", operationId ?? null, { current_target_version: expected.targetVersion });
  }
  mockPreviewRecords.get(state)?.delete(preview.confirmation_token);
}

function canonicalize(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value && typeof value === "object") return Object.fromEntries(Object.entries(value).sort(([left], [right]) => left.localeCompare(right)).map(([key, entry]) => [key, canonicalize(entry)]));
  return value;
}

function createMockOperation(action: Operation["action"], target: Operation["target"], reason: string, status?: Operation["status"], operationId = createOperationId(), requestPayload: unknown = { action, target, reason, status }, effectParameters: Record<string, unknown> = {}): Operation {
  const state = getMockState();
  const fingerprint = JSON.stringify(canonicalize(requestPayload));
  const fingerprints = mockOperationFingerprints.get(state) ?? new Map<string, string>();
  mockOperationFingerprints.set(state, fingerprints);
  const existing = state.operations.find((candidate) => candidate.operation_id === operationId);
  if (existing) {
    if (fingerprints.get(operationId) !== fingerprint || existing.action !== action || existing.target.kind !== target.kind || existing.target.id !== target.id || existing.reason !== reason) throw new ManagementRequestError("operation_conflict", "queued", operationId);
    return existing;
  }
  fingerprints.set(operationId, fingerprint);
  const outcome = isDemoRuntime() ? demoForcedOutcome() : "succeeded";
  const finalStatus = status === "running" || status === "awaiting_upload" ? status : status === "queued" && !isDemoRuntime() ? status : outcome;
  const operation: Operation = { operation_id: operationId, action, status: finalStatus, phase: finalStatus === "awaiting_upload" ? "awaiting_upload" : finalStatus === "running" ? "suitability_check" : finalStatus === "succeeded" ? "completed" : finalStatus === "failed" ? "reference_check" : "native_action", target, actor: "admin@example.invalid", reason, created_at: new Date().toISOString(), updated_at: new Date().toISOString(), result: finalStatus === "succeeded" ? { effect: "simulated_effect", affected_session_count: target.kind === "vm" ? 1 : 0 } : null, error: finalStatus === "failed" ? { code: "stale_target", phase: "precondition", retryable: false } : finalStatus === "outcome_unknown" ? { code: "authority_unavailable", phase: "native_action", retryable: true } : null };
  if (finalStatus === "succeeded") {
    if (target.kind === "vm") {
      const vm = state.vms.find((candidate) => candidate.task_id === target.id);
      if (vm && action === "destroy") {
        const previousState = vm.state;
        adjustTaskCount(state, previousState, "absent");
        adjustImageReferenceForVm(state, vm, previousState, "absent", operationId);
        adjustCapacityForVmTransition(state, vm, previousState, "absent");
        vm.state = "absent";
        vm.native_state = "absent";
        vm.guest_availability = "unavailable";
        vm.lease = { state: "absent", expires_at: null };
        vm.allowed_actions = [];
      }
      if (vm && action === "release") {
        const previousState = vm.state;
        adjustTaskCount(state, previousState, "cached");
        adjustImageReferenceForVm(state, vm, previousState, "cached", operationId);
        adjustCapacityForVmTransition(state, vm, previousState, "cached");
        vm.state = "cached";
        vm.native_state = "stopped";
        vm.guest_availability = "unavailable";
        vm.lease = { state: "absent", expires_at: null };
        vm.allowed_actions = ["release", "destroy"];
      }
      if (vm && action === "restart") { vm.native_state = "running"; vm.guest_availability = "ready"; }
      if (vm && action === "resize") {
        const nextResources = { ...vm.resources, cpu_vcpu: typeof effectParameters.cpu_vcpu === "number" ? effectParameters.cpu_vcpu : vm.resources.cpu_vcpu, memory_bytes: typeof effectParameters.memory_bytes === "string" ? effectParameters.memory_bytes : vm.resources.memory_bytes, disk_bytes: typeof effectParameters.disk_bytes === "string" ? effectParameters.disk_bytes : vm.resources.disk_bytes };
        adjustCapacityForResize(state, vm.resources, nextResources);
        vm.resources = nextResources;
      }
      if (vm && action === "port_add" && typeof effectParameters.port_name === "string" && typeof effectParameters.protocol === "string" && typeof effectParameters.host_port === "number" && typeof effectParameters.guest_port === "number") vm.port_mappings = [...vm.port_mappings.filter((port) => port.name !== effectParameters.port_name), { name: effectParameters.port_name, protocol: effectParameters.protocol as "tcp" | "udp", host_port: effectParameters.host_port, guest_port: effectParameters.guest_port }];
      if (vm && action === "port_remove" && typeof effectParameters.port_name === "string") vm.port_mappings = vm.port_mappings.filter((port) => port.name !== effectParameters.port_name);
      if (vm) vm.resource_version = `${vm.resource_version}:demo-${operationId.slice(0, 8)}`;
      if (vm && ["destroy", "release", "resize", "port_add", "port_remove"].includes(action)) state.overview.resource_version = `${state.overview.resource_version}:demo-${operationId.slice(0, 8)}`;
    }
    if (target.kind === "image" && action === "default_image_select") {
      state.images.forEach((image) => {
        const isDefault = image.fingerprint === target.id;
        image.is_default = isDefault;
        image.references = { ...image.references, default: isDefault ? 1 : 0 };
        image.resource_version = advanceMockVersion(image.resource_version, operationId);
      });
      state.image_selection_version = advanceMockVersion(state.image_selection_version, operationId);
      operation.result = { ...operation.result, default_fingerprint: target.id };
    }
    if (target.kind === "image" && action === "image_retire") {
      const image = state.images.find((candidate) => candidate.fingerprint === target.id);
      if (image) {
        image.state = "retired";
        image.resource_version = advanceMockVersion(image.resource_version, operationId);
        state.image_catalog_version = advanceMockVersion(state.image_catalog_version, operationId);
        operation.result = { ...operation.result, image_fingerprint: image.fingerprint };
      }
    }
    if (target.kind === "image" && action === "image_delete") {
      const imageIndex = state.images.findIndex((candidate) => candidate.fingerprint === target.id);
      if (imageIndex >= 0) {
        const [deleted] = state.images.splice(imageIndex, 1);
        state.image_catalog_version = advanceMockVersion(state.image_catalog_version, operationId);
        operation.result = { ...operation.result, image_fingerprint: deleted.fingerprint };
      }
    }
    if (target.kind === "image_catalog" && action === "image_import" && isImageImportRequest(effectParameters)) {
      const imported = applyImageImportEffect(state, effectParameters as unknown as ImageImportRequest, operationId);
      operation.result = { ...operation.result, image_fingerprint: imported?.fingerprint ?? null };
    }
    if (target.kind === "system" && action === "drain_enter") state.system.maintenance.draining = true;
    if (target.kind === "system" && action === "reconcile") state.system.maintenance.reconciliation_pending = 0;
    if (target.kind === "system" && action === "gc") state.system.maintenance.garbage_collection_pending = 0;
    if (target.kind === "system") state.system.resource_version = advanceMockVersion(state.system.resource_version, operationId);
  }
  state.operations.unshift(operation);
  state.audits.unshift({ event_id: createOperationId(), operation_id: operation.operation_id, action, actor: operation.actor, reason, target, outcome: finalStatus === "queued" || finalStatus === "running" || finalStatus === "awaiting_upload" ? "accepted" : finalStatus, phase: operation.phase, occurred_at: operation.updated_at, safe_code: operation.error?.code ?? null });
  emitDemoChange();
  return operation;
}

function adjustTaskCount(state: ReturnType<typeof getMockState>, from: ManagedVM["state"], to: ManagedVM["state"]): void {
  if (from === to) return;
  const counts = state.overview.task_counts as Record<string, number>;
  counts[from] = Math.max(0, (counts[from] ?? 0) - 1);
  counts[to] = (counts[to] ?? 0) + 1;
}

type VMResources = ManagedVM["resources"];

function advanceMockVersion(value: string, operationId: string): string {
  return `${value}:demo-${operationId.slice(0, 8)}`;
}

type ImageReferenceKey = keyof ManagedImage["references"];

function imageReferenceKeyForVmState(state: ManagedVM["state"]): ImageReferenceKey | null {
  if (state === "active") return "active";
  if (state === "cached") return "cached";
  if (state === "preparing") return "pending_creation";
  if (state === "closing" || state === "deleting" || state === "failed") return "releasing";
  return null;
}

function adjustImageReferenceForVm(state: ReturnType<typeof getMockState>, vm: ManagedVM, from: ManagedVM["state"], to: ManagedVM["state"], operationId: string): void {
  const image = state.images.find((candidate) => candidate.fingerprint === vm.image.fingerprint);
  const fromKey = imageReferenceKeyForVmState(from);
  const toKey = imageReferenceKeyForVmState(to);
  if (!image || fromKey === toKey) return;
  const references = { ...image.references };
  if (fromKey) references[fromKey] = Math.max(0, references[fromKey] - 1);
  if (toKey) references[toKey] += 1;
  image.references = references;
  image.resource_version = advanceMockVersion(image.resource_version, operationId);
  state.image_catalog_version = advanceMockVersion(state.image_catalog_version, operationId);
}

type CapacityContribution = { cpu: number; memory: bigint; disk: bigint };

function capacityContribution(vm: ManagedVM, state: ManagedVM["state"]): CapacityContribution {
  const cpu = typeof vm.resources.cpu_vcpu === "number" ? vm.resources.cpu_vcpu : 0;
  const memory = parseBytes(vm.resources.memory_bytes);
  const disk = parseBytes(vm.resources.disk_bytes);
  if (state === "absent") return { cpu: 0, memory: 0n, disk: 0n };
  if (state === "cached") return { cpu: 0, memory: 0n, disk };
  return { cpu, memory, disk };
}

function parseBytes(value: string | null): bigint {
  if (value === null) return 0n;
  try {
    return BigInt(value);
  } catch {
    return 0n;
  }
}

function adjustCapacityForVmTransition(state: ReturnType<typeof getMockState>, vm: ManagedVM, from: ManagedVM["state"], to: ManagedVM["state"]): void {
  const previous = capacityContribution(vm, from);
  const next = capacityContribution(vm, to);
  const cpuDelta = previous.cpu - next.cpu;
  if (state.overview.capacity.logical_cpu_remaining_vcpu.state === "available" && state.overview.capacity.logical_cpu_remaining_vcpu.value !== null) {
    state.overview.capacity.logical_cpu_remaining_vcpu = { ...state.overview.capacity.logical_cpu_remaining_vcpu, value: Math.max(0, state.overview.capacity.logical_cpu_remaining_vcpu.value + cpuDelta), observed_at: state.overview.observed_at };
  }
  const memoryDelta = previous.memory - next.memory;
  const diskDelta = previous.disk - next.disk;
  state.overview.capacity.logical_memory_remaining = adjustObservedBytes(state.overview.capacity.logical_memory_remaining, -memoryDelta, state.overview.observed_at);
  state.overview.capacity.ct_memory_available = adjustObservedBytes(state.overview.capacity.ct_memory_available, -memoryDelta, state.overview.observed_at);
  state.overview.capacity.physical_disk_available = adjustObservedBytes(state.overview.capacity.physical_disk_available, -diskDelta, state.overview.observed_at);
}

function adjustCapacityForResize(state: ReturnType<typeof getMockState>, previous: VMResources, next: VMResources): void {
  const cpuDelta = typeof previous.cpu_vcpu === "number" && typeof next.cpu_vcpu === "number" ? next.cpu_vcpu - previous.cpu_vcpu : null;
  if (cpuDelta !== null && state.overview.capacity.logical_cpu_remaining_vcpu.state === "available" && state.overview.capacity.logical_cpu_remaining_vcpu.value !== null) {
    state.overview.capacity.logical_cpu_remaining_vcpu = { ...state.overview.capacity.logical_cpu_remaining_vcpu, value: Math.max(0, state.overview.capacity.logical_cpu_remaining_vcpu.value - cpuDelta), observed_at: state.overview.observed_at };
  }
  const memoryDelta = byteDelta(previous.memory_bytes, next.memory_bytes);
  if (memoryDelta !== null) {
    state.overview.capacity.logical_memory_remaining = adjustObservedBytes(state.overview.capacity.logical_memory_remaining, memoryDelta, state.overview.observed_at);
    state.overview.capacity.ct_memory_available = adjustObservedBytes(state.overview.capacity.ct_memory_available, memoryDelta, state.overview.observed_at);
  }
  const diskDelta = byteDelta(previous.disk_bytes, next.disk_bytes);
  if (diskDelta !== null) state.overview.capacity.physical_disk_available = adjustObservedBytes(state.overview.capacity.physical_disk_available, diskDelta, state.overview.observed_at);
}

function byteDelta(previous: string | null, next: string | null): bigint | null {
  if (previous === null || next === null) return null;
  try {
    return BigInt(next) - BigInt(previous);
  } catch {
    return null;
  }
}

function adjustObservedBytes(observed: Overview["capacity"]["logical_memory_remaining"], delta: bigint, observedAt: string): Overview["capacity"]["logical_memory_remaining"] {
  if (observed.state !== "available" || observed.value_bytes === null) return observed;
  try {
    const next = BigInt(observed.value_bytes) - delta;
    return { ...observed, value_bytes: (next < 0n ? 0n : next).toString(), observed_at: observedAt };
  } catch {
    return observed;
  }
}

function isImageImportRequest(value: Record<string, unknown>): boolean {
  return typeof value === "object" && value !== null && "operation_id" in value && "source" in value;
}

function applyImageImportEffect(state: ReturnType<typeof getMockState>, request: ImageImportRequest, operationId: string): ManagedImage | null {
  const fingerprint = request.source.kind === "trusted_remote"
    ? request.source.expected_fingerprint
    : request.source.format === "unified"
      ? request.source.expected_sha256
      : request.source.metadata.expected_sha256;
  if (!fingerprint) return null;
  const existing = state.images.find((image) => image.fingerprint === fingerprint);
  if (existing) return existing;
  const sourceKind = request.source.kind === "trusted_remote" ? "trusted_remote" : "local_upload";
  const image: ManagedImage = {
    fingerprint,
    resource_version: `image:${fingerprint.slice(0, 4)}:${operationId.slice(0, 8)}`,
    version_label: request.version_label ?? `import-${fingerprint.slice(0, 8)}`,
    architecture: "x86_64",
    instance_type: "container",
    source: { kind: sourceKind, source_id: request.source.kind === "trusted_remote" ? request.source.source_id : null },
    state: "available",
    verification: { integrity: "passed", suitability: "passed", findings: [] },
    is_default: false,
    default_compatibility: "none",
    references: { ...emptyReferences },
    created_at: new Date().toISOString(),
  };
  state.images.unshift(image);
  state.image_catalog_version = advanceMockVersion(state.image_catalog_version, operationId);
  return image;
}

function continueMockUploadOperation(operationId: string): Operation {
  const state = getMockState();
  const operation = state.operations.find((candidate) => candidate.operation_id === operationId);
  if (!operation) throw new ManagementRequestError("not_found", "awaiting_upload", operationId);
  if (operation.action !== "image_import" || operation.target.kind !== "image_catalog" || operation.status !== "awaiting_upload") throw new ManagementRequestError("operation_conflict", "awaiting_upload", operationId);
  const outcome = isDemoRuntime() ? demoForcedOutcome() : "succeeded";
  const updatedAt = new Date().toISOString();
  operation.status = outcome;
  operation.phase = outcome === "succeeded" ? "completed" : outcome === "failed" ? "reference_check" : "native_action";
  operation.updated_at = updatedAt;
  operation.result = outcome === "succeeded" ? { effect: "simulated_effect", affected_session_count: 0 } : null;
  operation.error = outcome === "failed" ? { code: "stale_target", phase: "precondition", retryable: false } : outcome === "outcome_unknown" ? { code: "authority_unavailable", phase: "native_action", retryable: true } : null;
  if (outcome === "succeeded") {
    const request = mockImageImportRequests.get(state)?.get(operationId);
    const imported = request ? applyImageImportEffect(state, request, operationId) : null;
    operation.result = { ...operation.result, image_fingerprint: imported?.fingerprint ?? null };
  }
  state.audits.unshift({ event_id: createOperationId(), operation_id: operation.operation_id, action: operation.action, actor: operation.actor, reason: operation.reason, target: operation.target, outcome, phase: operation.phase, occurred_at: updatedAt, safe_code: operation.error?.code ?? null });
  emitDemoChange();
  return operation;
}
