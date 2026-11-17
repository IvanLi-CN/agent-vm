import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { parse } from "yaml";

const root = resolve(import.meta.dir, "../../..");
const read = (path: string) => readFileSync(resolve(root, path), "utf8");

test("product release does not build or publish guest images", () => {
  const workflow = parse(read(".github/workflows/release.yml"));
  const script = read("skills/agent-vm-ops/scripts/release.ts");

  expect(workflow.jobs["guest-images"]).toBeUndefined();
  expect(workflow.jobs.release.needs).toBeUndefined();
  expect(read(".github/workflows/release.yml")).not.toContain("image-preview.yml");
  expect(script).not.toContain("AGENT_VM_RELEASE_IMAGE_INPUT");
  expect(script).not.toContain("stageGuestImageAssets");
});

test("guest image publication is manual and waits for the parallel build matrix", () => {
  const release = parse(read(".github/workflows/image-release.yml"));
  const preview = parse(read(".github/workflows/image-preview.yml"));

  expect(release.on.pull_request).toBeUndefined();
  expect(release.on.workflow_dispatch).toBeDefined();
  expect(release.on.workflow_dispatch.inputs.confirm_immutable_releases.type).toBe("boolean");
  expect(release.on.workflow_dispatch.inputs.confirm_immutable_releases.required).toBe(true);
  expect(release.on.workflow_dispatch.inputs.confirm_immutable_releases.default).toBe(false);
  expect(release.jobs["validate-dispatch"]).toBeDefined();
  expect(release.jobs["validate-dispatch"].steps.some((step: { run?: string }) => step.run?.includes("IMMUTABLE_RELEASES_CONFIRMED"))).toBe(true);
  expect(release.jobs["build-images"].needs).toBe("validate-dispatch");
  expect(release.jobs.publish.steps.some((step: { name?: string }) => step.name === "Require protected main dispatch")).toBe(true);
  expect(release.jobs["build-images"].uses).toBe("./.github/workflows/image-preview.yml");
  expect(release.jobs.publish.needs).toBe("build-images");
  expect(release.jobs.publish.steps.some((step: { run?: string }) => step.run?.includes("releases/${IMAGE_RELEASE_ID}") && step.run?.includes("draft=false"))).toBe(true);
  expect(release.jobs.publish.steps.some((step: { run?: string }) => step.run?.includes("make_latest=false"))).toBe(true);
  expect(release.jobs.publish.steps.some((step: { run?: string }) => step.run?.includes("releases/${IMAGE_RELEASE_ID}/assets") && step.run?.includes("--method POST"))).toBe(true);
  expect(read(".github/workflows/image-release.yml")).toContain("verify_release_assets.py");
  expect(read(".github/workflows/image-release.yml")).toContain("releases?per_page=100");
  expect(read(".github/workflows/image-release.yml")).toContain("IMAGE_RELEASE_ID");
  expect(read(".github/workflows/image-release.yml")).toContain("IMAGE_RELEASE_ALREADY_PUBLISHED");
  expect(read(".github/workflows/image-release.yml")).toContain("verify_assets.py");
  expect(read(".github/workflows/image-release.yml")).toContain("Revalidate draft immediately before publication");
  expect(release.jobs.publish.steps.some((step: { run?: string }) => step.run?.includes('= "33"'))).toBe(true);

  expect(preview.on.pull_request).toBeDefined();
  expect(preview.on.workflow_call).toBeDefined();
  expect(preview.jobs["resolve-source"].outputs.revision).toBe("${{ steps.source.outputs.revision }}");
  expect(preview.jobs["lock-check"].needs).toBe("resolve-source");
  expect(preview.jobs["lock-check"].steps.some((step: { with?: { ref?: string } }) => step.with?.ref === "${{ needs.resolve-source.outputs.revision }}")).toBe(true);
  expect(preview.jobs["common-base"].needs).toEqual(["resolve-source", "lock-check"]);
  expect(preview.jobs["common-base"].strategy.matrix.variant).toEqual(["privileged", "unprivileged"]);
  expect(preview.jobs["common-base"].strategy["max-parallel"]).toBe(2);
  expect(preview.jobs["common-base"].steps.some((step: { run?: string }) => step.run?.includes("--base-only"))).toBe(true);
  expect(preview.jobs["common-base"].steps.some((step: { with?: { name?: string } }) => step.with?.name === "guest-common-base-${{ matrix.variant }}")).toBe(true);
  expect(preview.jobs.build.needs).toEqual(["resolve-source", "lock-check", "common-base"]);
  expect(preview.jobs.build.steps.some((step: { with?: { ref?: string } }) => step.with?.ref === "${{ needs.resolve-source.outputs.revision }}")).toBe(true);
  expect(preview.jobs.build.steps.some((step: { uses?: string }) => step.uses === "actions/download-artifact@v4")).toBe(true);
  expect(preview.jobs.build.strategy["fail-fast"]).toBe(false);
  expect(preview.jobs.build.strategy["max-parallel"]).toBe(3);
});
