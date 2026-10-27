#!/usr/bin/env bun

/** Build privacy-safe failure context and stop at the transport selection gate. */

import { existsSync, readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

const args = process.argv.slice(2);
const option = (name: string): string | undefined => {
  const index = args.indexOf(name);
  return index < 0 ? undefined : args[index + 1];
};

export interface FailureContext {
  repository: string;
  workflow: string;
  event: string;
  ref: string;
  head_branch: string;
  pull_request?: { number: string };
  run_id: string;
  attempt: string;
  head_sha: string;
  actor: string;
  run_url: string;
  rerun_url: string;
  rerun_guidance: string;
  conclusion: "failure";
  idempotency_key: string;
  release?: {
    version: string;
    tag: string;
    source_revision: string;
    reservation: string;
    recovery_stage: string;
    artifacts: string[];
    recovery_command: string;
  };
}

function required(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name];
  if (!value || /[\r\n]/.test(value)) throw new Error(`failure context is missing ${name}`);
  return value;
}

function config(): any {
  const path = resolve(option("--config") ?? ".github/release-failure-notification.json");
  return JSON.parse(readFileSync(path, "utf8"));
}

function optional(env: NodeJS.ProcessEnv, name: string, fallback: string): string {
  const value = env[name];
  return value && !/[\r\n]/.test(value) ? value : fallback;
}

function jsonlFiles(root: string): string[] {
  const result: string[] = [];
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = resolve(root, entry.name);
    if (entry.isDirectory()) result.push(...jsonlFiles(path));
    else if (entry.isFile() && entry.name.endsWith(".jsonl")) result.push(path);
  }
  return result;
}

function verifiedReleaseContext(env: NodeJS.ProcessEnv): FailureContext["release"] {
  const root = env.RELEASE_STATE_ROOT;
  if (!root || !existsSync(root)) return undefined;
  const source = required(env, "FAILED_HEAD_SHA");
  const files = jsonlFiles(root);
  const candidates: any[] = [];
  for (const file of files) {
    const text = readFileSync(file, "utf8");
    for (const line of text.split("\n").filter(Boolean)) {
      try {
        const event = JSON.parse(line);
        if (event?.identity?.dispatch_sha === source && event?.identity?.source_revision === source && event?.identity?.version) candidates.push(event);
      } catch { /* invalid recovery artifacts are ignored, never trusted */ }
    }
  }
  const event = candidates.at(-1);
  if (!event || !event.identity || !event.stage) return undefined;
  const version = String(event.identity.version);
  if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.test(version)) return undefined;
  const tag = typeof event.detail?.tag === "string" ? event.detail.tag : `v${version}`;
  const artifacts = event.detail?.artifacts && typeof event.detail.artifacts === "object" ? Object.entries(event.detail.artifacts).flatMap(([name, digest]) => typeof digest === "string" && /^[0-9a-f]{64}$/.test(digest) ? [`${name}:${digest}`] : []) : [];
  if (!artifacts.length || event.identity.source_revision !== source) return undefined;
  return { version, tag, source_revision: source, reservation: version, recovery_stage: String(event.stage), artifacts, recovery_command: `Release recovery: recover reservation ${version}` };
}

export function buildFailureContext(env: NodeJS.ProcessEnv = process.env): FailureContext {
  const repository = env.FAILED_REPOSITORY ?? env.GITHUB_REPOSITORY ?? required(env, "GITHUB_REPOSITORY");
  const workflow = env.FAILED_WORKFLOW ?? env.GITHUB_WORKFLOW ?? required(env, "GITHUB_WORKFLOW");
  const event = required(env, "FAILED_EVENT");
  const ref = required(env, "FAILED_REF");
  const head_branch = required(env, "FAILED_HEAD_BRANCH");
  const run_id = env.FAILED_RUN_ID ?? env.GITHUB_RUN_ID ?? required(env, "GITHUB_RUN_ID");
  const attempt = env.FAILED_RUN_ATTEMPT ?? env.GITHUB_RUN_ATTEMPT ?? "1";
  const head_sha = env.FAILED_HEAD_SHA ?? env.GITHUB_SHA ?? required(env, "GITHUB_SHA");
  const actor = env.FAILED_ACTOR ?? env.GITHUB_ACTOR ?? "unknown";
  const server = env.GITHUB_SERVER_URL ?? "https://github.com";
  const conclusion = env.FAILED_CONCLUSION ?? env.CONCLUSION;
  if (conclusion !== "failure") throw new Error("failure notification only accepts a failed workflow conclusion");
  if (!/^[0-9a-f]{40}$/.test(head_sha)) throw new Error("failure context head SHA is invalid");
  if (!/^refs\/(heads|pull|tags)\/.+/.test(ref)) throw new Error("failure context ref is invalid");
  const context: FailureContext = {
    repository,
    workflow,
    event,
    ref,
    head_branch,
    ...(env.FAILED_PR_NUMBER ? { pull_request: { number: required(env, "FAILED_PR_NUMBER") } } : {}),
    run_id,
    attempt,
    head_sha,
    actor,
    run_url: optional(env, "FAILED_RUN_URL", `${server}/${repository}/actions/runs/${run_id}`),
    rerun_url: `${server}/${repository}/actions/runs/${run_id}?check_suite_focus=true`,
    rerun_guidance: "Inspect the failed run, then use GitHub Actions Re-run failed jobs after confirming the source revision and inputs.",
    conclusion,
    idempotency_key: `${repository}:${workflow}:${run_id}:${attempt}`,
  };
  if (workflow === "Release") context.release = verifiedReleaseContext(env);
  return context;
}

try {
  const contract = config();
  const context = buildFailureContext({ ...process.env, CONCLUSION: process.env.CONCLUSION ?? "failure" });
  const workflowClass = contract.workflow_classes?.[context.workflow];
  const inventory = contract.workflow_inventory?.[context.workflow];
  if (!inventory || inventory.class === "excluded") throw new Error(`workflow is not an expected notification source: ${context.workflow}`);
  if (Array.isArray(inventory.events) && !inventory.events.includes(context.event)) throw new Error(`failure event is not allowed for ${context.workflow}`);
  if (Array.isArray(inventory.ref_prefixes) && !inventory.ref_prefixes.some((prefix: unknown) => typeof prefix === "string" && context.ref.startsWith(prefix))) throw new Error(`failure ref is not allowed for ${context.workflow}`);
  if (process.env.GITHUB_REPOSITORY && context.repository !== process.env.GITHUB_REPOSITORY) throw new Error("failure repository does not match the trusted workflow repository");
  if (workflowClass !== (option("--class") ?? "generic")) throw new Error(`workflow class is not declared for ${context.workflow}`);
  const payload = { schema_version: 1, delivery: "blocked", reason: contract.transport_selection?.status === "unselected" ? "transport_unselected" : "transport_not_ready", context };
  console.log(JSON.stringify(payload));
  if (contract.transport_selection?.status !== "selected") process.exitCode = 78;
  else throw new Error("selected notification transport has no delivery adapter in this repository");
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
}
