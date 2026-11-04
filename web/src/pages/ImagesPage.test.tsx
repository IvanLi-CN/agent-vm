import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import "../app/i18n";
import { writeOperationRecovery } from "../app/operationRecovery";

const listImagesMock = vi.hoisted(() => vi.fn());
const startImageImportMock = vi.hoisted(() => vi.fn());
const uploadImageImportContentMock = vi.hoisted(() => vi.fn());
const getOperationMock = vi.hoisted(() => vi.fn());

vi.mock("../app/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../app/api")>();
  return { ...actual, getOperation: getOperationMock, listImages: listImagesMock, startImageImport: startImageImportMock, uploadImageImportContent: uploadImageImportContentMock };
});

import { ImagesPage } from "./ImagesPage";

function renderImagesPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ImagesPage /></QueryClientProvider>);
}

describe("image import reason validation", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    getOperationMock.mockReset();
    uploadImageImportContentMock.mockReset();
  });

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

  it("blocks a pending import after an uncertain failure until recovery is checked", async () => {
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
    await waitFor(() => expect(screen.getByText(/操作恢复核对|operation recovery/i)).toBeInTheDocument());

    const invalidReason = "界".repeat(513);
    fireEvent.change(reason, { target: { value: invalidReason } });
    expect(reason).toHaveValue(invalidReason);
    expect(startButton).toBeDisabled();
    await user.click(startButton);
    expect(startImageImportMock).toHaveBeenCalledTimes(1);

    fireEvent.change(reason, { target: { value: "original reason" } });
    await user.click(startButton);
    expect(startImageImportMock).toHaveBeenCalledTimes(1);
  });

  it("retries an interrupted upload against the original operation", async () => {
    const user = userEvent.setup();
    listImagesMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null }, catalog_version: "catalog:1", selection_version: "selection:1", default_fingerprint: null });
    startImageImportMock.mockReset().mockResolvedValue({ operation_id: "operation-image-upload", status: "awaiting_upload" });
    uploadImageImportContentMock.mockReset().mockRejectedValueOnce(new Error("connection lost")).mockResolvedValueOnce({ operation_id: "operation-image-upload", status: "succeeded" });

    renderImagesPage();
    await screen.findByTestId("images-page");
    await waitFor(() => expect(listImagesMock).toHaveBeenCalled());
    await user.click(screen.getByRole("tab", { name: /本地包|local package/i }));
    const file = new File(["image-bytes"], "image.tar", { type: "application/octet-stream" });
    await user.upload(screen.getByLabelText(/unified 镜像包|unified image package/i), file);
    await user.click(screen.getByRole("button", { name: /开始导入|start import/i }));
    await waitFor(() => expect(uploadImageImportContentMock).toHaveBeenCalledTimes(1));
    await user.click(screen.getByRole("button", { name: /开始导入|start import/i }));
    await waitFor(() => expect(uploadImageImportContentMock).toHaveBeenCalledTimes(2));
    expect(startImageImportMock).toHaveBeenCalledTimes(1);
    expect(uploadImageImportContentMock.mock.calls[1]?.[0]).toBe("operation-image-upload");
  });

  it("resumes a recovered awaiting-upload operation after selecting content again", async () => {
    const user = userEvent.setup();
    const operationId = "50e8400e-ccf8-4c7b-8d1d-14f66c7fb126";
    writeOperationRecovery("/images", operationId);
    listImagesMock.mockReset().mockResolvedValue({ items: [], page: { limit: 25, next_cursor: null }, catalog_version: "catalog:1", selection_version: "selection:1", default_fingerprint: null });
    getOperationMock.mockResolvedValue({ operation_id: operationId, action: "image_import", status: "awaiting_upload", phase: "awaiting_upload", target: { kind: "image_catalog", id: "catalog" }, actor: { issuer: "https://issuer.example", subject: "admin", role: "administrator" }, reason: null, created_at: "2026-11-05T00:00:00Z", updated_at: "2026-11-05T00:00:00Z", result: null, error: null });
    uploadImageImportContentMock.mockResolvedValue({ operation_id: operationId, status: "succeeded" });

    renderImagesPage();
    await waitFor(() => expect(getOperationMock).toHaveBeenCalledWith(operationId));
    await user.click(screen.getByRole("tab", { name: /本地包|local package/i }));
    const file = new File(["image-bytes"], "image.tar", { type: "application/octet-stream" });
    await user.upload(screen.getByLabelText(/unified 镜像包|unified image package/i), file);
    await user.click(screen.getByRole("button", { name: /开始导入|start import/i }));
    await waitFor(() => expect(uploadImageImportContentMock).toHaveBeenCalledWith(operationId, file));
    expect(startImageImportMock).not.toHaveBeenCalled();
  });
});
