#!/usr/bin/env bun
/** Discover and run maintained TS, Python, and Shell tests without a root edit per sibling. */

import { join, relative } from "node:path";
import { REPO_ROOT, relativePath, walkFiles } from "./repo_util.ts";

export type TestScope = "repo" | "skills";
export type TestKind = "bun" | "python" | "shell";
export type TestFile = { path: string; kind: TestKind };

const IGNORED_DIRECTORIES = new Set([".git", "node_modules", ".venv", "venv", "__pycache__"]);

export function testKind(path: string): TestKind | undefined {
  const name = path.split(/[\\/]/).at(-1) ?? "";
  if (name === "test_runner.ts") return undefined;
  if (name.endsWith(".test.ts") || /^test[-_].*\.ts$/.test(name) || /_test\.ts$/.test(name)) return "bun";
  if (name.endsWith(".test.py") || /^test[-_].*\.py$/.test(name) || /_test\.py$/.test(name)) return "python";
  if (name.endsWith(".test.sh") || /^test[-_].*\.sh$/.test(name) || /_test\.sh$/.test(name)) return "shell";
  return undefined;
}

export function discoverTests(root: string = REPO_ROOT, scope: TestScope = "repo"): TestFile[] {
  const files = walkFiles(root, { ignoredDirectories: IGNORED_DIRECTORIES });
  return files
    .filter((path) => (scope === "repo" ? !path.startsWith(join(root, "skills") + "/") && !path.startsWith(join(root, "skills") + "\\") : path.startsWith(join(root, "skills") + "/") || path.startsWith(join(root, "skills") + "\\")))
    .flatMap((path) => {
      const kind = testKind(path);
      return kind ? [{ path, kind }] : [];
    })
    .sort((left, right) => left.path.localeCompare(right.path));
}

export function ensureTestSelection(files: TestFile[], scope: TestScope): void {
  if (files.length === 0) {
    throw new Error(`no maintained ${scope} tests were discovered; refusing to pass an empty test selection`);
  }
}

export function commandForTest(test: TestFile): string[] {
  if (test.kind === "bun") return [process.execPath, "test", test.path];
  if (test.kind === "python") return ["python3", test.path];
  return ["bash", test.path];
}

async function runTest(test: TestFile): Promise<number> {
  console.log(`::group::${relativePath(test.path)}`);
  const child = Bun.spawn(commandForTest(test), {
    cwd: REPO_ROOT,
    stdout: "inherit",
    stderr: "inherit",
  });
  const exitCode = await child.exited;
  console.log("::endgroup::");
  return exitCode;
}

function parseScope(args: string[]): TestScope {
  const value = args.find((arg) => arg.startsWith("--scope="))?.slice("--scope=".length) ?? args[args.indexOf("--scope") + 1] ?? "";
  if (value !== "repo" && value !== "skills") {
    throw new Error("usage: test_runner.ts --scope repo|skills");
  }
  return value;
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
