#!/usr/bin/env bun

import { chmodSync, mkdirSync, renameSync, rmSync } from "node:fs";
import { dirname, isAbsolute, resolve } from "node:path";

const moduleRoot = resolve(import.meta.dir, "../control");
const usage = "usage: build_go_control.ts --source-revision <40-hex> --target <linux/amd64|linux/arm64> --output <absolute-path> [--version <semver>] [--with-web-assets]";
const args = process.argv.slice(2);
const value = (name: string): string => {
  const index = args.indexOf(name);
  if (index < 0 || !args[index + 1]) throw new Error(usage);
  return args[index + 1];
};
const optional = (name: string, fallback: string): string => {
  const index = args.indexOf(name);
  return index < 0 ? fallback : args[index + 1] || (() => { throw new Error(usage); })();
};
const revision = value("--source-revision");
if (!/^[0-9a-f]{40}$/.test(revision)) throw new Error("source revision must be a 40-character lowercase Git revision");
const target = value("--target");
const match = /^(linux)\/(amd64|arm64)$/.exec(target);
if (!match) throw new Error("target must be linux/amd64 or linux/arm64");
const version = optional("--version", "dev");
if (version !== "dev" && !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.test(version)) throw new Error("version must be valid SemVer 2.0.0 or dev");
const rawOutput = value("--output");
if (!isAbsolute(rawOutput)) throw new Error("output must be absolute");
const output = resolve(rawOutput);
const withWebAssets = args.includes("--with-web-assets");
mkdirSync(dirname(output), { recursive: true, mode: 0o750 });
const temporary = `${output}.tmp-${process.pid}`;
rmSync(temporary, { force: true });
const result = Bun.spawnSync(
  [
    "go",
    "build",
    "-trimpath",
    "-buildvcs=false",
	    ...(withWebAssets ? ["-tags=releaseassets"] : []),
    `-ldflags=-X main.version=agent-vm-control/${version}`,
    "-o",
    temporary,
    "./cmd/agent-vm-control",
  ],
  {
    cwd: moduleRoot,
    env: { ...process.env, CGO_ENABLED: "0", GOOS: match[1], GOARCH: match[2] },
    stdout: "pipe",
    stderr: "pipe",
  },
);
if (result.exitCode !== 0) {
  rmSync(temporary, { force: true });
  throw new Error(new TextDecoder().decode(result.stderr));
}
chmodSync(temporary, 0o755);
renameSync(temporary, output);
console.log(JSON.stringify({ output, target, version, source_revision: revision, cgo_enabled: false, web_assets: withWebAssets }));
