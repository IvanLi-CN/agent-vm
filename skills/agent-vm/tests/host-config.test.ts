import { expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { bindProject, loadHostConfig, normalizeRemote, resolveHost, writeHostConfig } from "../scripts/host-config.ts";

function env(root: string) { return { XDG_CONFIG_HOME: root, AGENT_VM_PROJECT_REMOTE: "git@github.com:IvanLi-CN/agent-vm.git" }; }

test("host profiles resolve explicit, project and global defaults", () => {
  const root = `/tmp/agent-vm-host-${crypto.randomUUID()}`; mkdirSync(root, { recursive: true });
  const e = env(root); writeHostConfig({ default_host: "109", hosts: { "105": { transport: "https", control_host: "https://105.example" }, "109": { transport: "ssh", control_host: "host-109" } }, projects: { "github.com/IvanLi-CN/agent-vm": "105" } }, e);
  expect(resolveHost(undefined, { ...e }).alias).toBe("105");
  expect(resolveHost("109", e).control_host).toBe("host-109");
  expect(resolveHost(undefined, { ...e, AGENT_VM_PROJECT_REMOTE: "github.com/other/repo" }).alias).toBe("109");
});

test("legacy control host remains an explicit compatibility path", () => {
  const selection = resolveHost(undefined, { XDG_CONFIG_HOME: `/tmp/no-${crypto.randomUUID()}`, AGENT_VM_CONTROL_HOST: "legacy-host" });
  expect(selection.compatibility).toBe(true); expect(selection.control_host).toBe("legacy-host");
});

test("project remotes normalize and config shape is read without secrets", () => {
  expect(normalizeRemote("git@github.com:IvanLi-CN/agent-vm.git")).toBe("github.com/IvanLi-CN/agent-vm");
  const root = `/tmp/agent-vm-host-${crypto.randomUUID()}`; mkdirSync(join(root, "agent-vm"), { recursive: true });
  writeFileSync(join(root, "agent-vm", "config.toml"), `[hosts.105]\ntransport = "https"\ncontrol_host = "https://105.example"\ntoken_env = "SECRET"\n[projects."github.com/IvanLi-CN/agent-vm"]\nhost = "105"\n`);
  const config = loadHostConfig({ XDG_CONFIG_HOME: root }); expect(config.hosts["105"].token_env).toBe("SECRET"); expect(config.projects["github.com/IvanLi-CN/agent-vm"]).toBe("105");
  expect(() => bindProject("missing", "github.com/x/y", { XDG_CONFIG_HOME: root })).toThrow("unknown");
});
