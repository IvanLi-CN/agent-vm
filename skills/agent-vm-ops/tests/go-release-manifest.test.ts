import { expect, test } from "bun:test";
import { build_go_manifest, GO_FIXED_ARTIFACTS, validate_go_manifest } from "../scripts/go-release-manifest.ts";

const artifacts = GO_FIXED_ARTIFACTS.map(([name, path, role, mode]) => ({ name, path, role, mode, sha256: "a".repeat(64) }));

test("format-2 manifest fixes the Go target, toolchain, transport enum and retired units", () => {
  const manifest = build_go_manifest("a".repeat(40), artifacts);
  expect(manifest.format).toBe(2);
  expect(manifest.toolchain).toEqual({ go: "go1.27.3", cgo_enabled: false });
  expect(manifest.client_view.transport).toBe("go-request");
  expect(manifest.retired_units).toContain("agent-vm-native-expire@.service");
  expect(validate_go_manifest(manifest)).toEqual(manifest);
});

test("format-2 manifest rejects a candidate artifact or unknown transport before execution", () => {
  const manifest = build_go_manifest("a".repeat(40), artifacts);
  expect(() => validate_go_manifest({ ...manifest, client_view: { ...manifest.client_view, transport: "shell" } })).toThrow("transport");
  expect(() => validate_go_manifest({ ...manifest, artifacts: [{ ...manifest.artifacts[0], path: "../candidate" }, ...manifest.artifacts.slice(1)] })).toThrow();
});
