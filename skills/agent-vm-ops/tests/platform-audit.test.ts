import { test } from "bun:test";

import { evaluatePlatform, type PlatformSnapshot } from "../scripts/platform-audit.ts";

const quality = {
  required_checks: ["PR gate (ubuntu-latest)", "PR gate (macos-latest)"],
  platform_alignment: { status: "enabled" },
};
const release = { platform_preconditions: { status: "enabled" } };

const alignedSnapshot = (): PlatformSnapshot => ({
  branch_protection: {
    required_pull_request_reviews: {},
    required_status_checks: { contexts: ["PR gate (ubuntu-latest)", "PR gate (macos-latest)"] },
    allow_force_pushes: { enabled: false },
    allow_deletions: { enabled: false },
  },
  required_signatures: { enabled: true },
  rulesets: [],
  releases: [{ immutable: true }],
  errors: [],
});

test("platform audit distinguishes aligned controls from the current blocked contract", () => {
  if (evaluatePlatform(alignedSnapshot(), quality, release).status !== "aligned") throw new Error("aligned platform fixture was rejected");
  const current = evaluatePlatform({ ...alignedSnapshot(), errors: [], releases: [] }, { ...quality, platform_alignment: { status: "audit-required" } }, { platform_preconditions: { status: "audit-required" } });
  if (current.status !== "blocked" || !current.reasons.includes("repository platform contracts are not enabled")) throw new Error("audit-required platform was not blocked");
});
