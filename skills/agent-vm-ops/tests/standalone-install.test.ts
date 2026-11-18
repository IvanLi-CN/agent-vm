import { expect, test } from "bun:test";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";

const repository = resolve(import.meta.dir, "../../..");

function copySkill(name: "agent-vm" | "agent-vm-ops"): { root: string; skill: string; home: string; cwd: string } {
  const root = mkdtempSync(join(tmpdir(), "agent-vm-standalone-"));
  const skill = join(root, name);
  const home = join(root, "home");
  const cwd = join(root, "outside-worktree");
  mkdirSync(home, { recursive: true });
  mkdirSync(cwd, { recursive: true });
  cpSync(join(repository, "skills", name), skill, { recursive: true });
  return { root, skill, home, cwd };
}

function run(skillRoot: string, home: string, cwd: string, args: string[], extraEnv: Record<string, string> = {}) {
  return Bun.spawnSync([process.execPath, ...args], {
    cwd,
    env: {
      ...process.env,
      HOME: home,
      XDG_CONFIG_HOME: join(home, ".config"),
      ...extraEnv,
    },
    stdout: "pipe",
    stderr: "pipe",
  });
}

function text(value: Uint8Array): string {
  return new TextDecoder().decode(value);
}

test("agent-vm runs from a standalone directory without observability or source links", () => {
  const isolated = copySkill("agent-vm");
  try {
    const cli = run(isolated.skill, isolated.home, isolated.cwd, [join(isolated.skill, "scripts/agent_vm.ts"), "help"]);
    expect(cli.exitCode).toBe(0);
    expect(text(cli.stdout)).toContain("Usage: agent-vm");

    const sync = run(isolated.skill, isolated.home, isolated.cwd, [join(isolated.skill, "scripts/sync_worktree.ts"), "--help"]);
    expect(sync.exitCode).toBe(2);
    expect(text(sync.stderr) + text(sync.stdout)).toContain("usage: sync_worktree.ts");

    const skill = readFileSync(join(isolated.skill, "SKILL.md"), "utf8");
    expect(skill).not.toContain("render_observability_dashboard");
    expect(skill).not.toContain("validate_observability_dashboard");
    expect(skill).not.toContain("references/observability");
    expect(existsSync(join(isolated.skill, "scripts/render_observability_dashboard.ts"))).toBe(false);
    expect(text(cli.stderr)).not.toContain("Cannot find module");
  } finally {
    rmSync(isolated.root, { recursive: true, force: true });
  }
});

test("agent-vm-ops renders and validates observability from its own assets", () => {
  const isolated = copySkill("agent-vm-ops");
  try {
    const configPath = join(isolated.root, "observability.json");
    const config = JSON.parse(readFileSync(join(isolated.skill, "assets/observability/observability.config.json.example"), "utf8"));
    Object.assign(config, {
      dashboard_uid: "standalone-agent-vm",
      inventory_dashboard_uid: "standalone-agent-vm-inventory",
      dashboard_folder: "Standalone",
      prometheus_datasource_uid: "prometheus-standalone",
      controller_job: "agent-vm-deployd",
      dashboard_path: "/var/lib/grafana/dashboards/agent-vm/current",
    });
    writeFileSync(configPath, `${JSON.stringify(config, null, 2)}\n`);
    const output = join(isolated.root, "generated");
    const renderer = run(isolated.skill, isolated.home, isolated.cwd, [
      join(isolated.skill, "scripts/render_observability_dashboard.ts"),
      "--config", configPath,
      "--output-dir", output,
      "--force",
    ]);
    expect(renderer.exitCode).toBe(0);
    const provisioning = join(output, "agent-vm-observability.yml");
    for (const [filename, profile] of [
      ["agent-vm-observability.json", "capacity"],
      ["agent-vm-observability-inventory.json", "inventory"],
      ["agent-vm-deployd.json", "deployd"],
    ] as const) {
      const validator = run(isolated.skill, isolated.home, isolated.cwd, [
        join(isolated.skill, "scripts/validate_observability_dashboard.ts"),
        "--dashboard", join(output, "current", filename),
        "--provisioning", provisioning,
        "--profile", profile,
      ]);
      expect(validator.exitCode).toBe(0);
      expect(text(validator.stdout)).toContain("PASS Agent VM observability dashboard contract");
    }
  } finally {
    rmSync(isolated.root, { recursive: true, force: true });
  }
});

test("agent-vm-ops initializer uses a standalone generated entrypoint and keeps preview read-only", () => {
  const isolated = copySkill("agent-vm-ops");
  try {
    const rebuilt = join(isolated.root, "agent_vm_incus_init.rebuilt.ts");
    const build = run(isolated.skill, isolated.home, isolated.cwd, [
      join(isolated.skill, "scripts/build_agent_vm_incus_init.ts"),
      "--output", rebuilt,
    ]);
    expect(build.exitCode).toBe(0);
    expect(readFileSync(rebuilt, "utf8")).toBe(readFileSync(join(isolated.skill, "scripts/agent_vm_incus_init.ts"), "utf8"));
    expect(readFileSync(rebuilt, "utf8")).not.toContain("../../agent-vm/");
    expect(readFileSync(rebuilt, "utf8")).not.toContain("../../../deploy/");

    const fakeBin = join(isolated.root, "bin");
    mkdirSync(fakeBin);
    const marker = join(isolated.root, "mutated");
    writeFileSync(join(fakeBin, "ssh"), `#!/bin/sh\ncase "$*" in\n  *project*unset*|*profile*set*) touch "${marker}" ;;\n  */projects/*) printf '%s' '{"config":{"restricted":"true"}}' ;;\n  */profiles/*) printf '%s' '{"config":{"limits.processes":"512"}}' ;;\n  *) exit 1 ;;\nesac\n`);
    Bun.spawnSync(["chmod", "755", join(fakeBin, "ssh")]);
    const configHome = join(isolated.home, ".config", "agent-vm");
    mkdirSync(configHome, { recursive: true });
    writeFileSync(join(configHome, "config.toml"), "[hosts.demo]\ntransport = \"ssh\"\ncontrol_host = \"control-alias\"\n");
    const preview = run(isolated.skill, isolated.home, isolated.cwd, [
      join(isolated.skill, "scripts/agent_vm_incus_init.ts"),
      "--host", "demo",
    ], { PATH: `${fakeBin}:${process.env.PATH ?? ""}` });
    expect(preview.exitCode).toBe(0);
    expect(JSON.parse(text(preview.stdout)).status).toBe("aligned");
    expect(existsSync(marker)).toBe(false);
  } finally {
    rmSync(isolated.root, { recursive: true, force: true });
  }
});
