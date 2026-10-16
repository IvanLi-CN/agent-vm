import { expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { commandForTest, discoverTests, ensureTestSelection, ensureTestSources, executedTestCount, testKind } from "./test_runner.ts";

test("discovers maintained TS, Python, and Shell tests automatically", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-"));
  try {
    mkdirSync(join(root, "bin"), { recursive: true });
    mkdirSync(join(root, "skills", "alpha"), { recursive: true });
    mkdirSync(join(root, "skills", "build"), { recursive: true });
    mkdirSync(join(root, "build"), { recursive: true });
    mkdirSync(join(root, "alpha.egg-info"), { recursive: true });
    mkdirSync(join(root, "skills", "alpha", ".pytest_cache"), { recursive: true });
    writeFileSync(join(root, "bin", "example.test.ts"), "");
    writeFileSync(join(root, "bin", "test_runner.ts"), "");
    writeFileSync(join(root, "skills", "alpha", "test_python.py"), "");
    writeFileSync(join(root, "skills", "alpha", "test-shell.sh"), "");
    writeFileSync(join(root, "skills", "alpha", "test_runner.ts"), "");
    writeFileSync(join(root, "skills", "build", "test_direct.py"), "");
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
    expect(discoverTests(root, "skills").map((testFile) => testFile.kind).sort()).toEqual(["bun", "python", "python", "shell"]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed when deliberate test selection omits every file", () => {
  expect(() => ensureTestSelection([], "repo")).toThrow("refusing to pass an empty test selection");
});

test("runs Python files by path so files without a main guard or with hyphens are executed", () => {
  expect(commandForTest({ path: "/repo/skills/example/tests/test_reclamation.py", kind: "python" })).toEqual([
    "python3", "-c", expect.any(String), "/repo/skills/example/tests/test_reclamation.py",
  ]);
  const command = commandForTest({ path: "/repo/skills/example/tests/test_reclamation.py", kind: "python" });
  expect(command[2]).toContain("spec_from_file_location");
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

test("requires shell tests to contain a test command", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-"));
  try {
    const noOp = join(root, "test_noop.sh");
    const valid = join(root, "test_valid.sh");
    writeFileSync(noOp, "#!/usr/bin/env bash\necho 'test TESTS_RUN=1'\n");
    writeFileSync(valid, "#!/usr/bin/env bash\ntest -e /tmp\n");
    expect(() => ensureTestSources([{ path: noOp, kind: "shell" }])).toThrow("without maintained test declarations");
    expect(() => ensureTestSources([{ path: valid, kind: "shell" }])).not.toThrow();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("accepts Bun parameterized test declarations", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-bun-each-"));
  try {
    const path = join(root, "test_each.test.ts");
    writeFileSync(path, "test.each([1, 2])('value', (value) => expect(value).toBeTruthy())\n");
    expect(() => ensureTestSources([{ path, kind: "bun" }])).not.toThrow();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails shell suites when no test command executes", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-runtime-"));
  try {
    const path = join(root, "test_dead_branch.sh");
    writeFileSync(path, "#!/usr/bin/env bash\ncount=1\nif false; then test -e /missing; fi\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(1);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBeUndefined();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves shell assertion status in conditional contexts", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-status-"));
  try {
    const path = join(root, "test_failed_assertion.sh");
    writeFileSync(path, "test 1 -eq 2 || exit 1\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("reports shell counts before an explicit successful exit", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-explicit-exit-"));
  try {
    const path = join(root, "test_explicit_exit.sh");
    writeFileSync(path, "trap ':' EXIT\ntest 1 -eq 1\nexit 0\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("requires a nonzero runtime test count for Bun and Python suites", () => {
  expect(executedTestCount("bun", "Ran 0 tests across 1 files.\nRan 2 tests across 1 files.")).toBe(2);
  expect(executedTestCount("bun", "Ran 0 tests in a comment")).toBeUndefined();
  expect(executedTestCount("python", "Ran 21 tests in 0.1s")).toBe(21);
  expect(executedTestCount("shell", "TESTS_RUN=3\n")).toBe(3);
  expect(executedTestCount("shell", "shell completed")).toBeUndefined();
});

test("executes a Python unittest suite without a main guard", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-no-main-"));
  try {
    const marker = join(root, "executed.txt");
    const path = join(root, "test_no_main.py");
    writeFileSync(path, `import unittest\nfrom pathlib import Path\n\nclass NoMainTests(unittest.TestCase):\n    def test_runs(self):\n        Path(${JSON.stringify(marker)}).write_text("ok")\n`);
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
    expect(existsSync(marker)).toBe(true);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("executes a hyphenated Python unittest file", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-hyphenated-"));
  try {
    const path = join(root, "test-hyphenated.py");
    writeFileSync(path, "import unittest\n\nclass HyphenatedTests(unittest.TestCase):\n    def test_runs(self):\n        self.assertTrue(True)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("python", `${stdout}\n${stderr}`)).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("executes a module-level Python test function", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-function-"));
  try {
    const path = join(root, "test_function.py");
    writeFileSync(path, "def test_runs():\n    assert True\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("python", `${stdout}\n${stderr}`)).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for unsupported async Python tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-async-function-"));
  try {
    const path = join(root, "test_async.py");
    writeFileSync(path, "async def test_not_run():\n    raise AssertionError('must be awaited')\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);

    const generator = join(root, "test_generator.py");
    writeFileSync(generator, "def test_generator():\n    yield 1\n    raise AssertionError('must not be skipped')\n");
    const generatorChild = Bun.spawn(commandForTest({ path: generator, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await generatorChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for inherited async and async-generator Python tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-inherited-async-"));
  try {
    const inherited = join(root, "test_inherited_async.py");
    writeFileSync(inherited, "import unittest\n\nclass Base(unittest.TestCase):\n    async def test_inherited(self):\n        raise AssertionError('must be awaited')\n\nclass Derived(Base):\n    pass\n");
    const inheritedChild = Bun.spawn(commandForTest({ path: inherited, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await inheritedChild.exited).toBe(1);

    const generator = join(root, "test_async_generator.py");
    writeFileSync(generator, "async def test_generator():\n    yield 1\n    raise AssertionError('must not run')\n");
    const generatorChild = Bun.spawn(commandForTest({ path: generator, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await generatorChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for decorated async Python tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-decorated-async-"));
  try {
    const path = join(root, "test_decorated_async.py");
    writeFileSync(path, "import functools\n\n@functools.lru_cache()\nasync def test_not_run():\n    raise AssertionError('must be awaited')\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
