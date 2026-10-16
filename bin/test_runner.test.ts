import { expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { commandForTest, discoverTests, ensureTestSelection, ensureTestSources, testKind } from "./test_runner.ts";

test("discovers maintained TS, Python, and Shell tests automatically", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-"));
  try {
    mkdirSync(join(root, "bin"), { recursive: true });
    mkdirSync(join(root, "skills", "alpha"), { recursive: true });
    mkdirSync(join(root, "build"), { recursive: true });
    mkdirSync(join(root, "alpha.egg-info"), { recursive: true });
    mkdirSync(join(root, "skills", "alpha", ".pytest_cache"), { recursive: true });
    writeFileSync(join(root, "bin", "example.test.ts"), "");
    writeFileSync(join(root, "bin", "test_runner.ts"), "");
    writeFileSync(join(root, "skills", "alpha", "test_python.py"), "");
    writeFileSync(join(root, "skills", "alpha", "test-shell.sh"), "");
    writeFileSync(join(root, "skills", "alpha", "test_runner.ts"), "");
    mkdirSync(join(root, "skills", "alpha", "tmp"), { recursive: true });
    writeFileSync(join(root, "skills", "alpha", "tmp", "test_nested.py"), "");
    writeFileSync(join(root, "build", "test_generated.py"), "");
    writeFileSync(join(root, "alpha.egg-info", "test_generated.py"), "");
    writeFileSync(join(root, "skills", "alpha", ".pytest_cache", "test_generated.py"), "");
    expect(testKind("example.test.ts")).toBe("bun");
    expect(testKind("test_python.py")).toBe("python");
    expect(testKind("test-shell.sh")).toBe("shell");
    expect(testKind("test_runner.ts")).toBe("bun");
    expect(discoverTests(root, "repo").map((testFile) => testFile.kind)).toEqual(["bun"]);
    expect(discoverTests(root, "skills").map((testFile) => testFile.kind).sort()).toEqual(["bun", "python", "shell"]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed when deliberate test selection omits every file", () => {
  expect(() => ensureTestSelection([], "repo")).toThrow("refusing to pass an empty test selection");
});

test("runs Python files through unittest discovery so files without a main guard are executed", () => {
  expect(commandForTest({ path: "/repo/skills/example/tests/test_reclamation.py", kind: "python" })).toEqual([
    "python3", "-m", "unittest", "discover", "-s", "/repo/skills/example/tests", "-p", "test_reclamation.py",
  ]);
});

test("fails closed for empty and non-test source files", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-invalid-"));
  try {
    const empty = join(root, "test_empty.py");
    const nonTest = join(root, "test_non_test.py");
    writeFileSync(empty, "");
    writeFileSync(nonTest, "print('not a unittest suite')\n");
    expect(() => ensureTestSources([
      { path: empty, kind: "python" },
      { path: nonTest, kind: "python" },
    ])).toThrow("without maintained test declarations");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
