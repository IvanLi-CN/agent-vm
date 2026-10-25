import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";

const skill = readFileSync(new URL("../SKILL.md", import.meta.url), "utf8");
const entrypoint = new URL("../scripts/agent_vm.ts", import.meta.url);

test("Skill documentation points to its source-linked Bun entrypoint", () => {
  expect(existsSync(entrypoint)).toBe(true);
  expect(skill).toContain('bun "<skill-dir>/scripts/agent_vm.ts"');
  expect(skill).toContain("`<skill-dir>` is the directory containing this `SKILL.md`");
  expect(skill).not.toContain("scripts/agent_vm.py");
  expect(skill).not.toContain("Use the installed `agent-vm` CLI");
});
