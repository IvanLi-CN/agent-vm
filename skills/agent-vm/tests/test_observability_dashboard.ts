import { expect, test } from "bun:test";
import { existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readlinkSync, readdirSync, renameSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { renderBundle, readConfig, writeBundle, type ObservabilityConfig } from "../scripts/render_observability_dashboard.ts";
import {
  INVENTORY_REQUIRED_METRICS,
  INVENTORY_REQUIRED_PANEL_TITLES,
  validateDashboard,
  validateProvisioning,
  INVENTORY_REQUIRED_TASK_STATES,
} from "../scripts/validate_observability_dashboard.ts";

const config: ObservabilityConfig = {
  dashboard_uid: "agent-vm-test",
  dashboard_title: "Agent VM / Incus",
  grafana_org_id: 2,
  provider_name: "agent-vm-test",
  dashboard_folder: "Observability",
  prometheus_datasource_uid: "prometheus-test",
  controller_job: "controller-test",
  dashboard_path: "/var/lib/grafana/dashboards/agent-vm/current",
};

test("renders a portable dashboard and provisioning bundle", () => {
  const bundle = renderBundle(config);
  expect(bundle.dashboard.uid).toBe(config.dashboard_uid);
  expect(bundle.dashboard.title).toBe(config.dashboard_title);
  expect(bundle.inventoryDashboard.uid).toBe(`${config.dashboard_uid}-inventory`);
  expect(bundle.inventoryDashboard.title).toBe(`${config.dashboard_title} / Inventory & Operations`);
  expect(bundle.dashboardText).not.toContain("__PROMETHEUS_UID__");
  expect(bundle.inventoryDashboardText).not.toContain("__PROMETHEUS_UID__");
  expect(bundle.inventoryDashboardText).toContain('"id": "joinByField"');
  expect(bundle.inventoryDashboardText).toContain('"name": "state"');
  expect(bundle.inventoryDashboardText).toContain('"vm_ip": "VM IP"');
  expect(bundle.inventoryDashboardText).toContain('"ssh_host_port": "SSH port"');
  expect(bundle.inventoryDashboardText).toContain('"options": "state_order"');
  expect(bundle.inventoryDashboardText).toContain('"state_order": true');
  expect(bundle.inventoryDashboardText).toContain('"task_id 1": true');
  expect(bundle.inventoryDashboardText).toContain('"options": "Scrape age (s)"');
  expect(bundle.inventoryDashboardText).not.toContain('agent_vm_operations_total{job=\\"$controller_job\\",outcome=\\"failure\\"}) + sum(agent_vm_image_unpack_failures_total');
  expect(bundle.inventoryDashboardText).toContain('"legendFormat": "Image unpack failures"');
  expect(bundle.inventoryDashboardText).toContain('"expr": "agent_vm_observation_consistent{job=\\"$controller_job\\"}"');
  expect(bundle.inventoryDashboardText).toContain('"text": [\n            "active",\n            "preparing",\n            "closing",\n            "deleting",\n            "failed"');
  expect(bundle.inventoryDashboardText).not.toContain("or vector(0)");
  expect(bundle.inventoryDashboardText).toContain("time() - agent_vm_scrape_timestamp_seconds");
  expect(bundle.dashboardText).toContain('"uid": "prometheus-test"');
  expect(bundle.dashboardText).toContain('"value": "controller-test"');
  expect(bundle.dashboardText).toContain('job=\\"$controller_job\\"');
  expect(bundle.dashboardText).toContain("clamp_max");
  expect(bundle.dashboardText).toContain("agent_vm_admission_default_request_cpu_vcpu");
  expect(bundle.dashboardText).toContain("agent_vm_admission_default_request_disk_bytes");
  expect(bundle.dashboardText).toContain("agent_vm_admission_default_request_physical_cost_bytes");
  expect(bundle.dashboardText).toContain("agent_vm_task_states");
  expect(bundle.dashboardText).toContain("agent_vm_up");
  expect(bundle.dashboardText).toContain("agent_vm_scrape_timestamp_seconds");
  const panels = bundle.dashboard.panels as Array<Record<string, unknown>>;
  const cpuPanel = panels.find((panel) => panel.id === 328);
  const cpuTarget = (cpuPanel?.targets as Array<Record<string, unknown>>).find((target) => target.refId === "C");
  const cpuLimitTarget = (cpuPanel?.targets as Array<Record<string, unknown>>).find((target) => target.refId === "M");
  expect(String(cpuTarget?.expr)).not.toContain("by (mode)");
  expect(String(cpuLimitTarget?.expr)).toContain("agent_vm_admission_cpu_limit_vcpu");
  const inventoryPanels = bundle.inventoryDashboard.panels as Array<Record<string, unknown>>;
  const inventoryTable = inventoryPanels.find((panel) => panel.id === 7) as Record<string, unknown>;
  const stateOrderTarget = (inventoryTable.targets as Array<Record<string, unknown>>).find((target) => target.refId === "A");
  expect(String(stateOrderTarget?.expr)).toContain('"state_order", "3", "state", "^failed$"');
  expect(String(stateOrderTarget?.expr)).toContain('"state_order", "4", "state", "^closing$"');
  expect(String(stateOrderTarget?.expr)).toContain('"state_order", "4", "state", "^deleting$"');
  expect(String(stateOrderTarget?.expr)).toContain('"state_order", "5", "state", "^cached$"');
  expect(String(stateOrderTarget?.expr)).toContain('"state_order", "6", "state", "^absent$"');
  for (const title of ["Disk admission", "Managed-container slot admission", "Control filesystem capacity"]) {
    const panel = panels.find((candidate) => candidate.title === title) as Record<string, any> | undefined;
    expect(panel?.fieldConfig?.defaults?.custom?.spanNulls).toBe(false);
  }
  expect(bundle.dashboardText).not.toContain("vector(1600)");
  expect(bundle.dashboardText).not.toContain("16 cores");
  expect(bundle.dashboardText).not.toContain("64 GiB");
  expect(bundle.dashboardText).not.toContain("32 GiB");
  expect(bundle.provisioningText).toContain('path: "/var/lib/grafana/dashboards/agent-vm/current"');
  expect(bundle.provisioningText).toContain("orgId: 2");
  expect(bundle.provisioningText).not.toContain("__DASHBOARD_PATH__");
  expect(validateDashboard(bundle.inventoryDashboard, "prometheus-test", {
    requiredPanelTitles: INVENTORY_REQUIRED_PANEL_TITLES,
    requiredMetrics: INVENTORY_REQUIRED_METRICS,
    requiredTaskStates: INVENTORY_REQUIRED_TASK_STATES,
  })).toEqual([]);
});

test("requires the complete inventory task-state filter", () => {
  const bundle = renderBundle(config);
  const dashboard = structuredClone(bundle.inventoryDashboard) as Record<string, unknown>;
  const templating = dashboard.templating as Record<string, unknown>;
  const variables = templating.list as Array<Record<string, unknown>>;
  const stateVariable = variables.find((variable) => variable.name === "state");
  stateVariable!.query = "active,preparing,closing,failed,cached,absent";
  expect(validateDashboard(dashboard, "prometheus-test", {
    requiredPanelTitles: INVENTORY_REQUIRED_PANEL_TITLES,
    requiredMetrics: INVENTORY_REQUIRED_METRICS,
    requiredTaskStates: INVENTORY_REQUIRED_TASK_STATES,
  })).toContain("task state variable must include: deleting");
});

test("requires dashboard metrics in PromQL expressions", () => {
  const bundle = renderBundle(config);
  const dashboard = structuredClone(bundle.inventoryDashboard) as Record<string, unknown>;
  const panels = dashboard.panels as Array<Record<string, unknown>>;
  const healthPanel = panels.find((panel) => panel.title === "Controller scrape health") as Record<string, unknown>;
  const targets = healthPanel.targets as Array<Record<string, unknown>>;
  const controllerTarget = targets.find((target) => target.refId === "A") as Record<string, unknown>;
  controllerTarget.expr = "vector(1)";
  controllerTarget.legendFormat = "agent_vm_up";
  expect(validateDashboard(dashboard, "prometheus-test", {
    requiredPanelTitles: INVENTORY_REQUIRED_PANEL_TITLES,
    requiredMetrics: INVENTORY_REQUIRED_METRICS,
  })).toContain("missing required metric: agent_vm_up");
});

test("does not count metric names that only occur inside PromQL strings", () => {
  const bundle = renderBundle(config);
  const dashboard = structuredClone(bundle.inventoryDashboard) as Record<string, unknown>;
  const panels = dashboard.panels as Array<Record<string, unknown>>;
  const healthPanel = panels.find((panel) => panel.title === "Controller scrape health") as Record<string, unknown>;
  const targets = healthPanel.targets as Array<Record<string, unknown>>;
  const controllerTarget = targets.find((target) => target.refId === "A") as Record<string, unknown>;
  controllerTarget.expr = 'label_replace(vector(1), "name", "agent_vm_up", "", "")';
  expect(validateDashboard(dashboard, "prometheus-test", {
    requiredPanelTitles: INVENTORY_REQUIRED_PANEL_TITLES,
    requiredMetrics: INVENTORY_REQUIRED_METRICS,
  })).toContain("missing required metric: agent_vm_up");
});

test("requires the rendered provisioning path", () => {
  const bundle = renderBundle(config);
  expect(validateProvisioning(bundle.provisioningText, config.dashboard_path)).toEqual([]);
  expect(validateProvisioning(bundle.provisioningText.replace(config.dashboard_path, "/wrong/path"), config.dashboard_path)).toContain(
    `provisioning provider path must be ${config.dashboard_path}`,
  );
});

test("requires provisioning path to target the stable current directory", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-config-"));
  try {
    const path = join(root, "config.json");
    const invalidConfig = { ...config, dashboard_path: "/var/lib/grafana/dashboards/agent-vm/generated" };
    writeFileSync(path, JSON.stringify(invalidConfig));
    expect(() => readConfig(path)).toThrow("dashboard_path must target the stable current directory");
    expect(() => renderBundle(invalidConfig)).toThrow("dashboard_path must target the stable current directory");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects a provisioning mapping where a provider list is required", () => {
  const invalid = `apiVersion: 1\n\nproviders:\n  type: file\n  path: "/var/lib/grafana/dashboards/agent-vm/current"\n`;
  expect(validateProvisioning(invalid)).toContain("provisioning providers must contain exactly one provider list item");
});

test("rejects packet-rate expressions that convert counts to bits", () => {
  const bundle = renderBundle(config);
  const dashboard = structuredClone(bundle.dashboard) as Record<string, unknown>;
  const panels = dashboard.panels as Array<Record<string, unknown>>;
  const packetPanel = panels.find((panel) => panel.title === "Project Network Packets/s");
  expect(packetPanel).toBeDefined();
  const targets = packetPanel?.targets as Array<Record<string, unknown>>;
  targets[0].expr = `${String(targets[0].expr)} * 8`;
  expect(validateDashboard(dashboard).some((finding) => finding.includes("packet-rate target") && finding.includes("must not multiply by 8"))).toBe(true);
});

test("rejects dashboards that mix Prometheus datasource UIDs", () => {
  const bundle = renderBundle(config);
  const dashboard = structuredClone(bundle.dashboard) as Record<string, unknown>;
  const panels = dashboard.panels as Array<Record<string, unknown>>;
  const panel = panels.find((candidate) => candidate.datasource && typeof candidate.datasource === "object") as Record<string, unknown>;
  (panel.datasource as Record<string, unknown>).uid = "another-prometheus";
  expect(validateDashboard(dashboard)).toContain("dashboard must use exactly one Prometheus datasource UID");
});

test("rejects dashboards with duplicate panel ids", () => {
  const bundle = renderBundle(config);
  const dashboard = structuredClone(bundle.dashboard) as Record<string, unknown>;
  const panels = dashboard.panels as Array<Record<string, unknown>>;
  panels.push({ ...panels[0] });
  expect(validateDashboard(dashboard).some((finding) => finding.startsWith("dashboard panel id must be unique:"))).toBe(true);
});

test("refuses unresolved configuration placeholders", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-config-"));
  try {
    const path = join(root, "config.json");
    writeFileSync(path, JSON.stringify({ ...config, controller_job: "${AGENT_VM_CONTROLLER_JOB}" }));
    expect(() => readConfig(path)).toThrow("controller_job still contains a placeholder");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("requires a positive numeric Grafana organization ID", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-org-"));
  try {
    const path = join(root, "config.json");
    writeFileSync(path, JSON.stringify({ ...config, grafana_org_id: 0 }));
    expect(() => readConfig(path)).toThrow("grafana_org_id must be a positive integer");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects an inventory UID that cannot fit the Grafana UID contract", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-uid-"));
  try {
    const path = join(root, "config.json");
    writeFileSync(path, JSON.stringify({
      ...config,
      dashboard_uid: "a".repeat(40),
    }));
    expect(() => readConfig(path)).toThrow("inventory_dashboard_uid");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects colliding dashboard output filenames", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-filenames-"));
  try {
    const path = join(root, "config.json");
    writeFileSync(path, JSON.stringify({
      ...config,
      dashboard_filename: "agent-vm-test.json",
      inventory_dashboard_filename: "agent-vm-test.json",
    }));
    expect(() => readConfig(path)).toThrow("output filenames must be unique");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("writes both outputs and refuses accidental replacement", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-output-"));
  try {
    const bundle = renderBundle(config);
    const paths = writeBundle(bundle, root);
    expect(readFileSync(paths.dashboardPath, "utf8")).toContain('"refresh": "auto"');
    expect(readFileSync(paths.inventoryDashboardPath, "utf8")).toContain('"title": "Agent VM / Incus / Inventory & Operations"');
    expect(lstatSync(join(root, "current")).isSymbolicLink()).toBe(true);
    expect(readlinkSync(join(root, "current"))).toMatch(/^\.releases\/bundle-/);
    expect(readFileSync(join(root, "agent-vm-observability.yml"), "utf8")).toContain(
      'path: "/var/lib/grafana/dashboards/agent-vm/current"',
    );
    expect(() => writeBundle(bundle, root)).toThrow("pass --force");
    expect(writeBundle(bundle, root, true).dashboardPath).toBe(paths.dashboardPath);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("restores the previous bundle when provisioning activation fails", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-rollback-"));
  try {
    const bundle = renderBundle(config);
    const paths = writeBundle(bundle, root);
    const previousTarget = readlinkSync(join(root, "current"));
    const previousProvisioning = readFileSync(paths.provisioningPath, "utf8");
    const injectedRename: typeof renameSync = (oldPath, newPath) => {
      if (String(newPath) === paths.provisioningPath) throw new Error("injected provisioning rename failure");
      renameSync(oldPath, newPath);
    };

    expect(() => writeBundle(bundle, root, true, injectedRename)).toThrow("injected provisioning rename failure");
    expect(readlinkSync(join(root, "current"))).toBe(previousTarget);
    expect(readFileSync(paths.provisioningPath, "utf8")).toBe(previousProvisioning);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed when rollback cannot restore the prior link", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-rollback-failure-"));
  try {
    const bundle = renderBundle(config);
    const paths = writeBundle(bundle, root);
    const previousTarget = readlinkSync(join(root, "current"));
    const previousProvisioning = readFileSync(paths.provisioningPath, "utf8");
    const releaseCount = readdirSync(join(root, ".releases")).length;
    const injectedRename: typeof renameSync = (oldPath, newPath) => {
      if (String(newPath) === paths.provisioningPath) throw new Error("injected provisioning rename failure");
      if (String(oldPath).includes(".current-rollback-")) throw new Error("injected rollback rename failure");
      if (String(oldPath).includes(".current-quarantine-")) {
        rmSync(join(root, "current"), { force: true });
        symlinkSync(".releases/missing-release", join(root, "current"));
        throw new Error("injected quarantine rename failure");
      }
      renameSync(oldPath, newPath);
    };

    expect(() => writeBundle(bundle, root, true, injectedRename)).toThrow("failed to restore active dashboard bundle");
    expect(existsSync(join(root, "current"))).toBe(false);
    expect(readdirSync(join(root, ".releases"))).toHaveLength(releaseCount + 1);
    expect(previousTarget).toMatch(/^\.releases\/bundle-/);
    expect(readFileSync(paths.provisioningPath, "utf8")).toBe(previousProvisioning);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("reclaims a stale output-directory lock", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-stale-lock-"));
  try {
    const lockPath = join(root, ".agent-vm-observability.lock");
    mkdirSync(lockPath);
    writeFileSync(join(lockPath, "owner.json"), JSON.stringify({ pid: 2147483647, token: "stale", started_at: "2000-01-01T00:00:00Z" }));
    const paths = writeBundle(renderBundle(config), root);
    expect(readFileSync(paths.dashboardPath, "utf8")).toContain('"refresh": "auto"');
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("serializes bundle writes with an output-directory lock", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-lock-"));
  try {
    const bundle = renderBundle(config);
    mkdirSync(join(root, ".agent-vm-observability.lock"));
    expect(() => writeBundle(bundle, root, true)).toThrow("output directory is locked");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
