import { expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { discoverTests, ensureTestSelection, testKind } from "./test_runner.ts";

test("discovers maintained TS, Python, and Shell tests automatically", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-"));
  try {
    mkdirSync(join(root, "bin"), { recursive: true });
    mkdirSync(join(root, "skills", "alpha"), { recursive: true });
    writeFileSync(join(root, "bin", "example.test.ts"), "");
    writeFileSync(join(root, "skills", "alpha", "test_python.py"), "");
    writeFileSync(join(root, "skills", "alpha", "test-shell.sh"), "");
    expect(testKind("example.test.ts")).toBe("bun");
    expect(testKind("test_python.py")).toBe("python");
    expect(testKind("test-shell.sh")).toBe("shell");
    expect(discoverTests(root, "repo").map((testFile) => testFile.kind)).toEqual(["bun"]);
    expect(discoverTests(root, "skills").map((testFile) => testFile.kind)).toEqual(["python", "shell"]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed when deliberate test selection omits every file", () => {
  expect(() => ensureTestSelection([], "repo")).toThrow("refusing to pass an empty test selection");
});
