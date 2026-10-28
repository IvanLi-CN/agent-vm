import { mockState } from "./mockData";
import type { ActionPreview, AuditEvent, ImageImportRequest, ManagedImage, ManagedVM, Operation, Overview, SystemStatus } from "./types";

const useRealApi = import.meta.env.VITE_MANAGEMENT_API === "real";

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
  return typeof crypto !== "undefined" && "randomUUID" in crypto ? crypto.randomUUID() : `synthetic-${Date.now()}`;
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
  return new Promise((resolve) => window.setTimeout(() => resolve(value), 120));
}

export async function getOverview(): Promise<Overview> {
  return useRealApi ? realRequest<Overview>("/overview") : delay(mockState.overview);
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
  const items = mockState.vms.filter((vm) => (!normalized || `${vm.task_id} ${vm.session_id}`.toLowerCase().includes(normalized)) && (!filters.state || filters.state === "all" || vm.state === filters.state) && (!filters.leaseState || filters.leaseState === "all" || vm.lease.state === filters.leaseState));
  return delay({ items, page: { limit: 25, next_cursor: null } });
}

export async function getVM(taskId: string): Promise<ManagedVM> {
  return useRealApi ? realRequest<ManagedVM>(`/vms/${encodeURIComponent(taskId)}`) : delay(mockState.vms.find((vm) => vm.task_id === taskId) ?? mockState.vms[0]);
}

export async function listImages(query = "", cursor?: string): Promise<{ items: ManagedImage[]; page: { limit: number; next_cursor: string | null }; default_fingerprint: string | null; selection_version: string; catalog_version: string }> {
  if (useRealApi) return realRequest(`/images?limit=25${query ? `&q=${encodeURIComponent(query)}` : ""}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
  const normalized = query.toLowerCase();
  const items = mockState.images.filter((image) => !normalized || `${image.version_label} ${image.fingerprint}`.toLowerCase().includes(normalized));
  return delay({ items, page: { limit: 25, next_cursor: null }, default_fingerprint: mockState.images.find((image) => image.is_default)?.fingerprint ?? null, selection_version: "image-selection:revision-12", catalog_version: "image-catalog:revision-12" });
}

export async function listOperations(status = "all", cursor?: string): Promise<{ items: Operation[]; page: { limit: number; next_cursor: string | null } }> {
  if (useRealApi) return realRequest(`/operations?limit=25${status !== "all" ? `&status=${status}` : ""}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
  return delay({ items: mockState.operations.filter((operation) => status === "all" || operation.status === status), page: { limit: 25, next_cursor: null } });
}

export async function listAudit(cursor?: string): Promise<{ items: AuditEvent[]; page: { limit: number; next_cursor: string | null } }> {
  return useRealApi ? realRequest(`/audit?limit=25${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`) : delay({ items: mockState.audits, page: { limit: 25, next_cursor: null } });
}

export async function getSystem(): Promise<SystemStatus> {
  return useRealApi ? realRequest<SystemStatus>("/system") : delay(mockState.system);
}

export async function previewVMAction(taskId: string, action: string, current?: Pick<ManagedVM, "generation" | "resource_version">, parameters: Record<string, unknown> = {}): Promise<ActionPreview> {
  if (useRealApi) {
    const target = current ?? await getVM(taskId);
    return realRequest(`/vms/${encodeURIComponent(taskId)}/actions/preview`, { method: "POST", body: JSON.stringify({ action, expected_generation: target.generation, target_version: target.resource_version, parameters }) });
  }
  const vm = mockState.vms.find((candidate) => candidate.task_id === taskId) ?? mockState.vms[0];
  return delay({ target_version: vm.resource_version, confirmation_token: `synthetic-confirmation-${createOperationId()}`, expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: vm.lease.state === "effective" ? "effective_lease_affected" : "no_active_lease", affected_session_count: vm.lease.state === "effective" ? 1 : 0, affected_image_references: vm.image.fingerprint === mockState.images[0].fingerprint ? mockState.images[0].references : { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, changes: [{ code: action === "destroy" ? "session_access_revoked" : "native_action", count: 1, value: null }] } });
}

export async function submitVMAction(taskId: string, action: string, reason: string, preview: ActionPreview, expectedGeneration: string, parameters: Record<string, unknown> = {}, operationId = createOperationId()): Promise<Operation> {
  if (useRealApi) return realRequest(`/vms/${encodeURIComponent(taskId)}/actions`, { method: "POST", body: JSON.stringify({ operation_id: operationId, reason, target_version: preview.target_version, action, expected_generation: expectedGeneration, parameters, confirmation_token: preview.confirmation_token }) });
  return createMockOperation(action as Operation["action"], { kind: "vm", id: taskId }, reason);
}

export async function startImageImport(request: ImageImportRequest): Promise<Operation> {
  if (useRealApi) return realRequest("/images/imports", { method: "POST", body: JSON.stringify(request) });
  return createMockOperation("image_import", { kind: "image_catalog", id: "image-catalog" }, request.reason, request.source.kind === "local_upload" ? "awaiting_upload" : "running");
}

export async function uploadImageImportContent(operationId: string, content: File | { metadata: File; rootfs: File }): Promise<Operation> {
  if (!useRealApi) return createMockOperation("image_import", { kind: "image_catalog", id: "image-catalog" }, "upload image content", "running");
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
  const image = mockState.images.find((candidate) => candidate.fingerprint === fingerprint) ?? mockState.images[0];
  return delay({ target_version: image.resource_version, confirmation_token: `synthetic-confirmation-${createOperationId()}`, expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: action === "delete" && image.references.active + image.references.cached > 0 ? "image_referenced" : "image_change_preview", affected_session_count: image.references.active, affected_image_references: image.references, changes: [{ code: action === "default" ? "new_sessions_use_image" : action === "retire" ? "new_selection_blocked" : "image_removed", count: null, value: image.version_label }] } });
}

export async function submitImageAction(fingerprint: string, action: "default" | "delete" | "retire", reason: string, preview: ActionPreview, operationId = createOperationId()): Promise<Operation> {
  if (useRealApi) {
    const path = action === "default" ? "/images/default" : `/images/${fingerprint}/${action === "delete" ? "delete" : "retire"}`;
    const body = action === "default"
      ? { operation_id: operationId, reason, target_version: preview.target_version, fingerprint, confirmation_token: preview.confirmation_token }
      : { operation_id: operationId, reason, target_version: preview.target_version, confirmation_token: preview.confirmation_token };
    return realRequest(path, { method: "POST", body: JSON.stringify(body) });
  }
  return createMockOperation(action === "default" ? "default_image_select" : action === "delete" ? "image_delete" : "image_retire", { kind: "image", id: fingerprint }, reason);
}

export async function previewMaintenance(action: "drain_enter" | "reconcile" | "gc", currentResourceVersion?: string): Promise<ActionPreview> {
  if (useRealApi) {
    const targetVersion = currentResourceVersion ?? (await getSystem()).resource_version;
    return realRequest("/system/maintenance/preview", { method: "POST", body: JSON.stringify({ action, target_version: targetVersion, parameters: { apply: true } }) });
  }
  return delay({ target_version: mockState.system.resource_version, confirmation_token: `synthetic-confirmation-${createOperationId()}`, expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: "maintenance_preview", affected_session_count: action === "drain_enter" ? mockState.overview.task_counts.active : 0, affected_image_references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, changes: [{ code: action, count: null, value: null }] } });
}

export async function submitMaintenance(action: "drain_enter" | "reconcile" | "gc", reason: string, preview: ActionPreview, operationId = createOperationId()): Promise<Operation> {
  if (useRealApi) return realRequest("/system/maintenance", { method: "POST", body: JSON.stringify({ operation_id: operationId, reason, target_version: preview.target_version, action, confirmation_token: preview.confirmation_token, parameters: { apply: true } }) });
  return createMockOperation(action, { kind: "system", id: "system" }, reason);
}

function createMockOperation(action: Operation["action"], target: Operation["target"], reason: string, status: Operation["status"] = "queued"): Operation {
  const operation: Operation = { operation_id: createOperationId(), action, status, phase: status === "awaiting_upload" ? "awaiting_upload" : status === "running" ? "suitability_check" : "queued", target, actor: "admin@example.invalid", reason, created_at: new Date().toISOString(), updated_at: new Date().toISOString(), result: null, error: null };
  mockState.operations.unshift(operation);
  return operation;
}
