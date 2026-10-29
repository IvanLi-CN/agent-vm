import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import "../app/i18n";

const listImagesMock = vi.hoisted(() => vi.fn());
const startImageImportMock = vi.hoisted(() => vi.fn());

vi.mock("../app/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../app/api")>();
  return { ...actual, listImages: listImagesMock, startImageImport: startImageImportMock };
});

import { ImagesPage } from "./ImagesPage";

function renderImagesPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ImagesPage /></QueryClientProvider>);
}

describe("image import reason validation", () => {
  it.each([
    { label: "an omitted reason", correctedReason: "" },
    { label: "512 supplementary characters", correctedReason: "😀".repeat(512) },
  ])("blocks import until an overlong reason is corrected to $label", async ({ correctedReason }) => {
    const user = userEvent.setup();
    listImagesMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null }, catalog_version: "catalog:1", selection_version: "selection:1", default_fingerprint: null });
    startImageImportMock.mockReset().mockResolvedValue({ operation_id: "operation-image-reason", status: "succeeded" });

    renderImagesPage();
    await screen.findByTestId("images-page");
    await waitFor(() => expect(listImagesMock).toHaveBeenCalled());
    const reason = screen.getByRole("textbox", { name: /导入原因|import reason/i });
    const startButton = screen.getByRole("button", { name: /开始导入|start import/i });
    const invalidReason = "😀".repeat(513);
    fireEvent.change(reason, { target: { value: invalidReason } });
    expect(reason).toHaveValue(invalidReason);
    expect(reason).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("alert")).toHaveTextContent(/512/);
    expect(startButton).toBeDisabled();
    await user.click(startButton);
    expect(startImageImportMock).not.toHaveBeenCalled();

    fireEvent.change(reason, { target: { value: correctedReason } });
    expect(reason).toHaveValue(correctedReason);
    expect(reason).not.toHaveAttribute("aria-invalid", "true");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(startButton).toBeEnabled();
    await user.click(startButton);
    await waitFor(() => expect(startImageImportMock).toHaveBeenCalledTimes(1));
    const [request] = startImageImportMock.mock.calls[0];
    if (correctedReason) expect(request.reason).toBe(correctedReason);
    else expect(request).not.toHaveProperty("reason");
  });

  it("does not retry a pending import while the edited reason is overlong", async () => {
    const user = userEvent.setup();
    listImagesMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null }, catalog_version: "catalog:1", selection_version: "selection:1", default_fingerprint: null });
    startImageImportMock.mockReset().mockRejectedValueOnce(new Error("connection lost")).mockResolvedValueOnce({ operation_id: "operation-image-retry", status: "succeeded" });

    renderImagesPage();
    await screen.findByTestId("images-page");
    await waitFor(() => expect(listImagesMock).toHaveBeenCalled());
    const reason = screen.getByRole("textbox", { name: /导入原因|import reason/i });
    const startButton = screen.getByRole("button", { name: /开始导入|start import/i });
    fireEvent.change(reason, { target: { value: "original reason" } });
    await user.click(startButton);
    await waitFor(() => expect(startImageImportMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(startButton).toBeEnabled());

    const invalidReason = "界".repeat(513);
    fireEvent.change(reason, { target: { value: invalidReason } });
    expect(reason).toHaveValue(invalidReason);
    expect(startButton).toBeDisabled();
    await user.click(startButton);
    expect(startImageImportMock).toHaveBeenCalledTimes(1);

    fireEvent.change(reason, { target: { value: "original reason" } });
    await user.click(startButton);
    await waitFor(() => expect(startImageImportMock).toHaveBeenCalledTimes(2));
    expect(startImageImportMock.mock.calls[1][0]).toEqual(startImageImportMock.mock.calls[0][0]);
  });
});
