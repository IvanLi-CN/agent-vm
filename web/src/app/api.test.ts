import { afterEach, describe, expect, it } from "vitest";
import { mockState } from "./mockData";
import { ManagementRequestError, startImageImport, submitVMAction, uploadImageImportContent } from "./api";

describe("mock image uploads", () => {
  let snapshot = structuredClone(mockState);

  afterEach(() => {
    Object.assign(mockState, snapshot);
    snapshot = structuredClone(mockState);
  });

  it("continues the awaiting operation without reading demo content", async () => {
    const operation = await startImageImport({
      operation_id: "synthetic-upload-operation",
      reason: "Exercise the isolated upload path",
      target_version: "image-catalog:revision-12",
      version_label: "2026.02",
      source: { kind: "local_upload", format: "unified", expected_sha256: "c".repeat(64), expected_size_bytes: "7" },
    });
    const unreadable = new Proxy({} as File, { get() { throw new Error("demo upload content must not be read"); } });
    const completed = await uploadImageImportContent(operation.operation_id, unreadable);

    expect(completed.operation_id).toBe(operation.operation_id);
    expect(completed.status).toBe("succeeded");
    expect(mockState.operations.filter((item) => item.operation_id === operation.operation_id)).toHaveLength(1);
    await expect(uploadImageImportContent(operation.operation_id, unreadable)).rejects.toMatchObject({ code: "operation_conflict" });
  });

  it("returns the original operation for an identical retry and rejects a conflicting reuse", async () => {
    const operationId = "synthetic-idempotent-operation";
    const preview = { target_version: "vm:task_example_01:revision-9", confirmation_token: "synthetic", expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: "no_active_lease", affected_session_count: 0, affected_image_references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, changes: [] } };
    const first = await submitVMAction("task_example_01", "restart", "retry-safe action", preview, "42", {}, operationId);
    const retry = await submitVMAction("task_example_01", "restart", "retry-safe action", preview, "42", {}, operationId);

    expect(retry).toBe(first);
    await expect(submitVMAction("task_aurora_02", "restart", "different target", preview, "18", {}, operationId)).rejects.toBeInstanceOf(ManagementRequestError);
    expect(mockState.operations.filter((item) => item.operation_id === operationId)).toHaveLength(1);
  });
});
