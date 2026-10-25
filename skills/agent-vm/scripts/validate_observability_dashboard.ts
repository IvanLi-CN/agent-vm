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
  "Policy and task snapshot",
  "Disk admission",
  "VM slot admission",
  "Control filesystem capacity",
  "Admission state",
];

export const REQUIRED_METRICS = [
  "agent_vm_admission_cpu_capacity_vcpu",
  "agent_vm_admission_cpu_limit_vcpu",
  "agent_vm_admission_cpu_allocated_vcpu",
  "agent_vm_admission_cpu_reserved_vcpu",
  "agent_vm_admission_cpu_allocated_and_reserved_vcpu",
  "agent_vm_admission_cpu_remaining_vcpu",
  "agent_vm_admission_memory_capacity_bytes",
  "agent_vm_admission_memory_limit_bytes",
  "agent_vm_admission_memory_allocated_bytes",
  "agent_vm_admission_memory_reserved_bytes",
  "agent_vm_admission_memory_allocated_and_reserved_bytes",
  "agent_vm_admission_memory_remaining_bytes",
  "agent_vm_admission_default_request_cpu_vcpu",
  "agent_vm_admission_default_request_memory_bytes",
  "agent_vm_admission_default_request_disk_bytes",
  "agent_vm_admission_default_request_physical_cost_bytes",
  "agent_vm_admission_default_vm_logical_slots",
  "agent_vm_admission_default_vm_startable_slots",
  "agent_vm_admission_disk_allocated_bytes",
  "agent_vm_admission_disk_reserved_bytes",
  "agent_vm_admission_disk_allocated_and_reserved_bytes",
  "agent_vm_cpu_pressure_some_avg10_percent",
  "agent_vm_cpu_pressure_full_avg10_percent",
  "agent_vm_cpu_startup_pressure_threshold_percent",
  "agent_vm_cpu_startup_blocked",
  "agent_vm_admission_disk_remaining_bytes",
  "agent_vm_admission_disk_limit_bytes",
  "agent_vm_admission_disk_capacity_bytes",
  "agent_vm_admission_instances_allocated_count",
  "agent_vm_admission_instances_reserved_count",
  "agent_vm_admission_instances_allocated_and_reserved_count",
  "agent_vm_admission_instances_remaining_count",
  "agent_vm_admission_instances_limit_count",
  "agent_vm_admission_instances_capacity_count",
  "agent_vm_admission_ready",
  "agent_vm_admission_reclaim_required",
  "agent_vm_admission_draining",
  "agent_vm_up",
  "agent_vm_observation_consistent",
  "agent_vm_scrape_timestamp_seconds",
  "agent_vm_filesystem_size_bytes",
  "agent_vm_filesystem_used_bytes",
  "agent_vm_filesystem_available_bytes",
  "agent_vm_warnings_total",
  "agent_vm_memory_current_bytes",
  "agent_vm_memory_available_bytes",
  "agent_vm_memory_pressure",
  "agent_vm_memory_pressure_threshold_bytes",
  "agent_vm_memory_oom_events_total",
  "agent_vm_task_states",
  "incus_storage_pool_size_bytes",
  "incus_storage_pool_used_bytes",
  "incus_filesystem_size_bytes",
  "incus_filesystem_avail_bytes",
  "incus_network_receive_packets_total",
  "incus_network_transmit_packets_total",
];

export const INVENTORY_REQUIRED_PANEL_TITLES = [
  "Agent VM inventory summary",
  "Managed VM inventory",
  "Task state counts",
  "Controller scrape health",
  "Operation failures & image unpack",
];

export const INVENTORY_REQUIRED_METRICS = [
  "agent_vm_task_info",
  "agent_vm_task_cpu_vcpu",
  "agent_vm_task_memory_bytes",
  "agent_vm_task_disk_bytes",
  "agent_vm_task_lease_expires_at_seconds",
  "agent_vm_task_ssh_mapping",
  "agent_vm_task_last_error_info",
  "agent_vm_task_states",
  "agent_vm_up",
  "agent_vm_observation_consistent",
  "agent_vm_scrape_timestamp_seconds",
  "agent_vm_operations_total",
  "agent_vm_image_unpack_failures_total",
];

export const INVENTORY_REQUIRED_TASK_STATES = ["active", "preparing", "closing", "deleting", "failed", "cached", "absent"] as const;

export type DashboardValidationOptions = {
  requiredPanelTitles?: readonly string[];
  requiredMetrics?: readonly string[];
  requiredTaskStates?: readonly string[];
};

export type DashboardProfile = "capacity" | "inventory";

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

function stripPromqlStrings(expression: string): string {
  let result = "";
  let quote: string | undefined;
  let escaped = false;
  let comment = false;
  for (const character of expression) {
    if (comment) {
      if (character === "\n") comment = false;
      result += character === "\n" ? "\n" : " ";
      continue;
    }
    if (quote) {
      if (escaped) {
        escaped = false;
      } else if (character === "\\") {
        escaped = true;
      } else if (character === quote) {
        quote = undefined;
      }
      result += character === "\n" ? "\n" : " ";
      continue;
    }
    if (character === "#") {
      comment = true;
      result += " ";
    } else if (character === '"' || character === "'" || character === "`") {
      quote = character;
      result += " ";
    } else {
      result += character;
    }
  }
  return result;
}

function expressionReferencesMetric(expression: string, metric: string): boolean {
  const escapedMetric = metric.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(`(^|[^A-Za-z0-9_:])${escapedMetric}(?=$|[^A-Za-z0-9_:])`).test(stripPromqlStrings(expression));
}

function panelIds(dashboard: JsonRecord): Map<number, number> {
  const counts = new Map<number, number>();
  walk(dashboard.panels, (value) => {
    if (typeof value.id !== "number" || typeof value.type !== "string") return;
    counts.set(value.id, (counts.get(value.id) ?? 0) + 1);
  });
  return counts;
}

export function validateDashboard(dashboard: unknown, expectedDatasourceUid?: string, options: DashboardValidationOptions = {}): string[] {
  const findings: string[] = [];
  if (!isRecord(dashboard)) return ["dashboard must be a JSON object"];
  if (dashboard.refresh !== "auto") findings.push("dashboard refresh must be auto");
  if (!Array.isArray(dashboard.panels)) findings.push("dashboard panels must be an array");

  const titles = panelTitles(dashboard);
  for (const title of options.requiredPanelTitles ?? REQUIRED_PANEL_TITLES) {
    if (!titles.has(title)) findings.push(`missing panel or row: ${title}`);
  }

  for (const [id, count] of panelIds(dashboard)) {
    if (count > 1) findings.push(`dashboard panel id must be unique: ${id}`);
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

  if (options.requiredTaskStates) {
    const stateVariable = variables.find((variable) => variable.name === "state");
    const query = stateVariable?.query;
    if (typeof query !== "string") {
      findings.push("missing task state variable query");
    } else {
      for (const state of options.requiredTaskStates) {
        if (!query.split(",").map((value) => value.trim()).includes(state)) {
          findings.push(`task state variable must include: ${state}`);
        }
      }
    }
  }

  const targets = targetRecords(dashboard);
  const expressions = targets
    .map((target) => target.expr)
    .filter((expr): expr is string => typeof expr === "string");

  for (const metric of options.requiredMetrics ?? REQUIRED_METRICS) {
    if (!expressions.some((expression) => expressionReferencesMetric(expression, metric))) {
      findings.push(`missing required metric: ${metric}`);
    }
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
  if (expectedDatasourceUid) {
    if (datasourceUids.size !== 1 || !datasourceUids.has(expectedDatasourceUid)) {
      findings.push(`prometheus datasource UID must be ${expectedDatasourceUid}`);
    }
  } else if (datasourceUids.size !== 1) {
    findings.push("dashboard must use exactly one Prometheus datasource UID");
  }
  if ([...datasourceUids].some((uid) => uid.includes("__") || uid.includes("${"))) {
    findings.push("rendered dashboard contains a datasource placeholder");
  }

  return findings;
}

export function validateProvisioning(provisioning: string, expectedDashboardPath?: string): string[] {
  const findings: string[] = [];
  const lines = provisioning.replace(/\r\n?/g, "\n").split("\n").map((line) => line.trimEnd());
  if (!lines.some((line) => /^apiVersion:\s*1\s*$/.test(line))) findings.push("provisioning must declare apiVersion 1");
  if (!lines.some((line) => /^providers:\s*$/.test(line))) findings.push("provisioning must declare providers");
  const providerStarts = lines.filter((line) => /^  - name:\s+\"[^\"]+\"\s*$/.test(line));
  if (providerStarts.length !== 1) findings.push("provisioning providers must contain exactly one provider list item");
  if (!lines.some((line) => /^    orgId:\s*[1-9][0-9]*\s*$/.test(line))) findings.push("provisioning provider must declare a positive orgId");
  if (!lines.some((line) => /^    folder:\s+\"[^\"]*\"\s*$/.test(line))) findings.push("provisioning provider must declare a quoted folder");
  if (!lines.some((line) => /^    type:\s+file\s*$/.test(line))) findings.push("provisioning provider must use the file type");
  if (!lines.some((line) => /^    disableDeletion:\s+(?:true|false)\s*$/.test(line))) findings.push("provisioning provider must declare disableDeletion");
  if (!lines.some((line) => /^    editable:\s+(?:true|false)\s*$/.test(line))) findings.push("provisioning provider must declare editable");
  if (!lines.some((line) => /^    updateIntervalSeconds:\s*[1-9][0-9]*\s*$/.test(line))) findings.push("provisioning provider must declare updateIntervalSeconds");
  if (!lines.some((line) => /^    options:\s*$/.test(line))) findings.push("provisioning provider must declare options");
  const paths = lines.filter((line) => /^      path:\s+\"([^\"]+)\"\s*$/.test(line)).map((line) => line.replace(/^      path:\s+\"([^\"]+)\"\s*$/, "$1"));
  if (paths.length !== 1) findings.push("provisioning provider must declare exactly one quoted path");
  if (!lines.some((line) => /^      foldersFromFilesStructure:\s+(?:true|false)\s*$/.test(line))) {
    findings.push("provisioning provider must declare foldersFromFilesStructure");
  }
  if (expectedDashboardPath && paths.length === 1 && paths[0] !== expectedDashboardPath) {
    findings.push(`provisioning provider path must be ${expectedDashboardPath}`);
  }
  if (/__[A-Z0-9_]+__/.test(provisioning)) findings.push("provisioning contains an unresolved placeholder");
  return findings;
}

export function validateDashboardFile(
  dashboardPath: string,
  provisioningPath?: string,
  profile: DashboardProfile = "capacity",
): string[] {
  let dashboard: unknown;
  try {
    dashboard = JSON.parse(readFileSync(dashboardPath, "utf8"));
  } catch (error) {
    return [`cannot parse dashboard JSON: ${String(error)}`];
  }
  const options = profile === "inventory"
    ? {
        requiredPanelTitles: INVENTORY_REQUIRED_PANEL_TITLES,
        requiredMetrics: INVENTORY_REQUIRED_METRICS,
        requiredTaskStates: INVENTORY_REQUIRED_TASK_STATES,
      }
    : {};
  const findings = validateDashboard(dashboard, undefined, options);
  if (provisioningPath) {
    try {
      findings.push(...validateProvisioning(readFileSync(provisioningPath, "utf8")));
    } catch (error) {
      findings.push(`cannot read provisioning YAML: ${String(error)}`);
    }
  }
  return findings;
}

function parseArgs(args: string[]): { dashboard?: string; provisioning?: string; profile: DashboardProfile } {
  const parsed: { dashboard?: string; provisioning?: string; profile: DashboardProfile } = { profile: "capacity" };
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    const next = args[index + 1];
    if (arg === "--help") {
      if (args.length !== 1) throw new Error("usage: validate_observability_dashboard.ts --dashboard <path> [--provisioning <path>] [--profile capacity|inventory]");
      return parsed;
    }
    if (arg === "--dashboard" && next && !next.startsWith("--")) {
      parsed.dashboard = next;
      index += 1;
    } else if (arg === "--provisioning" && next && !next.startsWith("--")) {
      parsed.provisioning = next;
      index += 1;
    } else if (arg === "--profile" && next && !next.startsWith("--") && (next === "capacity" || next === "inventory")) {
      parsed.profile = next;
      index += 1;
    } else {
      throw new Error("usage: validate_observability_dashboard.ts --dashboard <path> [--provisioning <path>] [--profile capacity|inventory]");
    }
  }
  if (!parsed.dashboard) throw new Error("usage: validate_observability_dashboard.ts --dashboard <path> [--provisioning <path>] [--profile capacity|inventory]");
  return parsed;
}

export function main(args: string[] = Bun.argv.slice(2)): number {
  try {
    const parsed = parseArgs(args);
    if (!parsed.dashboard) {
      console.log("usage: validate_observability_dashboard.ts --dashboard <path> [--provisioning <path>] [--profile capacity|inventory]");
      return 0;
    }
    const findings = validateDashboardFile(parsed.dashboard, parsed.provisioning, parsed.profile);
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
