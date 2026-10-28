#!/usr/bin/env bun

/** Manual, fail-closed GitHub product release entrypoint. */

import { createHash } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";

import { policyRevision, resolveReleaseTarget, parseSemVer } from "./semver.ts";
import { auditRepository } from "./platform-audit.ts";
import {
  GhGithubAdapter,
  GithubReleaseStateStore,
  MirroredReleaseStateStore,
  ReleaseStateStore,
  type ReleaseIdentity,
  type ReleaseEvent,
} from "./release-coordinator.ts";

const args = process.argv.slice(2);
const option = (name: string): string | undefined => {
  const index = args.indexOf(name);
  return index < 0 ? undefined : args[index + 1];
};
const operation = option("--operation") ?? "release";
const target = option("--target");
const reservation = option("--reservation");
const stateRoot = resolve(option("--state-root") ?? "release-state");
const repositoryRoot = resolve(import.meta.dir, "../../..");

function requireOption(name: string): string {
  const value = option(name);
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function sha256(path: string): string {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function runBuilder(version: string, source: string, targetName: string, output: string): void {
  const result = Bun.spawnSync([
    process.execPath,
    join(import.meta.dir, "build_product_release.ts"),
    "--version", version,
    "--source-revision", source,
    "--target", targetName,
    "--output", output,
  ], { cwd: repositoryRoot, stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) throw new Error(new TextDecoder().decode(result.stderr).trim() || "product package build failed");
}

export function assertContractPreconditions(quality: any, release: any): void {
  const statuses = {
    quality: quality.platform_alignment?.status,
    release: release.platform_preconditions?.status,
  };
  if (statuses.quality !== "enabled" || statuses.release !== "enabled") {
    throw new Error(`platform preconditions are not enabled: ${JSON.stringify(statuses)}`);
  }
}

export function assertPlatformPreconditions(root = repositoryRoot): void {
  const quality = JSON.parse(readFileSync(join(root, ".github/quality-gates.json"), "utf8"));
  const release = JSON.parse(readFileSync(join(root, ".github/release-contract.json"), "utf8"));
  assertContractPreconditions(quality, release);
  const repository = process.env.GITHUB_REPOSITORY;
  if (!repository) throw new Error("GITHUB_REPOSITORY is required for the live platform preflight");
  const audit = auditRepository(repository);
  if (audit.status !== "aligned") throw new Error(`live platform audit is ${audit.status}: ${audit.reasons.join("; ")}`);
}

export function successfulMainGate(value: unknown): { id: string } {
  if (!Array.isArray(value)) throw new Error("GitHub check-runs response is invalid");
  const checks = value.filter((item: any) => item?.name === "Main gate" || /^Main gate \(.+\)$/.test(item?.name ?? ""));
  if (checks.length === 0 || checks.some((item: any) => item.status !== "completed" || item.conclusion !== "success")) {
    throw new Error("the fixed Main gate has not completed successfully for the source commit");
  }
  return { id: checks.map((item: any) => String(item.id ?? item.name)).sort().join(",") };
}

function mergedSourcePull(value: unknown, source: string): { number: string; head: string } {
  if (!Array.isArray(value)) throw new Error("GitHub source pull response is invalid");
  const pull = value.find((item: any) => item?.base?.ref === "main" && item?.merged_at && item?.merge_commit_sha === source && item?.head?.sha);
  if (!pull) throw new Error("source commit is not a merged pull request from main");
  return { number: String(pull.number), head: String(pull.head.sha) };
}

function frozenDetails(event: ReleaseEvent): { artifacts: Record<string, string>; assets: Record<string, string> } {
  const detail = event.detail;
  if (!detail || typeof detail.artifacts !== "object" || typeof detail.assets !== "object" || !detail.artifacts || !detail.assets || Array.isArray(detail.artifacts) || Array.isArray(detail.assets)) throw new Error("release reservation has no frozen artifact details");
  const artifacts = detail.artifacts as Record<string, unknown>;
  const assets = detail.assets as Record<string, unknown>;
  if (!Object.keys(artifacts).length || Object.keys(artifacts).length !== Object.keys(assets).length || Object.keys(artifacts).some((name) => typeof artifacts[name] !== "string" || !/^[0-9a-f]{64}$/.test(artifacts[name] as string) || typeof assets[name] !== "string" || !(assets[name] as string))) throw new Error("release reservation frozen artifact details are invalid");
  return { artifacts: artifacts as Record<string, string>, assets: assets as Record<string, string> };
}

async function verifyDownloaded(adapter: GhGithubAdapter, tag: string, digests: Record<string, string>): Promise<void> {
  mkdirSync(stateRoot, { recursive: true, mode: 0o750 });
  const directory = mkdtempSync(join(stateRoot, "download-"));
  try {
    await adapter.downloadRelease(tag, directory);
    for (const [name, expected] of Object.entries(digests)) {
      const path = join(directory, name);
      if (!existsSync(path) || sha256(path) !== expected) throw new Error(`downloaded release asset failed digest verification: ${name}`);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

function verifyReleaseView(value: unknown, tag: string, expectedDraft: boolean, assets: Record<string, string>): any {
  if (!value || typeof value !== "object") throw new Error("GitHub release response is invalid");
  const release = value as { tagName?: unknown; isDraft?: unknown; isPrerelease?: unknown; immutable?: unknown; assets?: unknown };
  if (release.tagName !== tag || release.isDraft !== expectedDraft || !Array.isArray(release.assets)) throw new Error("GitHub release identity or draft state is invalid");
  if (!expectedDraft && (release.isPrerelease === true || release.immutable !== true)) throw new Error("published GitHub release is not immutable or is a prerelease");
  const names = new Set(release.assets.flatMap((item: any) => typeof item?.name === "string" ? [item.name] : []));
  for (const name of Object.keys(assets)) if (!names.has(name)) throw new Error(`GitHub release is missing asset: ${name}`);
  return release;
}

async function verifyPublishedRelease(adapter: GhGithubAdapter, tag: string, source: string, assets: Record<string, string>, artifacts: Record<string, string>): Promise<any> {
  const release = verifyReleaseView(await adapter.viewRelease(tag), tag, false, assets);
  const tagSource = await adapter.tagCommit(tag);
  if (tagSource !== source) throw new Error(`Git tag ${tag} points to ${tagSource}, expected ${source}`);
  await verifyDownloaded(adapter, tag, artifacts);
  return release;
}

async function assertTagIsAbsentOrBound(adapter: GhGithubAdapter, tag: string, source: string): Promise<void> {
  try {
    const existing = await adapter.tagCommit(tag);
    if (existing !== source) throw new Error(`Git tag ${tag} already points to ${existing}, expected ${source}`);
  } catch (error) {
    if (!/404|not found/i.test(String(error))) throw error;
  }
}

async function trustedCompletedEvents(adapter: GhGithubAdapter, store: MirroredReleaseStateStore): Promise<ReleaseEvent[]> {
  const events = await store.completedEvents();
  for (const event of events) {
    const { artifacts, assets } = frozenDetails(event);
    const publication = event.detail?.publication;
    if (!publication || typeof publication !== "object" || (publication as any).source_revision !== event.identity.source_revision || (publication as any).tag !== `v${event.identity.version}` || (publication as any).immutable !== true) {
      throw new Error(`completed release record is not fully verified: ${event.identity.version}`);
    }
    await verifyPublishedRelease(adapter, String((publication as any).tag), event.identity.source_revision, assets, artifacts);
  }
  return events;
}

function latestFinalEvent(events: readonly ReleaseEvent[]): ReleaseEvent | undefined {
  return events.filter((event) => parseSemVer(event.identity.version).prerelease.length === 0).sort((left, right) => parseSemVer(left.identity.version).major - parseSemVer(right.identity.version).major || parseSemVer(left.identity.version).minor - parseSemVer(right.identity.version).minor || parseSemVer(left.identity.version).patch - parseSemVer(right.identity.version).patch).at(-1);
}

function stateStore(adapter: GhGithubAdapter): MirroredReleaseStateStore {
  return new MirroredReleaseStateStore(new GithubReleaseStateStore(adapter, stateRoot), new ReleaseStateStore(stateRoot));
}

async function release(): Promise<void> {
  if (!target) throw new Error("--target is required for a manual release");
  assertPlatformPreconditions();
  const adapter = new GhGithubAdapter();
  const store = stateStore(adapter);
  const completed = await trustedCompletedEvents(adapter, store);
  const reserved = await store.reservedVersions();
  const source = await adapter.mainHead();
  if (process.env.GITHUB_SHA && process.env.GITHUB_SHA !== source) throw new Error("GitHub dispatch ref moved after source preflight");
  const gate = successfulMainGate(await adapter.commitChecks(source));
  const sourcePull = mergedSourcePull(await adapter.sourcePulls(source), source);
  const verification = await adapter.json<{ commit?: { verification?: { verified?: boolean } } }>(["api", `repos/${adapter.repository}/commits/${source}`]);
  if (verification.commit?.verification?.verified !== true) throw new Error("source commit signature is not verified");
  const policyFiles = [".github/quality-gates.json", ".github/release-contract.json", ".github/release-failure-notification.json"];
  const policyText = policyFiles.map((path) => readFileSync(join(repositoryRoot, path), "utf8")).join("\n");
  const baseline = latestFinalEvent(completed);
  if (baseline) {
    const comparison = await adapter.compare(baseline.identity.source_revision, source);
    if (comparison.status !== "ahead") throw new Error("validated main head is not a descendant of the trusted release baseline");
  }
  const version = resolveReleaseTarget(target, completed.map((event) => event.identity.version), reserved);
  const identity: ReleaseIdentity = {
    version,
    target,
    source_revision: source,
    base_source: baseline?.identity.source_revision ?? "none",
    dispatch_sha: process.env.GITHUB_SHA ?? source,
    workflow_run: process.env.GITHUB_RUN_ID ?? "local",
    maintainer: process.env.GITHUB_ACTOR ?? "unknown",
    main_gate: gate.id,
    source_pr: sourcePull.number,
    source_pr_head: sourcePull.head,
    signed_commit: true,
    policy_revision: policyRevision(policyText),
    artifacts: {},
  };
  const claimed = await store.claim(identity);
  if (claimed.stage !== "claimed") throw new Error(`release reservation already progressed to ${claimed.stage}; use recovery`);
  const artifactRoot = join(stateRoot, "artifacts", version);
  mkdirSync(artifactRoot, { recursive: true, mode: 0o750 });
  const assets = {
    [`agent-vm-${version}-client.tar`]: join(artifactRoot, `agent-vm-${version}-client.tar`),
    [`agent-vm-${version}-linux-amd64.tar`]: join(artifactRoot, `agent-vm-${version}-linux-amd64.tar`),
    [`agent-vm-${version}-linux-arm64.tar`]: join(artifactRoot, `agent-vm-${version}-linux-arm64.tar`),
  };
  const targets: Record<string, string> = { client: assets[`agent-vm-${version}-client.tar`], "linux/amd64": assets[`agent-vm-${version}-linux-amd64.tar`], "linux/arm64": assets[`agent-vm-${version}-linux-arm64.tar`] };
  for (const [targetName, output] of Object.entries(targets)) if (!existsSync(output)) runBuilder(version, source, targetName, output);
  const packageDigests = Object.fromEntries(Object.entries(assets).map(([name, path]) => [name, sha256(path)]));
  const provenancePath = join(artifactRoot, "release-provenance.json");
  writeFileSync(provenancePath, `${JSON.stringify({ schema_version: 1, format: 3, version, source_revision: source, policy_revision: identity.policy_revision, toolchain: { bun: "1.4.4", go: "go1.27.3", cgo_enabled: false }, packages: packageDigests }, null, 2)}\n`, { mode: 0o644 });
  const sumsPath = join(artifactRoot, "SHA256SUMS");
  const completeAssets = { ...assets, "release-provenance.json": provenancePath, SHA256SUMS: sumsPath };
  const digests = Object.fromEntries(Object.entries(completeAssets).filter(([name]) => name !== "SHA256SUMS").map(([name, path]) => [name, sha256(path)]));
  writeFileSync(sumsPath, `${Object.entries(digests).map(([name, digest]) => `${digest}  ${name}`).join("\n")}\n`, { mode: 0o644 });
  const completeDigests = { ...digests, SHA256SUMS: sha256(sumsPath) };
  const prepared = await store.transition(identity, "prepared", { artifacts: completeDigests, assets: completeAssets });
  const tag = `v${version}`;
  await assertTagIsAbsentOrBound(adapter, tag, source);
  await adapter.createDraft(tag, source, Object.values(completeAssets), `Agent VM ${version}`, `Product release ${version} from ${source}.`);
  if (await adapter.tagCommit(tag) !== source) throw new Error(`Git tag ${tag} does not point to the release source`);
  const draft = await store.transition(identity, "draft", { artifacts: completeDigests, assets: completeAssets, tag });
  verifyReleaseView(await adapter.viewRelease(tag), tag, true, completeAssets);
  await verifyDownloaded(adapter, tag, completeDigests);
  await adapter.publish(tag);
  const publishedView = await verifyPublishedRelease(adapter, tag, source, completeAssets, completeDigests);
  const publication = { release_id: String(publishedView.releaseId ?? "unknown"), tag, source_revision: source, immutable: true };
  const published = await store.transition(identity, "published", { artifacts: completeDigests, assets: completeAssets, tag, publication });
  await store.transition(identity, "completed", { artifacts: completeDigests, assets: completeAssets, tag, publication });
  console.log(JSON.stringify({ stage: "completed", version, source_revision: source, reservation: version, events: [prepared.stage, draft.stage, published.stage, "completed"] }));
}

async function recover(): Promise<void> {
  const version = requireOption("--reservation");
  const adapter = new GhGithubAdapter();
  const store = stateStore(adapter);
  let current = await store.current(version);
  if (current.stage === "completed") {
    const { artifacts, assets } = frozenDetails(current);
    await verifyPublishedRelease(adapter, String(current.detail?.tag ?? `v${version}`), current.identity.source_revision, assets, artifacts);
    console.log(JSON.stringify({ stage: "completed", version, recovered: true, idempotent: true }));
    return;
  }
  if (current.stage === "abandoned" || current.stage === "claimed") throw new Error(`reservation at ${current.stage} cannot be recovered without frozen assets`);

  const frozen = frozenDetails(current);
  const { artifacts, assets } = frozen;
  const tag = String(current.detail?.tag ?? `v${version}`);
  if (current.stage === "prepared") {
    let existing: any | null = null;
    try {
      existing = await adapter.viewRelease(tag);
    } catch (error) {
      if (!/404|not found/i.test(String(error))) throw error;
      /* the draft may not have been created before interruption */
    }
    if (!existing) {
      assertPlatformPreconditions();
      for (const path of Object.values(assets)) if (!existsSync(path)) throw new Error(`recovery is missing frozen asset: ${path}`);
      await assertTagIsAbsentOrBound(adapter, tag, current.identity.source_revision);
      await adapter.createDraft(tag, current.identity.source_revision, Object.values(assets), `Agent VM ${version}`, `Recovered product release ${version} from ${current.identity.source_revision}.`);
    }
    if (await adapter.tagCommit(tag) !== current.identity.source_revision) throw new Error(`Git tag ${tag} does not point to the reserved source`);
    verifyReleaseView(await adapter.viewRelease(tag), tag, true, assets);
    await verifyDownloaded(adapter, tag, artifacts);
    current = await store.transition(current.identity, "draft", { artifacts, assets, tag, recovered: true });
  }

  if (current.stage === "draft") {
    const view = await adapter.viewRelease(tag) as { isDraft?: boolean };
    if (view.isDraft === true) {
      verifyReleaseView(view, tag, true, assets);
      assertPlatformPreconditions();
      await adapter.publish(tag);
    } else {
      verifyReleaseView(view, tag, false, assets);
    }
    const publishedView = await verifyPublishedRelease(adapter, tag, current.identity.source_revision, assets, artifacts);
    const publication = { release_id: String(publishedView.releaseId ?? "unknown"), tag, source_revision: current.identity.source_revision, immutable: true };
    current = await store.transition(current.identity, "published", { artifacts, assets, tag, publication, recovered: true });
  }
  if (current.stage === "published") {
    const publishedView = await verifyPublishedRelease(adapter, tag, current.identity.source_revision, assets, artifacts);
    const publication = { release_id: String(publishedView.releaseId ?? "unknown"), tag, source_revision: current.identity.source_revision, immutable: true };
    await store.transition(current.identity, "completed", { artifacts, assets, tag, publication, recovered: true });
  }
  console.log(JSON.stringify({ stage: "completed", version, recovered: true }));
}

async function main(): Promise<void> {
  if (operation === "release") return release();
  const version = requireOption("--reservation");
  if (operation === "abandon") {
    const adapter = new GhGithubAdapter();
    const event = await stateStore(adapter).abandon(version, "manual release recovery abandon");
    console.log(JSON.stringify({ stage: event.stage, version }));
    return;
  }
  if (operation === "recover") return recover();
  throw new Error("--operation must be release, recover, or abandon");
}

if (import.meta.main) {
  try {
    await main();
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
