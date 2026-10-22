#!/usr/bin/env bun

import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { validateDashboard, validateProvisioning } from "./validate_observability_dashboard.ts";

export type ObservabilityConfig = {
  dashboard_uid: string;
  dashboard_title: string;
  grafana_org_id: number;
  provider_name: string;
  dashboard_folder: string;
  prometheus_datasource_uid: string;
  controller_job: string;
  dashboard_path: string;
  dashboard_filename?: string;
};

export type RenderedBundle = {
  dashboard: Record<string, unknown>;
  dashboardText: string;
  provisioningText: string;
  dashboardFilename: string;
};

const TEMPLATE_DIR = resolve(dirname(fileURLToPath(import.meta.url)), "../references/observability");
const TOKENS = {
  dashboard_uid: "__DASHBOARD_UID__",
  dashboard_title: "__DASHBOARD_TITLE__",
  grafana_org_id: "__GRAFANA_ORG_ID__",
  provider_name: "__DASHBOARD_PROVIDER_NAME__",
  dashboard_folder: "__DASHBOARD_FOLDER__",
  prometheus_datasource_uid: "__PROMETHEUS_UID__",
  controller_job: "__CONTROLLER_JOB__",
  dashboard_path: "__DASHBOARD_PATH__",
} as const;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requiredString(config: Record<string, unknown>, field: keyof ObservabilityConfig): string {
  const value = config[field];
  if (typeof value !== "string" || value.trim() === "") throw new Error(`${field} must be a non-empty string`);
  if (/\r|\n|[\u0000-\u001f]/.test(value)) throw new Error(`${field} must not contain control characters`);
  if (/\$\{[^}]+\}|__[A-Z0-9_]+__/.test(value)) throw new Error(`${field} still contains a placeholder`);
  return value;
}

export function readConfig(path: string): ObservabilityConfig {
  let parsed: unknown;
  try {
    parsed = JSON.parse(readFileSync(path, "utf8"));
  } catch (error) {
    throw new Error(`cannot parse config ${path}: ${String(error)}`);
  }
  if (!isRecord(parsed)) throw new Error("config must be a JSON object");
  const grafanaOrgId = parsed.grafana_org_id;
  if (typeof grafanaOrgId !== "number" || !Number.isSafeInteger(grafanaOrgId) || grafanaOrgId <= 0) throw new Error("grafana_org_id must be a positive integer");

  const config = {
    dashboard_uid: requiredString(parsed, "dashboard_uid"),
    dashboard_title: requiredString(parsed, "dashboard_title"),
    grafana_org_id: grafanaOrgId,
    provider_name: requiredString(parsed, "provider_name"),
    dashboard_folder: requiredString(parsed, "dashboard_folder"),
    prometheus_datasource_uid: requiredString(parsed, "prometheus_datasource_uid"),
    controller_job: requiredString(parsed, "controller_job"),
    dashboard_path: requiredString(parsed, "dashboard_path"),
    dashboard_filename: typeof parsed.dashboard_filename === "undefined" ? undefined : requiredString(parsed, "dashboard_filename"),
  } satisfies ObservabilityConfig;

  if (!/^[A-Za-z0-9_-]{1,40}$/.test(config.dashboard_uid)) throw new Error("dashboard_uid contains unsupported characters");
  if (!/^[A-Za-z0-9_-]{1,64}$/.test(config.prometheus_datasource_uid)) throw new Error("prometheus_datasource_uid contains unsupported characters");
  if (!/^[A-Za-z0-9_.:-]{1,128}$/.test(config.controller_job)) throw new Error("controller_job contains unsupported characters");
  if (!config.dashboard_path.startsWith("/") || config.dashboard_path.includes("..")) throw new Error("dashboard_path must be an absolute path without ..");
  if (config.dashboard_filename && !/^[A-Za-z0-9._-]+\.json$/.test(config.dashboard_filename)) throw new Error("dashboard_filename must be a simple .json filename");
  return config;
}

function renderText(template: string, replacements: Record<string, string>): string {
  let rendered = template;
  for (const [token, value] of Object.entries(replacements)) rendered = rendered.split(token).join(value);
  const leftovers = rendered.match(/__[A-Z0-9_]+__/g);
  if (leftovers) throw new Error(`template contains unresolved placeholders: ${[...new Set(leftovers)].join(", ")}`);
  return rendered;
}

export function renderBundle(config: ObservabilityConfig, templateDir: string = TEMPLATE_DIR): RenderedBundle {
  const replacements = {
    [TOKENS.dashboard_uid]: config.dashboard_uid,
    [TOKENS.dashboard_title]: config.dashboard_title,
    [TOKENS.grafana_org_id]: String(config.grafana_org_id),
    [TOKENS.provider_name]: config.provider_name,
    [TOKENS.dashboard_folder]: config.dashboard_folder,
    [TOKENS.prometheus_datasource_uid]: config.prometheus_datasource_uid,
    [TOKENS.controller_job]: config.controller_job,
    [TOKENS.dashboard_path]: config.dashboard_path,
  };
  const dashboardText = renderText(readFileSync(resolve(templateDir, "dashboard.json.template"), "utf8"), replacements);
  let dashboard: Record<string, unknown>;
  try {
    const parsed: unknown = JSON.parse(dashboardText);
    if (!isRecord(parsed)) throw new Error("dashboard must be an object");
    dashboard = parsed;
  } catch (error) {
    throw new Error(`rendered dashboard is not valid JSON: ${String(error)}`);
  }
  const provisioningText = renderText(readFileSync(resolve(templateDir, "provisioning.yml.template"), "utf8"), replacements);
  const findings = [
    ...validateDashboard(dashboard, config.prometheus_datasource_uid),
    ...validateProvisioning(provisioningText),
  ];
  if (findings.length > 0) throw new Error(`rendered observability bundle failed validation:\n${findings.join("\n")}`);
  return {
    dashboard,
    dashboardText: `${JSON.stringify(dashboard, null, 2)}\n`,
    provisioningText,
    dashboardFilename: config.dashboard_filename ?? "agent-vm-observability.json",
  };
}

export function writeBundle(bundle: RenderedBundle, outputDir: string, force = false): { dashboardPath: string; provisioningPath: string } {
  const directory = resolve(outputDir);
  mkdirSync(directory, { recursive: true });
  const dashboardPath = resolve(directory, bundle.dashboardFilename);
  const provisioningPath = resolve(directory, "agent-vm-observability.yml");
  if (!force && (existsSync(dashboardPath) || existsSync(provisioningPath))) {
    throw new Error(`output exists; pass --force to replace ${basename(dashboardPath)} or agent-vm-observability.yml`);
  }
  writeFileSync(dashboardPath, bundle.dashboardText, "utf8");
  writeFileSync(provisioningPath, bundle.provisioningText, "utf8");
  return { dashboardPath, provisioningPath };
}

function parseArgs(args: string[]): { config: string; outputDir: string; templateDir?: string; force: boolean } {
  let config: string | undefined;
  let outputDir: string | undefined;
  let templateDir: string | undefined;
  let force = false;
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    const next = args[index + 1];
    if (arg === "--config" && next && !next.startsWith("--")) {
      config = next;
      index += 1;
    } else if (arg === "--output-dir" && next && !next.startsWith("--")) {
      outputDir = next;
      index += 1;
    } else if (arg === "--template-dir" && next && !next.startsWith("--")) {
      templateDir = next;
      index += 1;
    } else if (arg === "--force") {
      force = true;
    } else if (arg === "--help") {
      if (args.length !== 1) throw new Error("usage: render_observability_dashboard.ts --config <path> --output-dir <path> [--template-dir <path>] [--force]");
      console.log("usage: render_observability_dashboard.ts --config <path> --output-dir <path> [--template-dir <path>] [--force]");
      process.exit(0);
    } else {
      throw new Error("usage: render_observability_dashboard.ts --config <path> --output-dir <path> [--template-dir <path>] [--force]");
    }
  }
  if (!config || !outputDir) throw new Error("usage: render_observability_dashboard.ts --config <path> --output-dir <path> [--template-dir <path>] [--force]");
  return { config, outputDir, templateDir, force };
}

export function main(args: string[] = Bun.argv.slice(2)): number {
  try {
    const parsed = parseArgs(args);
    const config = readConfig(parsed.config);
    const bundle = renderBundle(config, parsed.templateDir ? resolve(parsed.templateDir) : TEMPLATE_DIR);
    const paths = writeBundle(bundle, parsed.outputDir, parsed.force);
    console.log(`WROTE dashboard ${paths.dashboardPath}`);
    console.log(`WROTE provisioning ${paths.provisioningPath}`);
    return 0;
  } catch (error) {
    console.error(`ERROR ${String(error)}`);
    return 1;
  }
}

if (import.meta.main) process.exitCode = main();
