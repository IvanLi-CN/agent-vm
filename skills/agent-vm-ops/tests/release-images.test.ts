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
  expect(release.jobs.publish.steps.some((step: { name?: string }) => step.name === "Require protected main dispatch")).toBe(true);
  expect(release.jobs["build-images"].uses).toBe("./.github/workflows/image-preview.yml");
  expect(release.jobs.publish.needs).toBe("build-images");
  expect(release.jobs.publish.steps.some((step: { run?: string }) => step.run?.includes("gh release edit"))).toBe(true);
  expect(release.jobs.publish.steps.some((step: { run?: string }) => step.run?.includes('= "33"'))).toBe(true);

  expect(preview.on.pull_request).toBeDefined();
  expect(preview.on.workflow_call).toBeDefined();
  expect(preview.jobs["common-base"].needs).toBe("lock-check");
  expect(preview.jobs["common-base"].strategy.matrix.variant).toEqual(["privileged", "unprivileged"]);
  expect(preview.jobs["common-base"].strategy["max-parallel"]).toBe(2);
  expect(preview.jobs["common-base"].steps.some((step: { run?: string }) => step.run?.includes("--base-only"))).toBe(true);
  expect(preview.jobs["common-base"].steps.some((step: { with?: { name?: string } }) => step.with?.name === "guest-common-base-${{ matrix.variant }}")).toBe(true);
  expect(preview.jobs.build.needs).toEqual(["lock-check", "common-base"]);
  expect(preview.jobs.build.steps.some((step: { uses?: string }) => step.uses === "actions/download-artifact@v4")).toBe(true);
  expect(preview.jobs.build.strategy["fail-fast"]).toBe(false);
  expect(preview.jobs.build.strategy["max-parallel"]).toBe(3);
});
