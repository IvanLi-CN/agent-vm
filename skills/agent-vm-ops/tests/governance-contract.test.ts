import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { parse } from "yaml";

const root = resolve(import.meta.dir, "../../..");
const read = (path: string) => readFileSync(resolve(root, path), "utf8");
const json = (path: string) => JSON.parse(read(path));

test("the five project topics have locked local surfaces", () => {
  const lock = json("skills-lock.json");
  for (const slug of ["semver-change-governance", "quality-gates", "release-failure-notification"]) {
    expect(lock.skills[slug]?.source).toBe("IvanLi-CN/style-playbook-skills");
    expect(read(`.agents/skills/${slug}/SKILL.md`)).toContain(`slug: ${slug}`);
  }
  const quality = json(".github/quality-gates.json");
  expect(quality.required_checks).toEqual(["PR gate (ubuntu-latest)"]);
  expect(quality.informational_checks).toEqual(["Main gate (ubuntu-latest)", "Build demo artifact"]);
  expect(quality.release_checks).toEqual(["Main gate (ubuntu-latest)"]);
  expect(quality.queue_policy.required_pull_request_events).toEqual(["opened", "synchronize", "reopened", "ready_for_review"]);
  expect(quality.queue_policy.queue).toBe("max");
  expect(json(".github/release-contract.json").product.manifest_format).toBe(4);
  expect(json(".github/release-failure-notification.json").transport_selection.status).toBe("selected");
  const topics = json(".github/topic-contract.json").topics;
  expect(Object.keys(topics).sort()).toEqual([
    "github-actions-scheduling",
    "manual-version-release-delivery",
    "quality-gates",
    "release-failure-notification",
    "semver-change-governance",
  ]);
  for (const topic of Object.values(topics) as Array<{ status: string; surface: string }>) {
    expect(topic.status).toBe("implemented");
    expect(topic.surface.length).toBeGreaterThan(0);
  }
});

test("PR scheduling is bounded and separate from Main", () => {
  const pr = read(".github/workflows/ci.yml");
  const main = read(".github/workflows/ci-main.yml");
  expect(pr).toContain("name: CI (PR)");
  expect(pr).not.toContain("macos-latest");
  expect(main).not.toContain("macos-latest");
  const pw = parse(pr);
  const mw = parse(main);
  expect(pw.on.pull_request.types).toEqual(["opened", "synchronize", "reopened", "ready_for_review"]);
  expect(pw.on.merge_group.types).toEqual(["checks_requested"]);
  expect(pw.concurrency["cancel-in-progress"]).toBe("${{ github.event_name == 'pull_request' }}");
  expect(mw.concurrency["cancel-in-progress"]).toBe(false);
  expect(pw.jobs["pr-gate"].name).toBe("PR gate");
  expect(mw.jobs["main-gate"].name).toBe("Main gate");
  expect(pw.jobs["pr-gate"].strategy.matrix.os).toEqual(["ubuntu-latest"]);
  expect(mw.jobs["main-gate"].strategy.matrix.os).toEqual(["ubuntu-latest"]);
  expect(pw.jobs.validation["runs-on"]).toBe("ubuntu-latest");
  expect(pw.jobs.heavy["runs-on"]).toBe("ubuntu-latest");
  for (const workflow of [pw, mw]) {
    expect(workflow.jobs.heavy.steps.some((s: any) => s.run === "bun run test:skills")).toBe(true);
    expect(workflow.jobs.validation.steps.find((s: any) => s.name === "Build product packages").if).toBeUndefined();
    expect(JSON.stringify(workflow)).not.toContain("Cache Go build and module data");
  }
  expect(mw.jobs.validation.strategy.matrix.os).toEqual(["ubuntu-latest"]);
  expect(mw.jobs.heavy.strategy.matrix.os).toEqual(["ubuntu-latest"]);
});

test("demo deployment is serialized and never exposes credentials to PR builds", () => {
  const workflow = parse(read(".github/workflows/demo-deploy.yml"));
  expect(workflow.concurrency["cancel-in-progress"]).toBe(false);
  expect(workflow.concurrency.group).toBe("demo-deploy-${{ github.ref }}");
  expect(workflow.jobs.deploy.if).toContain("github.event_name != 'pull_request'");
  expect(workflow.jobs.deploy.if).toContain("refs/heads/main");
  expect(JSON.stringify(workflow.jobs.build)).not.toContain("EDGEONE_API_TOKEN");
  const steps = workflow.jobs.deploy.steps;
  const names = steps.map((step: { name?: string }) => step.name);
  for (const name of ["Install locked deployment tools", "Reject stale main revision", "Verify existing EdgeOne project", "Deploy to EdgeOne Makers"]) expect(names).toContain(name);
  expect(names.indexOf("Install locked deployment tools")).toBeLessThan(names.indexOf("Verify existing EdgeOne project"));
  expect(names.indexOf("Reject stale main revision")).toBeLessThan(names.indexOf("Deploy to EdgeOne Makers"));
  expect(names.indexOf("Verify existing EdgeOne project")).toBeLessThan(names.indexOf("Deploy to EdgeOne Makers"));
});

test("notification routing prepares metadata for the selected transport", () => {
  const script = resolve(root, "skills/agent-vm-ops/scripts/failure-notification.ts");
  const result = Bun.spawnSync([process.execPath, script, "--class", "generic"], {
    cwd: root,
    env: {
      ...process.env,
      FAILED_REPOSITORY: "IvanLi-CN/agent-vm",
      FAILED_WORKFLOW: "CI (PR)",
      FAILED_EVENT: "pull_request",
      FAILED_REF: "refs/pull/123/merge",
      FAILED_HEAD_BRANCH: "feature/example",
      FAILED_RUN_ID: "123",
      FAILED_RUN_ATTEMPT: "1",
      FAILED_HEAD_SHA: "a".repeat(40),
      FAILED_ACTOR: "maintainer",
      FAILED_CONCLUSION: "failure",
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  expect(result.exitCode).toBe(0);
  expect(new TextDecoder().decode(result.stdout)).toContain('"delivery":"prepared"');
  expect(new TextDecoder().decode(result.stdout)).toContain('"event":"pull_request"');
  expect(new TextDecoder().decode(result.stdout)).toContain('"ref":"refs/pull/123/merge"');
  expect(read(".github/workflows/notify-failed-workflow.yml")).toContain("ref: ${{ github.event.repository.default_branch }}");
  expect(read(".github/workflows/notify-failed-release.yml")).toContain("workflows: [Release]");
  expect(read(".github/workflows/notify-failed-release.yml")).toContain("id-token: write");
  expect(read(".github/workflows/notify-failed-release.yml")).toContain("oidrune/.github/workflows/notify.yml@4b5901a8827731ea06a05211dc6edf06e46783bc");
  const notification = json(".github/release-failure-notification.json");
  expect(notification.workflow_inventory["Notification smoke"].class).toBe("excluded");
  expect(notification.workflow_inventory["Notify failed workflow"].class).toBe("excluded");
  expect(notification.workflow_inventory["Notify failed release"].class).toBe("excluded");
  const workflowNames = readdirSync(resolve(root, ".github/workflows"))
    .filter((name) => name.endsWith(".yml"))
    .map((name) => read(`.github/workflows/${name}`).match(/^name: (.+)$/m)?.[1])
    .filter((name): name is string => Boolean(name));
  expect(new Set(workflowNames)).toEqual(new Set(Object.keys(notification.workflow_inventory)));
});
