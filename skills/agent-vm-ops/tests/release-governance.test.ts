import { test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

import { compareSemVer, parseSemVer, resolveReleaseTarget } from "../scripts/semver.ts";
import { GhGithubAdapter, GithubReleaseStateStore, ReleaseStateStore, type ReleaseIdentity } from "../scripts/release-coordinator.ts";
import { assertContractPreconditions, assertWorkflowPlatformAttestation, releaseBuilderCommand, releaseBuilderEnvironment, successfulMainGate, verifyHistoricalReleaseView, verifyReleaseView, waitForImmutableRelease } from "../scripts/release.ts";

const identity = (overrides: Partial<ReleaseIdentity> = {}): ReleaseIdentity => ({
  version: "1.0.0",
  target: "1.0.0",
  source_revision: "a".repeat(40),
  base_source: "none",
  dispatch_sha: "a".repeat(40),
  workflow_run: "123",
  maintainer: "maintainer",
  main_gate: "check-1",
  source_pr: "42",
  source_pr_head: "b".repeat(40),
  signed_commit: true,
  policy_revision: "c".repeat(64),
  artifacts: {},
  ...overrides,
});

test("SemVer target resolution requires a trusted baseline", () => {
  if (resolveReleaseTarget("1.0.0", [] as string[]) !== "1.0.0") throw new Error("first release did not resolve to 1.0.0");
  if (resolveReleaseTarget("patch", ["1.0.0-rc.1", "1.0.0"]) !== "1.0.1") throw new Error("patch target did not use the final baseline");
  if (resolveReleaseTarget("minor", ["1.0.0"]) !== "1.1.0") throw new Error("minor target did not advance the minor");
  if (compareSemVer("1.0.0+build.1", "1.0.0+build.2") !== 0) throw new Error("build metadata changed precedence");
  if (parseSemVer("1.0.0-rc.1").prerelease.join(".") !== "rc.1") throw new Error("pre-release was not parsed");
  let occupiedError = false;
  try { resolveReleaseTarget("1.0.1", ["1.0.0"], ["1.0.0", "1.0.1+existing"]); } catch { occupiedError = true; }
  if (!occupiedError) throw new Error("occupied explicit target was reused");
  let prereleaseRegression = false;
  try { resolveReleaseTarget("1.1.0-rc.1", ["1.0.0"], ["1.0.0", "1.1.0-rc.2"]); } catch { prereleaseRegression = true; }
  if (!prereleaseRegression) throw new Error("pre-release precedence regression was accepted");
  let symbolicError = false;
  try { resolveReleaseTarget("major", []); } catch { symbolicError = true; }
  if (!symbolicError) throw new Error("symbolic target without a final baseline was accepted");
});

test("release assets select the management-enabled product builder", () => {
  const source = "a".repeat(40);
  const client = releaseBuilderCommand("1.0.3", source, "client", "/tmp/client.tar");
  const native = releaseBuilderCommand("1.0.3", source, "linux/amd64", "/tmp/native.tar");
  if (!client.some((value) => value.endsWith("build_product_release.ts")) || !client.includes("--version")) throw new Error("client assets did not select the product builder");
  if (!native.some((value) => value.endsWith("build_product_release.ts")) || !native.includes("--version")) throw new Error("native assets did not select the product builder");
});

test("release builders do not inherit platform credentials", () => {
  const environment = releaseBuilderEnvironment({ GH_TOKEN: "ghs-secret", GITHUB_TOKEN: "github-secret", RELEASE_PLATFORM_TOKEN: "platform-secret", SAFE_VALUE: "kept" });
  if ("GH_TOKEN" in environment || "GITHUB_TOKEN" in environment || "RELEASE_PLATFORM_TOKEN" in environment) throw new Error("release credentials leaked into the product builder environment");
  if (environment.SAFE_VALUE !== "kept") throw new Error("safe builder environment values were not preserved");
});

test("release reservation is append-only and identity-bound", () => {
  const root = mkdtempSync(join(tmpdir(), "agent-vm-release-state-"));
  try {
    const store = new ReleaseStateStore(root);
    const source = identity();
    if (store.claim(source).stage !== "claimed") throw new Error("claim did not create the claimed stage");
    if (store.claim(source).stage !== "claimed") throw new Error("idempotent claim did not return the original event");
    const artifacts = { "agent-vm-1.0.0-client.tar": "d".repeat(64) };
    store.transition(source, "prepared", { artifacts, assets: artifacts });
    let frozenError = false;
    try { store.transition(source, "draft", { artifacts: { ...artifacts, other: "e".repeat(64) }, assets: { ...artifacts, other: "/tmp/other" }, tag: "v1.0.0" }); } catch { frozenError = true; }
    if (!frozenError) throw new Error("release transition changed frozen artifacts");
    store.transition(source, "draft", { artifacts, assets: artifacts, tag: "v1.0.0" });
    if (store.current("1.0.0").stage !== "draft") throw new Error("draft stage was not appended");
    let conflict = false;
    try { store.claim(identity({ source_revision: "e".repeat(40) })); } catch { conflict = true; }
    if (!conflict) throw new Error("different source claimed an occupied version");
    const lines = readFileSync(join(root, "1.0.0.jsonl"), "utf8").trim().split("\n");
    if (lines.length !== 3) throw new Error("release state was overwritten instead of appended");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("release preflight is fail-closed and accepts the exact Main gate matrix", () => {
  let blocked = false;
  try {
    assertContractPreconditions(
      { platform_alignment: { status: "audit-required" } },
      { platform_preconditions: { status: "audit-required" } },
    );
  } catch (error) { blocked = String(error).includes("audit-required"); }
  if (!blocked) throw new Error("release preflight did not block the unenabled platform contract");
  let unprotectedError = false;
  try { assertWorkflowPlatformAttestation("refs/heads/main", "false"); } catch { unprotectedError = true; }
  if (!unprotectedError) throw new Error("workflow preflight accepted an unprotected ref");
  assertWorkflowPlatformAttestation("refs/heads/main", "true");
  const gate = successfulMainGate([
    { name: "Main gate (macos-latest)", status: "completed", conclusion: "success", id: 2, check_suite: { id: 99 }, app: { slug: "github-actions" } },
    { name: "Main gate (ubuntu-latest)", status: "completed", conclusion: "success", id: 1, check_suite: { id: 99 }, app: { slug: "github-actions" } },
  ]);
  if (gate.id !== "1,2") throw new Error(`unexpected Main gate identity: ${gate.id}`);
  let missingMatrixRejected = false;
  try { successfulMainGate([{ name: "Main gate (ubuntu-latest)", status: "completed", conclusion: "success", id: 1, check_suite: { id: 99 }, app: { slug: "github-actions" } }]); } catch { missingMatrixRejected = true; }
  if (!missingMatrixRejected) throw new Error("an incomplete Main gate matrix was accepted");
  let foreignCheckRejected = false;
  try { successfulMainGate([
    { name: "Main gate (macos-latest)", status: "completed", conclusion: "success", id: 2, check_suite: { id: 99 }, app: { slug: "third-party" } },
    { name: "Main gate (ubuntu-latest)", status: "completed", conclusion: "success", id: 1, check_suite: { id: 99 }, app: { slug: "github-actions" } },
  ]); } catch { foreignCheckRejected = true; }
  if (!foreignCheckRejected) throw new Error("a non-GitHub Actions Main gate was accepted");
  let mixedSuiteRejected = false;
  try { successfulMainGate([
    { name: "Main gate (macos-latest)", status: "completed", conclusion: "success", id: 2, check_suite: { id: 100 }, app: { slug: "github-actions" } },
    { name: "Main gate (ubuntu-latest)", status: "completed", conclusion: "success", id: 1, check_suite: { id: 99 }, app: { slug: "github-actions" } },
  ]); } catch { mixedSuiteRejected = true; }
  if (!mixedSuiteRejected) throw new Error("Main gates from different check suites were accepted");
  const rerunGate = successfulMainGate([
    { name: "Main gate (macos-latest)", status: "completed", conclusion: "success", id: 4, completed_at: "2026-11-05T00:05:00Z", check_suite: { id: 101, updated_at: "2026-11-05T00:06:00Z" }, app: { slug: "github-actions" } },
    { name: "Main gate (ubuntu-latest)", status: "completed", conclusion: "success", id: 3, completed_at: "2026-11-05T00:04:00Z", check_suite: { id: 101, updated_at: "2026-11-05T00:06:00Z" }, app: { slug: "github-actions" } },
    { name: "Main gate (macos-latest)", status: "completed", conclusion: "success", id: 2, completed_at: "2026-11-04T00:05:00Z", check_suite: { id: 100, updated_at: "2026-11-04T00:06:00Z" }, app: { slug: "github-actions" } },
    { name: "Main gate (ubuntu-latest)", status: "completed", conclusion: "success", id: 1, completed_at: "2026-11-04T00:04:00Z", check_suite: { id: 100, updated_at: "2026-11-04T00:06:00Z" }, app: { slug: "github-actions" } },
  ]);
  if (rerunGate.id !== "3,4") throw new Error(`latest Main gate rerun was not selected: ${rerunGate.id}`);
});

test("GitHub state refs are append-only and preserve the original identity", async () => {
  const refs = new Map<string, string>();
  const commits = new Map<string, { message: string; parents: string[] }>();
  let sequence = 0;
  const fake = {
    repository: "IvanLi-CN/agent-vm",
    async json(args: readonly string[], input?: string): Promise<any> {
      const refEndpoint = args.find((item) => item.includes("/git/ref/tags/"));
      if (refEndpoint && !args.includes("--method")) {
        const ref = refEndpoint.split("/git/ref/tags/")[1];
        const sha = refs.get(ref);
        if (!sha) throw new Error("HTTP 404 Not Found");
        return { object: { sha } };
      }
      const commitRead = args.find((item) => /\/git\/commits\/[0-9a-z-]+$/.test(item));
      if (commitRead && !args.includes("--method")) {
        const commit = commits.get(commitRead.split("/git/commits/")[1]);
        return commit ? { message: commit.message, parents: commit.parents.map((sha) => ({ sha })) } : undefined;
      }
      if (args.some((item) => item.endsWith("/git/blobs"))) return { sha: `blob-${++sequence}` };
      if (args.some((item) => item.endsWith("/git/trees"))) {
        if (!args.includes("--input") || input === undefined) throw new Error("tree JSON request body is missing");
        const body = JSON.parse(input) as { tree?: Array<Record<string, string>> };
        const entries = body.tree ?? [];
        if (entries.length !== 1 || entries[0]?.path !== "record.json" || entries[0]?.mode !== "100644" || entries[0]?.type !== "blob" || !entries[0]?.sha) {
          throw new Error("tree payload is not a valid GitHub tree entry");
        }
        return { sha: `tree-${++sequence}` };
      }
      if (args.some((item) => item.endsWith("/git/commits"))) {
        const sha = `commit-${++sequence}`;
        const parentIndex = args.indexOf("--field");
        const parentArg = parentIndex >= 0 ? args[parentIndex + 1] : undefined;
        commits.set(sha, { message: args.find((item) => item.startsWith("message="))!.slice("message=".length), parents: parentArg?.startsWith("parents[]=") ? [parentArg.slice("parents[]=".length)] : [] });
        return { sha };
      }
      if (args.some((item) => item.endsWith("/git/refs"))) {
        const ref = args.find((item) => item.startsWith("ref="))!.slice("ref=refs/tags/".length);
        if (refs.has(ref)) throw new Error("HTTP 422 Reference already exists");
        refs.set(ref, args.find((item) => item.startsWith("sha="))!.slice("sha=".length));
        return { ref: `refs/tags/${ref}` };
      }
      throw new Error(`unexpected fake GitHub API call: ${args.join(" ")}`);
    },
  } as unknown as GhGithubAdapter;
  const remote = new GithubReleaseStateStore(fake, "release-state");
  const source = identity();
  await remote.claim(source);
  await remote.transition(source, "prepared", { artifacts: { package: "d".repeat(64) }, assets: { package: "/tmp/package" } });
  const current = await remote.current(source.version);
  const assets = current.detail?.assets as Record<string, string> | undefined;
  if (current.stage !== "prepared" || assets?.package !== "release-state/artifacts/1.0.0/package") throw new Error("remote state did not hydrate the recovery asset path");
  const orphan = "orphan-commit";
  commits.set(orphan, { message: `agent-vm release state draft 1.0.0\n\n${JSON.stringify({ stage: "draft", identity: source, at: new Date().toISOString(), detail: { artifacts: { package: "d".repeat(64) }, assets: { package: "package" } } })}`, parents: ["unrelated-parent"] });
  refs.set("release-state/draft/1.0.0", orphan);
  let lineageRejected = false;
  try { await remote.current(source.version); } catch (error) { lineageRejected = String(error).includes("parent chain"); }
  if (!lineageRejected) throw new Error("a non-descendant higher-stage state ref was accepted");
  let conflict = false;
  try { await remote.claim(identity({ source_revision: "e".repeat(40) })); } catch { conflict = true; }
  if (!conflict || refs.size !== 3) throw new Error("remote state ref conflict was not fail-closed");
});

test("draft release lookup accepts an untagged draft only when its target is bound", async () => {
  const source = "a".repeat(40);
  const adapter = new (class extends GhGithubAdapter {
    async json<T>(args: readonly string[]): Promise<T> {
      if (args.some((item) => item.includes("/releases/tags/"))) throw new Error("HTTP 404 Not Found");
      return [{ tag_name: "v1.0.0", target_commitish: source, draft: true, assets: [{ name: "package" }] }] as T;
    }
  })("IvanLi-CN/agent-vm");
  const view = await adapter.viewRelease("v1.0.0");
  verifyReleaseView(view, "v1.0.0", true, { package: "/tmp/package" }, source);
  let wrongSourceRejected = false;
  try { verifyReleaseView(view, "v1.0.0", true, { package: "/tmp/package" }, "e".repeat(40)); } catch { wrongSourceRejected = true; }
  if (!wrongSourceRejected) throw new Error("draft release target binding was not enforced");
  let mutablePublishedRejected = false;
  try { verifyReleaseView({ tagName: "v1.0.0", targetCommitish: source, isDraft: false, isPrerelease: false, immutable: false, assets: [{ name: "package" }] }, "v1.0.0", false, { package: "/tmp/package" }, source); } catch { mutablePublishedRejected = true; }
  if (!mutablePublishedRejected) throw new Error("mutable published release was accepted");
  let extraAssetRejected = false;
  try { verifyReleaseView({ tagName: "v1.0.0", targetCommitish: source, isDraft: false, isPrerelease: false, immutable: true, assets: [{ name: "package" }, { name: "unexpected" }] }, "v1.0.0", false, { package: "/tmp/package" }, source); } catch { extraAssetRejected = true; }
  if (!extraAssetRejected) throw new Error("an unexpected release asset was accepted");
  verifyReleaseView({ tagName: "v1.0.0", targetCommitish: source, isDraft: false, isPrerelease: false, immutable: true, assets: [{ name: "package" }] }, "v1.0.0", false, { package: "/tmp/package" }, source);
});

test("published release verification waits for immutable propagation without accepting historical mutable releases", async () => {
  const source = "a".repeat(40);
  let reads = 0;
  const view = () => Promise.resolve({ tagName: "v1.4.0", targetCommitish: source, isDraft: false, isPrerelease: false, immutable: ++reads >= 3, assets: [{ name: "package" }] });
  const verified = await waitForImmutableRelease(view, "v1.4.0", { package: "/tmp/package" }, source, { timeoutMs: 100, pollMs: 1 });
  if (verified.immutable !== true || reads !== 3) throw new Error("immutable propagation was not awaited");
  let historicalRejected = false;
  try {
    await waitForImmutableRelease(() => Promise.resolve({ tagName: "v1.3.0", targetCommitish: source, isDraft: false, isPrerelease: false, immutable: false, assets: [{ name: "package" }] }), "v1.3.0", { package: "/tmp/package" }, source, { timeoutMs: 3, pollMs: 1 });
  } catch (error) { historicalRejected = /after waiting/.test(String(error)); }
  if (!historicalRejected) throw new Error("historical mutable release was not rejected after the bounded wait");
});

test("historical mutable releases are valid legacy baselines while cutover releases require immutable state", () => {
  const source = "a".repeat(40);
  const legacy = { tagName: "v1.3.0", targetCommitish: source, isDraft: false, isPrerelease: false, immutable: false, assets: [{ name: "package" }] };
  verifyHistoricalReleaseView(legacy, "v1.3.0", { package: "/tmp/package" }, source);
  let rejected = false;
  try { verifyReleaseView(legacy, "v1.4.0", false, { package: "/tmp/package" }, source); } catch { rejected = true; }
  if (!rejected) throw new Error("cutover release accepted mutable GitHub state");
});

test("published release verification fails closed on immutable timeout and identity changes", async () => {
  const source = "a".repeat(40);
  let timeoutRejected = false;
  try {
    await waitForImmutableRelease(() => Promise.resolve({ tagName: "v1.4.0", targetCommitish: source, isDraft: false, isPrerelease: false, immutable: false, assets: [{ name: "package" }] }), "v1.4.0", { package: "/tmp/package" }, source, { timeoutMs: 2, pollMs: 1 });
  } catch (error) { timeoutRejected = /after waiting/.test(String(error)); }
  if (!timeoutRejected) throw new Error("immutable timeout was not fail-closed");
  let identityRejected = false;
  try {
    await waitForImmutableRelease(() => Promise.resolve({ tagName: "v1.4.0", targetCommitish: "b".repeat(40), isDraft: false, isPrerelease: false, immutable: false, assets: [{ name: "package" }] }), "v1.4.0", { package: "/tmp/package" }, source, { timeoutMs: 100, pollMs: 1 });
  } catch (error) { identityRejected = /target is not bound/.test(String(error)); }
  if (!identityRejected) throw new Error("release identity drift was retried instead of rejected");
});
