import { expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { renderBundle, readConfig, writeBundle, type ObservabilityConfig } from "../scripts/render_observability_dashboard.ts";
import { validateDashboard } from "../scripts/validate_observability_dashboard.ts";

const config: ObservabilityConfig = {
  dashboard_uid: "agent-vm-test",
  dashboard_title: "Agent VM / Incus",
  grafana_org_id: 2,
  provider_name: "agent-vm-test",
  dashboard_folder: "Observability",
  prometheus_datasource_uid: "prometheus-test",
  controller_job: "controller-test",
  dashboard_path: "/var/lib/grafana/dashboards/agent-vm",
};

test("renders a portable dashboard and provisioning bundle", () => {
  const bundle = renderBundle(config);
  expect(bundle.dashboard.uid).toBe(config.dashboard_uid);
  expect(bundle.dashboard.title).toBe(config.dashboard_title);
  expect(bundle.dashboardText).not.toContain("__PROMETHEUS_UID__");
  expect(bundle.dashboardText).toContain('"uid": "prometheus-test"');
  expect(bundle.dashboardText).toContain('"value": "controller-test"');
  expect(bundle.dashboardText).toContain('job=\\"$controller_job\\"');
  expect(bundle.dashboardText).toContain("clamp_max");
  const panels = bundle.dashboard.panels as Array<Record<string, unknown>>;
  const cpuPanel = panels.find((panel) => panel.id === 328);
  const cpuTarget = (cpuPanel?.targets as Array<Record<string, unknown>>).find((target) => target.refId === "C");
  expect(String(cpuTarget?.expr)).not.toContain("by (mode)");
  expect(bundle.dashboardText).not.toContain("vector(1600)");
  expect(bundle.dashboardText).not.toContain("16 cores");
  expect(bundle.dashboardText).not.toContain("64 GiB");
  expect(bundle.dashboardText).not.toContain("32 GiB");
  expect(bundle.provisioningText).toContain('path: "/var/lib/grafana/dashboards/agent-vm"');
  expect(bundle.provisioningText).toContain("orgId: 2");
  expect(bundle.provisioningText).not.toContain("__DASHBOARD_PATH__");
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

test("writes both outputs and refuses accidental replacement", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-vm-observability-output-"));
  try {
    const bundle = renderBundle(config);
    const paths = writeBundle(bundle, root);
    expect(readFileSync(paths.dashboardPath, "utf8")).toContain('"refresh": "auto"');
    expect(() => writeBundle(bundle, root)).toThrow("pass --force");
    expect(writeBundle(bundle, root, true).dashboardPath).toBe(paths.dashboardPath);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
