import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
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
});
