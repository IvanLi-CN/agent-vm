import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { stageGuestImageAssets } from "../scripts/release.ts";

test("release caller accepts all eight real staged slots and refuses missing Base", () => {
  const temp = mkdtempSync(join(tmpdir(), "release-eight-images-"));
  try {
    const source = join(temp, "input");
    const fixture = Bun.spawnSync(["python3", "-c", `
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[2])
from test_stage_release_assets import ReleaseImageStagingTest
ReleaseImageStagingTest().fixture(Path(sys.argv[1]))
`, source, resolve(import.meta.dir, "../../../images/tests")], { stdout: "pipe", stderr: "pipe" });
    expect(fixture.exitCode).toBe(0);
    const assets = stageGuestImageAssets(source, join(temp, "output"), "a".repeat(40));
    expect(Object.keys(assets)).toHaveLength(33);
    expect(assets["agent-vm-ubuntu24-base-privileged-amd64.tar.gz"]).toBeDefined();
    expect(assets["agent-vm-ubuntu24-base-unprivileged-amd64.tar.gz"]).toBeDefined();
    rmSync(join(source, "guest-image-base-privileged"), { recursive: true });
    expect(() => stageGuestImageAssets(source, join(temp, "incomplete"), "a".repeat(40))).toThrow("exactly eight");
  } finally { rmSync(temp, { recursive: true, force: true }); }
});
