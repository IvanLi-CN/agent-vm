import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import "./i18n";
import { App } from "./App";

function renderApp() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><App /></QueryClientProvider>);
}

describe("management console workflow", () => {
  it("keeps the selected VM identity visible while the detail changes", async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, "", "/vms");
    renderApp();
    expect(await screen.findByTestId("vms-page")).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "task example 01" })).toBeInTheDocument();
    await user.click(screen.getByRole("option", { name: /task aurora 02/i }));
    expect(screen.getByText("task_aurora_02")).toBeInTheDocument();
    expect(screen.getAllByText("session_aurora_02")).toHaveLength(2);
  });

  it("switches language without removing the current page", async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, "", "/");
    renderApp();
    expect(await screen.findByTestId("overview-page")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "EN" }));
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
  });

  it("moves focus into a controlled action and returns it to the trigger", async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, "", "/vms");
    renderApp();
    expect(await screen.findByTestId("vms-page")).toBeInTheDocument();
    const trigger = screen.getByRole("button", { name: /释放租约|release lease/i });
    await user.click(trigger);
    const reason = await screen.findByRole("textbox", { name: /原因|reason/i });
    expect(reason).toHaveFocus();
    await user.click(screen.getByRole("button", { name: /取消|cancel/i }));
    expect(trigger).toHaveFocus();
  });

  it("creates a machine token and only exposes its plaintext until dismissed", async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, "", "/system");
    renderApp();
    expect(await screen.findByTestId("token-management")).toBeInTheDocument();
    expect(await screen.findByText("tok_read_01_7d9e22b1")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /创建 Token|create token/i }));
    expect(await screen.findByTestId("token-plaintext")).toBeInTheDocument();
    const plaintext = screen.getByTestId("token-plaintext").textContent;
    expect(plaintext).toBeTruthy();

    await user.click(screen.getByRole("button", { name: /隐藏明文|hide plaintext/i }));
    expect(screen.queryByTestId("token-plaintext")).not.toBeInTheDocument();
    expect(screen.getByTestId("token-disclosure")).toHaveTextContent(/明文披露状态已消费|disclosure latch is consumed/i);

    await waitFor(() => expect(screen.getAllByRole("button", { name: /撤销|revoke/i })).toHaveLength(3));
    await user.click(screen.getAllByRole("button", { name: /撤销|revoke/i })[0]);
    expect(await screen.findByRole("dialog")).toHaveTextContent(/撤销后新的 Agent API 授权会被拒绝|new Agent API authorization/i);
    await user.click(screen.getByRole("button", { name: /确认提交|confirm and submit/i }));
    await waitFor(() => expect(screen.getAllByRole("button", { name: /撤销|revoke/i })[0]).toBeDisabled());
  });
});
