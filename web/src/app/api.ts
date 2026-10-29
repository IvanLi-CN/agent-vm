import { mockState } from "./mockData";
import type { ActionPreview, AgentToken, AuditEvent, ImageImportRequest, ManagedImage, ManagedVM, ManagementSession, Operation, Overview, SystemStatus, TokenCreateResult, TokenPage, TokenRevokePreview, TokenScope } from "./types";

const useRealApi = import.meta.env.VITE_MANAGEMENT_API === "real";

type RealRequestOptions = { contentType?: string | null };

export class ManagementRequestError extends Error {
  readonly code: string;
  readonly phase: string | null;
  readonly operationId: string | null;
  readonly safeFields: Record<string, unknown> | null;
  readonly retryable: boolean;

  constructor(code: string, phase: string | null = null, operationId: string | null = null, safeFields: Record<string, unknown> | null = null, retryable = false) {
    super(code);
    this.name = "ManagementRequestError";
    this.code = code;
    this.phase = phase;
    this.operationId = operationId;
    this.safeFields = safeFields;
    this.retryable = retryable;
  }
}

let csrfToken: string | null = null;
let csrfExpiresAt = 0;
let managementSession: ManagementSession | null = null;
let sessionPromise: Promise<ManagementSession> | null = null;
const mockTokenCreates = new Map<string, { metadata: AgentToken; request: string }>();
const mockTokenRevocations = new Map<string, { operation: Operation; request: string }>();
const mockTokenRevocationPreviews = new Map<string, { tokenId: string; targetVersion: string; expiresAt: string }>();

function cloneTokenMetadata(token: AgentToken): AgentToken {
  return { ...token, scopes: [...token.scopes] };
}

function cloneOperation(operation: Operation): Operation {
  return {
    ...operation,
    actor: { ...operation.actor },
    target: { ...operation.target },
    result: operation.result ? { ...operation.result, token_metadata: operation.result.token_metadata ? cloneTokenMetadata(operation.result.token_metadata) : operation.result.token_metadata } : null,
    error: operation.error ? { ...operation.error, safe_fields: operation.error.safe_fields ? { ...operation.error.safe_fields } : operation.error.safe_fields } : null,
  };
}

export function createOperationId(): string {
  const cryptoApi = typeof globalThis.crypto !== "undefined" ? globalThis.crypto : null;
  if (cryptoApi?.randomUUID) return cryptoApi.randomUUID();
  const bytes = new Uint8Array(16);
  if (cryptoApi?.getRandomValues) cryptoApi.getRandomValues(bytes);
  else bytes.forEach((_, index) => { bytes[index] = Math.floor(Math.random() * 256); });
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
  let body: (T & { error?: { code?: string; phase?: string; operation_id?: string; retryable?: boolean; safe_fields?: Record<string, unknown> } }) | null = null;
  if (raw) {
    try {
      body = JSON.parse(raw) as T & { error?: { code?: string; phase?: string; operation_id?: string; retryable?: boolean; safe_fields?: Record<string, unknown> } };
    } catch {
      body = null;
    }
  }
  throw new ManagementRequestError(body?.error?.code ?? "internal_error", body?.error?.phase ?? null, body?.error?.operation_id ?? null, body?.error?.safe_fields ?? null, body?.error?.retryable === true);
}

function sessionIsFresh(): boolean {
  return Boolean(csrfToken && managementSession && Date.now() < csrfExpiresAt - 5_000);
}

async function loadManagementSession(): Promise<ManagementSession> {
  if (sessionIsFresh() && managementSession) return managementSession;
  csrfToken = null;
  managementSession = null;
  if (!sessionPromise) {
    sessionPromise = requestJSON<ManagementSession>("/session", undefined)
      .then((session) => {
        if (!session.csrf_token) throw new Error("authentication_required");
        managementSession = session;
        csrfToken = session.csrf_token;
        csrfExpiresAt = session.expires_at ? Date.parse(session.expires_at) : Date.now() + 14 * 60_000;
        return session;
      })
      .finally(() => {
        sessionPromise = null;
      });
  }
  return sessionPromise;
}

async function ensureSession(): Promise<string> {
  if (sessionIsFresh() && csrfToken) return csrfToken;
  csrfToken = null;
  managementSession = null;
  return (await loadManagementSession()).csrf_token;
}

async function realRequest<T>(path: string, init?: RequestInit, options: RealRequestOptions = {}): Promise<T> {
  const method = (init?.method ?? "GET").toUpperCase();
  if (path === "/session" && method === "GET") return loadManagementSession() as Promise<T>;
  const headers = new Headers(init?.headers);
  if (method !== "GET" && method !== "HEAD") headers.set("X-CSRF-Token", await ensureSession());
  else await ensureSession();
  try {
    const result = await requestJSON<T>(path, { ...init, headers }, options);
    return result;
  } catch (error) {
    if (error instanceof ManagementRequestError && (error.code === "authentication_required" || error.code === "csrf_invalid")) {
      csrfToken = null;
      csrfExpiresAt = 0;
      managementSession = null;
    }
    throw error;
  }
}

function delay<T>(value: T): Promise<T> {
  return new Promise((resolve) => window.setTimeout(() => resolve(value), 120));
}

export async function getManagementSession(): Promise<ManagementSession> {
  if (useRealApi) return realRequest<ManagementSession>("/session");
  return delay(mockState.session);
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

export async function listTokens(cursor?: string): Promise<TokenPage> {
  if (useRealApi) return realRequest<TokenPage>(`/tokens?limit=25${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
  const offset = cursor ? Number(/^mock-token:(\d+)$/.exec(cursor)?.[1] ?? NaN) : 0;
  if (!Number.isSafeInteger(offset) || offset < 0) throw new ManagementRequestError("invalid_request", "validation");
  const limit = 25;
  const pageItems = mockState.tokens.slice(offset, offset + limit).map(cloneTokenMetadata);
  const nextOffset = offset + pageItems.length;
  return delay({ items: pageItems, page: { limit, next_cursor: nextOffset < mockState.tokens.length ? `mock-token:${nextOffset}` : null } });
}

export async function getOperation(operationId: string): Promise<Operation> {
  if (useRealApi) return realRequest<Operation>(`/operations/${encodeURIComponent(operationId)}`);
  const operation = mockState.operations.find((candidate) => candidate.operation_id === operationId);
  if (!operation) throw new ManagementRequestError("not_found", "reconciliation", operationId);
  return delay(cloneOperation(operation));
}

export async function createAgentToken(scopes: TokenScope[], expiresAt: string, reason?: string, operationId = createOperationId()): Promise<TokenCreateResult> {
  const body = { operation_id: operationId, scopes, expires_at: expiresAt, ...(reason?.trim() ? { reason: reason.trim() } : {}) };
  if (useRealApi) return realRequest<TokenCreateResult>("/tokens", { method: "POST", body: JSON.stringify(body) });
  const request = JSON.stringify({ scopes: [...scopes].sort(), expires_at: expiresAt, reason: reason?.trim() || null });
  const previous = mockTokenCreates.get(operationId);
  if (previous) {
    if (previous.request !== request) throw new ManagementRequestError("operation_conflict", "precondition", operationId);
    return delay({ operation_id: operationId, metadata: cloneTokenMetadata(previous.metadata), plaintext_disclosure: "consumed" });
  }
  const tokenId = `tok_${createOperationId().replaceAll("-", "").slice(0, 24)}`;
  const metadata: AgentToken = { token_id: tokenId, resource_version: `token:${tokenId}:revision-1`, scopes: [...scopes], created_at: new Date().toISOString(), expires_at: expiresAt, revoked_at: null };
  const storedMetadata = cloneTokenMetadata(metadata);
  mockState.tokens.unshift(storedMetadata);
  mockTokenCreates.set(operationId, { metadata: cloneTokenMetadata(storedMetadata), request });
  const plaintext = `${createOperationId().replaceAll("-", "")}${createOperationId().replaceAll("-", "")}`;
  const operation = createMockOperation("token_create", { kind: "token", id: tokenId }, reason?.trim() || null, "succeeded", operationId);
  operation.result = { token_metadata: cloneTokenMetadata(storedMetadata), plaintext_disclosure: "returned" };
  createMockAudit(operation);
  return delay({ operation_id: operationId, token: plaintext, metadata: cloneTokenMetadata(storedMetadata), plaintext_disclosure: "returned" });
}

export async function previewTokenRevocation(tokenId: string, targetVersion: string): Promise<TokenRevokePreview> {
  if (useRealApi) return realRequest<TokenRevokePreview>(`/tokens/${encodeURIComponent(tokenId)}/revoke/preview`, { method: "POST", body: JSON.stringify({ target_version: targetVersion }) });
  const token = mockState.tokens.find((candidate) => candidate.token_id === tokenId);
  if (!token || token.resource_version !== targetVersion) throw new ManagementRequestError("stale_target", "precondition");
  const confirmationToken = `synthetic-confirmation-${createOperationId()}`;
  const expiresAt = new Date(Date.now() + 5 * 60 * 1000).toISOString();
  mockTokenRevocationPreviews.set(confirmationToken, { tokenId: token.token_id, targetVersion: token.resource_version, expiresAt });
  return delay({ token_id: token.token_id, target_version: token.resource_version, confirmation_token: confirmationToken, expires_at: expiresAt, metadata: { ...token, scopes: [...token.scopes] } });
}

export async function submitTokenRevocation(tokenId: string, reason: string | undefined, preview: TokenRevokePreview, operationId = createOperationId()): Promise<Operation> {
  const normalizedReason = reason?.trim() ?? "";
  const body = { operation_id: operationId, target_version: preview.target_version, confirmation_token: preview.confirmation_token, ...(normalizedReason ? { reason: normalizedReason } : {}) };
  if (useRealApi) return realRequest<Operation>(`/tokens/${encodeURIComponent(tokenId)}/revoke`, { method: "POST", body: JSON.stringify(body) });
  const request = JSON.stringify({ token_id: tokenId, target_version: preview.target_version, confirmation_token: preview.confirmation_token, reason: normalizedReason || null });
  const previous = mockTokenRevocations.get(operationId);
  if (previous) {
    if (previous.request !== request) throw new ManagementRequestError("operation_conflict", "precondition", operationId);
    return delay(cloneOperation(previous.operation));
  }
  const token = mockState.tokens.find((candidate) => candidate.token_id === tokenId);
  const previewBinding = mockTokenRevocationPreviews.get(preview.confirmation_token);
  if (!token || preview.token_id !== tokenId || !previewBinding || previewBinding.tokenId !== tokenId || previewBinding.targetVersion !== preview.target_version || token.resource_version !== preview.target_version) throw new ManagementRequestError("stale_target", "precondition");
  if (Date.parse(previewBinding.expiresAt) <= Date.now()) throw new ManagementRequestError("precondition_required", "precondition");
  if (token && !token.revoked_at) {
    token.revoked_at = new Date().toISOString();
    token.resource_version = `${token.resource_version}:revoked`;
  }
  const operation = createMockOperation("token_revoke", { kind: "token", id: tokenId }, normalizedReason || null, "succeeded", operationId);
  mockTokenRevocations.set(operationId, { operation, request });
  mockTokenRevocationPreviews.delete(preview.confirmation_token);
  createMockAudit(operation);
  return delay(cloneOperation(operation));
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
  if (useRealApi) return realRequest(`/vms/${encodeURIComponent(taskId)}/actions`, { method: "POST", body: JSON.stringify({ operation_id: operationId, ...(reason.trim() ? { reason: reason.trim() } : {}), target_version: preview.target_version, action, expected_generation: expectedGeneration, parameters, confirmation_token: preview.confirmation_token }) });
  return createMockOperation(action as Operation["action"], { kind: "vm", id: taskId }, reason, "queued", operationId);
}

export async function startImageImport(request: ImageImportRequest): Promise<Operation> {
  if (useRealApi) return realRequest("/images/imports", { method: "POST", body: JSON.stringify(request) });
  return createMockOperation("image_import", { kind: "image_catalog", id: "image-catalog" }, request.reason, request.source.kind === "local_upload" ? "awaiting_upload" : "running", request.operation_id);
}

export async function uploadImageImportContent(operationId: string, content: File | { metadata: File; rootfs: File }): Promise<Operation> {
  if (!useRealApi) return createMockOperation("image_import", { kind: "image_catalog", id: "image-catalog" }, "upload image content", "running", operationId);
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
      ? { operation_id: operationId, ...(reason.trim() ? { reason: reason.trim() } : {}), target_version: preview.target_version, fingerprint, confirmation_token: preview.confirmation_token }
      : { operation_id: operationId, ...(reason.trim() ? { reason: reason.trim() } : {}), target_version: preview.target_version, confirmation_token: preview.confirmation_token };
    return realRequest(path, { method: "POST", body: JSON.stringify(body) });
  }
  return createMockOperation(action === "default" ? "default_image_select" : action === "delete" ? "image_delete" : "image_retire", { kind: "image", id: fingerprint }, reason, "succeeded", operationId);
}

export async function previewMaintenance(action: "drain_enter" | "reconcile" | "gc", currentResourceVersion?: string): Promise<ActionPreview> {
  if (useRealApi) {
    const targetVersion = currentResourceVersion ?? (await getSystem()).resource_version;
    return realRequest("/system/maintenance/preview", { method: "POST", body: JSON.stringify({ action, target_version: targetVersion, parameters: { apply: true } }) });
  }
  return delay({ target_version: mockState.system.resource_version, confirmation_token: `synthetic-confirmation-${createOperationId()}`, expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: "maintenance_preview", affected_session_count: action === "drain_enter" ? mockState.overview.task_counts.active : 0, affected_image_references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, changes: [{ code: action, count: null, value: null }] } });
}

export async function submitMaintenance(action: "drain_enter" | "reconcile" | "gc", reason: string, preview: ActionPreview, operationId = createOperationId()): Promise<Operation> {
  if (useRealApi) return realRequest("/system/maintenance", { method: "POST", body: JSON.stringify({ operation_id: operationId, ...(reason.trim() ? { reason: reason.trim() } : {}), target_version: preview.target_version, action, confirmation_token: preview.confirmation_token, parameters: { apply: true } }) });
  return createMockOperation(action, { kind: "system", id: "system" }, reason, "succeeded", operationId);
}

function createMockOperation(action: Operation["action"], target: Operation["target"], reason: string | null | undefined, status: Operation["status"] = "queued", operationId = createOperationId()): Operation {
  const operation: Operation = { operation_id: operationId, action, status, phase: status === "awaiting_upload" ? "awaiting_upload" : status === "running" ? "suitability_check" : status === "succeeded" ? "completed" : "queued", target, actor: { issuer: mockState.session.issuer, subject: mockState.session.subject, role: "administrator" }, reason: reason?.trim() || null, created_at: new Date().toISOString(), updated_at: new Date().toISOString(), result: null, error: null };
  mockState.operations.unshift(operation);
  return operation;
}

function createMockAudit(operation: Operation): void {
  mockState.audits.unshift({ event_id: createOperationId(), operation_id: operation.operation_id, action: operation.action, actor: operation.actor, reason: operation.reason, target: operation.target, outcome: operation.status === "succeeded" ? "succeeded" : operation.status === "failed" ? "failed" : operation.status === "outcome_unknown" ? "outcome_unknown" : "accepted", phase: operation.phase, occurred_at: operation.updated_at, safe_code: operation.error?.code ?? null });
}
