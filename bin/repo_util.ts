import { existsSync, lstatSync, readdirSync, readFileSync, realpathSync, statSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");

export type GitResult = {
  exitCode: number;
  stdout: string;
  stderr: string;
};

function decode(output: Uint8Array): string {
  return new TextDecoder().decode(output);
}

export function runGit(args: string[], cwd: string = REPO_ROOT): GitResult {
  const result = Bun.spawnSync(["git", ...args], {
    cwd,
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
  const isInsideRoot = (path: string): boolean => path === rootRealPath || path.startsWith(`${rootRealPath}${sep}`);
  const visit = (directory: string, realDirectory: string = realpathSync(directory)): void => {
    if (visitedDirectories.has(realDirectory)) return;
    visitedDirectories.add(realDirectory);
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = join(directory, entry.name);
      if (entry.isSymbolicLink()) {
        try {
          const realPath = realpathSync(path);
          if (!isInsideRoot(realPath)) {
            options.onRejectedSymlink?.(path);
            continue;
          }
          const stats = statSync(path);
          if (stats.isFile()) {
            files.push(path);
          } else if (stats.isDirectory() && !ignored.has(entry.name)) {
            visit(path, realPath);
          } else {
            options.onRejectedSymlink?.(path);
          }
        } catch {
          options.onRejectedSymlink?.(path);
        }
        continue;
      }
      if (entry.isDirectory()) {
        if (!ignored.has(entry.name)) {
          visit(path);
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

export function relativePath(path: string, repoRoot: string = REPO_ROOT): string {
  return relative(repoRoot, path).split("\\").join("/");
}

export function resolveExistingFile(candidates: string[]): string | undefined {
  return candidates.find((candidate) => isRegularFile(candidate));
}
