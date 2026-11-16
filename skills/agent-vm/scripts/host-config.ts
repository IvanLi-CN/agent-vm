import { existsSync, mkdirSync, readFileSync, chmodSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { CliError, Data, fail, record } from "./client-io.ts";

export type HostTransport = "https" | "ssh";
export type HostProfile = { transport: HostTransport; control_host: string; ca_file?: string; token_env?: string; token_file?: string; ssh_config?: string; identity_file?: string; known_hosts?: string; remote_request?: string; remote_socket?: string };
export type HostConfig = { default_host?: string; hosts: Record<string, HostProfile>; projects: Record<string, string> };
export type HostSelection = { alias?: string; transport: HostTransport | "legacy"; control_host: string; identity: string; compatibility: boolean; profile?: HostProfile };

const ALIAS = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
export function configPath(env: Record<string, string | undefined> = process.env): string { return join(env.XDG_CONFIG_HOME ?? join(homedir(), ".config"), "agent-vm", "config.toml"); }
function expand(value: string): string { return value.startsWith("~/") ? join(homedir(), value.slice(2)) : value; }
function parseProfile(value: unknown, alias: string): HostProfile {
  if (!record(value) || (value.transport !== "https" && value.transport !== "ssh") || typeof value.control_host !== "string" || !value.control_host.trim()) fail("config_invalid", `host ${alias} must define transport and control_host`);
  const profile: HostProfile = { transport: value.transport, control_host: value.control_host };
  for (const key of ["ca_file", "token_env", "token_file", "ssh_config", "identity_file", "known_hosts", "remote_request", "remote_socket"] as const) if (value[key] !== undefined) {
    if (typeof value[key] !== "string" || !value[key].trim()) fail("config_invalid", `host ${alias}.${key} must be a non-empty string`);
    profile[key] = expand(value[key]);
  }
  return profile;
}
export function loadHostConfig(env: Record<string, string | undefined> = process.env): HostConfig {
  const path = configPath(env);
  if (!existsSync(path)) return { hosts: {}, projects: {} };
  let raw: any; try { raw = Bun.TOML.parse(readFileSync(path, "utf8")); } catch { fail("config_invalid", "agent-vm host configuration is invalid TOML"); }
  const hosts: Record<string, HostProfile> = {}, projects: Record<string, string> = {};
  if (raw.hosts !== undefined) {
    if (!record(raw.hosts)) fail("config_invalid", "hosts must be a table");
    for (const [alias, value] of Object.entries(raw.hosts)) { if (!ALIAS.test(alias)) fail("config_invalid", `invalid host alias ${alias}`); hosts[alias] = parseProfile(value, alias); }
  }
  if (raw.projects !== undefined) {
    if (!record(raw.projects)) fail("config_invalid", "projects must be a table");
    for (const [remote, value] of Object.entries(raw.projects)) {
      const alias = typeof value === "string" ? value : record(value) && typeof value.host === "string" ? value.host : undefined;
      if (!alias || !ALIAS.test(alias)) fail("config_invalid", `invalid project binding for ${remote}`); projects[remote] = alias;
    }
  }
  if (raw.default_host !== undefined && (typeof raw.default_host !== "string" || !ALIAS.test(raw.default_host))) fail("config_invalid", "default_host must be a valid host alias");
  return { default_host: raw.default_host, hosts, projects };
}
function projectRemote(env: Record<string, string | undefined>): string | undefined {
  if (env.AGENT_VM_PROJECT_REMOTE) return env.AGENT_VM_PROJECT_REMOTE;
  const result = spawnSync("git", ["remote", "get-url", "origin"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
  return result.status === 0 && result.stdout.trim() ? result.stdout.trim() : undefined;
}
export function normalizeRemote(remote: string): string {
  return remote.trim().replace(/^git@([^:]+):/, "$1/").replace(/^https?:\/\//, "").replace(/\.git$/, "").replace(/\/+$/, "");
}
export function resolveHost(explicit?: string, env: Record<string, string | undefined> = process.env): HostSelection {
  const config = loadHostConfig(env), alias = explicit ?? env.AGENT_VM_HOST ?? (projectRemote(env) ? config.projects[normalizeRemote(projectRemote(env)!)] : undefined) ?? config.default_host;
  if (!alias) {
    if (env.AGENT_VM_CONTROL_HOST) return { transport: (env.AGENT_VM_TRANSPORT as HostTransport) ?? "legacy", control_host: env.AGENT_VM_CONTROL_HOST, identity: `${env.AGENT_VM_TRANSPORT ?? "legacy"}:${env.AGENT_VM_CONTROL_HOST}`, compatibility: true };
    fail("host_selection_required", "select an Agent VM host with --host or configure default_host");
  }
  if (!ALIAS.test(alias) || !config.hosts[alias]) fail("config_invalid", `unknown Agent VM host alias: ${alias}`);
  const profile = config.hosts[alias];
  return { alias, transport: profile.transport, control_host: profile.control_host, identity: `${profile.transport}:${profile.control_host}`, compatibility: false, profile };
}
export function applyHostSelection(selection: HostSelection, env: Record<string, string | undefined>): Record<string, string | undefined> {
  env.AGENT_VM_CONTROL_HOST = selection.control_host;
  if (selection.transport !== "legacy") env.AGENT_VM_TRANSPORT = selection.transport;
  const profile = selection.profile;
  if (profile?.ca_file) env.AGENT_VM_CONTROL_CA = profile.ca_file;
  if (profile?.transport === "https" && !profile.token_env && !profile.token_file) fail("config_missing", `HTTPS host ${selection.alias} has no credential reference`);
  if (profile?.token_env) { if (!env[profile.token_env]) fail("config_missing", `credential environment variable ${profile.token_env} is missing`); env.AGENT_VM_CONTROL_TOKEN = env[profile.token_env]; }
  if (profile?.token_file) { try { env.AGENT_VM_CONTROL_TOKEN = readFileSync(profile.token_file, "utf8").trim(); } catch { fail("config_missing", `credential file is missing for host ${selection.alias}`); } }
  if (profile?.ssh_config) env.AGENT_VM_SSH_CONFIG = profile.ssh_config;
  if (profile?.remote_request) env.AGENT_VM_REMOTE_REQUEST = profile.remote_request;
  if (profile?.remote_socket) env.AGENT_VM_REMOTE_SOCKET = profile.remote_socket;
  return env;
}
export function ensureConfig(env: Record<string, string | undefined> = process.env): string {
  const path = configPath(env); mkdirSync(join(path, ".."), { recursive: true, mode: 0o700 }); chmodSync(join(path, ".."), 0o700);
  if (!existsSync(path)) { writeFileSync(path, "# agent-vm host profiles\n[hosts]\n[projects]\n", { mode: 0o600 }); chmodSync(path, 0o600); }
  return path;
}
function quote(value: string): string { return JSON.stringify(value); }
export function writeHostConfig(config: HostConfig, env: Record<string, string | undefined> = process.env): string {
  const path = ensureConfig(env), lines: string[] = [];
  if (config.default_host) lines.push(`default_host = ${quote(config.default_host)}`, "");
  for (const [alias, profile] of Object.entries(config.hosts)) {
    lines.push(`[hosts.${alias}]`, `transport = ${quote(profile.transport)}`, `control_host = ${quote(profile.control_host)}`);
    for (const key of ["ca_file", "token_env", "token_file", "ssh_config", "identity_file", "known_hosts", "remote_request", "remote_socket"] as const) if (profile[key]) lines.push(`${key} = ${quote(profile[key]!)}`);
    lines.push("");
  }
  for (const [remote, alias] of Object.entries(config.projects)) lines.push(`[projects.${quote(remote)}]`, `host = ${quote(alias)}`, "");
  writeFileSync(path, lines.join("\n") || "# agent-vm host profiles\n", { mode: 0o600 }); chmodSync(path, 0o600); return path;
}
export function setDefaultHost(alias: string, env: Record<string, string | undefined> = process.env): string {
  const config = loadHostConfig(env); if (!config.hosts[alias]) fail("config_invalid", `unknown Agent VM host alias: ${alias}`); config.default_host = alias; return writeHostConfig(config, env);
}
export function bindProject(alias: string, remote: string | undefined, env: Record<string, string | undefined> = process.env): string {
  const config = loadHostConfig(env); if (!config.hosts[alias]) fail("config_invalid", `unknown Agent VM host alias: ${alias}`); const key = normalizeRemote(remote ?? projectRemote(env) ?? ""); if (!key) fail("config_missing", "current directory has no Git origin remote"); config.projects[key] = alias; return writeHostConfig(config, env);
}
