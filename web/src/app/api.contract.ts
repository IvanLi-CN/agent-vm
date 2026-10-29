import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token" }))
      .mockResolvedValueOnce(response({ target_version: "vm:task:7", confirmation_token: "confirm", expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: "native_action", affected_session_count: 0, affected_image_references: {}, changes: [] } }));
    vi.stubGlobal("fetch", fetchMock);
    const { previewVMAction } = await import("./api");

    await previewVMAction("task", "restart", { generation: "7", resource_version: "vm:task:7" });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/session");
    const request = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(new Headers(request.headers).get("X-CSRF-Token")).toBe("csrf-token");
    expect(JSON.parse(String(request.body))).toMatchObject({ expected_generation: "7", target_version: "vm:task:7" });
  });

  it("streams a unified package without overriding its content type", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token" }))
      .mockResolvedValueOnce(response({ operation_id: "op-1" }));
    vi.stubGlobal("fetch", fetchMock);
    const { uploadImageImportContent } = await import("./api");
    const file = new File(["image-bytes"], "image.tar", { type: "application/octet-stream" });

    await uploadImageImportContent("op-1", file);

    expect(fetchMock).toHaveBeenCalledTimes(2);
    const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(url).toBe("/api/v1/images/imports/op-1/content");
    expect(init.method).toBe("PUT");
    expect(new Headers(init.headers).get("X-CSRF-Token")).toBe("csrf-token");
    expect(new Headers(init.headers).get("Content-Type")).toBe("application/octet-stream");
    expect(init.body).toBe(file);
  });

  it("omits an optional reason when creating a Token", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token", expires_at: "2027-01-15T11:20:30Z" }))
      .mockResolvedValueOnce(response({ operation_id: "op-token-create", token: "bearer-value", metadata: { token_id: "tok-1", resource_version: "token:tok-1:1", scopes: ["agent:read"], created_at: "2026-10-29T00:00:00Z", expires_at: "2027-01-15T00:00:00Z", revoked_at: null }, plaintext_disclosure: "returned" }));
    vi.stubGlobal("fetch", fetchMock);
    const { createAgentToken } = await import("./api");

    await createAgentToken(["agent:read"], "2027-01-15T00:00:00.000Z", "   ", "op-token-create");

    const request = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(JSON.parse(String(request.body))).toEqual({ operation_id: "op-token-create", scopes: ["agent:read"], expires_at: "2027-01-15T00:00:00.000Z" });
  });

  it("keeps Token revocation preview and confirmation separate", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: "csrf-token", expires_at: "2027-01-15T11:20:30Z" }))
      .mockResolvedValueOnce(response({ token_id: "tok-1", target_version: "token:tok-1:1", confirmation_token: "confirm-token", expires_at: "2026-10-29T00:05:00Z", metadata: { token_id: "tok-1", resource_version: "token:tok-1:1", scopes: ["agent:read"], created_at: "2026-10-29T00:00:00Z", expires_at: "2027-01-15T00:00:00Z", revoked_at: null } }))
      .mockResolvedValueOnce(response({ operation_id: "op-token-revoke", status: "queued" }));
    vi.stubGlobal("fetch", fetchMock);
    const { previewTokenRevocation, submitTokenRevocation } = await import("./api");

    const preview = await previewTokenRevocation("tok-1", "token:tok-1:1");
    await submitTokenRevocation("tok-1", "   ", preview, "op-token-revoke");

    const previewRequest = fetchMock.mock.calls[1]?.[1] as RequestInit;
    expect(JSON.parse(String(previewRequest.body))).toEqual({ target_version: "token:tok-1:1" });
    const revokeRequest = fetchMock.mock.calls[2]?.[1] as RequestInit;
    expect(JSON.parse(String(revokeRequest.body))).toEqual({ operation_id: "op-token-revoke", target_version: "token:tok-1:1", confirmation_token: "confirm-token" });
  });
});
