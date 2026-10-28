import { afterEach, describe, expect, it } from "vitest";
import { mockState } from "./mockData";
import { startImageImport, uploadImageImportContent } from "./api";

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
  });
});
