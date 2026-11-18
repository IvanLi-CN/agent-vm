#!/usr/bin/env bun
// @ts-nocheck
// @bun

// skill/scripts/host-config.ts
import { existsSync, mkdirSync, readFileSync, chmodSync, writeFileSync } from "fs";
import { homedir } from "os";
import { join } from "path";

// skill/scripts/client-io.ts
var record = (v) => v !== null && typeof v === "object" && !Array.isArray(v) && !(v instanceof FloatToken);

class CliError extends Error {
  code;
  detail;
  exitCode;
  constructor(code, message, detail, exitCode = 2) {
    super(message);
    this.code = code;
    this.detail = detail;
    this.exitCode = exitCode;
  }
  asDict() {
    return { code: this.code, message: this.message, ...this.detail === undefined ? {} : { detail: this.detail } };
  }
}
function fail(code, message, detail, exitCode = 2) {
  throw new CliError(code, message, detail, exitCode);
}
class FloatToken {
  token;
  constructor(token) {
    this.token = token;
  }
}

// skill/scripts/host-config.ts
var ALIAS = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
function configPath(env = process.env) {
  return join(env.XDG_CONFIG_HOME ?? join(homedir(), ".config"), "agent-vm", "config.toml");
}
function expand(value) {
  return value.startsWith("~/") ? join(homedir(), value.slice(2)) : value;
}
function parseProfile(value, alias) {
  if (!record(value) || value.transport !== "https" && value.transport !== "ssh" || typeof value.control_host !== "string" || !value.control_host.trim())
    fail("config_invalid", `host ${alias} must define transport and control_host`);
  const profile = { transport: value.transport, control_host: value.control_host };
  for (const key of ["ca_file", "token_env", "token_file", "ssh_config", "identity_file", "known_hosts", "remote_request", "remote_socket"])
    if (value[key] !== undefined) {
      if (typeof value[key] !== "string" || !value[key].trim())
        fail("config_invalid", `host ${alias}.${key} must be a non-empty string`);
      profile[key] = expand(value[key]);
    }
  return profile;
}
function loadHostConfig(env = process.env) {
  const path = configPath(env);
  if (!existsSync(path))
    return { hosts: {}, projects: {} };
  let raw;
  try {
    raw = Bun.TOML.parse(readFileSync(path, "utf8"));
  } catch {
    fail("config_invalid", "agent-vm host configuration is invalid TOML");
  }
  const hosts = {}, projects = {};
  if (raw.hosts !== undefined) {
    if (!record(raw.hosts))
      fail("config_invalid", "hosts must be a table");
    for (const [alias, value] of Object.entries(raw.hosts)) {
      if (!ALIAS.test(alias))
        fail("config_invalid", `invalid host alias ${alias}`);
      hosts[alias] = parseProfile(value, alias);
    }
  }
  if (raw.projects !== undefined) {
    if (!record(raw.projects))
      fail("config_invalid", "projects must be a table");
    for (const [remote, value] of Object.entries(raw.projects)) {
      const alias = typeof value === "string" ? value : record(value) && typeof value.host === "string" ? value.host : undefined;
      if (!alias || !ALIAS.test(alias))
        fail("config_invalid", `invalid project binding for ${remote}`);
      projects[remote] = alias;
    }
  }
  if (raw.default_host !== undefined && (typeof raw.default_host !== "string" || !ALIAS.test(raw.default_host)))
    fail("config_invalid", "default_host must be a valid host alias");
  return { default_host: raw.default_host, hosts, projects };
}
function projectRemote(env) {
  if (env.AGENT_VM_PROJECT_REMOTE)
    return env.AGENT_VM_PROJECT_REMOTE;
  return;
}
function normalizeRemote(remote) {
  return remote.trim().replace(/^git@([^:]+):/, "$1/").replace(/^https?:\/\//, "").replace(/\.git$/, "").replace(/\/+$/, "");
}
function resolveHost(explicit, env = process.env) {
  const config = loadHostConfig(env), project = projectRemote(env);
  if (!explicit && !env.AGENT_VM_HOST && project && !config.projects[normalizeRemote(project)])
    fail("config_invalid", "explicit project has no host binding");
  const alias = explicit ?? env.AGENT_VM_HOST ?? (project ? config.projects[normalizeRemote(project)] : undefined) ?? config.default_host;
  if (!alias)
    fail("host_selection_required", "select an Agent VM host with --host or configure default_host");
  if (!ALIAS.test(alias) || !config.hosts[alias])
    fail("config_invalid", `unknown Agent VM host alias: ${alias}`);
  const profile = config.hosts[alias];
  return { alias, transport: profile.transport, control_host: profile.control_host, identity: `${profile.transport}:${profile.control_host}`, compatibility: false, profile };
}

// skill/scripts/agent_vm_incus_init.source.ts
var PROJECT_AGGREGATE_LIMITS = [
  "limits.processes",
  "limits.cpu",
  "limits.memory",
  "limits.instances"
];
var DEFAULT_PROCESS_LIMIT = 512;
var USAGE = "usage: agent_vm_incus_init.ts (--host <alias> | --remote <configured-control-alias>) [--project <name>] [--profile <name>] [--process-limit <positive-integer>] [--apply]";
function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function decode(value) {
  return new TextDecoder().decode(value);
}
function remoteCommand(remote, args) {
  return ["ssh", "-o", "BatchMode=yes", remote, "--", ...args];
}
function projectQueryCommand(remote, project) {
  return remoteCommand(remote, ["incus", "query", `/1.0/projects/${encodeURIComponent(project)}`]);
}
function profileQueryCommand(remote, project, profile) {
  return remoteCommand(remote, [
    "incus",
    "query",
    `/1.0/profiles/${encodeURIComponent(profile)}?project=${encodeURIComponent(project)}`
  ]);
}
function actionCommand(action, options) {
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
    options.project
  ]);
}
function queryMetadata(result, label) {
  if (result.exitCode !== 0) {
    throw new Error(`${label} query failed: ${result.stderr.trim() || `exit ${result.exitCode}`}`);
  }
  let parsed;
  try {
    parsed = JSON.parse(result.stdout);
  } catch (error) {
    throw new Error(`${label} query returned invalid JSON: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (!isRecord(parsed))
    throw new Error(`${label} query did not return an Incus object`);
  if (isRecord(parsed.metadata))
    return parsed.metadata;
  return parsed;
}
function configFromMetadata(metadata, label) {
  if (!isRecord(metadata.config))
    throw new Error(`${label} query did not return a config object`);
  return metadata.config;
}
function readSnapshot(options, runner) {
  const project = configFromMetadata(queryMetadata(runner(projectQueryCommand(options.remote, options.project)), "project"), "project");
  const profile = configFromMetadata(queryMetadata(runner(profileQueryCommand(options.remote, options.project, options.profile)), "profile"), "profile");
  return { project, profile };
}
function buildInitializationPlan(snapshot, processLimit = DEFAULT_PROCESS_LIMIT) {
  const actions = [];
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
      desired: String(processLimit)
    });
  }
  return { aligned: actions.length === 0, actions };
}
function applyInitializationPlan(plan, options, runner) {
  for (const action of plan.actions) {
    const command = actionCommand(action, options);
    const result = runner(command);
    if (result.exitCode !== 0) {
      throw new Error(`initialization action failed (${action.kind}:${action.key}): ${result.stderr.trim() || `exit ${result.exitCode}`}`);
    }
  }
}
function initialize(options, runner) {
  const before = readSnapshot(options, runner);
  const plan = buildInitializationPlan(before, options.processLimit);
  const actions = plan.actions.map((action) => ({ ...action, command: actionCommand(action, options) }));
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
function runRemote(command) {
  const result = Bun.spawnSync(command, { stdout: "pipe", stderr: "pipe" });
  return {
    exitCode: result.exitCode,
    stdout: decode(result.stdout),
    stderr: decode(result.stderr)
  };
}
function option(args, name) {
  const index = args.indexOf(name);
  return index < 0 ? undefined : args[index + 1];
}
function parseOptions(args) {
  if (args.includes("--help"))
    return "help";
  const valueOptions = new Set(["--host", "--remote", "--project", "--profile", "--process-limit"]);
  for (let index = 0;index < args.length; index += 1) {
    const arg = args[index];
    if (arg === "--apply")
      continue;
    if (!valueOptions.has(arg) || !args[index + 1] || args[index + 1].startsWith("--"))
      throw new Error(USAGE);
    index += 1;
  }
  const host = option(args, "--host"), configuredRemote = option(args, "--remote");
  if (host && configuredRemote)
    throw new Error("use only one of --host and --remote");
  let remote = configuredRemote;
  if (host) {
    const selection = resolveHost(host);
    if (selection.transport !== "ssh")
      throw new Error("agent-vm-ops Incus initialization requires an SSH host profile");
    remote = selection.control_host;
  }
  if (!remote)
    throw new Error(`${USAGE}
--remote is required`);
  const project = option(args, "--project") ?? "agents";
  const profile = option(args, "--profile") ?? "agent-vm";
  const rawProcessLimit = option(args, "--process-limit") ?? String(DEFAULT_PROCESS_LIMIT);
  if (!/^\d+$/.test(rawProcessLimit))
    throw new Error("--process-limit must be a positive integer");
  const processLimit = Number(rawProcessLimit);
  if (!Number.isSafeInteger(processLimit) || processLimit < 1)
    throw new Error("--process-limit must be a positive integer");
  return { remote, project, profile, processLimit, apply: args.includes("--apply") };
}
function printHelp() {
  console.log(`${USAGE}

Default mode reads Incus JSON and prints the required changes. Add --apply only after entering maintenance drain and reviewing the plan.`);
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
        ...result
      }, null, 2));
      if (result.dry_run && result.actions.length > 0)
        process.exitCode = 2;
    }
  } catch (error) {
    console.error(JSON.stringify({ error: error instanceof Error ? error.message : String(error) }));
    process.exitCode = 1;
  }
}
export {
  DEFAULT_PROCESS_LIMIT,
  PROJECT_AGGREGATE_LIMITS,
  actionCommand,
  applyInitializationPlan,
  buildInitializationPlan,
  initialize,
  profileQueryCommand,
  projectQueryCommand,
  readSnapshot
};

export type CommandResult = { exitCode: number; stdout: string; stderr: string };
export type CommandRunner = (command: string[]) => CommandResult;
export type IncusSnapshot = { project: Record<string, unknown>; profile: Record<string, unknown> };
export type InitializationAction = {
  kind: "unset-project-config" | "set-profile-config";
  key: string;
  current: unknown;
  desired: string | null;
};
export type InitializationPlan = { aligned: boolean; actions: InitializationAction[] };
export type InitializationOptions = {
  remote: string;
  project: string;
  profile: string;
  processLimit: number;
  apply: boolean;
};
