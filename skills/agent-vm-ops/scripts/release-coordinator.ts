#!/usr/bin/env bun

/** Append-only release identity and recovery coordination. */

import { closeSync, existsSync, mkdirSync, openSync, readFileSync, unlinkSync, writeSync } from "node:fs";
import { isAbsolute, join } from "node:path";

import { parseSemVer } from "./semver.ts";

export type ReleaseStage = "claimed" | "prepared" | "draft" | "published" | "completed" | "abandoned";

export interface ReleaseIdentity {
  version: string;
  target: string;
  source_revision: string;
  base_source: string;
  dispatch_sha: string;
  workflow_run: string;
  maintainer: string;
  main_gate: string;
  source_pr: string;
  source_pr_head: string;
  signed_commit: boolean;
  policy_revision: string;
  artifacts: Record<string, string>;
}

export interface ReleaseEvent {
  stage: ReleaseStage;
  identity: ReleaseIdentity;
  at: string;
  detail?: Record<string, unknown>;
}

const terminalStages = new Set<ReleaseStage>(["completed", "abandoned"]);
const nextStages: Record<ReleaseStage, readonly ReleaseStage[]> = {
  claimed: ["prepared", "abandoned"],
  prepared: ["draft", "abandoned"],
  draft: ["published", "abandoned"],
  published: ["completed", "abandoned"],
  completed: [],
  abandoned: [],
};

function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value).sort(([left], [right]) => left.localeCompare(right)).map(([key, item]) => `${JSON.stringify(key)}:${canonical(item)}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function identityKey(identity: ReleaseIdentity): string {
  return canonical(identity);
}

function fileName(version: string): string {
  return version.replace(/[^0-9A-Za-z.+-]/g, "_");
}

function waitBriefly(): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 5);
}

function validateIdentity(identity: ReleaseIdentity): void {
  try { parseSemVer(identity.version); } catch { throw new Error("release identity version is invalid"); }
  for (const [name, value] of Object.entries(identity)) {
    if (name === "signed_commit" || name === "artifacts") continue;
    if (typeof value !== "string" || value.length === 0) throw new Error(`release identity field is empty: ${name}`);
  }
  if (!identity.signed_commit) throw new Error("release source commit is not signed");
  if (!/^[0-9a-f]{40}$/.test(identity.source_revision)) throw new Error("release source revision is invalid");
  if (identity.base_source !== "none" && !/^[0-9a-f]{40}$/.test(identity.base_source)) throw new Error("release base source is invalid");
  if (!/^[0-9a-f]{40}$/.test(identity.dispatch_sha)) throw new Error("release dispatch SHA is invalid");
  if (!/^[0-9a-f]{64}$/.test(identity.policy_revision)) throw new Error("release policy revision is invalid");
  if (Object.values(identity.artifacts).some((value) => !/^[0-9a-f]{64}$/.test(value))) throw new Error("release artifact digests are invalid");
}

function artifactDetails(detail: Record<string, unknown>): Record<string, string> {
  const artifacts = detail.artifacts;
  const assets = detail.assets;
  if (!artifacts || typeof artifacts !== "object" || Array.isArray(artifacts) || Object.keys(artifacts).length === 0 || Object.values(artifacts).some((value) => typeof value !== "string" || !/^[0-9a-f]{64}$/.test(value))) throw new Error("release transition requires frozen artifact digests");
  if (!assets || typeof assets !== "object" || Array.isArray(assets) || Object.keys(assets).length !== Object.keys(artifacts).length || Object.keys(assets).some((name) => typeof (assets as Record<string, unknown>)[name] !== "string" || !((assets as Record<string, unknown>)[name] as string))) throw new Error("release transition requires frozen asset bindings");
  return artifacts as Record<string, string>;
}

function frozenDetails(detail: Record<string, unknown> | undefined): { artifacts: Record<string, string>; assets: Record<string, string> } | null {
  if (!detail || typeof detail.artifacts !== "object" || !detail.artifacts || Array.isArray(detail.artifacts) || typeof detail.assets !== "object" || !detail.assets || Array.isArray(detail.assets)) return null;
  return { artifacts: detail.artifacts as Record<string, string>, assets: detail.assets as Record<string, string> };
}

function assertFrozenDetailsEqual(left: Record<string, unknown> | undefined, right: Record<string, unknown> | undefined): void {
  if (!right) return;
  const existing = frozenDetails(left);
  const candidate = frozenDetails(right);
  if (!existing || !candidate) return;
  const comparable = (value: { artifacts: Record<string, string>; assets: Record<string, string> }) => canonical({ artifacts: value.artifacts, asset_names: Object.keys(value.assets).sort() });
  if (comparable(existing) !== comparable(candidate)) throw new Error("release transition cannot change frozen artifact digests or asset bindings");
}

function comparableTransitionDetail(detail: Record<string, unknown> | undefined): string {
  if (!detail) return "";
  const frozen = frozenDetails(detail);
  if (!frozen) return canonical(detail);
  return canonical({ ...detail, assets: Object.keys(frozen.assets).sort() });
}

export class ReleaseStateStore {
  readonly root: string;

  constructor(root: string) {
    this.root = root;
    mkdirSync(root, { recursive: true, mode: 0o750 });
  }

  private path(version: string): string {
    return join(this.root, `${fileName(version)}.jsonl`);
  }

  private hydrate(event: ReleaseEvent): ReleaseEvent {
    const assets = event.detail?.assets;
    if (!assets || typeof assets !== "object" || Array.isArray(assets)) return event;
    return {
      ...event,
      detail: {
        ...event.detail,
        assets: Object.fromEntries(Object.keys(assets).map((name) => [name, join(this.root, "artifacts", event.identity.version, name)])),
      },
    };
  }

  private readEvents(version: string): ReleaseEvent[] {
    const path = this.path(version);
    if (!existsSync(path)) throw new Error(`release reservation does not exist: ${version}`);
    const lines = readFileSync(path, "utf8").split("\n").filter(Boolean);
    const events = lines.map((line) => JSON.parse(line) as ReleaseEvent);
    if (!events.length) throw new Error("release reservation is empty");
    const key = identityKey(events[0].identity);
    if (events.some((event) => identityKey(event.identity) !== key)) throw new Error("release reservation identity was overwritten");
    return events.map((event) => this.hydrate(event));
  }

  claim(identity: ReleaseIdentity): ReleaseEvent {
    validateIdentity(identity);
    const event: ReleaseEvent = { stage: "claimed", identity, at: new Date().toISOString() };
    const path = this.path(identity.version);
    try {
      const descriptor = openSync(path, "wx", 0o640);
      try { writeSync(descriptor, `${JSON.stringify(event)}\n`); } finally { closeSync(descriptor); }
      return event;
    } catch (error: any) {
      if (error?.code !== "EEXIST") throw error;
      for (let attempt = 0; attempt < 20; attempt += 1) {
        try {
          const existing = this.readEvents(identity.version)[0];
          if (identityKey(existing.identity) !== identityKey(identity)) throw new Error("release version is already claimed by a different identity");
          return existing;
        } catch (readError) {
          if (attempt === 19) throw readError;
          waitBriefly();
        }
      }
      throw new Error("release reservation claim did not converge");
    }
  }

  current(version: string): ReleaseEvent {
    return this.readEvents(version).at(-1)!;
  }

  transition(identity: ReleaseIdentity, stage: Exclude<ReleaseStage, "claimed">, detail: Record<string, unknown> = {}): ReleaseEvent {
    validateIdentity(identity);
    const path = this.path(identity.version);
    const current = this.current(identity.version);
    if (identityKey(current.identity) !== identityKey(identity)) throw new Error("release transition identity does not match the reservation");
    if (!nextStages[current.stage].includes(stage)) throw new Error(`release transition is invalid: ${current.stage} -> ${stage}`);
    if (stage === "prepared") artifactDetails(detail);
    if ((stage === "draft" || stage === "published" || stage === "completed") && !current.detail?.artifacts) artifactDetails(detail);
    if (stage !== "abandoned" && current.detail?.artifacts) artifactDetails(detail);
    assertFrozenDetailsEqual(current.detail, detail);
    const event: ReleaseEvent = { stage, identity, at: new Date().toISOString(), ...(Object.keys(detail).length ? { detail } : {}) };
    const descriptor = openSync(path, "a", 0o640);
    try { writeSync(descriptor, `${JSON.stringify(event)}\n`); } finally { closeSync(descriptor); }
    return event;
  }

  recover(version: string, stage: Exclude<ReleaseStage, "claimed" | "abandoned">, detail: Record<string, unknown> = {}): ReleaseEvent {
    const current = this.current(version);
    if (terminalStages.has(current.stage)) throw new Error(`release reservation is already terminal: ${current.stage}`);
    return this.transition(current.identity, stage, { recovered: true, ...detail });
  }

  abandon(version: string, reason: string): ReleaseEvent {
    const current = this.current(version);
    if (terminalStages.has(current.stage)) throw new Error(`release reservation is already terminal: ${current.stage}`);
    return this.transition(current.identity, "abandoned", { reason });
  }
}

const stateStages: readonly ReleaseStage[] = ["claimed", "prepared", "draft", "published", "completed", "abandoned"];

function stateRef(stage: ReleaseStage, version: string): string {
  return `release-state/${stage}/${fileName(version)}`;
}

function stateCommitMessage(event: ReleaseEvent): string {
  return `agent-vm release state ${event.stage} ${event.identity.version}\n\n${JSON.stringify(event)}`;
}

function eventFromCommit(message: unknown): ReleaseEvent {
  if (typeof message !== "string") throw new Error("release state commit message is missing");
  const marker = message.indexOf("\n\n");
  if (marker < 0) throw new Error("release state commit message has no record");
  const event = JSON.parse(message.slice(marker + 2)) as ReleaseEvent;
  if (!event || typeof event !== "object" || typeof event.stage !== "string" || !event.identity) throw new Error("release state record is invalid");
  validateIdentity(event.identity);
  return event;
}

/** GitHub-backed immutable state refs. Local JSONL remains a recovery artifact mirror. */
export class GithubReleaseStateStore {
  constructor(readonly adapter: GhGithubAdapter, readonly stateRoot = "release-state") {}

  private hydrate(event: ReleaseEvent): ReleaseEvent {
    const assets = event.detail?.assets;
    if (!assets || typeof assets !== "object" || Array.isArray(assets)) return event;
    const hydrated = Object.fromEntries(Object.entries(assets).map(([name, value]) => [
      name,
      typeof value === "string" && isAbsolute(value) ? value : join(this.stateRoot, "artifacts", event.identity.version, name),
    ]));
    return { ...event, detail: { ...event.detail, assets: hydrated } };
  }

  private portable(event: ReleaseEvent): ReleaseEvent {
    const assets = event.detail?.assets;
    if (!assets || typeof assets !== "object" || Array.isArray(assets)) return event;
    const portableAssets = Object.fromEntries(Object.keys(assets).map((name) => [name, name]));
    return { ...event, detail: { ...event.detail, assets: portableAssets } };
  }

  private async read(stage: ReleaseStage, version: string): Promise<{ event: ReleaseEvent; sha: string } | null> {
    const ref = stateRef(stage, version);
    try {
      const reference = await this.adapter.json<{ object?: { sha?: string } }>(["api", `repos/${this.adapter.repository}/git/ref/tags/${ref}`]);
      const sha = reference.object?.sha;
      if (!sha) throw new Error("release state ref has no commit SHA");
      const commit = await this.adapter.json<{ message?: string }>(["api", `repos/${this.adapter.repository}/git/commits/${sha}`]);
      return { event: this.hydrate(eventFromCommit(commit.message)), sha };
    } catch (error) {
      if (/404|not found/i.test(String(error))) return null;
      throw error;
    }
  }

  private async create(stage: ReleaseStage, event: ReleaseEvent, parent: string | null): Promise<void> {
    const record = this.portable(event);
    const blob = await this.adapter.json<{ sha: string }>([
      "api", "--method", "POST", `repos/${this.adapter.repository}/git/blobs`,
      "--raw-field", `content=${JSON.stringify(record)}`, "--raw-field", "encoding=utf-8",
    ]);
    const treeArgs = [
      "api", "--method", "POST", `repos/${this.adapter.repository}/git/trees`,
      "--input", "-",
    ];
    const tree = await this.adapter.json<{ sha: string }>(treeArgs, `${JSON.stringify({ tree: [{ path: "record.json", mode: "100644", type: "blob", sha: blob.sha }] })}\n`);
    const commitArgs = [
      "api", "--method", "POST", `repos/${this.adapter.repository}/git/commits`,
      "--raw-field", `message=${stateCommitMessage(record)}`, "--raw-field", `tree=${tree.sha}`,
    ];
    if (parent) commitArgs.push("--field", `parents[]=${parent}`);
    const commit = await this.adapter.json<{ sha: string }>(commitArgs);
    await this.adapter.json(["api", "--method", "POST", `repos/${this.adapter.repository}/git/refs`, "--raw-field", `ref=refs/tags/${stateRef(stage, event.identity.version)}`, "--raw-field", `sha=${commit.sha}`]);
  }

  async claim(identity: ReleaseIdentity): Promise<ReleaseEvent> {
    validateIdentity(identity);
    const event: ReleaseEvent = { stage: "claimed", identity, at: new Date().toISOString() };
    const existing = await this.read("claimed", identity.version);
    if (existing) {
      if (identityKey(existing.event.identity) !== identityKey(identity)) throw new Error("release version is already claimed by a different identity");
      return existing.event;
    }
    try {
      await this.create("claimed", event, null);
      return event;
    } catch (error) {
      if (!/422|already exists|reference/i.test(String(error))) throw error;
      const winner = await this.read("claimed", identity.version);
      if (!winner) throw error;
      if (identityKey(winner.event.identity) !== identityKey(identity)) throw new Error("release version claim conflicted");
      return winner.event;
    }
  }

  async current(version: string): Promise<ReleaseEvent> {
    const events = (await Promise.all(stateStages.map(async (stage) => ({ stage, value: await this.read(stage, version) })))).filter((item): item is { stage: ReleaseStage; value: { event: ReleaseEvent; sha: string } } => item.value !== null);
    if (!events.length) throw new Error(`release reservation does not exist: ${version}`);
    events.sort((left, right) => stateStages.indexOf(left.stage) - stateStages.indexOf(right.stage));
    return events.at(-1)!.value.event;
  }

  private async currentWithSha(version: string): Promise<{ event: ReleaseEvent; sha: string }> {
    const events = (await Promise.all(stateStages.map(async (stage) => ({ stage, value: await this.read(stage, version) })))).filter((item): item is { stage: ReleaseStage; value: { event: ReleaseEvent; sha: string } } => item.value !== null);
    if (!events.length) throw new Error(`release reservation does not exist: ${version}`);
    events.sort((left, right) => stateStages.indexOf(left.stage) - stateStages.indexOf(right.stage));
    return events.at(-1)!.value;
  }

  private async versions(stage: ReleaseStage): Promise<string[]> {
    const prefix = `release-state/${stage}/`;
    const refs = await this.adapter.json<Array<{ ref?: string }>>(["api", `repos/${this.adapter.repository}/git/matching-refs/tags/${prefix}`]);
    return refs.flatMap((item) => {
      const ref = item.ref ?? "";
      return ref.startsWith(`refs/tags/${prefix}`) ? [ref.slice(`refs/tags/${prefix}`.length)] : [];
    });
  }

  async reservedVersions(): Promise<string[]> {
    return this.versions("claimed");
  }

  async completedEvents(): Promise<ReleaseEvent[]> {
    const versions = await this.versions("completed");
    return Promise.all(versions.map(async (version) => (await this.read("completed", version))!.event));
  }

  async transition(identity: ReleaseIdentity, stage: Exclude<ReleaseStage, "claimed">, detail: Record<string, unknown> = {}): Promise<ReleaseEvent> {
    validateIdentity(identity);
    const current = await this.currentWithSha(identity.version);
    if (identityKey(current.event.identity) !== identityKey(identity)) throw new Error("release transition identity does not match the reservation");
    if (!nextStages[current.event.stage].includes(stage)) throw new Error(`release transition is invalid: ${current.event.stage} -> ${stage}`);
    if (stage === "prepared") artifactDetails(detail);
    if ((stage === "draft" || stage === "published" || stage === "completed") && !current.event.detail?.artifacts) artifactDetails(detail);
    if (stage !== "abandoned" && current.event.detail?.artifacts) artifactDetails(detail);
    assertFrozenDetailsEqual(current.event.detail, detail);
    const event: ReleaseEvent = { stage, identity, at: new Date().toISOString(), ...(Object.keys(detail).length ? { detail } : {}) };
    const existing = await this.read(stage, identity.version);
    if (existing) {
      if (identityKey(existing.event.identity) !== identityKey(identity)) throw new Error("release state transition conflicts with an existing identity");
      assertFrozenDetailsEqual(existing.event.detail, event.detail);
      if (comparableTransitionDetail(existing.event.detail) !== comparableTransitionDetail(event.detail)) throw new Error("release state transition conflicts with an existing ref");
      return existing.event;
    }
    try {
      await this.create(stage, event, current.sha);
      return event;
    } catch (error) {
      if (!/422|already exists|reference/i.test(String(error))) throw error;
      const winner = await this.read(stage, identity.version);
      if (!winner) throw error;
      if (identityKey(winner.event.identity) !== identityKey(identity)) throw new Error("release state transition conflicted");
      assertFrozenDetailsEqual(winner.event.detail, event.detail);
      return winner.event;
    }
  }

  async recover(version: string, stage: Exclude<ReleaseStage, "claimed" | "abandoned">, detail: Record<string, unknown> = {}): Promise<ReleaseEvent> {
    const current = await this.current(version);
    if (terminalStages.has(current.stage)) throw new Error(`release reservation is already terminal: ${current.stage}`);
    return this.transition(current.identity, stage, { recovered: true, ...detail });
  }

  async abandon(version: string, reason: string): Promise<ReleaseEvent> {
    const current = await this.current(version);
    if (terminalStages.has(current.stage)) throw new Error(`release reservation is already terminal: ${current.stage}`);
    return this.transition(current.identity, "abandoned", { reason });
  }
}

/** Keep the local artifact useful for recovery while the Git ref remains authoritative. */
export class MirroredReleaseStateStore {
  constructor(readonly primary: GithubReleaseStateStore, readonly mirror: ReleaseStateStore) {}

  async claim(identity: ReleaseIdentity): Promise<ReleaseEvent> {
    const event = await this.primary.claim(identity);
    try { this.mirror.claim(identity); } catch { /* remote state is authoritative; mirror is best-effort */ }
    return event;
  }

  async current(version: string): Promise<ReleaseEvent> {
    return this.primary.current(version);
  }

  async reservedVersions(): Promise<string[]> {
    return this.primary.reservedVersions();
  }

  async completedEvents(): Promise<ReleaseEvent[]> {
    return this.primary.completedEvents();
  }

  async transition(identity: ReleaseIdentity, stage: Exclude<ReleaseStage, "claimed">, detail: Record<string, unknown> = {}): Promise<ReleaseEvent> {
    const event = await this.primary.transition(identity, stage, detail);
    try { this.mirror.transition(identity, stage, detail); } catch { /* remote state is authoritative; mirror is best-effort */ }
    return event;
  }

  async recover(version: string, stage: Exclude<ReleaseStage, "claimed" | "abandoned">, detail: Record<string, unknown> = {}): Promise<ReleaseEvent> {
    const current = await this.current(version);
    if (terminalStages.has(current.stage)) throw new Error(`release reservation is already terminal: ${current.stage}`);
    return this.transition(current.identity, stage, { recovered: true, ...detail });
  }

  async abandon(version: string, reason: string): Promise<ReleaseEvent> {
    const current = await this.current(version);
    if (terminalStages.has(current.stage)) throw new Error(`release reservation is already terminal: ${current.stage}`);
    return this.transition(current.identity, "abandoned", { reason });
  }
}

export interface GithubCommand {
  run(args: readonly string[], input?: string): Promise<string>;
}

export class GhGithubAdapter implements GithubCommand {
  readonly repository: string;

  constructor(repository = process.env.GITHUB_REPOSITORY) {
    if (!repository) throw new Error("GITHUB_REPOSITORY is required for GitHub release operations");
    this.repository = repository;
  }

  async run(args: readonly string[], input?: string): Promise<string> {
    const result = Bun.spawnSync(["gh", ...args], {
      stdin: input === undefined ? "ignore" : new Blob([input]),
      stdout: "pipe",
      stderr: "pipe",
    });
    if (result.exitCode !== 0) throw new Error(new TextDecoder().decode(result.stderr).trim() || "gh command failed");
    return new TextDecoder().decode(result.stdout).trim();
  }

  async json<T>(args: readonly string[], input?: string): Promise<T> {
    return JSON.parse(await this.run(args, input)) as T;
  }

  async mainHead(): Promise<string> {
    const result = await this.json<{ object: { sha: string } }>(["api", `repos/${this.repository}/git/ref/heads/main`]);
    return result.object.sha;
  }

  async commitChecks(revision: string): Promise<unknown[]> {
    const result = await this.json<{ check_runs: unknown[] }>(["api", `repos/${this.repository}/commits/${revision}/check-runs`]);
    return result.check_runs;
  }

  async sourcePulls(revision: string): Promise<unknown[]> {
    return this.json<unknown[]>(["api", `repos/${this.repository}/commits/${revision}/pulls`]);
  }

  async releases(): Promise<unknown[]> {
    return this.json<unknown[]>(["api", `repos/${this.repository}/releases?per_page=100`]);
  }

  async tagCommit(tag: string): Promise<string> {
    const ref = await this.json<{ object?: { sha?: string; type?: string } }>(["api", `repos/${this.repository}/git/ref/tags/${tag}`]);
    const object = ref.object;
    if (!object?.sha) throw new Error(`Git tag has no object: ${tag}`);
    if (object.type === "commit" || !object.type) return object.sha;
    const annotated = await this.json<{ object?: { sha?: string; type?: string } }>(["api", `repos/${this.repository}/git/tags/${object.sha}`]);
    if (annotated.object?.type !== "commit" || !annotated.object.sha) throw new Error(`Git tag does not resolve to a commit: ${tag}`);
    return annotated.object.sha;
  }

  async compare(base: string, head: string): Promise<{ status?: string; ahead_by?: number; behind_by?: number }> {
    return this.json(["api", `repos/${this.repository}/compare/${base}...${head}`]);
  }

  async createDraft(tag: string, target: string, assets: readonly string[], title: string, notes: string): Promise<void> {
    await this.run(["release", "create", tag, ...assets, "--draft", "--target", target, "--title", title, "--notes", notes]);
  }

  async viewRelease(tag: string): Promise<unknown> {
    const release = await this.json<any>(["api", `repos/${this.repository}/releases/tags/${tag}`]);
    return {
      tagName: release.tag_name,
      isDraft: release.draft,
      isPrerelease: release.prerelease,
      immutable: release.immutable,
      releaseId: release.id,
      assets: Array.isArray(release.assets) ? release.assets.map((asset: any) => ({ name: asset.name, size: asset.size, digest: asset.digest })) : [],
    };
  }

  async downloadRelease(tag: string, directory: string): Promise<void> {
    await this.run(["release", "download", tag, "--dir", directory]);
  }

  async publish(tag: string): Promise<void> {
    await this.run(["release", "edit", tag, "--draft=false"]);
  }
}
