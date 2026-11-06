import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { parse } from "yaml";
import { parseArgs } from "../scripts/agent_vm.ts";

const root = resolve(import.meta.dir, "../../..");
const read = (path: string) => readFileSync(resolve(root, path), "utf8");
const profilesPath = "skills/agent-vm/references/resource-profiles.yaml";
const guidePath = "skills/agent-vm/references/resource-guide.md";
const evidencePath = "skills/agent-vm/references/resource-evidence.md";
const profileDocument = parse(read(profilesPath)) as Record<string, any>;

function gib(value: unknown): number {
  expect(typeof value).toBe("string");
  const match = /^(\d+)(GiB|MiB)$/.exec(String(value));
  expect(match).not.toBeNull();
  return Number(match![1]) * (match![2] === "MiB" ? 1 / 1024 : 1);
}

test("resource profiles define the advisory schema and all supported task types", () => {
  expect(profileDocument.version).toBe(1);
  expect(profileDocument.schema).toBe("agent-vm-resource-profile-v1");
  expect(profileDocument.status).toBe("advisory");
  expect(profileDocument.policy.default_profile).toBe("default");
  expect(profileDocument.policy.swap).toBe("host-dependent");
  expect(profileDocument.policy.disk_headroom).toBe("30%");
  const profiles = profileDocument.profiles as Record<string, any>;
  expect(Object.keys(profiles).sort()).toEqual(["default", "heavy-rust", "integration", "rust-build", "standard"]);
  for (const profile of Object.values(profiles)) {
    expect(String(profile.purpose).length).toBeGreaterThan(0);
    expect(profile.swap).toBe("host-dependent");
    expect(profile.concurrency.max_active_workloads).toBe(1);
    expect(profile.concurrency.recommended_build_jobs).toBeGreaterThan(0);
    expect(profile.cpu.minimum).toBeGreaterThan(0);
    expect(profile.cpu.recommended).toBeGreaterThanOrEqual(profile.cpu.minimum);
    for (const dimension of ["memory", "disk"]) {
      const values = profile[dimension];
      expect(gib(values.recommended)).toBeGreaterThanOrEqual(gib(values.minimum));
      if (values.peak !== undefined) expect(gib(values.peak)).toBeGreaterThanOrEqual(gib(values.recommended));
    }
  }
});

test("default profile retains the public default resource request", () => {
  const profile = profileDocument.profiles.default;
  expect(profile.cpu.recommended).toBe(2);
  expect(profile.memory.recommended).toBe("4GiB");
  expect(profile.disk.recommended).toBe("20GiB");
});

test("resource guide and evidence record preserve the operational boundaries", () => {
  const guide = read(guidePath);
  const evidence = read(evidencePath);
  const skill = read("skills/agent-vm/SKILL.md");
  expect(skill).toContain("references/resource-guide.md");
  expect(skill).toContain("references/resource-profiles.yaml");
  for (const phrase of ["default → standard → rust-build → heavy-rust", "CARGO_BUILD_JOBS", "memory.current", "memory.peak", "memory.events", "df -h", "host-dependent", "不会创建 swapfile", "不改变控制服务的 admission"]) expect(guide).toContain(phrase);
  for (const phrase of ["observed_at", "build_jobs", "memory.current", "memory.peak", "memory.events", "oom_kill", "df -h", "swapon --show", "连续三次"]) expect(evidence).toContain(phrase);
  expect(guide).not.toContain("Agent VM 已启用容器级 SWAP");
  expect(guide).not.toContain("容器级 SWAP 是默认配置");
});

test("documented acquire and resize examples use the current CLI contract", () => {
  expect(parseArgs(["acquire", "--cpu", "8", "--memory", "12GiB", "--disk", "40GiB"]).command).toBe("acquire");
  expect(parseArgs(["resize", "--memory", "16GiB", "--allow-restart"]).command).toBe("resize");
  expect(parseArgs(["resize", "--memory", "8GiB", "--live"]).command).toBe("resize");
  expect(parseArgs(["resize", "--cpu", "12", "--memory", "24GiB", "--disk", "80GiB", "--allow-restart"]).command).toBe("resize");
});
