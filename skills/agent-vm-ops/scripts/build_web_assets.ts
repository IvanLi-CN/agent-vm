#!/usr/bin/env bun

import { createHash } from "node:crypto";
import { chmodSync, copyFileSync, mkdirSync, readdirSync, lstatSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";

const usage = "usage: build_web_assets.ts --version <semver> --source-revision <40-hex> --manifest-output <absolute-path>";
const args = process.argv.slice(2);
const value = (name: string): string => {
  const index = args.indexOf(name);
  if (index < 0 || !args[index + 1]) throw new Error(usage);
  return args[index + 1];
};

const version = value("--version");
const revision = value("--source-revision");
if (!/^[0-9a-f]{40}$/.test(revision)) throw new Error("source revision must be a 40-character lowercase Git revision");
const rawOutput = value("--manifest-output");
if (!rawOutput.startsWith("/")) throw new Error("manifest output must be absolute");
const manifestOutput = resolve(rawOutput);
const repositoryRoot = resolve(import.meta.dir, "../../..");
const webRoot = resolve(repositoryRoot, "web");
const distRoot = resolve(webRoot, "dist");
const generatedRoot = resolve(repositoryRoot, "skills/agent-vm-ops/control/internal/webassets");
const staticRoot = join(generatedRoot, "static");
const generatedSource = join(generatedRoot, "assets_release.go");

function run(command: string[], cwd = repositoryRoot): void {
  const result = Bun.spawnSync(command, { cwd, env: { ...process.env, VITE_MANAGEMENT_API: "real" }, stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) throw new Error(new TextDecoder().decode(result.stderr));
}

function compareDeterministically(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function collectFiles(root: string, current = root): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(current, { withFileTypes: true }).sort((left, right) => compareDeterministically(left.name, right.name))) {
    const absolute = join(current, entry.name);
    const info = lstatSync(absolute);
    if (info.isSymbolicLink()) throw new Error(`Web dist contains a symbolic link: ${relative(root, absolute)}`);
    if (info.isDirectory()) files.push(...collectFiles(root, absolute));
    else if (info.isFile()) files.push(absolute);
    else throw new Error(`Web dist contains an unsupported entry: ${relative(root, absolute)}`);
  }
  return files.sort((left, right) => compareDeterministically(relative(root, left).split(sep).join("/"), relative(root, right).split(sep).join("/")));
}

function quote(value: string): string {
  return JSON.stringify(value);
}

try {
  run([process.execPath, "run", "build:web"]);
  const index = lstatSync(join(distRoot, "index.html"));
  if (!index.isFile()) throw new Error("Web build did not produce index.html");
  const files = collectFiles(distRoot);
  const relativeFiles = files.map((absolute) => relative(distRoot, absolute).split(sep).join("/"));
  if (!relativeFiles.some((path) => path.endsWith(".js")) || !relativeFiles.some((path) => path.endsWith(".css")) || !relativeFiles.some((path) => /\.(woff2?|ttf|otf)$/.test(path))) {
    throw new Error("Web build must contain JavaScript, CSS, and local font assets");
  }

  rmSync(staticRoot, { recursive: true, force: true });
  mkdirSync(staticRoot, { recursive: true, mode: 0o750 });
  const entries = files.map((absolute) => {
    const path = relative(distRoot, absolute).split(sep).join("/");
    const data = readFileSync(absolute);
    const digest = createHash("sha256").update(data).digest("hex");
    const destination = join(staticRoot, ...path.split("/"));
    mkdirSync(dirname(destination), { recursive: true, mode: 0o750 });
    copyFileSync(absolute, destination);
    chmodSync(destination, 0o644);
    return { path, sha256: digest, size: data.byteLength };
  });
  const manifest = { format: 1, version, source_revision: revision, files: entries };
  mkdirSync(dirname(manifestOutput), { recursive: true, mode: 0o750 });
  writeFileSync(manifestOutput, `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o644 });

  const generated = [
    "//go:build releaseassets",
    "",
    "package webassets",
    "",
    "import (",
    "\t\"embed\"",
    "\t\"io/fs\"",
    ")",
    "",
    "//go:embed static",
    "var embeddedAssets embed.FS",
    "",
    "var Files fs.FS",
    "",
    "var releaseEntries = []Entry{",
    ...entries.map((entry) => `\t{Path: ${quote(entry.path)}, SHA256: ${quote(entry.sha256)}, Size: ${entry.size}},`),
    "}",
    "",
    "func init() {",
    "\tFiles, _ = fs.Sub(embeddedAssets, \"static\")",
    "}",
    "",
    "func Verify() error {",
    "\treturn VerifyFS(Files, releaseEntries)",
    "}",
    "",
  ].join("\n");
  writeFileSync(generatedSource, generated, { mode: 0o644 });
  console.log(JSON.stringify({ manifest: manifestOutput, asset_count: entries.length, generated: generatedSource }));
} catch (error) {
  rmSync(staticRoot, { recursive: true, force: true });
  rmSync(generatedSource, { force: true });
  throw error;
}
