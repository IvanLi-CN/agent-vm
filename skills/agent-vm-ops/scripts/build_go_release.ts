#!/usr/bin/env bun

/** Build one immutable format-2 Go control release bundle. */

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
  GO_FIXED_ARTIFACTS,
  build_go_manifest,
  verify_go_bundle,
  type ClientTransport,
  type GoArtifact,
} from "./go-release-manifest.ts";

const usage = "usage: build_go_release.ts --source-revision <40-hex> --target <linux/amd64|linux/arm64> --output <absolute-path> [--transport <legacy-bun|go-request|https>]";
const args = process.argv.slice(2);
const value = (name: string): string => {
  const index = args.indexOf(name);
  if (index < 0 || !args[index + 1]) throw new Error(usage);
  return args[index + 1];
};

const revision = value("--source-revision");
if (!/^[0-9a-f]{40}$/.test(revision)) throw new Error("source revision must be a 40-character lowercase Git revision");
const target = value("--target");
const targetMatch = /^(linux)\/(amd64|arm64)$/.exec(target);
if (!targetMatch) throw new Error("target must be linux/amd64 or linux/arm64");
const transport = args.includes("--transport") ? value("--transport") : "go-request";
if (!CLIENT_TRANSPORTS.includes(transport as ClientTransport)) throw new Error("transport must be legacy-bun, go-request, or https");
if (transport === "legacy-bun") throw new Error("Linux Go releases require go-request or https transport");
const rawOutput = value("--output");
if (!isAbsolute(rawOutput)) throw new Error("output must be absolute");
const output = resolve(rawOutput);
if (existsSync(output)) throw new Error(`output already exists: ${output}`);

const clientRoot = resolve(import.meta.dir, "../../agent-vm/scripts");
const opsRoot = resolve(import.meta.dir, "..");
const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-go-release-"));
const binaryPath = join(temporaryRoot, "bin/agent-vm-control");

function digest(path: string): string {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function sourcePath(relative: string): string {
  if (relative === "agent_vm_control_bridge.ts") return join(import.meta.dir, relative);
  if (relative === "systemd/agent-vm-native.service") return join(opsRoot, "assets", relative);
  if (relative === "bin/agent-vm-control") return binaryPath;
  return join(clientRoot, relative);
}

function buildBinary(): void {
  mkdirSync(dirname(binaryPath), { recursive: true, mode: 0o750 });
  const result = Bun.spawnSync(
    [
      process.execPath,
      join(import.meta.dir, "build_go_control.ts"),
      "--source-revision",
      revision,
      "--target",
      target,
      "--output",
      binaryPath,
    ],
    { stdout: "pipe", stderr: "pipe" },
  );
  if (result.exitCode !== 0) {
    throw new Error(new TextDecoder().decode(result.stderr));
  }
}

try {
  buildBinary();
  const artifacts: GoArtifact[] = [];
  for (const [name, relative, role, mode] of [...GO_FIXED_ARTIFACTS].sort((left, right) => left[1].localeCompare(right[1]))) {
    const destination = join(temporaryRoot, relative);
    mkdirSync(dirname(destination), { recursive: true, mode: 0o750 });
    const source = sourcePath(relative);
    if (source !== destination) copyFileSync(source, destination);
    chmodSync(destination, mode);
    artifacts.push({ name, path: relative, role, mode, sha256: digest(destination) });
  }
  const manifest = build_go_manifest(revision, artifacts, {
    target: { os: "linux", arch: targetMatch[2] as "amd64" | "arm64" },
    transport: transport as ClientTransport,
  });
  writeFileSync(join(temporaryRoot, "release-manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o644 });
  const clientArtifact = artifacts.find((item) => item.role === "client");
  if (!clientArtifact) throw new Error("Go manifest has no client artifact");
  const sidecar = {
    format: 2,
    release_id: revision,
    source_revision: revision,
    protocol: 4,
    client_artifact: clientArtifact,
    client_dependencies: artifacts.filter((item) => ["byte-fs.ts", "client-io.ts", "client-release.ts", "client.ts", "guest-source.ts", "sync-native.ts", "sync_worktree.ts"].includes(item.path)),
    client_view: manifest.client_view,
  };
  writeFileSync(join(temporaryRoot, "client-release.json"), `${JSON.stringify(sidecar, null, 2)}\n`, { mode: 0o644 });
  verify_go_bundle(temporaryRoot, manifest);

  mkdirSync(dirname(output), { recursive: true, mode: 0o750 });
  const temporaryArchive = `${output}.tmp-${process.pid}`;
  rmSync(temporaryArchive, { force: true });
  const members = ["release-manifest.json", "client-release.json", ...artifacts.map((item) => item.path)];
  const tar = Bun.spawnSync(
    ["tar", "-cf", temporaryArchive, "-C", temporaryRoot, "--no-recursion", ...members],
    {
      stdout: "pipe",
      stderr: "pipe",
      // macOS tar otherwise emits AppleDouble `._*` members for source
      // extended attributes, which are outside the signed artifact set.
      env: { ...process.env, COPYFILE_DISABLE: "1" },
    },
  );
  if (tar.exitCode !== 0) {
    rmSync(temporaryArchive, { force: true });
    throw new Error(new TextDecoder().decode(tar.stderr));
  }
  chmodSync(temporaryArchive, 0o644);
  renameSync(temporaryArchive, output);
  console.log(JSON.stringify({ output, target, transport, source_revision: revision, artifact_count: artifacts.length, cgo_enabled: false }));
} finally {
  rmSync(temporaryRoot, { recursive: true, force: true });
}
