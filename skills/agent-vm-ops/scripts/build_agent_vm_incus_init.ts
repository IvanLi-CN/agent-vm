#!/usr/bin/env bun

/** Rebuild the standalone Incus initializer entrypoint from its local source. */

import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";

const source = resolve(import.meta.dir, "agent_vm_incus_init.source.ts");
const outputArg = process.argv.indexOf("--output");
const output = resolve(outputArg >= 0 && process.argv[outputArg + 1] ? process.argv[outputArg + 1] : resolve(import.meta.dir, "agent_vm_incus_init.ts"));
const typeFooter = `
export type CommandResult = { exitCode: number; stdout: string; stderr: string };
export type CommandRunner = (command: string[]) => CommandResult;
export type IncusSnapshot = { project: Record<string, unknown>; profile: Record<string, unknown> };
export type InitializationAction = {
  kind: "unset-project-config" | "set-profile-config";
  key: string;
  current: unknown;
  desired: string | null;
};
export type InitializationPlan = { aligned: boolean; actions: InitializationAction[] };
export type InitializationOptions = {
  remote: string;
  project: string;
  profile: string;
  processLimit: number;
  apply: boolean;
};
`;

if (!existsSync(source)) throw new Error(`missing initializer source: ${source}`);

const buildDirectory = mkdtempSync(join(tmpdir(), "agent-vm-incus-init-"));
try {
  const result = await Bun.build({
    entrypoints: [source],
    outdir: buildDirectory,
    naming: "agent_vm_incus_init.js",
    target: "bun",
    format: "esm",
    minify: false,
    sourcemap: "none",
    splitting: false,
  });

  if (!result.success) throw new Error(result.logs.map((log) => log.message).join("\n"));
  if (result.outputs.length !== 1) throw new Error(`expected one generated initializer, got ${result.outputs.length}`);

  const generated = readFileSync(result.outputs[0].path, "utf8");
  if (generated.includes("../../agent-vm/") || generated.includes("../../../deploy/")) {
    throw new Error("generated initializer retained a repository-relative dependency");
  }
  const normalized = generated
    .replace(/^#!\/usr\/bin\/env bun\n/, "#!/usr/bin/env bun\n// @ts-nocheck\n")
    .replace(/^\/\/ .*agent-vm-ops\/scripts\//gm, "// skill/scripts/");
  writeFileSync(output, `${normalized}${typeFooter}`);
  chmodSync(output, 0o755);
} finally {
  rmSync(buildDirectory, { recursive: true, force: true });
}
