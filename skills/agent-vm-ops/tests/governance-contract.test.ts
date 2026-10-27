import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

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
  expect(json(".github/release-contract.json").product.manifest_format).toBe(3);
  expect(json(".github/release-failure-notification.json").transport_selection.status).toBe("unselected");
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
});

test("notification routing is metadata-only and fail-closed before transport selection", () => {
  const script = resolve(root, "skills/agent-vm-ops/scripts/failure-notification.ts");
  const result = Bun.spawnSync([process.execPath, script, "--class", "generic"], {
    cwd: root,
    env: {
      ...process.env,
      FAILED_REPOSITORY: "IvanLi-CN/agent-vm",
      FAILED_WORKFLOW: "Notification smoke",
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
  expect(read(".github/workflows/notify-failed-workflow.yml")).toContain("ref: ${{ github.event.repository.default_branch }}");
  expect(read(".github/workflows/notify-failed-release.yml")).toContain("workflows: [Release]");
});
