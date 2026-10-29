import { afterEach, describe, expect, it } from "vitest";
import { mockState } from "./mockData";
import { getVM, ManagementRequestError, previewImageAction, previewMaintenance, previewVMAction, startImageImport, submitImageAction, submitMaintenance, submitVMAction, uploadImageImportContent } from "./api";

describe("mock image uploads", () => {
  let snapshot = structuredClone(mockState);

  afterEach(() => {
    Object.assign(mockState, snapshot);
    snapshot = structuredClone(mockState);
  });

  it("rejects unknown targets and previews the requested VM's current image references", async () => {
    await expect(getVM("missing-vm")).rejects.toMatchObject({ code: "not_found" });
    await expect(previewVMAction("missing-vm", "restart")).rejects.toMatchObject({ code: "not_found" });
    await expect(previewImageAction("missing-image", "retire")).rejects.toMatchObject({ code: "not_found" });
    const vm = mockState.vms[0];
    const image = mockState.images.find((candidate) => candidate.fingerprint === vm.image.fingerprint)!;
    image.references.active = 7;
    const preview = await previewVMAction(vm.task_id, "restart", vm);
    expect(preview.impact.affected_image_references).toEqual(image.references);
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
    const preview = await previewVMAction("task_example_01", "restart", mockState.vms[0], { alpha: 1, beta: 2 });
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
    const image = mockState.images.find((candidate) => candidate.fingerprint === vm.image.fingerprint)!;
    const beforeImageActive = image.references.active;
    const beforeImageCached = image.references.cached;
    const beforeCpu = mockState.overview.capacity.logical_cpu_remaining_vcpu.value!;
    const beforeMemory = BigInt(mockState.overview.capacity.logical_memory_remaining.value_bytes!);
    const releasePreview = await previewVMAction(vm.task_id, "release", vm);
    const release = await submitVMAction(vm.task_id, "release", "Align aggregate state", releasePreview, vm.generation);

    expect(release.status).toBe("succeeded");
    expect(mockState.overview.task_counts.active).toBe(beforeActive - 1);
    expect(mockState.overview.task_counts.cached).toBe(beforeCached + 1);
    expect(vm.state).toBe("cached");
    expect(vm.allowed_actions).toEqual(["release", "destroy"]);
    const cachedResizePreview = await previewVMAction(vm.task_id, "resize", vm, { cpu_vcpu: 8 });
    await expect(submitVMAction(vm.task_id, "resize", "Resize a cached VM", cachedResizePreview, vm.generation, { cpu_vcpu: 8 }, "synthetic-cached-resize")).rejects.toMatchObject({ code: "action_not_allowed" });
    expect(image.references.active).toBe(Math.max(0, beforeImageActive - 1));
    expect(image.references.cached).toBe(beforeImageCached + 1);
    expect(mockState.overview.capacity.logical_cpu_remaining_vcpu.value).toBe(beforeCpu + vm.resources.cpu_vcpu!);
    expect(mockState.overview.capacity.logical_memory_remaining.value_bytes).toBe((beforeMemory + BigInt(vm.resources.memory_bytes!)).toString());

    const destroyPreview = await previewVMAction(vm.task_id, "destroy", vm);
    const destroy = await submitVMAction(vm.task_id, "destroy", "Remove the cached demo VM", destroyPreview, vm.generation, {}, "synthetic-destroy-operation");
    expect(destroy.status).toBe("succeeded");
    expect(vm.state).toBe("absent");
    expect(vm.allowed_actions).toEqual([]);
    expect(image.references.cached).toBe(beforeImageCached);
    expect(mockState.overview.capacity.physical_disk_available.value_bytes).toBeNull();

    const activeVm = mockState.vms[1];
    const activeImage = mockState.images.find((candidate) => candidate.fingerprint === activeVm.image.fingerprint)!;
    const activeCpu = mockState.overview.capacity.logical_cpu_remaining_vcpu.value!;
    const activeMemory = BigInt(mockState.overview.capacity.logical_memory_remaining.value_bytes!);
    const activeImageRefs = activeImage.references.active;
    const activeDestroyPreview = await previewVMAction(activeVm.task_id, "destroy", activeVm);
    await submitVMAction(activeVm.task_id, "destroy", "Remove the active demo VM", activeDestroyPreview, activeVm.generation, {}, "synthetic-destroy-active-operation");
    expect(activeVm.state).toBe("absent");
    expect(activeImage.references.active).toBe(Math.max(0, activeImageRefs - 1));
    expect(mockState.overview.capacity.logical_cpu_remaining_vcpu.value).toBe(activeCpu + activeVm.resources.cpu_vcpu!);
    expect(mockState.overview.capacity.logical_memory_remaining.value_bytes).toBe((activeMemory + BigInt(activeVm.resources.memory_bytes!)).toString());
  });

  it("keeps default image references and selection version aligned", async () => {
    const previousDefault = mockState.images.find((image) => image.is_default)!;
    const nextDefault = mockState.images.find((image) => !image.is_default)!;
    const beforeVersion = mockState.image_selection_version;
    const defaultPreview = await previewImageAction(nextDefault.fingerprint, "default", mockState.image_selection_version);
    const operation = await submitImageAction(nextDefault.fingerprint, "default", "Select the approved image", defaultPreview, "synthetic-default-image");

    expect(operation.status).toBe("succeeded");
    expect(nextDefault.is_default).toBe(true);
    expect(nextDefault.references.default).toBe(1);
    expect(previousDefault.is_default).toBe(false);
    expect(previousDefault.references.default).toBe(0);
    expect(mockState.image_selection_version).not.toBe(beforeVersion);
    expect(operation.result?.default_fingerprint).toBe(nextDefault.fingerprint);
  });

  it("applies image lifecycle effects and blocks retired default selection", async () => {
    const retired = mockState.images.find((image) => image.state === "retired")!;
    await expect(previewImageAction(retired.fingerprint, "default", mockState.image_selection_version)).rejects.toMatchObject({ code: "image_not_applicable" });

    const candidate = mockState.images.find((image) => !image.is_default && image.state === "available")!;
    const beforeCatalogVersion = mockState.image_catalog_version;
    const retirePreview = await previewImageAction(candidate.fingerprint, "retire", candidate.resource_version);
    const retire = await submitImageAction(candidate.fingerprint, "retire", "Retire the demo image", retirePreview, "synthetic-image-retire");
    expect(retire.status).toBe("succeeded");
    expect(candidate.state).toBe("retired");
    expect(mockState.image_catalog_version).not.toBe(beforeCatalogVersion);

    const deletePreview = await previewImageAction(retired.fingerprint, "delete", retired.resource_version);
    const deleted = await submitImageAction(retired.fingerprint, "delete", "Delete the retired demo image", deletePreview, "synthetic-image-delete");
    expect(deleted.status).toBe("succeeded");
    expect(mockState.images.some((image) => image.fingerprint === retired.fingerprint)).toBe(false);
    expect(deleted.result?.image_fingerprint).toBe(retired.fingerprint);
    const retriedDelete = await submitImageAction(retired.fingerprint, "delete", "Delete the retired demo image", deletePreview, "synthetic-image-delete");
    expect(retriedDelete).toBe(deleted);
  });

  it("updates available capacity when a VM is resized", async () => {
    const vm = mockState.vms[0];
    const beforeCpu = mockState.overview.capacity.logical_cpu_remaining_vcpu.value!;
    const beforeMemory = BigInt(mockState.overview.capacity.logical_memory_remaining.value_bytes!);
    const beforeContainerMemory = BigInt(mockState.overview.capacity.ct_memory_available.value_bytes!);
    const nextMemory = (BigInt(vm.resources.memory_bytes!) + 1_073_741_824n).toString();
    const resizeParameters = { cpu_vcpu: vm.resources.cpu_vcpu! + 2, memory_bytes: nextMemory, disk_bytes: vm.resources.disk_bytes };
    const resizePreview = await previewVMAction(vm.task_id, "resize", vm, resizeParameters);
    const operation = await submitVMAction(vm.task_id, "resize", "Increase demo resources", resizePreview, vm.generation, resizeParameters, "synthetic-resize-operation");

    expect(operation.status).toBe("succeeded");
    expect(mockState.overview.capacity.logical_cpu_remaining_vcpu.value).toBe(beforeCpu - 2);
    expect(mockState.overview.capacity.logical_memory_remaining.value_bytes).toBe((beforeMemory - 1_073_741_824n).toString());
    expect(mockState.overview.capacity.ct_memory_available.value_bytes).toBe((beforeContainerMemory - 1_073_741_824n).toString());
  });
  it("rejects stale VM previews and mismatched confirmation tokens", async () => {
    const vm = mockState.vms[0];
    await expect(previewVMAction(vm.task_id, "restart", { generation: vm.generation, resource_version: "vm:task_example_01:stale" })).rejects.toMatchObject({ code: "stale_target" });
    await expect(previewVMAction(vm.task_id, "restart", { generation: "stale-generation", resource_version: vm.resource_version })).rejects.toMatchObject({ code: "stale_target" });
    const preview = await previewVMAction(vm.task_id, "restart", vm);
    await expect(submitVMAction(vm.task_id, "restart", "Reject a forged confirmation", { ...preview, confirmation_token: "synthetic-confirmation-forged" }, vm.generation, {}, "synthetic-forged-preview")).rejects.toMatchObject({ code: "stale_target" });

    const freshPreview = await previewVMAction(vm.task_id, "restart", vm);
    await submitVMAction(vm.task_id, "restart", "Advance the VM resource version", freshPreview, vm.generation, {}, "synthetic-advance-vm");
    await expect(submitVMAction(vm.task_id, "restart", "Reject an old VM preview", freshPreview, vm.generation, {}, "synthetic-stale-vm")).rejects.toMatchObject({ code: "stale_target" });

    const generationPreview = await previewVMAction(vm.task_id, "restart", vm);
    await expect(submitVMAction(vm.task_id, "restart", "Reject a mismatched generation", generationPreview, "wrong-generation", {}, "synthetic-wrong-generation")).rejects.toMatchObject({ code: "stale_target" });
  });

  it("rejects stale image and maintenance previews", async () => {
    const candidate = mockState.images.find((image) => !image.is_default && image.state === "available")!;
    await expect(previewImageAction(candidate.fingerprint, "retire", "image:stale")).rejects.toMatchObject({ code: "stale_target" });
    await expect(previewMaintenance("reconcile", "system:stale")).rejects.toMatchObject({ code: "stale_target" });
    const imagePreview = await previewImageAction(candidate.fingerprint, "retire", candidate.resource_version);
    await submitImageAction(candidate.fingerprint, "retire", "Advance the image resource version", imagePreview, "synthetic-advance-image");
    await expect(submitImageAction(candidate.fingerprint, "retire", "Reject an old image preview", imagePreview, "synthetic-stale-image")).rejects.toMatchObject({ code: "stale_target" });

    const maintenancePreview = await previewMaintenance("reconcile", mockState.system.resource_version);
    await submitMaintenance("reconcile", "Advance the maintenance resource version", maintenancePreview, "synthetic-advance-maintenance");
    await expect(submitMaintenance("reconcile", "Reject an old maintenance preview", maintenancePreview, "synthetic-stale-maintenance")).rejects.toMatchObject({ code: "stale_target" });
  });

  it("rejects stale image imports without creating an operation and preserves retries", async () => {
    const operationCount = mockState.operations.length;
    await expect(startImageImport({ operation_id: "synthetic-stale-import", reason: "Reject a stale catalog target", target_version: "image-catalog:stale", version_label: "2026.02", source: { kind: "trusted_remote", source_id: "ubuntu-staging", expected_fingerprint: "d".repeat(64) } })).rejects.toMatchObject({ code: "stale_target" });
    expect(mockState.operations).toHaveLength(operationCount);

    const request = { operation_id: "synthetic-import-retry", reason: "Import against the current catalog", target_version: mockState.image_catalog_version, version_label: "2026.03", source: { kind: "trusted_remote" as const, source_id: "ubuntu-staging", expected_fingerprint: "d".repeat(64) } };
    const operation = await startImageImport(request);
    expect(mockState.image_catalog_version).not.toBe(request.target_version);
    expect(await startImageImport(request)).toBe(operation);
  });
});
