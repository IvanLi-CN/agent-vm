import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import "../app/i18n";
import { images } from "../app/mockData";
import { ImageNameEditor } from "./ImageNameEditor";
const api = vi.hoisted(() => ({ preview: vi.fn(), submit: vi.fn() }));
vi.mock("../app/api", async (importOriginal) => ({ ...await importOriginal<typeof import("../app/api")>(), previewImageName: api.preview, submitImageName: api.submit }));
beforeEach(() => { window.sessionStorage.clear(); api.preview.mockReset(); api.submit.mockReset(); });
function mount(named = false) { return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}><ImageNameEditor image={{ ...images[1], name: named ? "build" : null }} catalogVersion="catalog:1"/></QueryClientProvider>); }
it("confirms the full old/new targets before rebinding and keeps the operation identity", async () => {
    const user = userEvent.setup(), old = "b".repeat(64), next = images[1].fingerprint;
    const preview = { target_version: "catalog:1", confirmation_token: "confirm", expires_at: "2099-01-01T00:00:00Z", impact: { summary_code: "image_name", affected_session_count: 0, affected_image_references: {}, changes: [{ code: "previous_fingerprint", count: null, value: old }, { code: "new_fingerprint", count: null, value: next }] } };
    api.preview.mockResolvedValue(preview);
    api.submit.mockResolvedValue({ status: "succeeded" });
    mount();
    await user.type(screen.getByRole("textbox"), " build ");
    await user.click(screen.getByRole("button", { name: /设置或改绑|Set or rebind/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getAllByText(old).length).toBeGreaterThan(0);
    expect(within(dialog).getAllByText(next).length).toBeGreaterThan(0);
    expect(api.submit).not.toHaveBeenCalled();
    await user.click(within(dialog).getByRole("button", { name: /确认提交|Confirm and submit/ }));
    await waitFor(() => expect(api.submit).toHaveBeenCalledTimes(1));
    expect(api.submit.mock.calls[0]).toEqual([next, "build", "", preview, expect.any(String)]);
});
it("requires clearing an already named target before rebinding", () => { mount(true); expect(screen.getByRole("textbox")).toBeDisabled(); expect(screen.getByRole("button", { name: /设置或改绑|Set or rebind/ })).toBeDisabled(); expect(screen.getByRole("button", { name: /清除名称|Clear name/ })).toBeEnabled(); });

for (const code of ["stale_target", "precondition_required"]) {
  it(`clears determinate ${code} recovery before reloading the editor`, async () => {
    const { ManagementRequestError } = await import("../app/api");
    const user = userEvent.setup();
    api.preview.mockResolvedValue({ target_version: "catalog:1", confirmation_token: "confirm", expires_at: "2099-01-01T00:00:00Z", impact: { summary_code: "image_name", affected_session_count: 0, affected_image_references: {}, changes: [] } });
    api.submit.mockRejectedValue(new ManagementRequestError(code, "precondition"));
    const rendered = mount();
    await user.type(screen.getByRole("textbox"), "build");
    await user.click(screen.getByRole("button", { name: /设置或改绑|Set or rebind/ }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /确认提交|Confirm and submit/ }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(window.sessionStorage.getItem(`agent-vm:operation-recovery:/images/name/${images[1].fingerprint}`)).toBeNull();
    rendered.unmount();
    mount();
    expect(screen.getByRole("textbox")).toBeEnabled();
    await user.type(screen.getByRole("textbox"), "retry");
    expect(screen.getByRole("button", { name: /设置或改绑|Set or rebind/ })).toBeEnabled();
  });
}

it("retains recovery and blocks another write after an unknown submit outcome", async () => {
  const user = userEvent.setup();
  api.preview.mockResolvedValue({ target_version: "catalog:1", confirmation_token: "confirm", expires_at: "2099-01-01T00:00:00Z", impact: { summary_code: "image_name", affected_session_count: 0, affected_image_references: {}, changes: [] } });
  api.submit.mockRejectedValue(new Error("transport interrupted"));
  mount();
  await user.type(screen.getByRole("textbox"), "build");
  await user.click(screen.getByRole("button", { name: /设置或改绑|Set or rebind/ }));
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /确认提交|Confirm and submit/ }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(window.sessionStorage.getItem(`agent-vm:operation-recovery:/images/name/${images[1].fingerprint}`)).not.toBeNull();
  expect(screen.getByRole("textbox")).toBeDisabled();
});
