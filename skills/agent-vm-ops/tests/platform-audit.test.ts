import { test } from "bun:test";

import { evaluatePlatform, type PlatformSnapshot } from "../scripts/platform-audit.ts";

const quality = {
  required_checks: ["PR gate (ubuntu-latest)"],
  platform_alignment: { status: "enabled" },
};
const release = { platform_preconditions: { status: "enabled" } };

const alignedSnapshot = (): PlatformSnapshot => ({
  branch_protection: {
    required_pull_request_reviews: {},
    required_status_checks: { strict: true, checks: [
      { context: "PR gate (ubuntu-latest)", app_id: 15368 },
    ] },
    enforce_admins: { enabled: true },
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

test("platform audit rejects an untrusted or bypassable branch configuration", () => {
  const invalid = alignedSnapshot();
  invalid.branch_protection.required_status_checks = {
    strict: false,
    checks: [
      { context: "PR gate (ubuntu-latest)", app_id: -1 },
    ],
  };
  invalid.branch_protection.enforce_admins = { enabled: false };
  const result = evaluatePlatform(invalid, quality, release);
  if (result.status !== "blocked" || !result.reasons.includes("administrators can bypass main protection")) throw new Error("platform audit accepted bypassable protection");
});

test("an empty release history is valid for the first immutable publication", () => {
  const snapshot = alignedSnapshot();
  snapshot.releases = [];
  const result = evaluatePlatform(snapshot, quality, release);
  if (result.status !== "aligned" || !result.evidence.immutable_release_observed || result.evidence.immutable_release_history) throw new Error("first-release empty history was treated as a missing precondition");
});

test("draft releases do not block recovery while published releases remain immutable", () => {
  const snapshot = alignedSnapshot();
  snapshot.releases = [{ draft: true, immutable: false }, { draft: false, immutable: true }];
  const result = evaluatePlatform(snapshot, quality, release);
  if (result.status !== "aligned" || !result.evidence.immutable_release_observed || !result.evidence.immutable_release_history) throw new Error("a recoverable draft was treated as a mutable published release");
});

test("enabled immutable releases establish a cutover for legacy published releases", () => {
  const snapshot = alignedSnapshot();
  snapshot.releases = [{ draft: false, immutable: false }];
  snapshot.immutable_releases = { enabled: true };
  const result = evaluatePlatform(snapshot, quality, release);
  if (result.status !== "aligned" || !result.evidence.immutable_release_observed || result.evidence.immutable_release_history || !result.evidence.immutable_releases_enabled) throw new Error("immutable release cutover did not accept legacy history with future enforcement");
});
