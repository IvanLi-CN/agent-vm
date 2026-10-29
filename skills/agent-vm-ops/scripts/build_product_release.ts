#!/usr/bin/env bun

/** Build one immutable SemVer product package for the client or Go control plane. */

import { createHash } from "node:crypto";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { tmpdir } from "node:os";

import {
  CLIENT_TRANSPORTS,
  build_product_manifest,
  LEGACY_PRODUCT_MANIFEST_FORMAT,
  PRODUCT_LINUX_ARTIFACTS,
  PRODUCT_MANIFEST_FORMAT,
  verify_client_sidecar,
  verify_product_bundle,
  type Artifact,
  type ClientTransport,
  type ProductTarget,
} from "./product-manifest.ts";
import { CLIENT_DEPENDENCIES } from "../../agent-vm/scripts/client-release.ts";

const usage = "usage: build_product_release.ts --version <semver> --source-revision <40-hex> --target <client|linux/amd64|linux/arm64> --output <absolute-path> [--transport <legacy-bun|go-request|https>]";
const args = process.argv.slice(2);
const value = (name: string): string => {
  const index = args.indexOf(name);
  if (index < 0 || !args[index + 1]) throw new Error(usage);
  return args[index + 1];
};

const version = value("--version");
const revision = value("--source-revision");
if (!/^[0-9a-f]{40}$/.test(revision)) throw new Error("source revision must be a 40-character lowercase Git revision");
const rawTarget = value("--target");
const targetMatch = /^(linux)\/(amd64|arm64)$/.exec(rawTarget);
if (rawTarget !== "client" && !targetMatch) throw new Error("target must be client or linux/amd64 or linux/arm64");
const defaultTransport: ClientTransport = rawTarget === "client" ? "legacy-bun" : "go-request";
const rawTransport = args.includes("--transport") ? value("--transport") : defaultTransport;
if (!CLIENT_TRANSPORTS.includes(rawTransport as ClientTransport)) throw new Error("transport must be legacy-bun, go-request, or https");
const transport = rawTransport as ClientTransport;
const rawOutput = value("--output");
if (!isAbsolute(rawOutput)) throw new Error("output must be absolute");
const output = resolve(rawOutput);
if (existsSync(output)) throw new Error(`output already exists: ${output}`);

const repositoryRoot = resolve(import.meta.dir, "../../..");
const clientRoot = resolve(import.meta.dir, "../../agent-vm/scripts");
const operationsRoot = resolve(import.meta.dir, "..");
const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-product-release-"));

function commandOutput(command: string[]): string {
  const result = Bun.spawnSync(command, { cwd: repositoryRoot, stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) throw new Error(new TextDecoder().decode(result.stderr));
  return new TextDecoder().decode(result.stdout).trim();
}

function digest(path: string): string {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function compareDeterministically(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function sourcePath(relative: string, binaryPath: string): string {
  if (relative === "agent_vm_control_bridge.ts") return join(import.meta.dir, relative);
	if (relative.startsWith("systemd/")) return join(operationsRoot, "assets", relative);
	if (relative === "config/agent-vm-web.json.example") return join(operationsRoot, "assets", "web.json.example");
	if (relative === "web-assets.manifest.json") return webAssetsManifestPath;
  if (relative === "bin/agent-vm-control") return binaryPath;
  return join(clientRoot, relative);
}

function buildBinary(target: string, binaryPath: string): void {
  mkdirSync(dirname(binaryPath), { recursive: true, mode: 0o750 });
  const result = Bun.spawnSync([
    process.execPath,
    join(import.meta.dir, "build_go_control.ts"),
    "--version", version,
    "--source-revision", revision,
    "--target", target,
    "--output", binaryPath,
    "--with-web-assets",
  ], { stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) throw new Error(new TextDecoder().decode(result.stderr));
}

function packageFiles(target: "client" | "linux/amd64" | "linux/arm64", binaryPath: string): readonly (readonly [string, string, Artifact["role"], number])[] {
  if (target === "client") {
    return [
      ["agent_vm.ts", "agent_vm.ts", "client", 0o755],
      ...CLIENT_DEPENDENCIES.map(([name, mode]) => [name, name, "support", mode] as const),
    ];
  }
  return [...PRODUCT_LINUX_ARTIFACTS];
}

let webAssetsManifestPath = "";

try {
  const status = commandOutput(["git", "status", "--porcelain=v1", "--untracked-files=all"]);
  if (status) throw new Error("refusing to build a release package from a dirty worktree");
  const head = commandOutput(["git", "rev-parse", "HEAD"]);
  if (head !== revision) throw new Error(`source revision is not the checked-out commit: expected ${revision}, got ${head}`);
  commandOutput(["git", "cat-file", "-e", `${revision}^{commit}`]);

  const target = (rawTarget === "client"
    ? { os: "source", arch: "any" }
    : { os: "linux", arch: targetMatch![2] }) as ProductTarget;
  const binaryPath = join(temporaryRoot, "bin/agent-vm-control");
  if (rawTarget !== "client") {
    webAssetsManifestPath = join(temporaryRoot, "web-assets.manifest.json");
    const assetBuild = Bun.spawnSync([
      process.execPath,
      join(import.meta.dir, "build_web_assets.ts"),
      "--version", version,
      "--source-revision", revision,
      "--manifest-output", webAssetsManifestPath,
    ], { stdout: "pipe", stderr: "pipe" });
    if (assetBuild.exitCode !== 0) throw new Error(new TextDecoder().decode(assetBuild.stderr));
    buildBinary(rawTarget, binaryPath);
  }

  const artifacts: Artifact[] = [];
  for (const [name, relative, role, mode] of [...packageFiles(rawTarget as "client" | "linux/amd64" | "linux/arm64", binaryPath)].sort((left, right) => compareDeterministically(left[1], right[1]))) {
    const destination = join(temporaryRoot, relative);
    mkdirSync(dirname(destination), { recursive: true, mode: 0o750 });
    const source = sourcePath(relative, binaryPath);
    if (source !== destination) copyFileSync(source, destination);
    chmodSync(destination, mode);
    artifacts.push({ name, path: relative, role, mode, sha256: digest(destination) });
  }

  const manifest = build_product_manifest(version, revision, artifacts, {
    format: rawTarget === "client" ? LEGACY_PRODUCT_MANIFEST_FORMAT : PRODUCT_MANIFEST_FORMAT,
    target,
    transport,
  });
  writeFileSync(join(temporaryRoot, "release-manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o644 });
  const clientArtifact = artifacts.find((item) => item.role === "client");
  if (!clientArtifact) throw new Error("product manifest has no client artifact");
  const sidecar = {
    format: manifest.format,
    version,
    source_revision: revision,
    protocol: 4,
    client_artifact: clientArtifact,
    client_dependencies: artifacts.filter((item) => CLIENT_DEPENDENCIES.some(([name]) => name === item.path)),
    client_view: manifest.client_view,
  };
  writeFileSync(join(temporaryRoot, "client-release.json"), `${JSON.stringify(sidecar, null, 2)}\n`, { mode: 0o644 });
  verify_product_bundle(temporaryRoot, manifest);
  verify_client_sidecar(temporaryRoot, manifest);

  mkdirSync(dirname(output), { recursive: true, mode: 0o750 });
  const temporaryArchive = `${output}.tmp-${process.pid}`;
  rmSync(temporaryArchive, { force: true });
  const members = ["release-manifest.json", "client-release.json", ...artifacts.map((item) => item.path)];
  const tar = Bun.spawnSync(["tar", "-cf", temporaryArchive, "-C", temporaryRoot, "--no-recursion", ...members], {
    stdout: "pipe",
    stderr: "pipe",
    env: { ...process.env, COPYFILE_DISABLE: "1" },
  });
  if (tar.exitCode !== 0) {
    rmSync(temporaryArchive, { force: true });
    throw new Error(new TextDecoder().decode(tar.stderr));
  }
  chmodSync(temporaryArchive, 0o644);
  renameSync(temporaryArchive, output);
  console.log(JSON.stringify({ output, target: rawTarget, version, source_revision: revision, transport, artifact_count: artifacts.length, format: manifest.format, cgo_enabled: false, web_assets: rawTarget !== "client" }));
} finally {
  rmSync(temporaryRoot, { recursive: true, force: true });
}
