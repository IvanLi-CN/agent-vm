import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import "../app/i18n";
import type { AgentToken, TokenPage } from "../app/types";

const listTokensMock = vi.hoisted(() => vi.fn());
const createAgentTokenMock = vi.hoisted(() => vi.fn());
const getOperationMock = vi.hoisted(() => vi.fn());
const previewTokenRevocationMock = vi.hoisted(() => vi.fn());
const submitTokenRevocationMock = vi.hoisted(() => vi.fn());

vi.mock("../app/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../app/api")>();
  return { ...actual, createAgentToken: createAgentTokenMock, getOperation: getOperationMock, listTokens: listTokensMock, previewTokenRevocation: previewTokenRevocationMock, submitTokenRevocation: submitTokenRevocationMock };
});

import { TokenManagement } from "./TokenManagement";
import { ManagementRequestError } from "../app/api";

function token(tokenId: string): AgentToken {
  return { token_id: tokenId, resource_version: `token:${tokenId}:revision-1`, scopes: ["agent:read"], created_at: "2027-01-01T00:00:00.000Z", expires_at: "2027-01-15T00:00:00.000Z", revoked_at: null };
}

function renderTokenManagement(client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  return { client, ...render(<QueryClientProvider client={client}><TokenManagement /></QueryClientProvider>) };
}

describe("TokenManagement pagination", () => {
  it("moves through bounded token pages and returns to the first page", async () => {
    const user = userEvent.setup();
    const firstPage: TokenPage = { items: Array.from({ length: 25 }, (_, index) => token(`tok_page_01_${String(index).padStart(2, "0")}`)), page: { limit: 25, next_cursor: "mock-token:25" } };
    const secondPageTokenId = "tok_page_02_00";
    const secondPage: TokenPage = { items: [token(secondPageTokenId)], page: { limit: 25, next_cursor: null } };
    listTokensMock.mockReset();
    listTokensMock.mockImplementation((cursor?: string) => Promise.resolve(cursor === "mock-token:25" ? secondPage : firstPage));

    renderTokenManagement();
    await waitFor(() => expect(screen.getByRole("button", { name: /下一页|next/i })).toBeEnabled());
    expect(screen.queryByText(secondPageTokenId)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /下一页|next/i }));
    expect(await screen.findByText(secondPageTokenId)).toBeInTheDocument();
    expect(listTokensMock).toHaveBeenCalledWith("mock-token:25");
    expect(screen.getByRole("button", { name: /上一页|previous/i })).toBeEnabled();

    await user.click(screen.getByRole("button", { name: /上一页|previous/i }));
    await waitFor(() => expect(screen.queryByText(secondPageTokenId)).not.toBeInTheDocument());
  });

  it("does not render an empty state when token listing fails", async () => {
    listTokensMock.mockReset();
    listTokensMock.mockRejectedValueOnce(new Error("authority unavailable"));

    renderTokenManagement();
    expect(await screen.findByRole("alert")).toHaveTextContent(/请求未完成|request did not complete/i);
    expect(screen.queryByText(/暂无机器 Token|no machine tokens/i)).not.toBeInTheDocument();
  });

  it("blocks replacement after an unresolved operation lookup", async () => {
    const user = userEvent.setup();
    listTokensMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null } });
    createAgentTokenMock.mockReset().mockRejectedValueOnce(new ManagementRequestError("internal_error", "internal"));
    getOperationMock.mockReset().mockRejectedValueOnce(new ManagementRequestError("not_found", "reconciliation"));

    renderTokenManagement();
    await screen.findByTestId("token-management");
    (document.querySelector("form") as HTMLFormElement).noValidate = true;
    const createButton = screen.getByRole("button", { name: /创建 Token|create token/i });
    await waitFor(() => expect(createButton).toBeEnabled());
    await user.click(createButton);
    await waitFor(() => expect(createAgentTokenMock).toHaveBeenCalled());
    const checkButton = await screen.findByRole("button", { name: /查询操作|check operation/i });
    expect(screen.getByRole("button", { name: /创建 Token|create token/i })).toBeDisabled();

    await user.click(checkButton);
    expect(await screen.findByText(/暂时没有权威的操作结果|No authoritative operation result/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /创建 Token|create token/i })).toBeDisabled();
  });

  it("releases the form after a deterministic create rejection", async () => {
    const user = userEvent.setup();
    listTokensMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null } });
    createAgentTokenMock.mockReset().mockRejectedValueOnce(new ManagementRequestError("invalid_request", "validation"));

    renderTokenManagement();
    await screen.findByTestId("token-management");
    (document.querySelector("form") as HTMLFormElement).noValidate = true;
    const createButton = screen.getByRole("button", { name: /创建 Token|create token/i });
    await waitFor(() => expect(createButton).toBeEnabled());
    await user.click(createButton);

    expect(await screen.findByRole("alert")).toHaveTextContent(/请求参数无效|request parameters are invalid/i);
    expect(screen.queryByRole("button", { name: /查询操作|check operation/i })).not.toBeInTheDocument();
    expect(createButton).toBeEnabled();
  });

  it("keeps unresolved create recovery across a page unmount", async () => {
    const user = userEvent.setup();
    listTokensMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null } });
    createAgentTokenMock.mockReset().mockRejectedValueOnce(new ManagementRequestError("internal_error", "internal"));

    const firstRender = renderTokenManagement();
    await screen.findByTestId("token-management");
    (document.querySelector("form") as HTMLFormElement).noValidate = true;
    const createButton = screen.getByRole("button", { name: /创建 Token|create token/i });
    await waitFor(() => expect(createButton).toBeEnabled());
    await user.click(createButton);
    expect(await screen.findByRole("button", { name: /查询操作|check operation/i })).toBeInTheDocument();
    expect(firstRender.client.getQueryCache().find({ queryKey: ["token-create-recovery"], exact: true })?.options.gcTime).toBe(Infinity);

    firstRender.unmount();
    renderTokenManagement(firstRender.client);
    expect(await screen.findByRole("button", { name: /查询操作|check operation/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /创建 Token|create token/i })).toBeDisabled();
  });

  it("keeps create protection when the page unmounts before the response", async () => {
    const user = userEvent.setup();
    let resolveCreate: ((result: unknown) => void) | undefined;
    listTokensMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null } });
    createAgentTokenMock.mockReset().mockImplementationOnce(() => new Promise((resolve) => { resolveCreate = resolve; }));

    const firstRender = renderTokenManagement();
    await screen.findByTestId("token-management");
    (document.querySelector("form") as HTMLFormElement).noValidate = true;
    const createButton = screen.getByRole("button", { name: /创建 Token|create token/i });
    await waitFor(() => expect(createButton).toBeEnabled());
    await user.click(createButton);
    await waitFor(() => expect(createAgentTokenMock).toHaveBeenCalled());

    firstRender.unmount();
    renderTokenManagement(firstRender.client);
    expect(await screen.findByRole("button", { name: /查询操作|check operation/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /创建 Token|create token/i })).toBeDisabled();
    resolveCreate?.({ operation_id: "operation-pending-01", metadata: token("tok_pending_01"), plaintext_disclosure: "consumed" });
  });

  it("clears deterministic create recovery when the response arrives after page unmount", async () => {
    const user = userEvent.setup();
    let rejectCreate: ((error: unknown) => void) | undefined;
    listTokensMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null } });
    createAgentTokenMock.mockReset().mockImplementationOnce(() => new Promise((_, reject) => { rejectCreate = reject; }));

    const firstRender = renderTokenManagement();
    await screen.findByTestId("token-management");
    (document.querySelector("form") as HTMLFormElement).noValidate = true;
    const createButton = screen.getByRole("button", { name: /创建 Token|create token/i });
    await waitFor(() => expect(createButton).toBeEnabled());
    await user.click(createButton);
    await waitFor(() => expect(createAgentTokenMock).toHaveBeenCalled());

    firstRender.unmount();
    rejectCreate?.(new ManagementRequestError("invalid_request", "validation"));
    await waitFor(() => expect(firstRender.client.getQueryData(["token-create-recovery"])).toMatchObject({ createErrorCode: "invalid_request" }));

    renderTokenManagement(firstRender.client);
    expect(await screen.findByRole("button", { name: /创建 Token|create token/i })).toBeEnabled();
    expect(screen.queryByRole("button", { name: /查询操作|check operation/i })).not.toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(/请求参数无效|request parameters are invalid/i);
  });

  it("syncs deterministic recovery cleanup when the page returns before the response", async () => {
    const user = userEvent.setup();
    let rejectCreate: ((error: unknown) => void) | undefined;
    listTokensMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null } });
    createAgentTokenMock.mockReset().mockImplementationOnce(() => new Promise((_, reject) => { rejectCreate = reject; }));

    const firstRender = renderTokenManagement();
    await screen.findByTestId("token-management");
    (document.querySelector("form") as HTMLFormElement).noValidate = true;
    const createButton = screen.getByRole("button", { name: /创建 Token|create token/i });
    await waitFor(() => expect(createButton).toBeEnabled());
    await user.click(createButton);
    await waitFor(() => expect(createAgentTokenMock).toHaveBeenCalled());

    firstRender.unmount();
    renderTokenManagement(firstRender.client);
    expect(await screen.findByRole("button", { name: /查询操作|check operation/i })).toBeInTheDocument();
    rejectCreate?.(new ManagementRequestError("invalid_request", "validation"));

    await waitFor(() => expect(screen.queryByRole("button", { name: /查询操作|check operation/i })).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: /创建 Token|create token/i })).toBeEnabled();
    expect(screen.getByRole("alert")).toHaveTextContent(/请求参数无效|request parameters are invalid/i);
  });

  it("keeps successful create recovery after an unmounted response without storing plaintext", async () => {
    const user = userEvent.setup();
    let resolveCreate: ((result: unknown) => void) | undefined;
    const created = token("tok_success_01");
    listTokensMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null } });
    createAgentTokenMock.mockReset().mockImplementationOnce(() => new Promise((resolve) => { resolveCreate = resolve; }));

    const firstRender = renderTokenManagement();
    await screen.findByTestId("token-management");
    (document.querySelector("form") as HTMLFormElement).noValidate = true;
    const createButton = screen.getByRole("button", { name: /创建 Token|create token/i });
    await waitFor(() => expect(createButton).toBeEnabled());
    await user.click(createButton);
    await waitFor(() => expect(createAgentTokenMock).toHaveBeenCalled());

    firstRender.unmount();
    renderTokenManagement(firstRender.client);
    expect(await screen.findByRole("button", { name: /查询操作|check operation/i })).toBeInTheDocument();
    resolveCreate?.({ operation_id: "operation-success-01", token: "secret-token-must-not-persist", metadata: created, plaintext_disclosure: "returned" });
    await waitFor(() => expect(firstRender.client.getQueryData(["token-create-recovery"])).toMatchObject({ createRecoveryOperationId: "operation-success-01", createRecoveryTokenId: created.token_id, createRecoveryDecision: "blocked" }));
    expect(JSON.stringify(firstRender.client.getQueryData(["token-create-recovery"]))).not.toContain("secret-token-must-not-persist");

    expect(screen.getByRole("button", { name: /创建 Token|create token/i })).toBeDisabled();
    expect(screen.queryByTestId("token-plaintext")).not.toBeInTheDocument();
    expect(screen.getByTestId("token-disclosure")).toHaveTextContent(/明文披露状态已消费|disclosure latch is consumed/i);
  });

  it("allows revocation of an expired but not revoked token", async () => {
    const expired = { ...token("tok_expired_01"), expires_at: "2026-01-15T00:00:00.000Z" };
    listTokensMock.mockReset().mockResolvedValue({ items: [expired], page: { limit: 25, next_cursor: null } });

    renderTokenManagement();
    await waitFor(() => expect(screen.getByRole("button", { name: /撤销|revoke/i })).toBeEnabled());
  });

  it("releases recovery when the token list confirms an external revocation", async () => {
    const user = userEvent.setup();
    const recovered = token("tok_recovered_01");
    const revoked = { ...recovered, revoked_at: "2026-10-29T09:00:00.000Z" };
    let listCalls = 0;
    listTokensMock.mockReset().mockImplementation(() => Promise.resolve({ items: listCalls++ === 0 ? [] : [revoked], page: { limit: 25, next_cursor: null } }));
    createAgentTokenMock.mockReset().mockRejectedValueOnce(new ManagementRequestError("internal_error", "internal"));
    getOperationMock.mockReset().mockResolvedValueOnce({ operation_id: "operation-recovered-01", action: "token_create", status: "succeeded", phase: "completed", target: { kind: "token", id: recovered.token_id }, actor: { issuer: "issuer.example.invalid", subject: "admin-maintainer", role: "administrator" }, reason: null, created_at: "2026-10-29T09:00:00.000Z", updated_at: "2026-10-29T09:00:00.000Z", result: { token_metadata: recovered, plaintext_disclosure: "returned" }, error: null });

    renderTokenManagement();
    await screen.findByTestId("token-management");
    (document.querySelector("form") as HTMLFormElement).noValidate = true;
    const createButton = screen.getByRole("button", { name: /创建 Token|create token/i });
    await waitFor(() => expect(createButton).toBeEnabled());
    await user.click(createButton);
    const checkButton = await screen.findByRole("button", { name: /查询操作|check operation/i });
    await user.click(checkButton);

    await waitFor(() => expect(createButton).toBeEnabled());
    expect(screen.queryByRole("button", { name: /查询操作|check operation/i })).not.toBeInTheDocument();
  });

  it("persists an uncertain revoke and recovers it after navigation", async () => {
    const user = userEvent.setup();
    const target = token("tok_revoke_recovery_01");
    const preview = { token_id: target.token_id, target_version: target.resource_version, confirmation_token: "confirmation-revoke-recovery-01", expires_at: "2027-01-01T00:05:00.000Z", metadata: target };
    const operation = { operation_id: "operation-revoke-recovery-01", action: "token_revoke" as const, status: "succeeded" as const, phase: "completed" as const, target: { kind: "token" as const, id: target.token_id }, actor: { issuer: "issuer.example.invalid", subject: "admin-maintainer", role: "administrator" as const }, reason: "  preserve this reason  ", created_at: "2026-10-29T09:00:00.000Z", updated_at: "2026-10-29T09:00:00.000Z", result: null, error: null };
    listTokensMock.mockReset().mockResolvedValue({ items: [target], page: { limit: 25, next_cursor: null } });
    previewTokenRevocationMock.mockReset().mockResolvedValue(preview);
    submitTokenRevocationMock.mockReset().mockRejectedValueOnce(new Error("connection lost"));
    getOperationMock.mockReset().mockResolvedValueOnce(operation);

    const firstRender = renderTokenManagement();
    await waitFor(() => expect(screen.getByRole("button", { name: /撤销|revoke/i })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: /撤销|revoke/i }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    await user.type(screen.getByRole("textbox", { name: /原因|reason/i }), "  preserve this reason  ");
    await user.click(screen.getByRole("button", { name: /确认提交|confirm and submit/i }));

    await screen.findByTestId("token-revoke-recovery");
    const recovery = firstRender.client.getQueryData(["token-revoke-recovery"]) as { operationId: string };
    expect(recovery).toMatchObject({ auditReason: "  preserve this reason  ", decision: "check", preview });
    firstRender.unmount();

    renderTokenManagement(firstRender.client);
    expect(await screen.findByTestId("token-revoke-recovery")).toHaveTextContent(/连接中断|connection failed|operation/i);
    await user.click(screen.getByRole("button", { name: /查询操作|check operation/i }));
    await waitFor(() => expect(screen.queryByTestId("token-revoke-recovery")).not.toBeInTheDocument());
    expect(getOperationMock).toHaveBeenCalledWith(recovery.operationId);
  });

  it("bounds Token creation reasons at the contract limit", async () => {
    listTokensMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null } });
    createAgentTokenMock.mockReset();

    renderTokenManagement();
    await screen.findByTestId("token-management");
    const reason = screen.getByRole("textbox", { name: /审计原因|audit reason/i });
    expect(reason).toHaveAttribute("maxLength", "512");
    expect(createAgentTokenMock).not.toHaveBeenCalled();
  });
});
