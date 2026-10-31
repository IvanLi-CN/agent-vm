import { expect, test } from "bun:test";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

import { build_product_manifest, PRODUCT_LINUX_ARTIFACTS, validate_product_manifest, verify_product_bundle, type Artifact } from "../scripts/product-manifest.ts";
import { default_operations } from "../scripts/release-manifest.ts";

test("format-3 client manifest fixes product identity, source and action windows", () => {
  const root = mkdtempSync(join(tmpdir(), "agent-vm-product-manifest-"));
  try {
    const specs: Array<[string, "client" | "support", number]> = [
      ["agent_vm.ts", "client", 0o755], ["byte-fs.ts", "support", 0o644], ["client-io.ts", "support", 0o644],
      ["client-release.ts", "support", 0o644], ["client.ts", "support", 0o644], ["guest-source.ts", "support", 0o644],
      ["sync-native.ts", "support", 0o644], ["sync_worktree.ts", "support", 0o755],
    ];
    const artifacts: Artifact[] = specs.map(([name, role, mode]) => {
      const path = join(root, name);
      const content = name === "agent_vm.ts" ? "#!/usr/bin/env bun\n" : `// ${name}\n`;
      writeFileSync(path, content, { mode });
      chmodSync(path, mode);
      return { name, path: name, role, mode, sha256: new Bun.CryptoHasher("sha256").update(content).digest("hex") };
    });
    const manifest = build_product_manifest("1.0.0", "a".repeat(40), artifacts, { format: 3, target: { os: "source", arch: "any" }, transport: "legacy-bun", operations: default_operations() });
    expect(manifest.format).toBe(3);
    expect(manifest.version).toBe("1.0.0");
    expect(manifest.release_id).toBe("a".repeat(40));
    expect(manifest.source_revision).toBe("a".repeat(40));
    expect(manifest.client_view.version).toBe("1.0.0");
    expect(manifest.state_compatibility).toBeUndefined();
    expect(verify_product_bundle(root, manifest)).toEqual(manifest);
    const httpsManifest = structuredClone(manifest);
    httpsManifest.client_view.transport = "https";
    expect(validate_product_manifest(httpsManifest)).toEqual(httpsManifest);
    expect(() => build_product_manifest("1.0.0", "a".repeat(40), artifacts, {
      format: 3,
      target: { os: "source", arch: "any" },
      transport: "https",
      operations: default_operations(),
    })).toThrow("validation-only");
    expect(() => verify_product_bundle(root, httpsManifest)).toThrow("validation-only");
    expect(() => build_product_manifest("1.0.0", "a".repeat(40), artifacts.slice(0, 1), { target: { os: "source", arch: "any" } })).toThrow();
    expect(() => build_product_manifest("1.0", "a".repeat(40), artifacts)).toThrow();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("format-4 management manifest requires the Linux product target", () => {
  const root = mkdtempSync(join(tmpdir(), "agent-vm-product-linux-manifest-"));
  try {
    const artifacts: Artifact[] = PRODUCT_LINUX_ARTIFACTS.map(([name, relative, role, mode]) => {
      const path = join(root, relative);
      const content = relative === "bin/agent-vm-control"
        ? (() => {
          const bytes = new Uint8Array(20);
          bytes.set([0x7f, 0x45, 0x4c, 0x46, 2, 1, 1]);
          new DataView(bytes.buffer).setUint16(18, 62, true);
          return bytes;
        })()
        : new TextEncoder().encode(`// ${relative}\n`);
      const parent = path.slice(0, path.lastIndexOf("/"));
      if (parent !== root) mkdirSync(parent, { recursive: true, mode: 0o755 });
      writeFileSync(path, content, { mode });
      chmodSync(path, mode);
      return { name, path: relative, role, mode, sha256: new Bun.CryptoHasher("sha256").update(content).digest("hex") };
    });
    const manifest = build_product_manifest("1.0.0", "b".repeat(40), artifacts, {
      format: 4,
      target: { os: "linux", arch: "amd64" },
      transport: "go-request",
      operations: default_operations(),
    });
    expect(manifest.format).toBe(4);
    expect(manifest.client_view.transport).toBe("go-request");
    expect(manifest.state_compatibility?.reads).toEqual([
      "legacy-agent-vm-core@1", "managed-image-catalog@1", "management-operation-audit@1", "principal-identity@1", "session-image-binding@1", "token-revocation@1",
    ]);
    expect(verify_product_bundle(root, manifest)).toEqual(manifest);
    const httpsManifest = structuredClone(manifest);
    httpsManifest.client_view.transport = "https";
    expect(validate_product_manifest(httpsManifest)).toEqual(httpsManifest);
    expect(() => verify_product_bundle(root, httpsManifest)).toThrow("validation-only");
    const historical = structuredClone(manifest);
    historical.client_view.transport = "legacy-bun";
    expect(validate_product_manifest(historical)).toEqual(historical);
    expect(() => build_product_manifest("1.0.0", "b".repeat(40), artifacts, {
      format: 4,
      target: { os: "linux", arch: "amd64" },
      transport: "legacy-bun",
      operations: default_operations(),
    })).toThrow("Linux product");
    expect(() => build_product_manifest("1.0.0", "b".repeat(40), artifacts, {
      format: 4,
      target: { os: "source", arch: "any" },
      transport: "legacy-bun",
      operations: default_operations(),
    })).toThrow();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("product release CLI rejects HTTPS before creating an archive", () => {
  const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-product-release-https-test-"));
  const output = join(temporaryRoot, "release.tar");
  try {
    const result = Bun.spawnSync(
      [
        process.execPath,
        "scripts/build_product_release.ts",
        "--version",
        "1.0.0",
        "--source-revision",
        "a".repeat(40),
        "--target",
        "linux/amd64",
        "--transport",
        "https",
        "--output",
        output,
      ],
      { cwd: new URL("../", import.meta.url).pathname, stdout: "pipe", stderr: "pipe" },
    );
    expect(result.exitCode).not.toBe(0);
    expect(new TextDecoder().decode(result.stderr)).toContain("validation-only");
    expect(existsSync(output)).toBe(false);
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});

test("product release CLI preserves target-specific default transports", { timeout: 120_000 }, () => {
  const repositoryRoot = new URL("../", import.meta.url).pathname;
  const revisionResult = Bun.spawnSync(["git", "rev-parse", "HEAD"], { cwd: repositoryRoot, stdout: "pipe", stderr: "pipe" });
  expect(revisionResult.exitCode).toBe(0);
  const revision = new TextDecoder().decode(revisionResult.stdout).trim();
  const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-product-release-defaults-test-"));
  try {
    for (const [target, expectedTransport] of [["client", "legacy-bun"], ["linux/amd64", "go-request"]] as const) {
      const output = join(temporaryRoot, `${target.replace("/", "-")}.tar`);
      const result = Bun.spawnSync(
        [
          process.execPath,
          "scripts/build_product_release.ts",
          "--version",
          "1.0.0",
          "--source-revision",
          revision,
          "--target",
          target,
          "--output",
          output,
        ],
        { cwd: repositoryRoot, stdout: "pipe", stderr: "pipe" },
      );
      expect(result.exitCode).toBe(0);
      const manifestResult = Bun.spawnSync(["tar", "-xOf", output, "release-manifest.json"], { stdout: "pipe", stderr: "pipe" });
      expect(manifestResult.exitCode).toBe(0);
      const manifest = JSON.parse(new TextDecoder().decode(manifestResult.stdout));
      expect(manifest.client_view.transport).toBe(expectedTransport);
    }
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});
