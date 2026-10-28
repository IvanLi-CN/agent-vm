import { afterEach, describe, expect, it } from "vitest";
import { mockState } from "./mockData";
import { ManagementRequestError, startImageImport, submitImageAction, submitVMAction, uploadImageImportContent } from "./api";
import type { ActionPreview } from "./types";

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
      source: { kind: "local_upload", format: "unified", expected_sha256: "f".repeat(64), expected_size_bytes: "7" },
    });
    const unreadable = new Proxy({} as File, { get() { throw new Error("demo upload content must not be read"); } });
    const completed = await uploadImageImportContent(operation.operation_id, unreadable);

    expect(completed.operation_id).toBe(operation.operation_id);
    expect(completed.status).toBe("succeeded");
    expect(mockState.images[0]).toMatchObject({ fingerprint: "f".repeat(64), version_label: "2026.02", source: { kind: "local_upload" } });
    expect(mockState.operations.filter((item) => item.operation_id === operation.operation_id)).toHaveLength(1);
    await expect(uploadImageImportContent(operation.operation_id, unreadable)).rejects.toMatchObject({ code: "operation_conflict" });
  });

  it("returns the original operation for an identical retry and rejects a conflicting reuse", async () => {
    const operationId = "synthetic-idempotent-operation";
    const preview = { target_version: "vm:task_example_01:revision-9", confirmation_token: "synthetic", expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: "no_active_lease", affected_session_count: 0, affected_image_references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, changes: [] } };
    const first = await submitVMAction("task_example_01", "restart", "retry-safe action", preview, "42", { alpha: 1, beta: 2 }, operationId);
    const retry = await submitVMAction("task_example_01", "restart", "retry-safe action", preview, "42", { beta: 2, alpha: 1 }, operationId);

    expect(retry).toBe(first);
    await expect(submitVMAction("task_example_01", "restart", "retry-safe action", { ...preview, target_version: "vm:task_example_01:revision-10" }, "42", {}, operationId)).rejects.toMatchObject({ code: "operation_conflict" });
    await expect(submitVMAction("task_aurora_02", "restart", "different target", preview, "18", {}, operationId)).rejects.toBeInstanceOf(ManagementRequestError);
    expect(mockState.operations.filter((item) => item.operation_id === operationId)).toHaveLength(1);
  });

  it("terminalizes trusted imports and keeps VM aggregate counts aligned", async () => {
    const importedFingerprint = "e".repeat(64);
    const imported = await startImageImport({ operation_id: "synthetic-trusted-import", reason: "Exercise trusted import", target_version: "image-catalog:revision-12", version_label: "2026.02", source: { kind: "trusted_remote", source_id: "ubuntu-staging", expected_fingerprint: importedFingerprint } });
    expect(imported.status).toBe("succeeded");
    expect(mockState.images[0]).toMatchObject({ fingerprint: importedFingerprint, version_label: "2026.02", source: { kind: "trusted_remote", source_id: "ubuntu-staging" } });
    expect(mockState.image_catalog_version).toContain(imported.operation_id.slice(0, 8));
    expect(imported.result?.image_fingerprint).toBe(importedFingerprint);

    const beforeActive = mockState.overview.task_counts.active;
    const beforeCached = mockState.overview.task_counts.cached;
    const vm = mockState.vms[0];
    const release = await submitVMAction(vm.task_id, "release", "Align aggregate state", { ...previewFor(vm.resource_version), impact: { summary_code: "effective_lease_affected", affected_session_count: 1, affected_image_references: mockState.images[0].references, changes: [] } }, vm.generation);

    expect(release.status).toBe("succeeded");
    expect(mockState.overview.task_counts.active).toBe(beforeActive - 1);
    expect(mockState.overview.task_counts.cached).toBe(beforeCached + 1);
    expect(vm.state).toBe("cached");
  });

  it("keeps default image references and selection version aligned", async () => {
    const previousDefault = mockState.images.find((image) => image.is_default)!;
    const nextDefault = mockState.images.find((image) => !image.is_default)!;
    const beforeVersion = mockState.image_selection_version;
    const operation = await submitImageAction(nextDefault.fingerprint, "default", "Select the approved image", previewFor(nextDefault.resource_version), "synthetic-default-image");

    expect(operation.status).toBe("succeeded");
    expect(nextDefault.is_default).toBe(true);
    expect(nextDefault.references.default).toBe(1);
    expect(previousDefault.is_default).toBe(false);
    expect(previousDefault.references.default).toBe(0);
    expect(mockState.image_selection_version).not.toBe(beforeVersion);
    expect(operation.result?.default_fingerprint).toBe(nextDefault.fingerprint);
  });

  it("updates available capacity when a VM is resized", async () => {
    const vm = mockState.vms[0];
    const beforeCpu = mockState.overview.capacity.logical_cpu_remaining_vcpu.value!;
    const beforeMemory = BigInt(mockState.overview.capacity.logical_memory_remaining.value_bytes!);
    const beforeContainerMemory = BigInt(mockState.overview.capacity.ct_memory_available.value_bytes!);
    const nextMemory = (BigInt(vm.resources.memory_bytes!) + 1_073_741_824n).toString();
    const operation = await submitVMAction(vm.task_id, "resize", "Increase demo resources", previewFor(vm.resource_version), vm.generation, { cpu_vcpu: vm.resources.cpu_vcpu! + 2, memory_bytes: nextMemory, disk_bytes: vm.resources.disk_bytes }, "synthetic-resize-operation");

    expect(operation.status).toBe("succeeded");
    expect(mockState.overview.capacity.logical_cpu_remaining_vcpu.value).toBe(beforeCpu - 2);
    expect(mockState.overview.capacity.logical_memory_remaining.value_bytes).toBe((beforeMemory - 1_073_741_824n).toString());
    expect(mockState.overview.capacity.ct_memory_available.value_bytes).toBe((beforeContainerMemory - 1_073_741_824n).toString());
  });
});

function previewFor(targetVersion: string): ActionPreview {
  return { target_version: targetVersion, confirmation_token: "synthetic", expires_at: "2026-01-15T10:25:00Z", impact: { summary_code: "no_active_lease", affected_session_count: 0, affected_image_references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, changes: [] } };
}
