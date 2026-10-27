#!/usr/bin/env bun

/** Read-only audit of the GitHub controls required before product release. */

import { readFileSync } from "node:fs";
import { join, resolve } from "node:path";

export interface PlatformSnapshot {
  branch_protection: any | null;
  required_signatures: any | null;
  rulesets: any[];
  releases: any[];
  errors: string[];
}

export interface PlatformAudit {
  status: "aligned" | "blocked" | "unavailable";
  reasons: string[];
  evidence: {
    contract_status: { quality: unknown; release: unknown };
    branch_protection: boolean;
    pull_request_required: boolean;
    required_checks: string[];
    required_checks_aligned: boolean;
    signed_commits_required: boolean;
    force_push_blocked: boolean;
    deletion_blocked: boolean;
    immutable_release_observed: boolean;
    immutable_release_history: boolean;
    required_checks_source_aligned: boolean;
    strict_checks: boolean;
    administrators_enforced: boolean;
    protection_source: "branch-protection" | "ruleset" | "none";
  };
}

function requiredChecks(protection: any): string[] {
  const checks = protection?.required_status_checks;
  if (!checks || typeof checks !== "object") return [];
  const contexts = Array.isArray(checks.contexts) ? checks.contexts.filter((value: unknown): value is string => typeof value === "string") : [];
  const checkObjects = Array.isArray(checks.checks)
    ? checks.checks.flatMap((value: any) => typeof value?.context === "string" ? [value.context] : [])
    : [];
  return [...new Set([...contexts, ...checkObjects])].sort();
}

const GITHUB_ACTIONS_APP_ID = 15368;

function branchChecksSourceAligned(protection: any, declared: Set<string>): boolean {
  const checks = protection?.required_status_checks;
  if (!checks || checks.strict !== true || !Array.isArray(checks.checks)) return false;
  const byContext = new Map<string, any>(checks.checks.flatMap((item: any) => typeof item?.context === "string" ? [[item.context, item]] : []));
  return [...declared].every((name) => byContext.get(name)?.app_id === GITHUB_ACTIONS_APP_ID);
}

function rulesetAppliesToMain(ruleset: any): boolean {
  if (ruleset?.enforcement !== "active") return false;
  const include = ruleset?.conditions?.ref_name?.include;
  return !Array.isArray(include) || include.includes("refs/heads/main") || include.includes("main");
}

function rulesetControls(snapshot: PlatformSnapshot, declared: Set<string>): { matches: boolean; checks: string[]; signatures: boolean; pullRequest: boolean; forcePush: boolean; deletion: boolean; strict: boolean } {
  for (const ruleset of snapshot.rulesets.filter(rulesetAppliesToMain)) {
    const rules = Array.isArray(ruleset.rules) ? ruleset.rules : [];
    const required = rules.find((rule: any) => rule?.type === "required_status_checks");
    const checkItems = required?.parameters?.required_status_checks;
    const checks = Array.isArray(checkItems) ? checkItems.flatMap((item: any) => typeof item?.context === "string" ? [item.context] : []) : [];
    const trusted = Array.isArray(checkItems) && [...declared].every((name) => checkItems.some((item: any) => item?.context === name && item?.integration_id === GITHUB_ACTIONS_APP_ID));
    const strict = required?.parameters?.strict_required_status_checks_policy === true;
    const types = new Set(rules.map((rule: any) => rule?.type));
    const pullRequest = types.has("pull_request");
    const forcePush = types.has("non_fast_forward");
    const deletion = types.has("deletion");
    const signatures = types.has("required_signatures");
    if (trusted && strict && pullRequest && forcePush && deletion && signatures) return { matches: true, checks, signatures, pullRequest, forcePush, deletion, strict };
  }
  return { matches: false, checks: [], signatures: false, pullRequest: false, forcePush: false, deletion: false, strict: false };
}

export function evaluatePlatform(
  snapshot: PlatformSnapshot,
  qualityContract: any,
  releaseContract: any,
): PlatformAudit {
  const reasons: string[] = [];
  const declaredRequired = new Set<string>(qualityContract?.required_checks ?? []);
  const observedChecks = requiredChecks(snapshot.branch_protection);
  const branchSourceAligned = branchChecksSourceAligned(snapshot.branch_protection, declaredRequired);
  const branchChecksAligned = [...declaredRequired].every((check) => observedChecks.includes(check));
  const ruleset = rulesetControls(snapshot, declaredRequired);
  const branchProtection = !!snapshot.branch_protection || ruleset.matches;
  const checksAligned = (branchChecksAligned && branchSourceAligned) || ruleset.matches;
  const pullRequestRequired = !!snapshot.branch_protection?.required_pull_request_reviews || ruleset.pullRequest;
  const signedCommitsRequired = snapshot.required_signatures?.enabled === true || ruleset.signatures;
  const forcePushBlocked = snapshot.branch_protection?.allow_force_pushes?.enabled === false || ruleset.forcePush;
  const deletionBlocked = snapshot.branch_protection?.allow_deletions?.enabled === false || ruleset.deletion;
  const strictChecks = snapshot.branch_protection?.required_status_checks?.strict === true || ruleset.strict;
  const administratorsEnforced = snapshot.branch_protection?.enforce_admins?.enabled === true;
  const immutableReleaseHistory = snapshot.releases.length > 0 && snapshot.releases.every((release) => release?.immutable === true);
  // An empty release history is the valid first-release state; immutability is
  // verified on the first publication and on every subsequent release.
  const immutableReleaseObserved = snapshot.releases.length === 0 || immutableReleaseHistory;
  const contractStatus = {
    quality: qualityContract?.platform_alignment?.status,
    release: releaseContract?.platform_preconditions?.status,
  };

  if (snapshot.errors.length) reasons.push(...snapshot.errors);
  if (contractStatus.quality !== "enabled" || contractStatus.release !== "enabled") reasons.push("repository platform contracts are not enabled");
  if (!branchProtection) reasons.push("main branch protection is absent or unreadable");
  if (!pullRequestRequired) reasons.push("main does not require pull requests");
  if (!checksAligned) reasons.push("declared PR required checks are not aligned with branch protection");
  if (!strictChecks) reasons.push("required checks do not require the current branch to be up to date");
  if (!administratorsEnforced && !ruleset.matches) reasons.push("administrators can bypass main protection");
  if (!signedCommitsRequired) reasons.push("verified signed commits are not required");
  if (!forcePushBlocked) reasons.push("force pushes are not blocked");
  if (!deletionBlocked) reasons.push("branch deletion is not blocked");
  if (!immutableReleaseObserved) reasons.push("an existing GitHub Release is not immutable");

  const unavailable = snapshot.errors.some((error) => error.startsWith("api:"));
  const status = unavailable ? "unavailable" : reasons.length ? "blocked" : "aligned";
  return {
    status,
    reasons,
    evidence: {
      contract_status: contractStatus,
      branch_protection: branchProtection,
      pull_request_required: pullRequestRequired,
      required_checks: observedChecks,
      required_checks_aligned: checksAligned,
      signed_commits_required: signedCommitsRequired,
      force_push_blocked: forcePushBlocked,
      deletion_blocked: deletionBlocked,
      immutable_release_observed: immutableReleaseObserved,
      immutable_release_history: immutableReleaseHistory,
      required_checks_source_aligned: branchSourceAligned || ruleset.matches,
      strict_checks: strictChecks,
      administrators_enforced: administratorsEnforced || ruleset.matches,
      protection_source: ruleset.matches ? "ruleset" : snapshot.branch_protection ? "branch-protection" : "none",
    },
  };
}

function option(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index < 0 ? undefined : process.argv[index + 1];
}

function ghJson(repository: string, endpoint: string): { ok: true; value: any } | { ok: false } {
  const result = Bun.spawnSync(["gh", "api", `repos/${repository}/${endpoint}`], { stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) return { ok: false };
  try { return { ok: true, value: JSON.parse(new TextDecoder().decode(result.stdout)) }; } catch { return { ok: false }; }
}

export function auditRepository(repository: string): PlatformAudit {
  const errors: string[] = [];
  const protection = ghJson(repository, "branches/main/protection");
  const signatures = ghJson(repository, "branches/main/protection/required_signatures");
  const rulesets = ghJson(repository, "rulesets?includes_parents=true");
  const releases = ghJson(repository, "releases?per_page=10");
  if (!protection.ok) errors.push("api:branch_protection");
  if (!signatures.ok) errors.push("api:required_signatures");
  if (!rulesets.ok) errors.push("api:rulesets");
  if (!releases.ok) errors.push("api:releases");
  const root = resolve(import.meta.dir, "../../..");
  const qualityContract = JSON.parse(readFileSync(join(root, ".github/quality-gates.json"), "utf8"));
  const releaseContract = JSON.parse(readFileSync(join(root, ".github/release-contract.json"), "utf8"));
  return evaluatePlatform({
    branch_protection: protection.ok ? protection.value : null,
    required_signatures: signatures.ok ? signatures.value : null,
    rulesets: rulesets.ok && Array.isArray(rulesets.value) ? rulesets.value : [],
    releases: releases.ok && Array.isArray(releases.value) ? releases.value : [],
    errors,
  }, qualityContract, releaseContract);
}

if (import.meta.main) {
  const repository = option("--repository") ?? process.env.GITHUB_REPOSITORY;
  if (!repository) throw new Error("--repository or GITHUB_REPOSITORY is required");
  const result = auditRepository(repository);
  console.log(JSON.stringify(result));
  if (result.status !== "aligned") process.exitCode = result.status === "unavailable" ? 3 : 2;
}
