import { test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

import { compareSemVer, parseSemVer, resolveReleaseTarget } from "../scripts/semver.ts";
import { GhGithubAdapter, GithubReleaseStateStore, ReleaseStateStore, type ReleaseIdentity } from "../scripts/release-coordinator.ts";
import { assertContractPreconditions, assertWorkflowPlatformAttestation, releaseBuilderCommand, successfulMainGate, verifyReleaseView } from "../scripts/release.ts";

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

test("release preflight is fail-closed and accepts every Main gate matrix job", () => {
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
    { name: "Main gate (macos-latest)", status: "completed", conclusion: "success", id: 2 },
    { name: "Main gate (ubuntu-latest)", status: "completed", conclusion: "success", id: 1 },
  ]);
  if (gate.id !== "1,2") throw new Error(`unexpected Main gate identity: ${gate.id}`);
});

test("GitHub state refs are append-only and preserve the original identity", async () => {
  const refs = new Map<string, string>();
  const commits = new Map<string, string>();
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
      if (commitRead && !args.includes("--method")) return { message: commits.get(commitRead.split("/git/commits/")[1]) };
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
        commits.set(sha, args.find((item) => item.startsWith("message="))!.slice("message=".length));
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
  let conflict = false;
  try { await remote.claim(identity({ source_revision: "e".repeat(40) })); } catch { conflict = true; }
  if (!conflict || refs.size !== 2) throw new Error("remote state ref conflict was not fail-closed");
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
  verifyReleaseView({ tagName: "v1.0.0", targetCommitish: source, isDraft: false, isPrerelease: false, immutable: false, assets: [{ name: "package" }] }, "v1.0.0", false, { package: "/tmp/package" }, source);
});
