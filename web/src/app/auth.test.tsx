import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import i18n from "./i18n";
import { ManagementAuthBoundary, ManagementAuthProvider, consumeAuthFailureFlag } from "./auth";
import { ManagementRequestError } from "./api";

const { getSessionMock, emitAuthFailure, subscribeAuthFailure } = vi.hoisted(() => {
  const listeners = new Set<(error: ManagementRequestError) => void>();
  return {
    getSessionMock: vi.fn(),
    emitAuthFailure: (error: ManagementRequestError) => listeners.forEach((listener) => listener(error)),
    subscribeAuthFailure: (listener: (error: ManagementRequestError) => void) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
});

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return {
    ...actual,
    getManagementSession: getSessionMock,
    isManagementApiEnabled: () => true,
    subscribeManagementAuthFailures: subscribeAuthFailure,
  };
});

function renderGate() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ManagementAuthProvider enabled><ManagementAuthBoundary><div data-testid="protected">protected management data</div></ManagementAuthBoundary></ManagementAuthProvider></QueryClientProvider>);
}

describe("management authentication gate", () => {
  beforeEach(() => {
    window.history.replaceState({}, "", "/vms");
    getSessionMock.mockReset();
    void i18n.changeLanguage("zh-CN");
  });

  afterEach(() => {
    window.history.replaceState({}, "", "/");
  });

  it("shows a standalone login layout without rendering the management shell", async () => {
    getSessionMock.mockRejectedValue(new ManagementRequestError("authentication_required"));
    renderGate();

    expect(await screen.findByRole("link", { name: "使用 OIDC 登录" })).toHaveAttribute("href", "/api/v1/auth/oidc/start?return_to=%2Fvms");
    expect(screen.getByTestId("auth-error-layout")).toBeInTheDocument();
    expect(document.querySelector(".app-frame")).not.toBeInTheDocument();
    expect(screen.queryByTestId("protected")).not.toBeInTheDocument();
    expect(screen.queryByText("单一管理员")).not.toBeInTheDocument();
  });

  it("consumes auth=failed and keeps the URL clean", async () => {
    window.history.replaceState({}, "", "/vms?auth=failed");
    getSessionMock.mockRejectedValue(new ManagementRequestError("authentication_required"));
    renderGate();

    expect(await screen.findByText("登录未完成")).toBeInTheDocument();
    expect(window.location.pathname).toBe("/vms");
    expect(window.location.search).toBe("");
    expect(consumeAuthFailureFlag()).toBe(false);
  });

  it("removes the management shell when a live session is suspended", async () => {
    getSessionMock.mockResolvedValue({ issuer: "https://issuer.example", subject: "admin@example", role: "administrator", capabilities: ["read"], csrf_token: "csrf", expires_at: "2099-01-01T00:00:00Z" });
    renderGate();
    expect(await screen.findByTestId("protected")).toBeInTheDocument();

    emitAuthFailure(new ManagementRequestError("csrf_invalid"));
    expect(await screen.findByText("会话已失效")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByTestId("protected")).not.toBeInTheDocument());
    expect(document.querySelector(".app-frame")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "重新使用 OIDC 登录" })).toHaveAttribute("href", "/api/v1/auth/oidc/start?return_to=%2Fvms");
  });
});
