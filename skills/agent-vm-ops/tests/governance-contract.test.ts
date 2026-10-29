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
  expect(quality.required_checks).toEqual(["PR gate (ubuntu-latest)", "PR gate (macos-latest)"]);
  expect(quality.queue_policy.required_pull_request_events).toEqual(["opened", "synchronize", "reopened", "ready_for_review"]);
  expect(quality.queue_policy.queue).toBe("max");
  expect(json(".github/release-contract.json").product.manifest_format).toBe(4);
  expect(json(".github/release-failure-notification.json").transport_selection.status).toBe("unselected");
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

test("PR scheduling is exact, non-preemptive, and separate from Main", () => {
  const pr = read(".github/workflows/ci.yml");
  const main = read(".github/workflows/ci-main.yml");
  expect(pr).toContain("name: CI (PR)");
  expect(pr).toContain("types: [opened, synchronize, reopened, ready_for_review]");
  expect(pr).toContain("types: [checks_requested]");
  expect(pr).toContain("group: pr-${{ github.event.pull_request.number || github.event.merge_group.head_sha }}");
  expect(pr).toContain("cancel-in-progress: false");
  expect(pr).toContain("name: PR gate");
  expect(main).toContain("name: CI (Main)");
  expect(main).toContain("branches: [main]");
  expect(main).toContain("name: Main gate");
  expect(main).toContain("group: main");
  expect(main).toContain("max-parallel: 1");
});

test("demo deployment is serialized and never exposes credentials to PR builds", () => {
  const workflow = parse(read(".github/workflows/demo-deploy.yml"));
  expect(workflow.concurrency["cancel-in-progress"]).toBe(false);
  expect(workflow.jobs.deploy.if).toContain("github.event_name != 'pull_request'");
  expect(workflow.jobs.deploy.if).toContain("refs/heads/main");
  expect(JSON.stringify(workflow.jobs.build)).not.toContain("EDGEONE_API_TOKEN");
  const steps = workflow.jobs.deploy.steps;
  const names = steps.map((step: { name?: string }) => step.name);
  expect(names.indexOf("Install locked deployment tools")).toBeLessThan(names.indexOf("Verify existing EdgeOne project"));
  expect(names.indexOf("Reject stale main revision")).toBeLessThan(names.indexOf("Deploy to EdgeOne Makers"));
  expect(names.indexOf("Verify existing EdgeOne project")).toBeLessThan(names.indexOf("Deploy to EdgeOne Makers"));
});

test("notification routing is metadata-only and fail-closed before transport selection", () => {
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
  expect(result.exitCode).toBe(78);
  expect(new TextDecoder().decode(result.stdout)).toContain('"reason":"transport_unselected"');
  expect(new TextDecoder().decode(result.stdout)).toContain('"event":"pull_request"');
  expect(new TextDecoder().decode(result.stdout)).toContain('"ref":"refs/pull/123/merge"');
  expect(read(".github/workflows/notify-failed-workflow.yml")).toContain("ref: ${{ github.event.repository.default_branch }}");
  expect(read(".github/workflows/notify-failed-release.yml")).toContain("workflows: [Release]");
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
