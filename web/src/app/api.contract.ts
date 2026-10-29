import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import i18n from "./i18n";
import { auditReasonCharacterLength, auditReasonWithinLimit } from "./validation";

const managementErrorCodes = [
  "authentication_required", "oidc_unavailable", "administrator_required", "csrf_invalid", "origin_invalid", "invalid_request", "invalid_state", "stale_target", "operation_conflict", "not_found", "unsupported_action", "permission_denied", "token_expired", "token_revoked", "source_not_allowed", "upload_too_large", "staging_capacity_low", "fingerprint_mismatch", "image_not_applicable", "image_referenced", "default_image_required", "capacity_unverified", "admission_denied", "precondition_required", "service_unavailable", "state_unavailable", "rate_limited", "lease_expired", "task_not_active", "draining", "mapping_unverified", "cleanup_incomplete", "resource_unverified", "startup_reconcile_required", "internal_error",
] as const;

function response(body: unknown, status = 200): Response {
  return { ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(body), json: async () => body } as Response;
}

describe("real management API client", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.stubEnv("VITE_MANAGEMENT_API", "real");
  });

  afterEach(() => {
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
  });

  it("bootstraps the management session and sends the live VM versions", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token-0123456789abcdef0123456789abcdef" }))
      .mockResolvedValueOnce(response({ target_version: "vm:task:7", confirmation_token: "confirm", expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: "native_action", affected_session_count: 0, affected_image_references: {}, changes: [] } }));
    vi.stubGlobal("fetch", fetchMock);
    const { previewVMAction } = await import("./api");

    await previewVMAction("task", "restart", { generation: "7", resource_version: "vm:task:7" });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/session");
    const request = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(new Headers(request.headers).get("X-CSRF-Token")).toBe("csrf-token-0123456789abcdef0123456789abcdef");
    expect(JSON.parse(String(request.body))).toMatchObject({ expected_generation: "7", target_version: "vm:task:7" });
  });

  it("shares one session bootstrap and preserves retryable error metadata", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token-0123456789abcdef0123456789abcdef", expires_at: "2027-01-15T11:20:30Z", issuer: "issuer", subject: "subject", role: "administrator", capabilities: ["read"] }))
      .mockResolvedValueOnce(response({ error: { code: "authority_unavailable", phase: "reconciliation", retryable: true } }, 503));
    vi.stubGlobal("fetch", fetchMock);
    const { getManagementSession, getOverview } = await import("./api");

    const session = getManagementSession();
    const overview = getOverview();
    await expect(session).resolves.toMatchObject({ csrf_token: "csrf-token-0123456789abcdef0123456789abcdef" });
    await expect(overview).rejects.toMatchObject({ code: "authority_unavailable", retryable: true });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/session");
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/overview");
  });

  it("streams a unified package without overriding its content type", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token-0123456789abcdef0123456789abcdef" }))
      .mockResolvedValueOnce(response({ operation_id: "op-1" }));
    vi.stubGlobal("fetch", fetchMock);
    const { uploadImageImportContent } = await import("./api");
    const file = new File(["image-bytes"], "image.tar", { type: "application/octet-stream" });

    await uploadImageImportContent("op-1", file);

    expect(fetchMock).toHaveBeenCalledTimes(2);
    const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(url).toBe("/api/v1/images/imports/op-1/content");
    expect(init.method).toBe("PUT");
    expect(new Headers(init.headers).get("X-CSRF-Token")).toBe("csrf-token-0123456789abcdef0123456789abcdef");
    expect(new Headers(init.headers).get("Content-Type")).toBe("application/octet-stream");
    expect(init.body).toBe(file);
  });

  it("omits an optional reason when creating a Token", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token-0123456789abcdef0123456789abcdef", expires_at: "2027-01-15T11:20:30Z" }))
      .mockResolvedValueOnce(response({ operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb126", token: "bearer-contract-0123456789abcdef0123456789", metadata: { token_id: "tok_contract_01_7d9e22b1", resource_version: "token:tok_contract_01_7d9e22b1:1", scopes: ["agent:read"], created_at: "2026-10-29T00:00:00Z", expires_at: "2027-01-15T00:00:00Z", revoked_at: null }, plaintext_disclosure: "returned" }));
    vi.stubGlobal("fetch", fetchMock);
    const { createAgentToken } = await import("./api");

    await createAgentToken(["agent:read"], "2027-01-15T00:00:00.000Z", "   ", "50e8400e-ccf8-4c7b-8d1d-14f66c7fb126");

    const request = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(JSON.parse(String(request.body))).toEqual({ operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb126", scopes: ["agent:read"], expires_at: "2027-01-15T00:00:00.000Z" });
  });

  it("preserves non-empty Token reasons exactly as entered", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token-0123456789abcdef0123456789abcdef", expires_at: "2027-01-15T11:20:30Z" }))
      .mockResolvedValueOnce(response({ operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb140", token: "bearer-contract-0123456789abcdef0123456789", metadata: { token_id: "tok_contract_01_7d9e22b1", resource_version: "token:tok_contract_01_7d9e22b1:1", scopes: ["agent:read"], created_at: "2026-10-29T00:00:00Z", expires_at: "2027-01-15T00:00:00Z", revoked_at: null }, plaintext_disclosure: "returned" }));
    vi.stubGlobal("fetch", fetchMock);
    const { createAgentToken } = await import("./api");
    const reason = "  preserve this reason  ";

    await createAgentToken(["agent:read"], "2027-01-15T00:00:00.000Z", reason, "50e8400e-ccf8-4c7b-8d1d-14f66c7fb140");

    const request = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(JSON.parse(String(request.body))).toMatchObject({ reason });
  });

  it("keeps Token revocation preview and confirmation separate", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token-0123456789abcdef0123456789abcdef", expires_at: "2027-01-15T11:20:30Z" }))
      .mockResolvedValueOnce(response({ token_id: "tok_contract_01_7d9e22b1", target_version: "token:tok_contract_01_7d9e22b1:1", confirmation_token: "confirmation-contract-0123456789abcdef", expires_at: "2026-10-29T00:05:00Z", metadata: { token_id: "tok_contract_01_7d9e22b1", resource_version: "token:tok_contract_01_7d9e22b1:1", scopes: ["agent:read"], created_at: "2026-10-29T00:00:00Z", expires_at: "2027-01-15T00:00:00Z", revoked_at: null } }))
      .mockResolvedValueOnce(response({ operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb127", status: "queued" }));
    vi.stubGlobal("fetch", fetchMock);
    const { previewTokenRevocation, submitTokenRevocation } = await import("./api");

    const preview = await previewTokenRevocation("tok_contract_01_7d9e22b1", "token:tok_contract_01_7d9e22b1:1");
    await submitTokenRevocation("tok_contract_01_7d9e22b1", "   ", preview, "50e8400e-ccf8-4c7b-8d1d-14f66c7fb127");

    const previewRequest = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(JSON.parse(String(previewRequest.body))).toEqual({ target_version: "token:tok_contract_01_7d9e22b1:1" });
    const revokeRequest = fetchMock.mock.calls[2]?.[1] as RequestInit;
    expect(JSON.parse(String(revokeRequest.body))).toEqual({ operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb127", target_version: "token:tok_contract_01_7d9e22b1:1", confirmation_token: "confirmation-contract-0123456789abcdef" });
  });

  it("preserves non-empty Token revocation reasons exactly as entered", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token-0123456789abcdef0123456789abcdef", expires_at: "2027-01-15T11:20:30Z" }))
      .mockResolvedValueOnce(response({ token_id: "tok_contract_01_7d9e22b1", target_version: "token:tok_contract_01_7d9e22b1:1", confirmation_token: "confirmation-contract-0123456789abcdef", expires_at: "2026-10-29T00:05:00Z", metadata: { token_id: "tok_contract_01_7d9e22b1", resource_version: "token:tok_contract_01_7d9e22b1:1", scopes: ["agent:read"], created_at: "2026-10-29T00:00:00Z", expires_at: "2027-01-15T00:00:00Z", revoked_at: null } }))
      .mockResolvedValueOnce(response({ operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb141", status: "queued" }));
    vi.stubGlobal("fetch", fetchMock);
    const { previewTokenRevocation, submitTokenRevocation } = await import("./api");
    const reason = "  preserve this revoke reason  ";
    const preview = await previewTokenRevocation("tok_contract_01_7d9e22b1", "token:tok_contract_01_7d9e22b1:1");

    await submitTokenRevocation("tok_contract_01_7d9e22b1", reason, preview, "50e8400e-ccf8-4c7b-8d1d-14f66c7fb141");

    const request = fetchMock.mock.calls[2]?.[1] as RequestInit;
    expect(JSON.parse(String(request.body))).toMatchObject({ reason });
  });
});

describe("mock Token management contract", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.stubEnv("VITE_MANAGEMENT_API", "mock");
  });

  afterEach(() => {
    vi.unstubAllEnvs();
  });

  it("replays Token creation without minting or redisclosing", async () => {
    const { createAgentToken, listTokens } = await import("./api");
    const operationId = "50e8400e-ccf8-4c7b-8d1d-14f66c7fb132";
    const before = (await listTokens()).items.length;
    const first = await createAgentToken(["agent:read"], "2027-01-15T00:00:00.000Z", undefined, operationId);
    const replay = await createAgentToken(["agent:read"], "2027-01-15T00:00:00.000Z", undefined, operationId);
    const after = (await listTokens()).items;

    expect("token" in first).toBe(true);
    expect("token" in replay).toBe(false);
    expect(replay.metadata.token_id).toBe(first.metadata.token_id);
    expect(after).toHaveLength(before + 1);
    await expect(createAgentToken(["agent:read"], "2027-01-16T00:00:00.000Z", undefined, operationId)).rejects.toMatchObject({ code: "operation_conflict" });
  });

  it("keeps create operation snapshots immutable after revocation", async () => {
    const { createAgentToken, getOperation, listTokens, previewTokenRevocation, submitTokenRevocation } = await import("./api");
    const operationId = "50e8400e-ccf8-4c7b-8d1d-14f66c7fb133";
    const created = await createAgentToken(["agent:read"], "2027-01-15T00:00:00.000Z", undefined, operationId);
    const preview = await previewTokenRevocation(created.metadata.token_id, created.metadata.resource_version);
    await submitTokenRevocation(created.metadata.token_id, undefined, preview, "50e8400e-ccf8-4c7b-8d1d-14f66c7fb134");
    const replay = await createAgentToken(["agent:read"], "2027-01-15T00:00:00.000Z", undefined, operationId);
    const operation = await getOperation(operationId);
    const listed = (await listTokens()).items.find((token) => token.token_id === created.metadata.token_id);

    expect(replay.metadata.revoked_at).toBeNull();
    expect(operation.result?.token_metadata?.revoked_at).toBeNull();
    expect(listed?.revoked_at).not.toBeNull();
  });

  it("bounds mock Token pages and returns an opaque next cursor", async () => {
    const { createAgentToken, listTokens } = await import("./api");
    const operationIds = Array.from({ length: 26 }, (_, index) => `50e8400e-ccf8-4c7b-8d1d-${String(index + 200).padStart(12, "0")}`);
    await Promise.all(operationIds.map((operationId) => createAgentToken(["agent:read"], "2027-01-15T00:00:00.000Z", undefined, operationId)));

    const first = await listTokens();
    const second = await listTokens(first.page.next_cursor ?? undefined);

    expect(first.items).toHaveLength(25);
    expect(first.page.limit).toBe(25);
    expect(first.page.next_cursor).toMatch(/^mock-token:/);
    expect(second.items.length).toBeGreaterThan(0);
    expect(second.items.length).toBeLessThanOrEqual(25);
    expect(second.page.next_cursor).toBeNull();
  });
});

describe("management error localization", () => {
  it("provides non-generic copy for every OpenAPI safe error code", async () => {
    for (const locale of ["zh-CN", "en"] as const) {
      await i18n.changeLanguage(locale);
      for (const code of managementErrorCodes) {
        const message = i18n.getResource(locale, "translation", `errors.${code}`);
        expect(message, `${locale}:${code}`).toBeTruthy();
        if (code !== "internal_error") expect(message, `${locale}:${code}`).not.toBe(i18n.t("errors.generic"));
      }
    }
    await i18n.changeLanguage("en");
  });
});

describe("audit reason limit", () => {
  it("matches the OpenAPI 512-character limit", () => {
    expect(auditReasonCharacterLength("界".repeat(512))).toBe(512);
    expect(auditReasonWithinLimit("界".repeat(512))).toBe(true);
    expect(auditReasonWithinLimit("界".repeat(513))).toBe(false);
    expect(auditReasonCharacterLength("😀".repeat(512))).toBe(512);
    expect(auditReasonWithinLimit("😀".repeat(512))).toBe(true);
    expect(auditReasonWithinLimit("😀".repeat(513))).toBe(false);
  });
});
