#!/usr/bin/env bun

import { readFileSync } from "node:fs";

export const REQUIRED_PANEL_TITLES = [
  "Agent VM capacity summary",
  "Project overview",
  "Project CPU Usage",
  "Agent VM Memory Capacity & Runtime",
  "Guest rootfs allocation",
  "Incus physical pool usage",
  "Logical RAM remaining",
  "CT MemAvailable",
  "Runtime pressure",
  "Admission headroom",
];

export const REQUIRED_METRICS = [
  "agent_vm_admission_cpu_capacity_vcpu",
  "agent_vm_admission_cpu_remaining_vcpu",
  "agent_vm_admission_memory_capacity_bytes",
  "agent_vm_admission_memory_limit_bytes",
  "agent_vm_admission_memory_allocated_bytes",
  "agent_vm_admission_memory_reserved_bytes",
  "agent_vm_admission_memory_remaining_bytes",
  "agent_vm_admission_disk_remaining_bytes",
  "agent_vm_admission_ready",
  "agent_vm_memory_current_bytes",
  "agent_vm_memory_available_bytes",
  "agent_vm_memory_pressure",
  "agent_vm_memory_pressure_threshold_bytes",
  "agent_vm_memory_oom_events_total",
  "incus_storage_pool_size_bytes",
  "incus_storage_pool_used_bytes",
  "incus_filesystem_size_bytes",
  "incus_network_receive_packets_total",
  "incus_network_transmit_packets_total",
];

type JsonRecord = Record<string, unknown>;

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function walk(value: unknown, visit: (value: JsonRecord) => void): void {
  if (Array.isArray(value)) {
    for (const item of value) walk(item, visit);
    return;
  }
  if (!isRecord(value)) return;
  visit(value);
  for (const child of Object.values(value)) walk(child, visit);
}

function panelTitles(dashboard: JsonRecord): Set<string> {
  const titles = new Set<string>();
  walk(dashboard.panels, (value) => {
    if (typeof value.type === "string" && typeof value.title === "string") {
      titles.add(value.title);
    }
  });
  return titles;
}

function targetRecords(dashboard: JsonRecord): JsonRecord[] {
  const targets: JsonRecord[] = [];
  walk(dashboard.panels, (value) => {
    if (!Array.isArray(value.targets)) return;
    for (const target of value.targets) {
      if (isRecord(target)) targets.push(target);
    }
  });
  return targets;
}

function stringsInTargets(targets: JsonRecord[]): string[] {
  return targets.flatMap((target) => Object.values(target).filter((value): value is string => typeof value === "string"));
}

export function validateDashboard(dashboard: unknown, expectedDatasourceUid?: string): string[] {
  const findings: string[] = [];
  if (!isRecord(dashboard)) return ["dashboard must be a JSON object"];
  if (dashboard.refresh !== "auto") findings.push("dashboard refresh must be auto");
  if (!Array.isArray(dashboard.panels)) findings.push("dashboard panels must be an array");

  const titles = panelTitles(dashboard);
  for (const title of REQUIRED_PANEL_TITLES) {
    if (!titles.has(title)) findings.push(`missing panel or row: ${title}`);
  }

  const variables = isRecord(dashboard.templating) && Array.isArray(dashboard.templating.list)
    ? dashboard.templating.list.filter(isRecord)
    : [];
  const controllerVariable = variables.find((variable) => variable.name === "controller_job");
  if (!controllerVariable) {
    findings.push("missing hidden controller_job variable");
  } else if (controllerVariable.type !== "custom" || controllerVariable.hide !== 2) {
    findings.push("controller_job variable must be a hidden custom variable");
  }

  const targets = targetRecords(dashboard);
  const targetStrings = stringsInTargets(targets);
  const expressions = targets
    .map((target) => target.expr)
    .filter((expr): expr is string => typeof expr === "string");
  const allTargetText = targetStrings.join("\n");

  for (const metric of REQUIRED_METRICS) {
    if (!allTargetText.includes(metric)) findings.push(`missing required metric: ${metric}`);
  }

  for (const [index, expression] of expressions.entries()) {
    if (/incus_network_(?:receive|transmit)_packets_total/.test(expression) && /\*\s*8\b/.test(expression)) {
      findings.push(`packet-rate target ${index + 1} must not multiply by 8`);
    }
    if (/\bagent_vm_/.test(expression) && !/job=\"\$controller_job\"/.test(expression)) {
      findings.push(`controller target ${index + 1} must use the controller_job variable`);
    }
  }

  const datasourceUids = new Set<string>();
  walk(dashboard, (value) => {
    if (isRecord(value.datasource) && value.datasource.type === "prometheus" && typeof value.datasource.uid === "string") {
      datasourceUids.add(value.datasource.uid);
    }
  });
  if (expectedDatasourceUid && (datasourceUids.size !== 1 || !datasourceUids.has(expectedDatasourceUid))) {
    findings.push(`prometheus datasource UID must be ${expectedDatasourceUid}`);
  }
  if ([...datasourceUids].some((uid) => uid.includes("__") || uid.includes("${"))) {
    findings.push("rendered dashboard contains a datasource placeholder");
  }

  return findings;
}

export function validateProvisioning(provisioning: string): string[] {
  const findings: string[] = [];
  if (!/^apiVersion:\s*1\s*$/m.test(provisioning)) findings.push("provisioning must declare apiVersion 1");
  if (!/^providers:\s*$/m.test(provisioning)) findings.push("provisioning must declare providers");
  if (!/^\s+type:\s+file\s*$/m.test(provisioning)) findings.push("provisioning provider must use the file type");
  if (!/^\s+path:\s+\"[^\"]+\"\s*$/m.test(provisioning)) findings.push("provisioning provider must declare a quoted path");
  if (/__[A-Z0-9_]+__/.test(provisioning)) findings.push("provisioning contains an unresolved placeholder");
  return findings;
}

export function validateDashboardFile(dashboardPath: string, provisioningPath?: string): string[] {
  let dashboard: unknown;
  try {
    dashboard = JSON.parse(readFileSync(dashboardPath, "utf8"));
  } catch (error) {
    return [`cannot parse dashboard JSON: ${String(error)}`];
  }
  const findings = validateDashboard(dashboard);
  if (provisioningPath) {
    try {
      findings.push(...validateProvisioning(readFileSync(provisioningPath, "utf8")));
    } catch (error) {
      findings.push(`cannot read provisioning YAML: ${String(error)}`);
    }
  }
  return findings;
}

function parseArgs(args: string[]): { dashboard?: string; provisioning?: string } {
  const parsed: { dashboard?: string; provisioning?: string } = {};
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    const next = args[index + 1];
    if (arg === "--help") {
      if (args.length !== 1) throw new Error("usage: validate_observability_dashboard.ts --dashboard <path> [--provisioning <path>]");
      return parsed;
    }
    if (arg === "--dashboard" && next && !next.startsWith("--")) {
      parsed.dashboard = next;
      index += 1;
    } else if (arg === "--provisioning" && next && !next.startsWith("--")) {
      parsed.provisioning = next;
      index += 1;
    } else {
      throw new Error("usage: validate_observability_dashboard.ts --dashboard <path> [--provisioning <path>]");
    }
  }
  if (!parsed.dashboard) throw new Error("usage: validate_observability_dashboard.ts --dashboard <path> [--provisioning <path>]");
  return parsed;
}

export function main(args: string[] = Bun.argv.slice(2)): number {
  try {
    const parsed = parseArgs(args);
    if (!parsed.dashboard) {
      console.log("usage: validate_observability_dashboard.ts --dashboard <path> [--provisioning <path>]");
      return 0;
    }
    const findings = validateDashboardFile(parsed.dashboard, parsed.provisioning);
    for (const finding of findings) console.log(`FINDING ${finding}`);
    if (findings.length > 0) return 1;
    console.log("PASS Agent VM observability dashboard contract");
    return 0;
  } catch (error) {
    console.error(`ERROR ${String(error)}`);
    return 2;
  }
}

if (import.meta.main) process.exitCode = main();
