import { expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
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

test("format-2 manifest rejects malformed action schemas and compatibility windows", () => {
  const manifest = build_go_manifest("a".repeat(40), artifacts);
  const malformed = structuredClone(manifest);
  malformed.actions.status = {
    ...malformed.actions.status,
    request: { schema: 2, semantic_revision: 1 },
  };
  malformed.client_view.actions.status = malformed.actions.status;
  expect(() => validate_go_manifest(malformed)).toThrow("request is outside");

  const inverted = structuredClone(manifest);
  inverted.actions.status = {
    ...inverted.actions.status,
    control_to_client: {
      ...inverted.actions.status.control_to_client,
      schema_min: 2,
      schema_max: 1,
    },
  };
  inverted.client_view.actions.status = inverted.actions.status;
  expect(() => validate_go_manifest(inverted)).toThrow("inverted");
});

test("format-2 release archive excludes host metadata members", { timeout: 30_000 }, () => {
  const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-go-release-test-"));
  const output = join(temporaryRoot, "release.tar");
  try {
    const result = Bun.spawnSync(
      [
        process.execPath,
        "scripts/build_go_release.ts",
        "--source-revision",
        "a".repeat(40),
        "--target",
        "linux/amd64",
        "--output",
        output,
      ],
      { cwd: new URL("../", import.meta.url).pathname, stdout: "pipe", stderr: "pipe" },
    );
    expect(result.exitCode).toBe(0);
    const listing = Bun.spawnSync(["tar", "-tf", output], { stdout: "pipe", stderr: "pipe" });
    expect(listing.exitCode).toBe(0);
    const members = new TextDecoder().decode(listing.stdout).trim().split("\n").filter(Boolean);
    expect(members.some((member) => member.split("/").some((part) => part.startsWith("._")))).toBe(false);
    expect(members).toContain("release-manifest.json");
    expect(readFileSync(output).byteLength).toBeGreaterThan(0);
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});
