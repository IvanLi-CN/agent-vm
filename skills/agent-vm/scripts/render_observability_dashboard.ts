#!/usr/bin/env bun

import { randomUUID } from "node:crypto";
import { existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readlinkSync, renameSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from "node:fs";
import { basename, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  INVENTORY_REQUIRED_METRICS,
  INVENTORY_REQUIRED_PANEL_TITLES,
  validateDashboard,
  validateProvisioning,
  INVENTORY_REQUIRED_TASK_STATES,
} from "./validate_observability_dashboard.ts";

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
  inventory_dashboard_uid?: string;
  inventory_dashboard_title?: string;
  inventory_dashboard_filename?: string;
};

export type RenderedBundle = {
  dashboard: Record<string, unknown>;
  dashboardText: string;
  inventoryDashboard: Record<string, unknown>;
  inventoryDashboardText: string;
  provisioningText: string;
  dashboardFilename: string;
  inventoryDashboardFilename: string;
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

function optionalString(config: Record<string, unknown>, field: keyof ObservabilityConfig): string | undefined {
  if (typeof config[field] === "undefined") return undefined;
  return requiredString(config, field);
}

function inventoryUid(config: ObservabilityConfig): string {
  const value = config.inventory_dashboard_uid ?? `${config.dashboard_uid}-inventory`;
  if (!/^[A-Za-z0-9_-]{1,40}$/.test(value)) throw new Error("derived inventory_dashboard_uid contains unsupported characters or exceeds 40 characters");
  if (value === config.dashboard_uid) throw new Error("capacity and inventory dashboard UIDs must be unique");
  return value;
}

function validateOutputFilenames(config: ObservabilityConfig): void {
  const filenames = [
    config.dashboard_filename ?? "agent-vm-observability.json",
    config.inventory_dashboard_filename ?? "agent-vm-observability-inventory.json",
    "agent-vm-observability.yml",
  ];
  if (new Set(filenames).size !== filenames.length) throw new Error("dashboard output filenames must be unique");
}

function validateStableDashboardPath(path: string): void {
  if (!path.startsWith("/") || path.includes("..")) throw new Error("dashboard_path must be an absolute path without ..");
  if (basename(path) !== "current") throw new Error("dashboard_path must target the stable current directory");
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
    inventory_dashboard_uid: optionalString(parsed, "inventory_dashboard_uid"),
    inventory_dashboard_title: optionalString(parsed, "inventory_dashboard_title"),
    inventory_dashboard_filename: optionalString(parsed, "inventory_dashboard_filename"),
  } satisfies ObservabilityConfig;

  if (!/^[A-Za-z0-9_-]{1,40}$/.test(config.dashboard_uid)) throw new Error("dashboard_uid contains unsupported characters");
  if (config.inventory_dashboard_uid && !/^[A-Za-z0-9_-]{1,40}$/.test(config.inventory_dashboard_uid)) throw new Error("inventory_dashboard_uid contains unsupported characters");
  if (!/^[A-Za-z0-9_-]{1,64}$/.test(config.prometheus_datasource_uid)) throw new Error("prometheus_datasource_uid contains unsupported characters");
  if (!/^[A-Za-z0-9_.:-]{1,128}$/.test(config.controller_job)) throw new Error("controller_job contains unsupported characters");
  validateStableDashboardPath(config.dashboard_path);
  if (config.dashboard_filename && !/^[A-Za-z0-9._-]+\.json$/.test(config.dashboard_filename)) throw new Error("dashboard_filename must be a simple .json filename");
  if (config.inventory_dashboard_filename && !/^[A-Za-z0-9._-]+\.json$/.test(config.inventory_dashboard_filename)) throw new Error("inventory_dashboard_filename must be a simple .json filename");
  inventoryUid(config);
  validateOutputFilenames(config);
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
  validateStableDashboardPath(config.dashboard_path);
  const commonReplacements = {
    [TOKENS.grafana_org_id]: String(config.grafana_org_id),
    [TOKENS.provider_name]: config.provider_name,
    [TOKENS.dashboard_folder]: config.dashboard_folder,
    [TOKENS.prometheus_datasource_uid]: config.prometheus_datasource_uid,
    [TOKENS.controller_job]: config.controller_job,
    [TOKENS.dashboard_path]: config.dashboard_path,
  };
  const renderDashboard = (templateName: string, uid: string, title: string): { dashboard: Record<string, unknown>; text: string } => {
    const replacements = {
      ...commonReplacements,
      [TOKENS.dashboard_uid]: uid,
      [TOKENS.dashboard_title]: title,
    };
    const text = renderText(readFileSync(resolve(templateDir, templateName), "utf8"), replacements);
    let dashboard: Record<string, unknown>;
    try {
      const parsed: unknown = JSON.parse(text);
      if (!isRecord(parsed)) throw new Error("dashboard must be an object");
      dashboard = parsed;
    } catch (error) {
      throw new Error(`rendered ${templateName} is not valid JSON: ${String(error)}`);
    }
    return { dashboard, text: `${JSON.stringify(dashboard, null, 2)}\n` };
  };

  const capacity = renderDashboard("dashboard.json.template", config.dashboard_uid, config.dashboard_title);
  const inventory = renderDashboard(
    "inventory-dashboard.json.template",
    inventoryUid(config),
    config.inventory_dashboard_title ?? `${config.dashboard_title} / Inventory & Operations`,
  );
  const dashboard = capacity.dashboard;
  const inventoryDashboard = inventory.dashboard;
  const dashboardText = capacity.text;
  const inventoryDashboardText = inventory.text;
  const provisioningText = renderText(readFileSync(resolve(templateDir, "provisioning.yml.template"), "utf8"), commonReplacements);
  const findings = [
    ...validateDashboard(dashboard, config.prometheus_datasource_uid),
    ...validateDashboard(inventoryDashboard, config.prometheus_datasource_uid, {
      requiredPanelTitles: INVENTORY_REQUIRED_PANEL_TITLES,
      requiredMetrics: INVENTORY_REQUIRED_METRICS,
      requiredTaskStates: INVENTORY_REQUIRED_TASK_STATES,
    }),
    ...validateProvisioning(provisioningText, config.dashboard_path),
  ];
  if (findings.length > 0) throw new Error(`rendered observability bundle failed validation:\n${findings.join("\n")}`);
  return {
    dashboard,
    dashboardText,
    inventoryDashboard,
    inventoryDashboardText,
    provisioningText,
    dashboardFilename: config.dashboard_filename ?? "agent-vm-observability.json",
    inventoryDashboardFilename: config.inventory_dashboard_filename ?? "agent-vm-observability-inventory.json",
  };
}

type RenameFile = typeof renameSync;
type OutputLock = { path: string; token: string };

function readLockOwner(lockPath: string): { pid: number; token: string } | undefined {
  try {
    const parsed: unknown = JSON.parse(readFileSync(join(lockPath, "owner.json"), "utf8"));
    if (!isRecord(parsed) || typeof parsed.pid !== "number" || !Number.isSafeInteger(parsed.pid) || parsed.pid <= 0 || typeof parsed.token !== "string" || parsed.token === "") {
      return undefined;
    }
    return { pid: parsed.pid, token: parsed.token };
  } catch {
    return undefined;
  }
}

function isProcessAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    const code = isRecord(error) && typeof error.code === "string" ? error.code : undefined;
    return code !== "ESRCH";
  }
}

function acquireOutputLock(directory: string): OutputLock {
  const lockPath = join(directory, ".agent-vm-observability.lock");
  for (let attempt = 0; attempt < 3; attempt += 1) {
    const token = randomUUID();
    try {
      mkdirSync(lockPath);
      try {
        writeFileSync(
          join(lockPath, "owner.json"),
          `${JSON.stringify({ pid: process.pid, token, started_at: new Date().toISOString() })}\n`,
          "utf8",
        );
      } catch (error) {
        rmSync(lockPath, { recursive: true, force: true });
        throw new Error(`cannot initialize output directory lock: ${directory}: ${String(error)}`);
      }
      return { path: lockPath, token };
    } catch (error) {
      const code = isRecord(error) && typeof error.code === "string" ? error.code : undefined;
      if (code !== "EEXIST") {
        if (String(error).includes("cannot initialize output directory lock")) throw error;
        throw new Error(`cannot acquire output directory lock: ${directory}: ${String(error)}`);
      }
    }

    const owner = readLockOwner(lockPath);
    if (!owner) throw new Error(`output directory is locked: ${directory}; owner metadata is unavailable`);
    if (isProcessAlive(owner.pid)) throw new Error(`output directory is locked: ${directory}; owner pid ${owner.pid} is alive`);
    try {
      rmSync(lockPath, { recursive: true, force: false });
    } catch (error) {
      throw new Error(`output directory is locked: ${directory}; stale lock cannot be reclaimed: ${String(error)}`);
    }
  }
  throw new Error(`output directory is locked: ${directory}; stale-lock recovery did not converge`);
}

function releaseOutputLock(lock: OutputLock): void {
  const owner = readLockOwner(lock.path);
  if (owner?.pid !== process.pid || owner.token !== lock.token) return;
  rmSync(lock.path, { recursive: true, force: false });
}

export function writeBundle(
  bundle: RenderedBundle,
  outputDir: string,
  force = false,
  renameFile: RenameFile = renameSync,
): { dashboardPath: string; inventoryDashboardPath: string; provisioningPath: string } {
  const directory = resolve(outputDir);
  mkdirSync(directory, { recursive: true });
  const activeDirectory = join(directory, "current");
  const releasesDirectory = join(directory, ".releases");
  const dashboardPath = resolve(activeDirectory, bundle.dashboardFilename);
  const inventoryDashboardPath = resolve(activeDirectory, bundle.inventoryDashboardFilename);
  const provisioningPath = resolve(directory, "agent-vm-observability.yml");
  if (new Set([dashboardPath, inventoryDashboardPath, provisioningPath]).size !== 3) {
    throw new Error("dashboard output paths must be unique");
  }
  let outputLock: OutputLock | undefined;
  let staging: string | undefined;
  let releasePath: string | undefined;
  let currentNextPath: string | undefined;
  let rollbackCurrentNextPath: string | undefined;
  let provisioningTempPath: string | undefined;
  let activeDirectoryReplaced = false;
  let previousCurrentTarget: string | undefined;
  let preserveActiveRelease = false;
  try {
    outputLock = acquireOutputLock(directory);
    const activeStat = existsSync(activeDirectory) ? lstatSync(activeDirectory) : undefined;
    if (activeStat && !activeStat.isSymbolicLink()) {
      throw new Error(`active bundle path must be a symlink: ${activeDirectory}; migrate the existing dashboard directory before rendering`);
    }
    if (activeStat?.isSymbolicLink()) previousCurrentTarget = readlinkSync(activeDirectory);
    const legacyPaths = [join(directory, bundle.dashboardFilename), join(directory, bundle.inventoryDashboardFilename)];
    if (legacyPaths.some((path) => existsSync(path))) {
      throw new Error(`legacy dashboard files exist beside the atomic bundle path; migrate ${directory} before rendering`);
    }
    if (!force && (activeStat || existsSync(provisioningPath))) {
      throw new Error(`output exists; pass --force to replace the active bundle or agent-vm-observability.yml`);
    }
    mkdirSync(releasesDirectory, { recursive: true });
    staging = mkdtempSync(join(releasesDirectory, ".bundle-"));
    writeFileSync(join(staging, bundle.dashboardFilename), bundle.dashboardText, "utf8");
    writeFileSync(join(staging, bundle.inventoryDashboardFilename), bundle.inventoryDashboardText, "utf8");
    releasePath = join(releasesDirectory, basename(staging).replace(/^\.bundle-/, "bundle-"));
    renameFile(staging, releasePath);
    staging = undefined;

    // The dashboard provider points at the stable `current` symlink. Replacing
    // that one directory entry makes both dashboards switch as one bundle.
    currentNextPath = join(directory, `.current-${basename(releasePath)}`);
    symlinkSync(join(".releases", basename(releasePath)), currentNextPath);

    // Switch the stable current link first, then publish provisioning. If the
    // second rename fails, the old link and old provisioning remain active.
    renameFile(currentNextPath, activeDirectory);
    currentNextPath = undefined;
    activeDirectoryReplaced = true;

    // Provisioning is a separate single-file artifact. Its provider path is
    // stable, so an atomic file rename cannot expose a mixed dashboard path.
    provisioningTempPath = join(directory, `.agent-vm-observability.yml.${basename(releasePath)}.tmp`);
    writeFileSync(provisioningTempPath, bundle.provisioningText, "utf8");
    renameFile(provisioningTempPath, provisioningPath);
    provisioningTempPath = undefined;
  } catch (error) {
    let rollbackFailure: Error | undefined;
    if (activeDirectoryReplaced) {
      try {
        if (previousCurrentTarget !== undefined) {
          rollbackCurrentNextPath = join(directory, `.current-rollback-${randomUUID()}`);
          symlinkSync(previousCurrentTarget, rollbackCurrentNextPath);
          renameFile(rollbackCurrentNextPath, activeDirectory);
          rollbackCurrentNextPath = undefined;
        } else {
          unlinkSync(activeDirectory);
        }
      } catch (rollbackError) {
        preserveActiveRelease = true;
        let failClosedError: Error | undefined;
        try {
          // Never leave the new current link beside the old provisioning file.
          // If rollback cannot restore the prior link, remove the stable link
          // so Grafana is unavailable rather than serving a mixed release.
          const quarantinePath = join(directory, `.current-quarantine-${randomUUID()}`);
          renameFile(activeDirectory, quarantinePath);
          rmSync(quarantinePath, { force: true });
        } catch (quarantineError) {
          try {
            // rmSync removes the link itself even when its target is dangling.
            rmSync(activeDirectory, { force: true });
          } catch (unlinkError) {
            failClosedError = new Error(
              `failed to disable the mixed dashboard bundle: ${String(unlinkError)}; quarantine error: ${String(quarantineError)}`,
            );
          }
        }
        rollbackFailure = new Error(
          `failed to restore active dashboard bundle after provisioning failure: ${String(rollbackError)}; original error: ${String(error)}${failClosedError ? `; ${failClosedError.message}` : "; active dashboard link was removed to avoid a mixed release"}`,
        );
      }
    }
    if (activeDirectoryReplaced) {
      activeDirectoryReplaced = false;
    }
    if (provisioningTempPath && existsSync(provisioningTempPath)) rmSync(provisioningTempPath, { force: true });
    if (currentNextPath && existsSync(currentNextPath)) rmSync(currentNextPath, { force: true });
    if (rollbackCurrentNextPath && existsSync(rollbackCurrentNextPath)) rmSync(rollbackCurrentNextPath, { force: true });
    if (!preserveActiveRelease && releasePath && existsSync(releasePath)) rmSync(releasePath, { recursive: true, force: true });
    if (rollbackFailure) throw rollbackFailure;
    throw error;
  } finally {
    if (staging && existsSync(staging)) rmSync(staging, { recursive: true, force: true });
    if (outputLock) releaseOutputLock(outputLock);
  }
  return { dashboardPath, inventoryDashboardPath, provisioningPath };
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
    console.log(`WROTE inventory dashboard ${paths.inventoryDashboardPath}`);
    console.log(`WROTE provisioning ${paths.provisioningPath}`);
    return 0;
  } catch (error) {
    console.error(`ERROR ${String(error)}`);
    return 1;
  }
}

if (import.meta.main) process.exitCode = main();
