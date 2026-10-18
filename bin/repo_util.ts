import { existsSync, lstatSync, readdirSync, readFileSync, realpathSync, statSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");

export type GitResult = {
  exitCode: number;
  stdout: string;
  stderr: string;
};

const GIT_REPOSITORY_ENVIRONMENT = new Set([
  "GIT_ALTERNATE_OBJECT_DIRECTORIES",
  "GIT_COMMON_DIR",
  "GIT_DIR",
  "GIT_INDEX_FILE",
  "GIT_NAMESPACE",
  "GIT_OBJECT_DIRECTORY",
  "GIT_OBJECT_DIRECTORY_RELATIVE",
  "GIT_SHALLOW_FILE",
  "GIT_WORK_TREE",
]);

export function gitEnvironment(environment: Record<string, string | undefined>): Record<string, string> {
  const sanitized = Object.fromEntries(Object.entries(environment).filter(([key, value]) => value !== undefined && !GIT_REPOSITORY_ENVIRONMENT.has(key) && key !== "GIT_CONFIG" && !key.startsWith("GIT_CONFIG_") && key !== "GIT_TEMPLATE_DIR")) as Record<string, string>;
  return {
    ...sanitized,
    GIT_CONFIG_GLOBAL: process.platform === "win32" ? "NUL" : "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
  };
}

function decode(output: Uint8Array): string {
  return new TextDecoder().decode(output);
}

export function runGit(args: string[], cwd: string = REPO_ROOT): GitResult {
  const result = Bun.spawnSync(["git", ...args], {
    cwd,
    env: gitEnvironment(Bun.env),
    stdout: "pipe",
    stderr: "pipe",
  });
  return {
    exitCode: result.exitCode,
    stdout: decode(result.stdout),
    stderr: decode(result.stderr),
  };
}

export function trackedFiles(repoRoot: string = REPO_ROOT): string[] {
  const result = runGit(["ls-files", "-co", "--exclude-standard", "-z"], repoRoot);
  if (result.exitCode !== 0) {
    throw new Error(`git ls-files failed: ${result.stderr.trim()}`);
  }
  return result.stdout
    .split("\0")
    .filter(Boolean)
    .map((path) => join(repoRoot, path));
}

export type WalkOptions = {
  ignoredDirectories?: Set<string>;
  shouldIgnoreDirectory?: (path: string, name: string) => boolean;
  onRejectedSymlink?: (path: string) => void;
};

export function walkFiles(root: string, options: WalkOptions = {}): string[] {
  if (!existsSync(root) || !lstatSync(root).isDirectory()) {
    return [];
  }

  const ignored = options.ignoredDirectories ?? new Set<string>();
  const rootRealPath = realpathSync(root);
  const visitedDirectories = new Set<string>();
  const files: string[] = [];
  const rejectSymlink = options.onRejectedSymlink;
  const reportRejectedSymlink = (path: string): void => {
    rejectSymlink?.(path);
  };
  const isInsideRoot = (path: string): boolean => path === rootRealPath || path.startsWith(`${rootRealPath}${sep}`);
  const shouldIgnore = (path: string, name: string): boolean => options.shouldIgnoreDirectory
    ? options.shouldIgnoreDirectory(path, name)
    : ignored.has(name);
  const visit = (directory: string, realDirectory: string = realpathSync(directory)): void => {
    if (visitedDirectories.has(realDirectory)) return;
    visitedDirectories.add(realDirectory);
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = join(directory, entry.name);
      if (entry.isSymbolicLink()) {
        if (shouldIgnore(path, entry.name)) continue;
        if (rejectSymlink) {
          rejectSymlink(path);
          continue;
        }
        try {
          const realPath = realpathSync(path);
          if (!isInsideRoot(realPath)) {
            reportRejectedSymlink(path);
            continue;
          }
          const stats = statSync(path);
          if (stats.isFile()) {
            files.push(path);
          } else if (stats.isDirectory() && !shouldIgnore(path, entry.name)) {
            visit(path, realPath);
          } else {
            reportRejectedSymlink(path);
          }
        } catch {
          reportRejectedSymlink(path);
        }
        continue;
      }
      if (entry.isDirectory()) {
        if (!shouldIgnore(path, entry.name)) {
          visit(path, realpathSync(path));
        }
        continue;
      }
      if (entry.isFile()) {
        files.push(path);
      }
    }
  };
  visit(root);
  return files.sort();
}

export function readText(path: string): string {
  return readFileSync(path, "utf8");
}

export function isRegularFile(path: string): boolean {
  try {
    return statSync(path).isFile();
  } catch {
    return false;
  }
}

export function isDirectRegularFile(path: string): boolean {
  try {
    return lstatSync(path).isFile();
  } catch {
    return false;
  }
}

export function relativePath(path: string, repoRoot: string = REPO_ROOT): string {
  return relative(repoRoot, path).split("\\").join("/");
}

export function isPathInside(root: string, path: string): boolean {
  const absoluteRoot = resolve(root);
  const absolutePath = resolve(path);
  if (absolutePath !== absoluteRoot && !absolutePath.startsWith(`${absoluteRoot}${sep}`)) {
    return false;
  }
  let current = absoluteRoot;
  try {
    if (lstatSync(current).isSymbolicLink()) return false;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") return false;
  }
  const parts = relative(absoluteRoot, absolutePath).split(sep).filter(Boolean);
  for (const part of parts) {
    current = join(current, part);
    try {
      if (lstatSync(current).isSymbolicLink()) return false;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") break;
      return false;
    }
  }
  return true;
}

export function resolveExistingFile(candidates: string[]): string | undefined {
  return candidates.find((candidate) => isRegularFile(candidate));
}
