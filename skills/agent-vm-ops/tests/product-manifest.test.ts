import { expect, test } from "bun:test";
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

import { build_product_manifest, PRODUCT_LINUX_ARTIFACTS, verify_product_bundle, type Artifact } from "../scripts/product-manifest.ts";
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
    expect(manifest.state_compatibility?.reads).toEqual([
      "legacy-agent-vm-core@1", "managed-image-catalog@1", "management-operation-audit@1", "session-image-binding@1",
    ]);
    expect(verify_product_bundle(root, manifest)).toEqual(manifest);
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
