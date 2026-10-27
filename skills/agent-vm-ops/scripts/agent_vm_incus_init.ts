#!/usr/bin/env bun

/** Check and explicitly initialize the Incus project/profile policy for Agent VM. */

export const PROJECT_AGGREGATE_LIMITS = [
  "limits.processes",
  "limits.cpu",
  "limits.memory",
  "limits.instances",
] as const;

export const DEFAULT_PROCESS_LIMIT = 512;
const USAGE = "usage: agent_vm_incus_init.ts --remote <configured-control-alias> [--project <name>] [--profile <name>] [--process-limit <positive-integer>] [--apply]";

export type CommandResult = {
  exitCode: number;
  stdout: string;
  stderr: string;
};

export type CommandRunner = (command: string[]) => CommandResult;

export type IncusSnapshot = {
  project: Record<string, unknown>;
  profile: Record<string, unknown>;
};

export type InitializationAction = {
  kind: "unset-project-config" | "set-profile-config";
  key: string;
  current: unknown;
  desired: string | null;
};

export type InitializationPlan = {
  aligned: boolean;
  actions: InitializationAction[];
};

export type InitializationOptions = {
  remote: string;
  project: string;
  profile: string;
  processLimit: number;
  apply: boolean;
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function decode(value: Uint8Array): string {
  return new TextDecoder().decode(value);
}

function remoteCommand(remote: string, args: string[]): string[] {
  return ["ssh", "-o", "BatchMode=yes", remote, "--", ...args];
}

export function projectQueryCommand(remote: string, project: string): string[] {
  return remoteCommand(remote, ["incus", "query", `/1.0/projects/${encodeURIComponent(project)}`]);
}

export function profileQueryCommand(remote: string, project: string, profile: string): string[] {
  return remoteCommand(remote, [
    "incus",
    "query",
    `/1.0/profiles/${encodeURIComponent(profile)}?project=${encodeURIComponent(project)}`,
  ]);
}

export function actionCommand(action: InitializationAction, options: InitializationOptions): string[] {
  if (action.kind === "unset-project-config") {
    return remoteCommand(options.remote, ["incus", "project", "unset", options.project, action.key]);
  }
  return remoteCommand(options.remote, [
    "incus",
    "profile",
    "set",
    options.profile,
    `${action.key}=${action.desired}`,
    "--project",
    options.project,
  ]);
}

function queryMetadata(result: CommandResult, label: string): Record<string, unknown> {
  if (result.exitCode !== 0) {
    throw new Error(`${label} query failed: ${result.stderr.trim() || `exit ${result.exitCode}`}`);
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(result.stdout);
  } catch (error) {
    throw new Error(`${label} query returned invalid JSON: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (!isRecord(parsed)) throw new Error(`${label} query did not return an Incus object`);
  // `incus query` normally unwraps metadata; --raw and older wrappers may retain the envelope.
  if (isRecord(parsed.metadata)) return parsed.metadata;
  return parsed;
}

function configFromMetadata(metadata: Record<string, unknown>, label: string): Record<string, unknown> {
  if (!isRecord(metadata.config)) throw new Error(`${label} query did not return a config object`);
  return metadata.config;
}

export function readSnapshot(options: InitializationOptions, runner: CommandRunner): IncusSnapshot {
  const project = configFromMetadata(
    queryMetadata(runner(projectQueryCommand(options.remote, options.project)), "project"),
    "project",
  );
  const profile = configFromMetadata(
    queryMetadata(runner(profileQueryCommand(options.remote, options.project, options.profile)), "profile"),
    "profile",
  );
  return { project, profile };
}

export function buildInitializationPlan(snapshot: IncusSnapshot, processLimit: number = DEFAULT_PROCESS_LIMIT): InitializationPlan {
  const actions: InitializationAction[] = [];
  for (const key of PROJECT_AGGREGATE_LIMITS) {
    if (Object.hasOwn(snapshot.project, key)) {
      actions.push({ kind: "unset-project-config", key, current: snapshot.project[key], desired: null });
    }
  }

  const current = snapshot.profile["limits.processes"];
  if (!Object.hasOwn(snapshot.profile, "limits.processes") || String(current) !== String(processLimit)) {
    actions.push({
      kind: "set-profile-config",
      key: "limits.processes",
      current: Object.hasOwn(snapshot.profile, "limits.processes") ? current : null,
      desired: String(processLimit),
    });
  }
  return { aligned: actions.length === 0, actions };
}

export function applyInitializationPlan(
  plan: InitializationPlan,
  options: InitializationOptions,
  runner: CommandRunner,
): void {
  for (const action of plan.actions) {
    const command = actionCommand(action, options);
    const result = runner(command);
    if (result.exitCode !== 0) {
      throw new Error(`initialization action failed (${action.kind}:${action.key}): ${result.stderr.trim() || `exit ${result.exitCode}`}`);
    }
  }
}

export function initialize(
  options: InitializationOptions,
  runner: CommandRunner,
): {
  status: "aligned" | "needs_action";
  dry_run: boolean;
  actions: Array<InitializationAction & { command: string[] }>;
} {
  const before = readSnapshot(options, runner);
  const plan = buildInitializationPlan(before, options.processLimit);
  const actions = plan.actions.map(action => ({ ...action, command: actionCommand(action, options) }));
  if (!options.apply) {
    return { status: plan.aligned ? "aligned" : "needs_action", dry_run: true, actions };
  }

  applyInitializationPlan(plan, options, runner);
  const after = readSnapshot(options, runner);
  const verification = buildInitializationPlan(after, options.processLimit);
  if (!verification.aligned) {
    throw new Error(`post-apply verification failed: ${JSON.stringify(verification.actions)}`);
  }
  return { status: "aligned", dry_run: false, actions };
}

function runRemote(command: string[]): CommandResult {
  const result = Bun.spawnSync(command, { stdout: "pipe", stderr: "pipe" });
  return {
    exitCode: result.exitCode,
    stdout: decode(result.stdout),
    stderr: decode(result.stderr),
  };
}

function option(args: string[], name: string): string | undefined {
  const index = args.indexOf(name);
  return index < 0 ? undefined : args[index + 1];
}

function parseOptions(args: string[]): InitializationOptions | "help" {
  if (args.includes("--help")) return "help";
  const valueOptions = new Set(["--remote", "--project", "--profile", "--process-limit"]);
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    if (arg === "--apply") continue;
    if (!valueOptions.has(arg) || !args[index + 1] || args[index + 1].startsWith("--")) throw new Error(USAGE);
    index += 1;
  }

  const remote = option(args, "--remote");
  if (!remote) throw new Error(`${USAGE}\n--remote is required`);
  const project = option(args, "--project") ?? "agents";
  const profile = option(args, "--profile") ?? "agent-vm";
  const rawProcessLimit = option(args, "--process-limit") ?? String(DEFAULT_PROCESS_LIMIT);
  if (!/^\d+$/.test(rawProcessLimit)) throw new Error("--process-limit must be a positive integer");
  const processLimit = Number(rawProcessLimit);
  if (!Number.isSafeInteger(processLimit) || processLimit < 1) throw new Error("--process-limit must be a positive integer");
  return { remote, project, profile, processLimit, apply: args.includes("--apply") };
}

function printHelp(): void {
  console.log(`${USAGE}\n\nDefault mode reads Incus JSON and prints the required changes. Add --apply only after entering maintenance drain and reviewing the plan.`);
}

if (import.meta.main) {
  try {
    const parsed = parseOptions(process.argv.slice(2));
    if (parsed === "help") {
      printHelp();
    } else {
      const result = initialize(parsed, runRemote);
      console.log(JSON.stringify({
        schema_version: 1,
        remote: parsed.remote,
        project: parsed.project,
        profile: parsed.profile,
        process_limit: parsed.processLimit,
        ...result,
      }, null, 2));
      if (result.dry_run && result.actions.length > 0) process.exitCode = 2;
    }
  } catch (error) {
    console.error(JSON.stringify({ error: error instanceof Error ? error.message : String(error) }));
    process.exitCode = 1;
  }
}
