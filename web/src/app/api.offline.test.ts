import { expect, it, vi } from "vitest";

vi.mock("../demo/runtime", async (importOriginal) => ({
  ...await importOriginal<typeof import("../demo/runtime")>(),
  isDemoRuntime: () => true,
  demoMutationError: () => "network_unavailable",
}));

import { previewVMAction, startImageImport } from "./api";
import { mockState } from "./mockData";

it("rejects offline previews and mutations without creating an operation", async () => {
  const before = mockState.operations.length;
  await expect(previewVMAction("task_example_01", "restart")).rejects.toMatchObject({ code: "network_unavailable" });
  await expect(startImageImport({ operation_id: "offline-import", reason: "offline", target_version: mockState.image_catalog_version, source: { kind: "trusted_remote", source_id: "offline", expected_fingerprint: "d".repeat(64) } })).rejects.toMatchObject({ code: "network_unavailable" });
  expect(mockState.operations).toHaveLength(before);
});
