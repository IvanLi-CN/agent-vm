#!/usr/bin/env bun
/** Discover and run maintained TS, Python, and Shell tests without a root edit per sibling. */

import { dirname, join, relative } from "node:path";
import { isDirectRegularFile, isPathInside, readText, REPO_ROOT, relativePath, walkFiles } from "./repo_util.ts";

export type TestScope = "repo" | "skills";
export type TestKind = "bun" | "python" | "shell";
export type TestFile = { path: string; kind: TestKind };

const IGNORED_DIRECTORIES = new Set([
  "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", "dist", "build", "coverage", "reports", "artifacts", "tmp",
  ".git",
]);

const PYTHON_TEST_RUNNER = [
  "import importlib.util",
  "import inspect",
  "import pathlib",
  "import sys",
  "import unittest",
  "path = pathlib.Path(sys.argv[1])",
  "sys.path.insert(0, str(path.parent))",
  "spec = importlib.util.spec_from_file_location('agent_toolkit_test_module', path)",
  "if spec is None or spec.loader is None:",
  "    raise ImportError(path)",
  "module = importlib.util.module_from_spec(spec)",
  "sys.modules[spec.name] = module",
  "spec.loader.exec_module(module)",
  "async_names = [name for name, value in vars(module).items() if name.startswith('test_') and (inspect.iscoroutinefunction(value) or inspect.isasyncgenfunction(value))]",
  "async_names += [f'{name}.{method}' for name, value in vars(module).items() if inspect.isclass(value) and issubclass(value, unittest.TestCase) for method, member in vars(value).items() if method.startswith('test_') and inspect.iscoroutinefunction(member)]",
  "suite = unittest.defaultTestLoader.loadTestsFromModule(module)",
  "def async_test_names(test_suite):",
  "    names = []",
  "    for case in test_suite:",
  "        if isinstance(case, unittest.TestSuite):",
  "            names.extend(async_test_names(case))",
  "        elif isinstance(case, unittest.TestCase):",
  "            method_name = getattr(case, '_testMethodName', '')",
  "            method = getattr(case, method_name, None)",
  "            if method is not None and (inspect.iscoroutinefunction(method) or inspect.isasyncgenfunction(method)):",
  "                names.append(f'{case.__class__.__name__}.{method_name}')",
  "    return names",
  "async_names += async_test_names(suite)",
  "if async_names:",
  "    raise TypeError('async tests are unsupported: ' + ', '.join(async_names))",
  "suite.addTests(unittest.FunctionTestCase(value) for name, value in vars(module).items() if name.startswith('test_') and callable(value))",
  "result = unittest.TextTestRunner(verbosity=1).run(suite)",
  "raise SystemExit(0 if result.wasSuccessful() else 1)",
].join("\n");

export function testKind(path: string): TestKind | undefined {
  const name = path.split(/[\\/]/).at(-1) ?? "";
  if (name.endsWith(".test.ts") || /^test[-_].*\.ts$/.test(name) || /_test\.ts$/.test(name)) return "bun";
  if (name.endsWith(".test.py") || /^test[-_].*\.py$/.test(name) || /_test\.py$/.test(name)) return "python";
  if (name.endsWith(".test.sh") || /^test[-_].*\.sh$/.test(name) || /_test\.sh$/.test(name)) return "shell";
  return undefined;
}

export function discoverTests(root: string = REPO_ROOT, scope: TestScope = "repo"): TestFile[] {
  const skillsRoot = join(root, "skills");
  const rejectedSymlinks: string[] = [];
  const files = walkFiles(root, {
    ignoredDirectories: IGNORED_DIRECTORIES,
    shouldIgnoreDirectory: (path, name) => {
      const isDirectSkill = path !== skillsRoot && dirname(path) === skillsRoot;
      return !isDirectSkill && (IGNORED_DIRECTORIES.has(name) || name.endsWith(".egg-info"));
    },
    onRejectedSymlink: (path) => rejectedSymlinks.push(path),
  });
  if (rejectedSymlinks.length > 0) {
    throw new Error(`refusing to discover tests through rejected symlink paths: ${rejectedSymlinks.sort().join(", ")}`);
  }
  return files
    .filter((path) => path !== join(root, "bin", "test_runner.ts"))
    .filter((path) => (scope === "repo" ? !path.startsWith(join(root, "skills") + "/") && !path.startsWith(join(root, "skills") + "\\") : path.startsWith(join(root, "skills") + "/") || path.startsWith(join(root, "skills") + "\\")))
    .flatMap((path) => {
      const kind = testKind(path);
      return kind ? [{ path, kind }] : [];
    })
    .sort((left, right) => (left.path < right.path ? -1 : left.path > right.path ? 1 : 0));
}

export function ensureTestSelection(files: TestFile[], scope: TestScope): void {
  if (files.length === 0) {
    throw new Error(`no maintained ${scope} tests were discovered; refusing to pass an empty test selection`);
  }
}

export function ensureTestSources(files: TestFile[]): void {
  const invalid = files.filter((test) => {
    const source = readText(test.path);
    if (source.trim().length === 0) return true;
    if (test.kind === "bun") return !/\b(?:test|it|describe)\s*\(/.test(source);
    if (test.kind === "python") return !/\b(?:unittest\.|TestCase\b|def\s+test_)/.test(source);
    const hasTestCommand = source.split("\n").some((line) => {
      const executableLine = line
        .replace(/'(?:[^'\\]|\\.)*'/g, "''")
        .replace(/"(?:[^"\\]|\\.)*"/g, "\"\"")
        .replace(/\s+#.*$/, "");
      const commandBoundary = /(?:^|[;&|]\s*|\bthen\s+)(?:if\s+)?!?\s*/;
      return new RegExp(`${commandBoundary.source}test\\b`).test(executableLine);
    });
    return !hasTestCommand;
  });
  if (invalid.length > 0) {
    throw new Error(`refusing to run test files without maintained test declarations: ${invalid.map((test) => test.path).join(", ")}`);
  }
}

export function executedTestCount(kind: TestKind, output: string): number | undefined {
  const summary = kind === "shell"
    ? /^\s*TESTS_RUN\s*=\s*(\d+)\s*$/gim
    : kind === "bun"
      ? /^\s*Ran\s+(\d+)\s+tests?\s+across\b/gim
      : /^\s*Ran\s+(\d+)\s+tests?\s+in\b/gim;
  const matches = [...output.matchAll(summary)];
  const match = matches.at(-1);
  return match ? Number(match[1]) : undefined;
}

export function commandForTest(test: TestFile): string[] {
  if (test.kind === "bun") return [process.execPath, "test", test.path];
  if (test.kind === "python") return ["python3", "-c", PYTHON_TEST_RUNNER, test.path];
  return [
    "bash",
    "-c",
    "set -euo pipefail\nmarker_file=$(mktemp)\nreadonly marker_file\ntrap 'rm -f -- \"$marker_file\"' EXIT\ntest() { local status; builtin test \"$@\"; status=$?; builtin printf '.\\n' >> \"$marker_file\" || true; return \"$status\"; }\nreadonly -f test\nset +e\n(\n  set -euo pipefail\n  source \"$0\"\n)\nexit_status=$?\nset -e\ncount=$(wc -l < \"$marker_file\")\nif ((exit_status == 0 && count == 0)); then exit_status=1; fi\nif ((exit_status == 0)); then builtin printf \"TESTS_RUN=%s\\n\" \"$count\"; fi\nexit \"$exit_status\"\n",
    test.path,
  ];
}

async function runTest(test: TestFile): Promise<number> {
  if (!isPathInside(REPO_ROOT, test.path) || !isDirectRegularFile(test.path)) {
    console.error(`ERROR refusing to run a test outside the regular repository file boundary: ${test.path}`);
    return 1;
  }
  console.log(`::group::${relativePath(test.path)}`);
  const child = Bun.spawn(commandForTest(test), {
    cwd: REPO_ROOT,
    stdout: "pipe",
    stderr: "pipe",
  });
  const stdout = new Response(child.stdout).text();
  const stderr = new Response(child.stderr).text();
  const exitCode = await child.exited;
  const [out, err] = await Promise.all([stdout, stderr]);
  process.stdout.write(out);
  process.stderr.write(err);
  console.log("::endgroup::");
  const count = executedTestCount(test.kind, `${out}\n${err}`);
  if (exitCode === 0 && (count === undefined || count === 0)) {
    console.error(`ERROR test command reported zero executed tests: ${test.path}`);
    return 1;
  }
  return exitCode;
}

function parseScope(args: string[]): TestScope {
  if (args.length === 1 && args[0].startsWith("--scope=")) {
    const value = args[0].slice("--scope=".length);
    if (value === "repo" || value === "skills") return value;
  }
  if (args.length === 2 && args[0] === "--scope") {
    const value = args[1];
    if (value === "repo" || value === "skills") return value;
  }
  throw new Error("usage: test_runner.ts --scope repo|skills");
}

export async function main(args: string[] = Bun.argv.slice(2)): Promise<number> {
  let scope: TestScope;
  try {
    scope = parseScope(args);
  } catch (error) {
    console.error(`ERROR ${String(error)}`);
    return 2;
  }
  const tests = discoverTests(REPO_ROOT, scope);
  try {
    ensureTestSelection(tests, scope);
    ensureTestSources(tests);
  } catch (error) {
    console.error(`ERROR ${String(error)}`);
    return 1;
  }
  console.log(`Discovered ${tests.length} ${scope} test file(s)`);
  for (const test of tests) {
    const exitCode = await runTest(test);
    if (exitCode !== 0) return exitCode;
  }
  return 0;
}

if (import.meta.main) {
  process.exitCode = await main();
}
