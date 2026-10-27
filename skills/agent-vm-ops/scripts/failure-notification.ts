#!/usr/bin/env bun

/** Build privacy-safe failure context and stop at the transport selection gate. */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const args = process.argv.slice(2);
const option = (name: string): string | undefined => {
  const index = args.indexOf(name);
  return index < 0 ? undefined : args[index + 1];
};

export interface FailureContext {
  repository: string;
  workflow: string;
  run_id: string;
  attempt: string;
  head_sha: string;
  actor: string;
  run_url: string;
  conclusion: "failure";
  idempotency_key: string;
  release?: {
    version: string;
    source_revision: string;
    reservation: string;
    recovery_stage: string;
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

export function buildFailureContext(env: NodeJS.ProcessEnv = process.env): FailureContext {
  const repository = env.FAILED_REPOSITORY ?? env.GITHUB_REPOSITORY ?? required(env, "GITHUB_REPOSITORY");
  const workflow = env.FAILED_WORKFLOW ?? env.GITHUB_WORKFLOW ?? required(env, "GITHUB_WORKFLOW");
  const run_id = env.FAILED_RUN_ID ?? env.GITHUB_RUN_ID ?? required(env, "GITHUB_RUN_ID");
  const attempt = env.FAILED_RUN_ATTEMPT ?? env.GITHUB_RUN_ATTEMPT ?? "1";
  const head_sha = env.FAILED_HEAD_SHA ?? env.GITHUB_SHA ?? required(env, "GITHUB_SHA");
  const actor = env.FAILED_ACTOR ?? env.GITHUB_ACTOR ?? "unknown";
  const server = env.GITHUB_SERVER_URL ?? "https://github.com";
  const conclusion = env.FAILED_CONCLUSION ?? env.CONCLUSION;
  if (conclusion !== "failure") throw new Error("failure notification only accepts a failed workflow conclusion");
  const context: FailureContext = {
    repository,
    workflow,
    run_id,
    attempt,
    head_sha,
    actor,
    run_url: `${server}/${repository}/actions/runs/${run_id}`,
    conclusion,
    idempotency_key: `${repository}:${workflow}:${run_id}:${attempt}`,
  };
  if (workflow === "Release") {
    const version = env.RELEASE_VERSION;
    const source_revision = env.RELEASE_SOURCE_REVISION;
    const reservation = env.RELEASE_RESERVATION;
    const recovery_stage = env.RELEASE_RECOVERY_STAGE;
    if (version && source_revision && reservation && recovery_stage) context.release = { version, source_revision, reservation, recovery_stage };
  }
  return context;
}

try {
  const contract = config();
  const context = buildFailureContext({ ...process.env, CONCLUSION: process.env.CONCLUSION ?? "failure" });
  const workflowClass = contract.workflow_classes?.[context.workflow];
  if (workflowClass !== (option("--class") ?? "generic")) throw new Error(`workflow class is not declared for ${context.workflow}`);
  const payload = { schema_version: 1, delivery: "blocked", reason: contract.transport_selection?.status === "unselected" ? "transport_unselected" : "transport_not_ready", context };
  console.log(JSON.stringify(payload));
  if (contract.transport_selection?.status !== "selected") process.exitCode = 78;
  else throw new Error("selected notification transport has no delivery adapter in this repository");
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
}
