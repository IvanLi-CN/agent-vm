import { expect, test } from "bun:test";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

import { build_product_manifest, verify_product_bundle, type Artifact } from "../scripts/product-manifest.ts";
import { default_operations } from "../scripts/release-manifest.ts";

test("format-3 manifest fixes product identity, source, toolchain and action windows", () => {
  const root = mkdtempSync(join(tmpdir(), "agent-vm-product-manifest-"));
  try {
    const path = join(root, "agent_vm.ts");
    writeFileSync(path, "#!/usr/bin/env bun\n", { mode: 0o755 });
    chmodSync(path, 0o755);
    const artifact: Artifact = { name: "agent_vm.ts", path: "agent_vm.ts", role: "client", mode: 0o755, sha256: new Bun.CryptoHasher("sha256").update("#!/usr/bin/env bun\n").digest("hex") };
    const manifest = build_product_manifest("1.0.0", "a".repeat(40), [artifact], { target: { os: "source", arch: "any" }, transport: "legacy-bun", operations: default_operations() });
    expect(manifest.format).toBe(3);
    expect(manifest.version).toBe("1.0.0");
    expect(manifest.source_revision).toBe("a".repeat(40));
    expect(manifest.client_view.version).toBe("1.0.0");
    expect(verify_product_bundle(root, manifest)).toEqual(manifest);
    expect(() => build_product_manifest("1.0", "a".repeat(40), [artifact])).toThrow();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
